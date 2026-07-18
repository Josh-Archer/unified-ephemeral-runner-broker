package externaldispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/runtime"
)

const (
	secretKeyDispatchURL          = "dispatch_url"
	secretKeyCleanupURL           = "cleanup_url"
	secretKeyHealthURL            = "health_url"
	secretKeyDispatchToken        = "dispatch_token"
	defaultDispatchTimeout        = 20 * time.Second
	azureFunctionsDispatchTimeout = 90 * time.Second
)

type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

type Backend struct {
	name    model.BackendName
	cfg     model.BrokerConfig
	secrets runtime.SecretReader
	client  HTTPClient
}

type dispatchRequest struct {
	Action            string         `json:"action"`
	Backend           string         `json:"backend"`
	AllocationID      string         `json:"allocation_id"`
	Pool              string         `json:"pool"`
	RunnerLabel       string         `json:"runner_label"`
	RunnerName        string         `json:"runner_name"`
	RunnerLabels      []string       `json:"runner_labels"`
	RequestedLabels   []string       `json:"requested_labels,omitempty"`
	LaunchMode        string         `json:"launch_mode,omitempty"`
	JobTimeout        string         `json:"job_timeout"`
	JobTimeoutSeconds int64          `json:"job_timeout_seconds"`
	GitHub            dispatchGitHub `json:"github"`
}

type dispatchGitHub struct {
	ScopeType    string `json:"scope_type"`
	Organization string `json:"organization,omitempty"`
	Owner        string `json:"owner,omitempty"`
	Repository   string `json:"repository,omitempty"`
	TargetURL    string `json:"target_url"`
	RunnerGroup  string `json:"runner_group,omitempty"`
}

type dispatchResponse struct {
	RunnerLabel string            `json:"runner_label"`
	Metadata    map[string]string `json:"metadata"`
	StatusURL   string            `json:"status_url"`
	DetailsURL  string            `json:"details_url"`
	ExecutionID string            `json:"execution_id"`
}

// cleanupRequest is the JSON body POSTed to cleanup_url (or derived cleanup endpoint).
// Launchers should treat cleanup as idempotent: 200/204 and 404 are success.
type cleanupRequest struct {
	Action         string            `json:"action"`
	Backend        string            `json:"backend"`
	AllocationID   string            `json:"allocation_id"`
	CorrelationID  string            `json:"correlation_id,omitempty"`
	Pool           string            `json:"pool"`
	RunnerLabel    string            `json:"runner_label"`
	State          string            `json:"state"`
	Error          string            `json:"error,omitempty"`
	Metadata       map[string]string `json:"metadata,omitempty"`
}

func New(name model.BackendName, cfg model.BrokerConfig, secrets runtime.SecretReader) *Backend {
	return &Backend{
		name:    name,
		cfg:     cfg,
		secrets: secrets,
		client: &http.Client{
			Timeout: dispatchTimeout(name),
		},
	}
}

func dispatchTimeout(name model.BackendName) time.Duration {
	switch name {
	case model.BackendAzureFunctions:
		return azureFunctionsDispatchTimeout
	default:
		return defaultDispatchTimeout
	}
}

func (b *Backend) Name() model.BackendName {
	return b.name
}

