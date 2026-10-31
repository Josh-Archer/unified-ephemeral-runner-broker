package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/config"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/scheduler"
)

func drainService(t *testing.T) *Service {
	t.Helper()
	cfg := config.Default()
	cfg.Broker.StateStore = model.StateStoreConfig{Type: "memory"}
	cfg.Broker.Queue = model.AdmissionQueueConfig{Enabled: true, RetryAfter: time.Millisecond, MaxAttempts: 5}
	cfg.Pools[0].Backends = map[model.BackendName]model.BackendConfig{
		model.BackendARC: {Enabled: true, Healthy: true, MaxRunners: 2, Template: "arc-full"},
	}
	return NewService(cfg, backend.NewRegistry(testBackend{name: model.BackendARC}), nil)
}

func TestDrainRejectsNewAllocationsAsRetryableCapacity(t *testing.T) {
	svc := drainService(t)
	svc.BeginDrain()
	_, err := svc.Allocate(context.Background(), model.AllocationRequest{Pool: model.PoolFull, JobTimeout: time.Minute})
	if !errors.Is(err, ErrDraining) || !errors.Is(err, scheduler.ErrNoCapacity) {
		t.Fatalf("expected draining no-capacity error, got %v", err)
	}
	if n := len(svc.store.List()); n != 0 {
		t.Fatalf("draining must not reserve or provision, got %d allocations", n)
	}
}

func TestDrainQueuesWebhookDemandWithoutProvisioning(t *testing.T) {
	svc := drainService(t)
	pool, err := svc.resolvePool(model.PoolFull)
	if err != nil {
		t.Fatal(err)
	}
	svc.BeginDrain()
	status, _, err := svc.AllocateForWorkflowJob(context.Background(), pool, 901, "example/project")
	if err != nil {
		t.Fatalf("webhook demand should be queued, got %v", err)
	}
	if status.State != model.StatePending {
		t.Fatalf("expected pending, got %s", status.State)
	}
	if n := svc.ReconcileQueue(context.Background(), time.Now().Add(time.Minute)); n != 0 {
		t.Fatalf("queue must not retry while draining, updated %d", n)
	}
	if got, _ := svc.Get(status.ID); got.State != model.StatePending {
		t.Fatalf("pending demand must survive for the next process, got %s", got.State)
	}
}
