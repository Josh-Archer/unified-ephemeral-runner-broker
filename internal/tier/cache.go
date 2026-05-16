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
	m.decisions[key{pool: decision.Pool, backend: decision.Backend}] = normalizeDecision(decision)
}

func (m *Manager) Decision(pool model.PoolName, backend model.BackendName) (Decision, bool) {
	if m == nil {
		return Decision{}, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	decision, ok := m.decisions[key{pool: pool, backend: backend}]
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
