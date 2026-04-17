package api

import (
	"context"
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	arcbackend "github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend/arc"
	azurebackend "github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend/azurefunctions"
	cloudbackend "github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend/cloudrun"
	lambdabackend "github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend/lambda"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/config"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

func newService() *Service {
	cfg := config.Default()
	for index, pool := range cfg.Pools {
		if pool.Name != model.PoolLite {
			continue
		}
		lambdaCfg := pool.Backends[model.BackendLambda]
		lambdaCfg.Enabled = true
		pool.Backends[model.BackendLambda] = lambdaCfg
		cfg.Pools[index] = pool
	}

	return NewService(
		cfg,
		backend.NewRegistry(
			arcbackend.New(),
			lambdabackend.New(),
			cloudbackend.New(),
			azurebackend.New(),
		),
	)
}

func TestAllocateReturnsRunnerLabel(t *testing.T) {
	service := newService()

	allocation, err := service.Allocate(context.Background(), model.AllocationRequest{Pool: model.PoolFull})
	if err != nil {
		t.Fatalf("allocate failed: %v", err)
	}

	if allocation.SelectedBackend != model.BackendARC {
		t.Fatalf("expected ARC backend, got %s", allocation.SelectedBackend)
	}

	if allocation.RunnerLabel == "" {
		t.Fatal("expected non-empty runner label")
	}
}

func TestAllocateRespectsPinnedBackendTimeoutLimit(t *testing.T) {
	service := newService()
	backend := model.BackendLambda

	if _, err := service.Allocate(context.Background(), model.AllocationRequest{
		Pool:       model.PoolLite,
		Backend:    &backend,
		JobTimeout: 30 * time.Minute,
	}); err == nil {
		t.Fatal("expected timeout limit validation to fail")
	}
}

func TestCancelReleasesCapacity(t *testing.T) {
	service := newService()
	backend := model.BackendLambda

	first, err := service.Allocate(context.Background(), model.AllocationRequest{
		Pool:       model.PoolLite,
		Backend:    &backend,
		JobTimeout: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("first allocation failed: %v", err)
	}

	if _, ok := service.Cancel(first.ID); !ok {
		t.Fatal("expected cancel to succeed")
	}

	if _, err := service.Allocate(context.Background(), model.AllocationRequest{
		Pool:       model.PoolLite,
		Backend:    &backend,
		JobTimeout: 5 * time.Minute,
	}); err != nil {
		t.Fatalf("expected capacity to be reusable after cancel: %v", err)
	}
}

func TestSweepExpiredMarksReadyAllocationsExpired(t *testing.T) {
	service := newService()
	service.now = func() time.Time { return time.Unix(1000, 0) }

	allocation, err := service.Allocate(context.Background(), model.AllocationRequest{
		Pool:       model.PoolFull,
		JobTimeout: time.Minute,
	})
	if err != nil {
		t.Fatalf("allocate failed: %v", err)
	}

	expired := service.SweepExpired(time.Unix(1100, 0))
	if expired != 1 {
		t.Fatalf("expected 1 expired allocation, got %d", expired)
	}

	updated, ok := service.Get(allocation.ID)
	if !ok {
		t.Fatal("allocation disappeared after sweep")
	}

	if updated.State != model.StateExpired {
		t.Fatalf("expected expired state, got %s", updated.State)
	}
}
