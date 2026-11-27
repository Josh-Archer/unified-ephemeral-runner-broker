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

func TestManagerPreservesRuleStateSinceAndCopiesReadings(t *testing.T) {
	m := NewManager()
	start := time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC)
	decision := Decision{Pool: model.PoolLite, Backend: model.BackendARC, Rules: []RuleReading{{Name: "rootfs", Source: "prometheus", State: StateExceeded, HasValue: true, Since: start}}}
	m.SetDecision(decision)
	decision.Rules[0].Since = start.Add(time.Minute)
	decision.Rules[0].Value = 0.9
	m.SetDecision(decision)
	got, _ := m.Decision(model.PoolLite, model.BackendARC)
	if !got.Rules[0].Since.Equal(start) || got.Rules[0].Value != 0.9 {
		t.Fatalf("reading: %+v", got.Rules[0])
	}
	got.Rules[0].Name = "changed"
	again, _ := m.Decision(model.PoolLite, model.BackendARC)
	if again.Rules[0].Name != "rootfs" {
		t.Fatal("caller mutated cached rule")
	}
	decision.Rules[0].State = StateHealthy
	m.SetDecision(decision)
	again, _ = m.Decision(model.PoolLite, model.BackendARC)
	if !again.Rules[0].Since.Equal(start.Add(time.Minute)) {
		t.Fatalf("transition time: %v", again.Rules[0].Since)
	}
}
