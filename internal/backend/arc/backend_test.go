package arc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/config"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

type staticSecrets map[string]map[string]string

func (s staticSecrets) ReadSecret(_ context.Context, name string) (map[string]string, error) {
	values, ok := s[name]
	if !ok {
		return nil, context.DeadlineExceeded
	}
	copyValues := make(map[string]string, len(values))
	for key, value := range values {
		copyValues[key] = value
	}
	return copyValues, nil
}

func configureArcBackend(cfg model.BrokerConfig, poolName model.PoolName, mutate func(*model.BackendConfig)) model.BrokerConfig {
	for index := range cfg.Pools {
		if cfg.Pools[index].Name != poolName {
			continue
		}
		backendCfg := cfg.Pools[index].Backends[model.BackendARC]
		mutate(&backendCfg)
		cfg.Pools[index].Backends[model.BackendARC] = backendCfg
		break
	}
	return cfg
}

func TestProvisionReturnsConfiguredRunnerLabelAndUsefulMetadata(t *testing.T) {
	cfg := configureArcBackend(config.Default(), model.PoolLite, func(backendCfg *model.BackendConfig) {
		backendCfg.Enabled = true
		backendCfg.RunnerLabel = "arc-scale-set"
		backendCfg.Template = "arc-lite"
	})

	provisioned, err := New(cfg, nil).Provision(context.Background(), model.AllocationRequest{
		Pool: model.PoolLite,
	}, model.AllocationStatus{
		ID:   "arc-001",
		Pool: model.PoolLite,
	})
	if err != nil {
		t.Fatalf("provision failed: %v", err)
	}

	if got := provisioned.RunnerLabel; got != "arc-scale-set" {
		t.Fatalf("expected configured runner label, got %q", got)
	}

	wantMetadata := map[string]string{
		"pool":            string(model.PoolLite),
		"capability":      "full",
		"provisioner":     "arc-job",
		"lifecycle":       "ephemeral",
		"runner_label":    "arc-scale-set",
		"supports_docker": "true",
	}
	for key, want := range wantMetadata {
		if got := provisioned.Metadata[key]; got != want {
			t.Fatalf("expected metadata %q=%q, got %q", key, want, got)
		}
	}
}

func TestProvisionFallsBackToTemplateThenGeneratedLabel(t *testing.T) {
	t.Run("template", func(t *testing.T) {
		cfg := configureArcBackend(config.Default(), model.PoolLite, func(backendCfg *model.BackendConfig) {
			backendCfg.Enabled = true
			backendCfg.RunnerLabel = ""
			backendCfg.Template = "arc-lite-template"
		})

		provisioned, err := New(cfg, nil).Provision(context.Background(), model.AllocationRequest{
			Pool: model.PoolLite,
		}, model.AllocationStatus{
			ID:   "arc-002",
			Pool: model.PoolLite,
		})
		if err != nil {
			t.Fatalf("provision failed: %v", err)
		}

		if got := provisioned.RunnerLabel; got != "arc-lite-template" {
			t.Fatalf("expected template fallback label, got %q", got)
		}
		if got := provisioned.Metadata["runner_label"]; got != "arc-lite-template" {
			t.Fatalf("expected metadata runner_label to match template fallback, got %q", got)
		}
	})

	t.Run("generated", func(t *testing.T) {
		cfg := configureArcBackend(config.Default(), model.PoolLite, func(backendCfg *model.BackendConfig) {
			backendCfg.Enabled = true
			backendCfg.RunnerLabel = ""
			backendCfg.Template = ""
		})

		provisioned, err := New(cfg, nil).Provision(context.Background(), model.AllocationRequest{
			Pool: model.PoolLite,
		}, model.AllocationStatus{
			ID:   "arc-003",
			Pool: model.PoolLite,
		})
		if err != nil {
			t.Fatalf("provision failed: %v", err)
		}

		defaultLabel := backend.DefaultRunnerLabel(model.BackendARC, "arc-003")
		if got := provisioned.RunnerLabel; got != defaultLabel {
			t.Fatalf("expected generated fallback label, got %q", got)
		}
		if got := provisioned.Metadata["runner_label"]; got != defaultLabel {
			t.Fatalf("expected metadata runner_label to match generated label, got %q", got)
		}
		if !strings.HasPrefix(provisioned.RunnerLabel, "uecb-arc-") {
			t.Fatalf("expected generated ARC label prefix, got %q", provisioned.RunnerLabel)
		}
	})
}

func TestCapacityFromConfiguredScale(t *testing.T) {
	// Default config enables ARC on full (max 4) and lite (max 2).
	status, err := New(config.Default(), nil).Capacity(context.Background())
	if err != nil {
		t.Fatalf("capacity: %v", err)
	}
	if status.MaxRunners != 6 || status.ActiveRunners != 0 || status.PendingRunners != 0 || status.WarmRunners != 0 {
		t.Fatalf("unexpected capacity %+v", status)
	}
	if free := backend.FreeSlots(status); free != 6 {
		t.Fatalf("expected 6 free slots from summed configured scale, got %d", free)
	}
}

func TestCapacityFromCapacityURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("expected GET, got %s", r.Method)
		}
		if got := r.Header.Get("X-UECB-Backend"); got != "arc" {
			t.Fatalf("expected X-UECB-Backend=arc, got %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer arc-token" {
			t.Fatalf("expected bearer token, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"max_runners":5,"active_runners":2,"pending_runners":1,"warm_runners":0,"free_slots":2}`))
	}))
	defer server.Close()

	cfg := configureArcBackend(config.Default(), model.PoolFull, func(backendCfg *model.BackendConfig) {
		backendCfg.Enabled = true
		backendCfg.MaxRunners = 4
		backendCfg.SecretRef = "uecb-arc"
	})

	status, err := New(cfg, staticSecrets{
		"uecb-arc": {
			secretKeyCapacityURL:   server.URL,
			secretKeyDispatchToken: "arc-token",
		},
	}).Capacity(context.Background())
	if err != nil {
		t.Fatalf("capacity: %v", err)
	}
	if status.MaxRunners != 5 || status.ActiveRunners != 2 || status.PendingRunners != 1 || status.WarmRunners != 0 {
		t.Fatalf("unexpected capacity %+v", status)
	}
	if free := backend.FreeSlots(status); free != 2 {
		t.Fatalf("expected 2 free slots, got %d", free)
	}
}

func TestCapacityURLExhaustion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Full scale set: free_slots 0 with explicit counters.
		_, _ = w.Write([]byte(`{"max_runners":3,"active_runners":2,"pending_runners":1,"warm_runners":0,"free_slots":0}`))
	}))
	defer server.Close()

	cfg := configureArcBackend(config.Default(), model.PoolFull, func(backendCfg *model.BackendConfig) {
		backendCfg.Enabled = true
		backendCfg.MaxRunners = 10
		backendCfg.SecretRef = "uecb-arc"
	})

	status, err := New(cfg, staticSecrets{
		"uecb-arc": {secretKeyCapacityURL: server.URL},
	}).Capacity(context.Background())
	if err != nil {
		t.Fatalf("capacity: %v", err)
	}
	if free := backend.FreeSlots(status); free != 0 {
		t.Fatalf("expected exhausted free slots, got %d from %+v", free, status)
	}
}

func TestCapacityURLFreeSlotsOnlyExhaustion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// free_slots:0 with active work and no max reconstructs a full ceiling.
		_, _ = w.Write([]byte(`{"active_runners":2,"pending_runners":0,"warm_runners":0,"free_slots":0}`))
	}))
	defer server.Close()

	cfg := configureArcBackend(config.Default(), model.PoolLite, func(backendCfg *model.BackendConfig) {
		backendCfg.Enabled = true
		backendCfg.MaxRunners = 8
		backendCfg.SecretRef = "uecb-arc"
	})

	status, err := New(cfg, staticSecrets{
		"uecb-arc": {secretKeyCapacityURL: server.URL},
	}).Capacity(context.Background())
	if err != nil {
		t.Fatalf("capacity: %v", err)
	}
	if status.MaxRunners != 2 {
		t.Fatalf("expected reconstructed max_runners=2, got %+v", status)
	}
	if free := backend.FreeSlots(status); free != 0 {
		t.Fatalf("expected free_slots exhaustion, got %d from %+v", free, status)
	}
}

func TestCapacityFallsBackToConfigWhenCapacityURLMissing(t *testing.T) {
	cfg := configureArcBackend(config.Default(), model.PoolFull, func(backendCfg *model.BackendConfig) {
		backendCfg.Enabled = true
		backendCfg.MaxRunners = 6
		backendCfg.SecretRef = "uecb-arc"
	})
	// Disable lite so configured scale is only full's maxRunners.
	cfg = configureArcBackend(cfg, model.PoolLite, func(backendCfg *model.BackendConfig) {
		backendCfg.Enabled = false
	})

	status, err := New(cfg, staticSecrets{
		"uecb-arc": {"dispatch_url": "https://example.invalid/dispatch"},
	}).Capacity(context.Background())
	if err != nil {
		t.Fatalf("capacity: %v", err)
	}
	if status.MaxRunners != 6 || backend.FreeSlots(status) != 6 {
		t.Fatalf("expected config scale fallback, got %+v free=%d", status, backend.FreeSlots(status))
	}
}

func TestCapacityImplementsCapacityBackend(t *testing.T) {
	var _ backend.CapacityBackend = New(config.Default(), nil)
}
