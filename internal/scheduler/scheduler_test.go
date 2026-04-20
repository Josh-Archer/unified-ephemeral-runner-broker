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

func TestRoundRobinSkipsFullBackend(t *testing.T) {
	scheduler := NewRoundRobin()
	pool := poolByName(model.PoolLite)
	pool.Backends[model.BackendARC] = model.BackendConfig{Enabled: true, Healthy: true, MaxRunners: 1}
	pool.Backends[model.BackendLambda] = model.BackendConfig{Enabled: true, Healthy: true, MaxRunners: 1}
	pool.Backends[model.BackendCloudRun] = model.BackendConfig{Enabled: true, Healthy: true, MaxRunners: 1}

	first, err := scheduler.Reserve(pool, nil)
	if err != nil {
		t.Fatalf("reserve #1 failed: %v", err)
	}
	second, err := scheduler.Reserve(pool, nil)
	if err != nil {
		t.Fatalf("reserve #2 failed: %v", err)
	}

	if first != model.BackendARC || second != model.BackendLambda {
		t.Fatalf("expected full backend to be skipped, got %s %s", first, second)
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

func TestWeightedRoundRobinUsesWeights(t *testing.T) {
	scheduler := NewWeightedRoundRobin()
	pool := poolByName(model.PoolLite)
	pool.Scheduler = NameWeightedRoundRobin
	pool.Backends[model.BackendARC] = model.BackendConfig{Enabled: true, Healthy: true, MaxRunners: 1, Weight: 1}
	pool.Backends[model.BackendLambda] = model.BackendConfig{Enabled: true, Healthy: true, MaxRunners: 1, Weight: 3}

	want := []model.BackendName{
		model.BackendARC,
		model.BackendLambda,
		model.BackendLambda,
		model.BackendLambda,
	}

	for index, expected := range want {
		selected, err := scheduler.Reserve(pool, nil)
		if err != nil {
			t.Fatalf("reserve #%d failed: %v", index+1, err)
		}
		if selected != expected {
			t.Fatalf("reserve #%d selected %s, want %s", index+1, selected, expected)
		}
		scheduler.Release(pool.Name, selected)
	}
}

func TestWeightedRoundRobinSkipsUnhealthyBackendDeterministically(t *testing.T) {
	scheduler := NewWeightedRoundRobin()
	pool := poolByName(model.PoolLite)
	pool.Scheduler = NameWeightedRoundRobin
	pool.Backends[model.BackendARC] = model.BackendConfig{Enabled: false, Healthy: true, MaxRunners: 1, Weight: 1}
	pool.Backends[model.BackendLambda] = model.BackendConfig{Enabled: true, Healthy: false, MaxRunners: 1, Weight: 3}
	pool.Backends[model.BackendCloudRun] = model.BackendConfig{Enabled: true, Healthy: true, MaxRunners: 1, Weight: 2}

	selected, err := scheduler.Reserve(pool, nil)
	if err != nil {
		t.Fatalf("reserve failed: %v", err)
	}
	if selected != model.BackendCloudRun {
		t.Fatalf("expected deterministic fallback to cloud-run, got %s", selected)
	}
}

func TestWeightedRoundRobinSkipsFullBackendDeterministically(t *testing.T) {
	scheduler := NewWeightedRoundRobin()
	pool := poolByName(model.PoolLite)
	pool.Scheduler = NameWeightedRoundRobin
	pool.Backends[model.BackendARC] = model.BackendConfig{Enabled: false, Healthy: true, MaxRunners: 1, Weight: 1}
	pool.Backends[model.BackendLambda] = model.BackendConfig{Enabled: true, Healthy: true, MaxRunners: 1, Weight: 3}
	pool.Backends[model.BackendCloudRun] = model.BackendConfig{Enabled: true, Healthy: true, MaxRunners: 1, Weight: 2}

	first, err := scheduler.Reserve(pool, nil)
	if err != nil {
		t.Fatalf("reserve #1 failed: %v", err)
	}
	if first != model.BackendLambda {
		t.Fatalf("expected lambda first, got %s", first)
	}

	second, err := scheduler.Reserve(pool, nil)
	if err != nil {
		t.Fatalf("reserve #2 failed: %v", err)
	}
	if second != model.BackendCloudRun {
		t.Fatalf("expected deterministic fallback to cloud-run, got %s", second)
	}
}
