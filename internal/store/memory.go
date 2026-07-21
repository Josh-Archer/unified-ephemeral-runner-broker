package store

import (
	"context"
	"sync"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

type Memory struct {
	mu          sync.RWMutex
	allocations map[string]model.AllocationStatus
	admission   AdmissionStateDocument
	leader      map[string]leaderLease
}

type leaderLease struct {
	holder    string
	expiresAt time.Time
}

func NewMemory() *Memory {
	return &Memory{
		allocations: map[string]model.AllocationStatus{},
		admission: AdmissionStateDocument{
			Circuits: map[string]AdmissionCircuitState{},
			Limits:   map[string]AdmissionRateLimit{},
		},
		leader: map[string]leaderLease{},
	}
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
	status = applyMarkState(status, state, now, message)
	m.allocations[id] = status
	return status, true
}

func (m *Memory) CompareAndMarkState(id string, expectedFrom model.AllocationState, to model.AllocationState, now time.Time, message string) (model.AllocationStatus, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	status, ok := m.allocations[id]
	if !ok || status.State != expectedFrom {
		return model.AllocationStatus{}, false
	}
	status = applyMarkState(status, to, now, message)
	m.allocations[id] = status
	return status, true
}

func (m *Memory) SaveIfCapacity(status model.AllocationStatus, maxRunners int, tenantQuota int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := capacityAllowed(m.allocations, status, maxRunners, tenantQuota); err != nil {
		return err
	}
	m.allocations[status.ID] = status
	return nil
}

func (m *Memory) CountActive(pool model.PoolName, backend model.BackendName) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return countActiveLocked(m.allocations, pool, backend, "")
}

func (m *Memory) CountTenantActive(pool model.PoolName, tenant string) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return countTenantActiveLocked(m.allocations, pool, tenant, "")
}

func (m *Memory) Ping(context.Context) error { return nil }

func (m *Memory) Close() error { return nil }

func (m *Memory) TryAcquireLeadership(_ context.Context, name, identity string, ttl time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ttl <= 0 {
		ttl = 15 * time.Second
	}
	now := time.Now()
	lease, ok := m.leader[name]
	if ok && lease.expiresAt.After(now) && lease.holder != identity {
		return false, nil
	}
	m.leader[name] = leaderLease{holder: identity, expiresAt: now.Add(ttl)}
	return true, nil
}

func (m *Memory) ReleaseLeadership(_ context.Context, name, identity string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	lease, ok := m.leader[name]
	if !ok || lease.holder != identity {
		return nil
	}
	delete(m.leader, name)
	return nil
}

func (m *Memory) LoadAdmissionState(context.Context) (AdmissionStateDocument, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return cloneAdmission(m.admission), nil
}

func (m *Memory) SaveAdmissionState(_ context.Context, doc AdmissionStateDocument) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.admission = cloneAdmission(doc)
	return nil
}

func cloneAdmission(doc AdmissionStateDocument) AdmissionStateDocument {
	out := AdmissionStateDocument{
		Circuits: map[string]AdmissionCircuitState{},
		Limits:   map[string]AdmissionRateLimit{},
	}
	for k, v := range doc.Circuits {
		cp := v
		if len(v.Failures) > 0 {
			cp.Failures = append([]time.Time(nil), v.Failures...)
		}
		out.Circuits[k] = cp
	}
	for k, v := range doc.Limits {
		out.Limits[k] = v
	}
	return out
}
