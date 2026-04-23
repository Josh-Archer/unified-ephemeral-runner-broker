package externaldispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/runtime"
)

const (
	secretKeyDispatchURL   = "dispatch_url"
	secretKeyDispatchToken = "dispatch_token"
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
	Action        string          `json:"action"`
	Backend       string          `json:"backend"`
	AllocationID  string          `json:"allocation_id"`
	Pool          string          `json:"pool"`
	RunnerLabel   string          `json:"runner_label"`
	RunnerName    string          `json:"runner_name"`
	RunnerLabels  []string        `json:"runner_labels"`
	RequestedLabels []string      `json:"requested_labels,omitempty"`
	JobTimeout    string          `json:"job_timeout"`
	JobTimeoutSeconds int64       `json:"job_timeout_seconds"`
	GitHub        dispatchGitHub  `json:"github"`
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

func New(name model.BackendName, cfg model.BrokerConfig, secrets runtime.SecretReader) *Backend {
	return &Backend{
		name:    name,
		cfg:     cfg,
		secrets: secrets,
		client: &http.Client{
			Timeout: 20 * time.Second,
		},
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
		Action:          "dispatch",
		Backend:         string(b.name),
		AllocationID:    allocation.ID,
		Pool:            string(pool.Name),
		RunnerLabel:     runnerLabel,
		RunnerName:      runnerLabel,
		RunnerLabels:    combineLabels(pool.Labels, allocation.RequestedLabels, runnerLabel),
		RequestedLabels: append([]string(nil), allocation.RequestedLabels...),
		JobTimeout:      request.JobTimeout.String(),
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
		return backend.ProvisionedRunner{}, fmt.Errorf("dispatch backend %s: %w", b.name, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message, readErr := readErrorMessage(resp.Body)
		if readErr != nil {
			return backend.ProvisionedRunner{}, fmt.Errorf("dispatch backend %s: unexpected status %d", b.name, resp.StatusCode)
		}
		if message == "" {
			return backend.ProvisionedRunner{}, fmt.Errorf("dispatch backend %s: unexpected status %d", b.name, resp.StatusCode)
		}
		return backend.ProvisionedRunner{}, fmt.Errorf("dispatch backend %s: %s", b.name, message)
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
