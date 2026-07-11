package store

import (
	"sync"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

type Store interface {
	Save(model.AllocationStatus) error
	Delete(string) error
	Get(string) (model.AllocationStatus, bool)
	List() []model.AllocationStatus
	MarkState(string, model.AllocationState, time.Time, string) (model.AllocationStatus, bool)
}

type Memory struct {
	mu          sync.RWMutex
	allocations map[string]model.AllocationStatus
}

func NewMemory() *Memory {
	return &Memory{allocations: map[string]model.AllocationStatus{}}
}

func (m *Memory) Save(status model.AllocationStatus) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.allocations[status.ID] = status
	return nil
}

func (m *Memory) Delete(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.allocations, id)
	return nil
}

func (m *Memory) Get(id string) (model.AllocationStatus, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	status, ok := m.allocations[id]
	return status, ok
}

func (m *Memory) List() []model.AllocationStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make([]model.AllocationStatus, 0, len(m.allocations))
	for _, status := range m.allocations {
		result = append(result, status)
	}
	return result
}

func (m *Memory) MarkState(id string, state model.AllocationState, now time.Time, message string) (model.AllocationStatus, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	status, ok := m.allocations[id]
	if !ok {
		return model.AllocationStatus{}, false
	}

	status.State = state
	if message != "" {
		status.Error = message
	}
	if state == model.StateExpired || state == model.StateQuarantined {
		status.ExpiresAt = now
	}

	m.allocations[id] = status
	return status, true
}
