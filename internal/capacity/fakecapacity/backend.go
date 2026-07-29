package fakecapacity

import (
	"context"
	"sync"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

// Backend is an in-process CapacityBackend fixture.
// It does not implement full Backend.Provision; pair it with a test provisioner
// when exercising the allocation path.
type Backend struct {
	name model.BackendName

	mu     sync.Mutex
	status backend.CapacityStatus
	err    error
}

// NewBackend returns a CapacityBackend that reports status until SetError is used.
func NewBackend(name model.BackendName, status backend.CapacityStatus) *Backend {
	return &Backend{
		name:   name,
		status: status,
	}
}

// Name returns the backend name (useful when embedding in multi-backend tests).
func (b *Backend) Name() model.BackendName {
	if b == nil {
		return ""
	}
	return b.name
}

// Capacity implements backend.CapacityBackend.
func (b *Backend) Capacity(context.Context) (backend.CapacityStatus, error) {
	if b == nil {
		return backend.CapacityStatus{}, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return backend.CapacityStatus{}, b.err
	}
	return b.status, nil
}

// SetStatus updates the reported capacity counters.
func (b *Backend) SetStatus(status backend.CapacityStatus) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.status = status
	b.err = nil
}

// SetError makes subsequent Capacity calls fail with err.
// Pass nil to clear the error and resume returning status.
func (b *Backend) SetError(err error) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.err = err
}

// MapReporter adapts a name→CapacityBackend map to capacity.Reporter.
type MapReporter map[model.BackendName]backend.CapacityBackend

// CapacityBackend implements capacity.Reporter.
func (m MapReporter) CapacityBackend(name model.BackendName) (backend.CapacityBackend, bool) {
	if m == nil {
		return nil, false
	}
	reporter, ok := m[name]
	return reporter, ok && reporter != nil
}
