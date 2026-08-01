package capacity_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/capacity"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/capacity/fakecapacity"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/config"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

func TestRefreshUsesFakeCapacityBackends(t *testing.T) {
	cfg := config.Default()
	for i := range cfg.Pools {
		if cfg.Pools[i].Name != model.PoolLite {
			continue
		}
		codebuildCfg := cfg.Pools[i].Backends[model.BackendCodeBuild]
		codebuildCfg.Enabled = true
		cfg.Pools[i].Backends[model.BackendCodeBuild] = codebuildCfg
		arcCfg := cfg.Pools[i].Backends[model.BackendARC]
		arcCfg.Enabled = true
		cfg.Pools[i].Backends[model.BackendARC] = arcCfg
	}

	codebuild := fakecapacity.NewBackend(model.BackendCodeBuild, fakecapacity.Free(5, 2))
	// ARC intentionally omitted from reporter → no snapshot written.
	reporter := fakecapacity.MapReporter{
		model.BackendCodeBuild: codebuild,
	}

	manager := capacity.NewManager()
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	capacity.Refresh(context.Background(), manager, reporter, cfg, now)

	snap, ok := manager.Get(model.BackendCodeBuild)
	if !ok {
		t.Fatal("expected codebuild snapshot after refresh")
	}
	if snap.Source != "live" || snap.Err != "" || snap.Status.MaxRunners != 5 {
		t.Fatalf("unexpected live snapshot: %+v", snap)
	}
	if !snap.UpdatedAt.Equal(now) {
		t.Fatalf("expected UpdatedAt=%s, got %s", now, snap.UpdatedAt)
	}
	if _, ok := manager.Get(model.BackendARC); ok {
		t.Fatal("arc has no CapacityBackend; expected no snapshot")
	}
}

func TestRefreshPreservesLastGoodOnProbeError(t *testing.T) {
	cfg := config.Default()
	for i := range cfg.Pools {
		if cfg.Pools[i].Name != model.PoolLite {
			continue
		}
		for name, backendCfg := range cfg.Pools[i].Backends {
			backendCfg.Enabled = name == model.BackendCodeBuild
			cfg.Pools[i].Backends[name] = backendCfg
		}
	}

	codebuild := fakecapacity.NewBackend(model.BackendCodeBuild, fakecapacity.Free(4, 3))
	reporter := fakecapacity.MapReporter{model.BackendCodeBuild: codebuild}
	manager := capacity.NewManager()
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	capacity.Refresh(context.Background(), manager, reporter, cfg, now)
	codebuild.SetError(errors.New("simulated probe failure"))
	later := now.Add(30 * time.Second)
	capacity.Refresh(context.Background(), manager, reporter, cfg, later)

	snap, ok := manager.Get(model.BackendCodeBuild)
	if !ok {
		t.Fatal("expected snapshot after failed refresh")
	}
	if snap.Source != "error" || snap.Err == "" {
		t.Fatalf("expected error source, got %+v", snap)
	}
	if snap.Status.MaxRunners != 4 || backend.FreeSlots(snap.Status) != 3 {
		t.Fatalf("expected last-good counters preserved, got %+v", snap.Status)
	}
	if !snap.UpdatedAt.Equal(later) {
		t.Fatalf("expected UpdatedAt advanced to %s, got %s", later, snap.UpdatedAt)
	}
}

func TestRefreshSkipsDisabledBackends(t *testing.T) {
	cfg := config.Default()
	for i := range cfg.Pools {
		if cfg.Pools[i].Name != model.PoolLite {
			continue
		}
		for name, backendCfg := range cfg.Pools[i].Backends {
			backendCfg.Enabled = false
			cfg.Pools[i].Backends[name] = backendCfg
		}
	}

	codebuild := fakecapacity.NewBackend(model.BackendCodeBuild, fakecapacity.Free(2, 2))
	reporter := fakecapacity.MapReporter{model.BackendCodeBuild: codebuild}
	manager := capacity.NewManager()
	capacity.Refresh(context.Background(), manager, reporter, cfg, time.Now().UTC())

	if snaps := manager.Snapshot(); len(snaps) != 0 {
		t.Fatalf("expected no snapshots for disabled backends, got %+v", snaps)
	}
}
