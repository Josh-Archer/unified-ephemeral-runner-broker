package tier

import (
	"sync"
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

func TestManagerStoresAndMarksStaleDecisions(t *testing.T) {
	manager := NewManager()
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	manager.SetDecision(Decision{
		Pool:      model.PoolLite,
		Backend:   model.BackendCodeBuild,
		State:     StateHealthy,
		Action:    ActionDisable,
		UpdatedAt: now,
	})

	got, ok := manager.Decision(model.PoolLite, model.BackendCodeBuild)
	if !ok {
		t.Fatal("expected cached decision")
	}
	if got.State != StateHealthy {
		t.Fatalf("unexpected state: %s", got.State)
	}

	manager.MarkStale(time.Minute, now.Add(2*time.Minute))
	got, ok = manager.Decision(model.PoolLite, model.BackendCodeBuild)
	if !ok || !got.Stale {
		t.Fatalf("expected stale decision, got %+v", got)
	}
}

func TestManagerConcurrentAccess(t *testing.T) {
	manager := NewManager()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			manager.SetDecision(Decision{Pool: model.PoolLite, Backend: model.BackendLambda, State: StateHealthy})
			_, _ = manager.Decision(model.PoolLite, model.BackendLambda)
		}()
	}
	wg.Wait()
	if len(manager.Snapshot()) != 1 {
		t.Fatalf("expected one cached decision, got %d", len(manager.Snapshot()))
	}
}
