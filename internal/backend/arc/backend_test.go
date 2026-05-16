package arc

import (
	"context"
	"strings"
	"testing"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/config"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

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

	provisioned, err := New(cfg).Provision(context.Background(), model.AllocationRequest{
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

		provisioned, err := New(cfg).Provision(context.Background(), model.AllocationRequest{
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

		provisioned, err := New(cfg).Provision(context.Background(), model.AllocationRequest{
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