func (b *Backend) Provision(ctx context.Context, request model.AllocationRequest, allocation model.AllocationStatus) (backend.ProvisionedRunner, error) {
	pool, err := resolvePool(b.cfg, allocation.Pool)
	if err != nil {
		return backend.ProvisionedRunner{}, err
	}

	backendCfg, ok := pool.Backends[b.name]
	if !ok {
		return backend.ProvisionedRunner{}, fmt.Errorf("backend %s is not configured for pool %s", b.name, pool.Name)
	}
	secretRef := strings.TrimSpace(backendCfg.SecretRef)
	if secretRef == "" {
		return backend.ProvisionedRunner{}, fmt.Errorf("backend %s is missing secretRef", b.name)
	}

	secretData, err := b.secrets.ReadSecret(ctx, secretRef)
	if err != nil {
		return backend.ProvisionedRunner{}, fmt.Errorf("read backend secret %s: %w", secretRef, err)
	}

	dispatchURL := strings.TrimSpace(secretData[secretKeyDispatchURL])
	if dispatchURL == "" {
		return backend.ProvisionedRunner{}, fmt.Errorf("backend secret %s is missing %q", secretRef, secretKeyDispatchURL)
	}
	dispatchToken := strings.TrimSpace(secretData[secretKeyDispatchToken])

	targetURL, err := b.cfg.GitHub.Scope.TargetURL()
	if err != nil {
		return backend.ProvisionedRunner{}, err
	}

	runnerLabel := backend.DefaultRunnerLabel(b.name, allocation.ID)
	payload := dispatchRequest{
		Action:            "dispatch",
		Backend:           string(b.name),
		AllocationID:      allocation.ID,
		Pool:              string(pool.Name),
		RunnerLabel:       runnerLabel,
		RunnerName:        runnerLabel,
		RunnerLabels:      combineLabels(pool.Labels, allocation.RequestedLabels, runnerLabel),
		RequestedLabels:   append([]string(nil), allocation.RequestedLabels...),
		LaunchMode:        strings.TrimSpace(allocation.Metadata[backend.MetadataLaunchModeKey]),
		JobTimeout:        request.JobTimeout.String(),
		JobTimeoutSeconds: int64(request.JobTimeout / time.Second),
		GitHub: dispatchGitHub{
			ScopeType:    b.cfg.GitHub.Scope.Type,
			Organization: b.cfg.GitHub.Scope.Organization,
			Owner:        b.cfg.GitHub.Scope.Owner,
			Repository:   b.cfg.GitHub.Scope.Repository,
			TargetURL:    targetURL,
			RunnerGroup:  b.cfg.GitHub.Scope.RunnerGroup(pool.Name),
		},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return backend.ProvisionedRunner{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, dispatchURL, bytes.NewReader(body))
	if err != nil {
		return backend.ProvisionedRunner{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-UECB-Backend", string(b.name))
	if dispatchToken != "" {
		req.Header.Set("Authorization", "Bearer "+dispatchToken)
	}

	resp, err := b.client.Do(req)
	if err != nil {
		return backend.ProvisionedRunner{}, classifyDispatchError(b.name, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message, readErr := readErrorMessage(resp.Body)
		reason := classifyStatus(resp.StatusCode)
		baseErr := fmt.Errorf("dispatch backend %s: unexpected status %d", b.name, resp.StatusCode)
		if readErr != nil {
			if reason != "" {
				return backend.ProvisionedRunner{}, backend.NewClassifiedError(reason, baseErr)
			}
			return backend.ProvisionedRunner{}, baseErr
		}
		if message == "" {
			if reason != "" {
				return backend.ProvisionedRunner{}, backend.NewClassifiedError(reason, baseErr)
			}
			return backend.ProvisionedRunner{}, baseErr
		}
		err := fmt.Errorf("dispatch backend %s: %s", b.name, message)
		if reason != "" {
			return backend.ProvisionedRunner{}, backend.NewClassifiedError(reason, err)
		}
		return backend.ProvisionedRunner{}, err
	}

	metadata := map[string]string{}
	if resp.ContentLength != 0 {
		var payload dispatchResponse
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil && err != io.EOF {
			return backend.ProvisionedRunner{}, fmt.Errorf("decode backend %s response: %w", b.name, err)
		}
		if strings.TrimSpace(payload.RunnerLabel) != "" {
			runnerLabel = strings.TrimSpace(payload.RunnerLabel)
		}
		for key, value := range payload.Metadata {
			metadata[key] = value
		}
		if strings.TrimSpace(payload.StatusURL) != "" {
			metadata["status_url"] = strings.TrimSpace(payload.StatusURL)
		}
		if strings.TrimSpace(payload.DetailsURL) != "" {
			metadata["details_url"] = strings.TrimSpace(payload.DetailsURL)
		}
		if strings.TrimSpace(payload.ExecutionID) != "" {
			metadata["execution_id"] = strings.TrimSpace(payload.ExecutionID)
		}
	}

	if runnerLabel == "" {
		runnerLabel = backend.DefaultRunnerLabel(b.name, allocation.ID)
	}

	if metadata["target_url"] == "" {
		metadata["target_url"] = targetURL
	}
	metadata["dispatch_url"] = dispatchURL
	metadata["scope_type"] = strings.TrimSpace(b.cfg.GitHub.Scope.Type)

	return backend.ProvisionedRunner{
		RunnerLabel: runnerLabel,
		Metadata:    metadata,
	}, nil
}

func (b *Backend) Probe(ctx context.Context, pool model.PoolConfig, cfg model.BackendConfig) error {
	secretRef := strings.TrimSpace(cfg.SecretRef)
	if secretRef == "" {
		return fmt.Errorf("backend %s is missing secretRef", b.name)
	}
	secretData, err := b.secrets.ReadSecret(ctx, secretRef)
	if err != nil {
		return err
	}
	dispatchURL := strings.TrimSpace(secretData[secretKeyDispatchURL])
	if dispatchURL == "" {
		return fmt.Errorf("backend secret %s is missing %q", secretRef, secretKeyDispatchURL)
	}
	healthURL := strings.TrimSpace(secretData[secretKeyHealthURL])
	if healthURL == "" {
		return fmt.Errorf("backend secret %s is missing %q", secretRef, secretKeyHealthURL)
	}
	dispatchToken := strings.TrimSpace(secretData[secretKeyDispatchToken])
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-UECB-Backend", string(b.name))
	if dispatchToken != "" {
		req.Header.Set("Authorization", "Bearer "+dispatchToken)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return classifyDispatchError(b.name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	reason := classifyStatus(resp.StatusCode)
	err = fmt.Errorf("probe backend %s: unexpected status %d for pool %s", b.name, resp.StatusCode, pool.Name)
	if reason != "" {
		return backend.NewClassifiedError(reason, err)
	}
	return err
}

// Cleanup tears down provider-side runners for a terminal allocation.
//
// Contract:
//   - Optional secret key cleanup_url. When set, the broker POSTs a cleanup JSON
//     body to that URL with the same Bearer dispatch_token used for dispatch.
//   - When cleanup_url is absent, Cleanup logs and returns nil so capacity release
//     still succeeds (launchers that do not implement teardown remain compatible).
//   - Idempotent responses: HTTP 2xx and 404 are treated as success.
func (b *Backend) Cleanup(ctx context.Context, status model.AllocationStatus) error {
	pool, err := resolvePool(b.cfg, status.Pool)
	if err != nil {
		// Soft-skip: release capacity even if pool config has been removed.
		log.Printf("externaldispatch cleanup skipped for allocation %s backend %s: %v", status.ID, b.name, err)
		return nil
	}

	backendCfg, ok := pool.Backends[b.name]
	if !ok {
		log.Printf("externaldispatch cleanup skipped for allocation %s: backend %s not configured for pool %s", status.ID, b.name, pool.Name)
		return nil
	}
	secretRef := strings.TrimSpace(backendCfg.SecretRef)
	if secretRef == "" {
		log.Printf("externaldispatch cleanup skipped for allocation %s backend %s: missing secretRef", status.ID, b.name)
		return nil
	}

	secretData, err := b.secrets.ReadSecret(ctx, secretRef)
	if err != nil {
		return fmt.Errorf("read backend secret %s: %w", secretRef, err)
	}

	cleanupURL := strings.TrimSpace(secretData[secretKeyCleanupURL])
	if cleanupURL == "" {
		log.Printf("externaldispatch cleanup skipped for allocation %s backend %s: secret %s has no %q", status.ID, b.name, secretRef, secretKeyCleanupURL)
		return nil
	}

	dispatchToken := strings.TrimSpace(secretData[secretKeyDispatchToken])
	runnerLabel := strings.TrimSpace(status.RunnerLabel)
	if runnerLabel == "" {
		runnerLabel = backend.DefaultRunnerLabel(b.name, status.ID)
	}

	payload := cleanupRequest{
		Action:        "cleanup",
		Backend:       string(b.name),
		AllocationID:  status.ID,
		CorrelationID: strings.TrimSpace(status.CorrelationID),
		Pool:          string(status.Pool),
		RunnerLabel:   runnerLabel,
		State:         string(status.State),
		Error:         strings.TrimSpace(status.Error),
		Metadata:      cloneMetadata(status.Metadata),
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cleanupURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-UECB-Backend", string(b.name))
	if dispatchToken != "" {
		req.Header.Set("Authorization", "Bearer "+dispatchToken)
	}

	resp, err := b.client.Do(req)
	if err != nil {
		return classifyDispatchError(b.name, err)
	}
	defer resp.Body.Close()

	// Idempotent cleanup: success and not-found are both OK.
	if (resp.StatusCode >= 200 && resp.StatusCode < 300) || resp.StatusCode == http.StatusNotFound {
		return nil
	}

	message, readErr := readErrorMessage(resp.Body)
	baseErr := fmt.Errorf("cleanup backend %s: unexpected status %d", b.name, resp.StatusCode)
	if readErr != nil || message == "" {
		return baseErr
	}
	return fmt.Errorf("cleanup backend %s: %s", b.name, message)
}

func cloneMetadata(metadata map[string]string) map[string]string {
	if len(metadata) == 0 {
		return nil
	}
	result := make(map[string]string, len(metadata))
	for key, value := range metadata {
		result[key] = value
	}
	return result
}

func classifyDispatchError(name model.BackendName, err error) error {
	reason := backend.FailureReasonTransport
	if errors.Is(err, context.DeadlineExceeded) {
		reason = backend.FailureReasonTimeout
	} else {
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			reason = backend.FailureReasonTimeout
		}
	}
	return backend.NewClassifiedError(reason, fmt.Errorf("dispatch backend %s: %w", name, err))
}

func classifyStatus(status int) string {
	switch {
	case status == http.StatusTooManyRequests:
		return backend.FailureReasonThrottled
	case status >= 500:
		return backend.FailureReasonServerError
	default:
		return ""
	}
}

func resolvePool(cfg model.BrokerConfig, name model.PoolName) (model.PoolConfig, error) {
	for _, pool := range cfg.Pools {
		if pool.Name == name {
			return pool, nil
		}
	}
	return model.PoolConfig{}, fmt.Errorf("pool %s is not configured", name)
}

func combineLabels(base []string, requested []string, runnerLabel string) []string {
	seen := map[string]struct{}{}
	labels := make([]string, 0, len(base)+len(requested)+1)
	add := func(value string) {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			return
		}
		if _, ok := seen[trimmed]; ok {
			return
		}
		seen[trimmed] = struct{}{}
		labels = append(labels, trimmed)
	}

	for _, value := range base {
		add(value)
	}
	for _, value := range requested {
		add(value)
	}
	add(runnerLabel)
	return labels
}

func readErrorMessage(reader io.Reader) (string, error) {
	body, err := io.ReadAll(io.LimitReader(reader, 16*1024))
	if err != nil {
		return "", err
	}

	var payload struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &payload) == nil && strings.TrimSpace(payload.Error) != "" {
		return strings.TrimSpace(payload.Error), nil
	}

	return strings.TrimSpace(string(body)), nil
}
