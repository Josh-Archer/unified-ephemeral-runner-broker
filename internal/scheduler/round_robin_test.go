package scheduler

import (
	"testing"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/config"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

func poolByName(name model.PoolName) model.PoolConfig {
	for _, pool := range config.Default().Pools {
		if pool.Name == name {
			return pool
		}
	}
	panic("pool not found")
}

func TestRoundRobinRotatesAcrossHealthyLiteBackends(t *testing.T) {
	scheduler := NewRoundRobin()
	pool := poolByName(model.PoolLite)
	pool.Backends[model.BackendLambda] = model.BackendConfig{Enabled: true, Healthy: true, MaxRunners: 3}
	pool.Backends[model.BackendCloudRun] = model.BackendConfig{Enabled: true, Healthy: true, MaxRunners: 2}
	pool.Backends[model.BackendAzureFunctions] = model.BackendConfig{Enabled: true, Healthy: true, MaxRunners: 2}

	first, err := scheduler.Reserve(pool, nil)
	if err != nil {
		t.Fatalf("reserve #1 failed: %v", err)
	}
	second, err := scheduler.Reserve(pool, nil)
	if err != nil {
		t.Fatalf("reserve #2 failed: %v", err)
	}
	third, err := scheduler.Reserve(pool, nil)
	if err != nil {
		t.Fatalf("reserve #3 failed: %v", err)
	}

	if first != model.BackendARC || second != model.BackendLambda || third != model.BackendCloudRun {
		t.Fatalf("unexpected rotation order: %s %s %s", first, second, third)
	}
}

func TestRoundRobinSkipsUnhealthyBackend(t *testing.T) {
	scheduler := NewRoundRobin()
	pool := poolByName(model.PoolLite)
	pool.Backends[model.BackendLambda] = model.BackendConfig{Enabled: true, Healthy: false, MaxRunners: 1}
	pool.Backends[model.BackendCloudRun] = model.BackendConfig{Enabled: true, Healthy: true, MaxRunners: 1}

	first, err := scheduler.Reserve(pool, nil)
	if err != nil {
		t.Fatalf("reserve #1 failed: %v", err)
	}
	second, err := scheduler.Reserve(pool, nil)
	if err != nil {
		t.Fatalf("reserve #2 failed: %v", err)
	}

	if first != model.BackendARC || second != model.BackendCloudRun {
		t.Fatalf("expected unhealthy backend to be skipped, got %s %s", first, second)
	}
}

func TestRoundRobinPinnedBackendUsesCapacity(t *testing.T) {
	scheduler := NewRoundRobin()
	pool := poolByName(model.PoolLite)
	pool.Backends[model.BackendLambda] = model.BackendConfig{Enabled: true, Healthy: true, MaxRunners: 1}

	pinned := model.BackendLambda
	selected, err := scheduler.Reserve(pool, &pinned)
	if err != nil {
		t.Fatalf("reserve failed: %v", err)
	}
	if selected != model.BackendLambda {
		t.Fatalf("expected lambda, got %s", selected)
	}

	if _, err := scheduler.Reserve(pool, &pinned); err == nil {
		t.Fatal("expected capacity exhaustion for pinned backend")
	}
}

func TestRoundRobinReleaseReturnsCapacity(t *testing.T) {
	scheduler := NewRoundRobin()
	pool := poolByName(model.PoolLite)
	pool.Backends[model.BackendLambda] = model.BackendConfig{Enabled: true, Healthy: true, MaxRunners: 1}

	pinned := model.BackendLambda
	if _, err := scheduler.Reserve(pool, &pinned); err != nil {
		t.Fatalf("reserve failed: %v", err)
	}
	scheduler.Release(pool.Name, model.BackendLambda)
	if _, err := scheduler.Reserve(pool, &pinned); err != nil {
		t.Fatalf("expected capacity to be available after release: %v", err)
	}
}
