package tier

import (
	"sync"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

type Manager struct {
	mu        sync.RWMutex
	decisions map[key]Decision
}

type key struct {
	pool    model.PoolName
	backend model.BackendName
}

func NewManager() *Manager {
	return &Manager{
		decisions: map[key]Decision{},
	}
}

func (m *Manager) SetDecision(decision Decision) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cacheKey := key{pool: decision.Pool, backend: decision.Backend}
	previous := m.decisions[cacheKey]
	decision.Rules = append([]RuleReading(nil), decision.Rules...)
	for i := range decision.Rules {
		for _, old := range previous.Rules {
			if !previous.Stale && old.Name == decision.Rules[i].Name && old.Source == decision.Rules[i].Source && old.State == decision.Rules[i].State && old.Action == decision.Rules[i].Action && old.HasValue == decision.Rules[i].HasValue && old.NoData == decision.Rules[i].NoData {
				decision.Rules[i].Since = old.Since
				break
			}
		}
	}
	m.decisions[cacheKey] = normalizeDecision(decision)
}

func (m *Manager) Decision(pool model.PoolName, backend model.BackendName) (Decision, bool) {
	if m == nil {
		return Decision{}, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	decision, ok := m.decisions[key{pool: pool, backend: backend}]
	decision.Rules = append([]RuleReading(nil), decision.Rules...)
	return decision, ok
}

func (m *Manager) Snapshot() []Decision {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]Decision, 0, len(m.decisions))
	for _, decision := range m.decisions {
		decision.Rules = append([]RuleReading(nil), decision.Rules...)
		result = append(result, decision)
	}
	return result
}

func (m *Manager) MarkStale(staleAfter time.Duration, now time.Time) {
	if m == nil || staleAfter <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for cacheKey, decision := range m.decisions {
		if decision.UpdatedAt.IsZero() || now.Sub(decision.UpdatedAt) > staleAfter {
			decision.Stale = true
			if decision.State == "" {
				decision.State = StateUnknown
			}
			m.decisions[cacheKey] = decision
		}
	}
}

func normalizeDecision(decision Decision) Decision {
	if decision.State == "" {
		decision.State = StateUnknown
	}
	if decision.Action == "" {
		decision.Action = ActionDisable
	}
	return decision
}
