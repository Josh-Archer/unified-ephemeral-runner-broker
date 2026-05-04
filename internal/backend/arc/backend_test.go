package arc

import (
	"context"
	"strings"
	"testing"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/config"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

func TestProvisionReturnsConfiguredRunnerLabel(t *testing.T) {
	cfg := config.Default()
	lite := &cfg.Pools[1]
	arcCfg := lite.Backends[model.BackendARC]
	arcCfg.RunnerLabel = "example-arc-scale-set"
	arcCfg.Template = "arc-lite"
	lite.Backends[model.BackendARC] = arcCfg

	provisioned, err := New(cfg).Provision(context.Background(), model.AllocationRequest{
		Pool: model.PoolLite,
	}, model.AllocationStatus{
		ID:   "abc123",
		Pool: model.PoolLite,
	})
	if err != nil {
		t.Fatalf("provision failed: %v", err)
	}
	if provisioned.RunnerLabel != "example-arc-scale-set" {
		t.Fatalf("runner label = %q, want configured scale-set label", provisioned.RunnerLabel)
	}
	if strings.HasPrefix(provisioned.RunnerLabel, "uecb-arc-") {
		t.Fatalf("ARC returned synthetic label %q", provisioned.RunnerLabel)
	}
	if provisioned.Metadata["runner_label"] != "example-arc-scale-set" {
		t.Fatalf("metadata runner_label = %q, want example-arc-scale-set", provisioned.Metadata["runner_label"])
	}
}

func TestProvisionFallsBackToTemplateForBackwardCompatibility(t *testing.T) {
	cfg := config.Default()

	provisioned, err := New(cfg).Provision(context.Background(), model.AllocationRequest{
		Pool: model.PoolLite,
	}, model.AllocationStatus{
		ID:   "abc123",
		Pool: model.PoolLite,
	})
	if err != nil {
		t.Fatalf("provision failed: %v", err)
	}
	if provisioned.RunnerLabel != "arc-lite" {
		t.Fatalf("runner label = %q, want template fallback", provisioned.RunnerLabel)
	}
}

func TestProvisionRequiresConfiguredRunnerTarget(t *testing.T) {
	cfg := config.Default()
	lite := &cfg.Pools[1]
	arcCfg := lite.Backends[model.BackendARC]
	arcCfg.RunnerLabel = ""
	arcCfg.Template = ""
	lite.Backends[model.BackendARC] = arcCfg

	_, err := New(cfg).Provision(context.Background(), model.AllocationRequest{
		Pool: model.PoolLite,
	}, model.AllocationStatus{
		ID:   "abc123",
		Pool: model.PoolLite,
	})
	if err == nil {
		t.Fatal("expected missing runner target to fail")
	}
	if !strings.Contains(err.Error(), "requires runnerLabel or template") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestProvisionDoesNotUseDefaultARCLabel(t *testing.T) {
	cfg := config.Default()

	provisioned, err := New(cfg).Provision(context.Background(), model.AllocationRequest{
		Pool: model.PoolLite,
	}, model.AllocationStatus{
		ID:   "abc123",
		Pool: model.PoolLite,
	})
	if err != nil {
		t.Fatalf("provision failed: %v", err)
	}

	defaultLabel := backend.DefaultRunnerLabel(model.BackendARC, "abc123")
	if provisioned.RunnerLabel == defaultLabel {
		t.Fatalf("ARC returned default synthetic label %q", defaultLabel)
	}
}
