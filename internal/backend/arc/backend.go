package arc

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/runtime"
)

const (
	secretKeyCapacityURL   = "capacity_url"
	secretKeyDispatchToken = "dispatch_token"
	defaultCapacityTimeout = 2 * time.Second
)

// HTTPClient is the subset of http.Client used for optional capacity_url probes.
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

type Backend struct {
	cfg     model.BrokerConfig
	secrets runtime.SecretReader
	client  HTTPClient
}

func New(cfg model.BrokerConfig, secrets runtime.SecretReader) *Backend {
	return &Backend{cfg: cfg, secrets: secrets}
}

// WithHTTPClient overrides the HTTP client used for capacity_url probes (tests).
func (b *Backend) WithHTTPClient(client HTTPClient) *Backend {
	b.client = client
	return b
}

func (b *Backend) Name() model.BackendName {
	return model.BackendARC
}

func (b *Backend) Provision(_ context.Context, request model.AllocationRequest, allocation model.AllocationStatus) (backend.ProvisionedRunner, error) {
	runnerLabel := b.runnerLabel(allocation.Pool, allocation.ID)
	return backend.ProvisionedRunner{
		RunnerLabel: runnerLabel,
		Metadata: map[string]string{
			"pool":            string(request.Pool),
			"capability":      "full",
			"provisioner":     "arc-job",
			"lifecycle":       "ephemeral",
			"runner_label":    runnerLabel,
			"supports_docker": "true",
		},
	}, nil
}

// Capacity reports ARC free slots using the same shape as HTTP capacity_url JSON.
//
// Resolution order:
//  1. Optional secret key capacity_url on any ARC pool secretRef (aligns with
//     home GitOps capacity feeds for runner scale set / node capacity).
//  2. Sum of configured maxRunners across enabled ARC pools (native scale
//     ceiling so live-capacity routing always has a first-class Capacity()
//     reading for multi-pool scale sets such as arc-full + arc-lite).
//
// A full provider returns counters with FreeSlots == 0 rather than an error.
func (b *Backend) Capacity(ctx context.Context) (backend.CapacityStatus, error) {
	if status, ok, err := b.probeCapacityURL(ctx); ok {
		return status, err
	}

	maxRunners := b.configuredScale()
	if maxRunners <= 0 {
		return backend.CapacityStatus{}, fmt.Errorf("backend %s has no configured maxRunners", model.BackendARC)
	}
	// Native scale: publish the configured runner ceiling with no in-flight
	// provider work reported. Local scheduler reservations remain the broker
	// concurrency authority; capacity_url can tighten this later.
	return backend.CapacityStatus{MaxRunners: maxRunners}, nil
}

func (b *Backend) configuredScale() int {
	total := 0
	for _, pool := range b.cfg.Pools {
		cfg, ok := pool.Backends[model.BackendARC]
		if !ok || !cfg.Enabled || cfg.MaxRunners <= 0 {
			continue
		}
		total += cfg.MaxRunners
	}
	if total > 0 {
		return total
	}
	// Fall back to any configured (disabled) pool so Capacity can still report.
	for _, pool := range b.cfg.Pools {
		if cfg, ok := pool.Backends[model.BackendARC]; ok && cfg.MaxRunners > 0 {
			return cfg.MaxRunners
		}
	}
	return 0
}

func (b *Backend) probeCapacityURL(ctx context.Context) (backend.CapacityStatus, bool, error) {
	if b.secrets == nil {
		return backend.CapacityStatus{}, false, nil
	}

	seen := map[string]struct{}{}
	var lastReadErr error
	for _, pool := range b.cfg.Pools {
		cfg, ok := pool.Backends[model.BackendARC]
		if !ok {
			continue
		}
		secretRef := strings.TrimSpace(cfg.SecretRef)
		if secretRef == "" {
			continue
		}
		if _, dup := seen[secretRef]; dup {
			continue
		}
		seen[secretRef] = struct{}{}

		secretData, err := b.secrets.ReadSecret(ctx, secretRef)
		if err != nil {
			lastReadErr = fmt.Errorf("read backend secret %s: %w", secretRef, err)
			continue
		}
		capacityURL := strings.TrimSpace(secretData[secretKeyCapacityURL])
		if capacityURL == "" {
			continue
		}
		dispatchToken := strings.TrimSpace(secretData[secretKeyDispatchToken])
		status, err := b.getCapacityURL(ctx, capacityURL, dispatchToken)
		return status, true, err
	}
	if lastReadErr != nil && len(seen) > 0 {
		// Secret refs were configured but none could be read and no other feed
		// succeeded; surface the failure so failureMode can apply.
		return backend.CapacityStatus{}, true, lastReadErr
	}
	return backend.CapacityStatus{}, false, nil
}

func (b *Backend) getCapacityURL(ctx context.Context, capacityURL, dispatchToken string) (backend.CapacityStatus, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, capacityURL, nil)
	if err != nil {
		return backend.CapacityStatus{}, err
	}
	req.Header.Set("X-UECB-Backend", string(model.BackendARC))
	if dispatchToken != "" {
		req.Header.Set("Authorization", "Bearer "+dispatchToken)
	}

	client := b.client
	if client == nil {
		client = &http.Client{Timeout: defaultCapacityTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return backend.CapacityStatus{}, fmt.Errorf("capacity backend %s: %w", model.BackendARC, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return backend.CapacityStatus{}, fmt.Errorf("capacity backend %s: unexpected status %d", model.BackendARC, resp.StatusCode)
	}

	status, err := backend.DecodeCapacityJSON(resp.Body)
	if err != nil {
		return backend.CapacityStatus{}, fmt.Errorf("decode backend %s capacity response: %w", model.BackendARC, err)
	}
	return status, nil
}

func (b *Backend) runnerLabel(poolName model.PoolName, allocationID string) string {
	if cfg, ok := b.backendConfig(poolName); ok {
		if runnerLabel := strings.TrimSpace(cfg.RunnerLabel); runnerLabel != "" {
			return runnerLabel
		}
		if template := strings.TrimSpace(cfg.Template); template != "" {
			return template
		}
	}

	return backend.DefaultRunnerLabel(model.BackendARC, allocationID)
}

func (b *Backend) backendConfig(poolName model.PoolName) (model.BackendConfig, bool) {
	for _, pool := range b.cfg.Pools {
		if pool.Name != poolName {
			continue
		}
		cfg, ok := pool.Backends[model.BackendARC]
		return cfg, ok
	}
	return model.BackendConfig{}, false
}


