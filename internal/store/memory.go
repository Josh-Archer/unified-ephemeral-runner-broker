package store

import (
	"sync"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

type Memory struct {
	mu          sync.RWMutex
	allocations map[string]model.AllocationStatus
}

func NewMemory() *Memory {
	return &Memory{allocations: map[string]model.AllocationStatus{}}
}

func (m *Memory) Save(status model.AllocationStatus) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.allocations[status.ID] = status
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
	if state == model.StateExpired {
		status.ExpiresAt = now
	}

	m.allocations[id] = status
	return status, true
}
