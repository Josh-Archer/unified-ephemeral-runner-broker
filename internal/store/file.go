package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

type File struct {
	mu          sync.RWMutex
	path        string
	allocations map[string]model.AllocationStatus
	admission   AdmissionStateDocument
	leader      map[string]leaderLeaseSnapshot
}

type fileSnapshot struct {
	Allocations map[string]model.AllocationStatus   `json:"allocations"`
	Admission   AdmissionStateDocument              `json:"admission,omitempty"`
	Leader      map[string]leaderLeaseSnapshot      `json:"leader,omitempty"`
}

type leaderLeaseSnapshot struct {
	Holder    string    `json:"holder"`
	ExpiresAt time.Time `json:"expiresAt"`
}

func NewFile(path string) (*File, error) {
	if path == "" {
		return nil, fmt.Errorf("broker.stateStore.path is required when type is file")
	}
	store := &File{
		path:        path,
		allocations: map[string]model.AllocationStatus{},
		admission: AdmissionStateDocument{
			Circuits: map[string]AdmissionCircuitState{},
			Limits:   map[string]AdmissionRateLimit{},
		},
		leader: map[string]leaderLeaseSnapshot{},
	}
	if err := store.load(); err != nil {
		return nil, err
	}
	return store, nil
}

func (f *File) Save(status model.AllocationStatus) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.allocations[status.ID] = status
	return f.persistLocked()
}

func (f *File) Delete(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.allocations, id)
	return f.persistLocked()
}

func (f *File) Get(id string) (model.AllocationStatus, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	status, ok := f.allocations[id]
	return status, ok
}

func (f *File) List() []model.AllocationStatus {
	f.mu.RLock()
	defer f.mu.RUnlock()

	result := make([]model.AllocationStatus, 0, len(f.allocations))
	for _, status := range f.allocations {
		result = append(result, status)
	}
	return result
}

func (f *File) MarkState(id string, state model.AllocationState, now time.Time, message string) (model.AllocationStatus, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	status, ok := f.allocations[id]
	if !ok {
		return model.AllocationStatus{}, false
	}

	status = applyMarkState(status, state, now, message)
	f.allocations[id] = status
	if err := f.persistLocked(); err != nil {
		status.Error = err.Error()
		f.allocations[id] = status
	}
	return status, true
}

func (f *File) CompareAndMarkState(id string, expectedFrom model.AllocationState, to model.AllocationState, now time.Time, message string) (model.AllocationStatus, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	status, ok := f.allocations[id]
	if !ok || status.State != expectedFrom {
		return model.AllocationStatus{}, false
	}
	status = applyMarkState(status, to, now, message)
	f.allocations[id] = status
	if err := f.persistLocked(); err != nil {
		status.Error = err.Error()
		f.allocations[id] = status
	}
	return status, true
}

func (f *File) SaveIfCapacity(status model.AllocationStatus, maxRunners int, tenantQuota int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := capacityAllowed(f.allocations, status, maxRunners, tenantQuota); err != nil {
		return err
	}
	f.allocations[status.ID] = status
	return f.persistLocked()
}

func (f *File) CountActive(pool model.PoolName, backend model.BackendName) int {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return countActiveLocked(f.allocations, pool, backend, "")
}

func (f *File) CountTenantActive(pool model.PoolName, tenant string) int {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return countTenantActiveLocked(f.allocations, pool, tenant, "")
}

func (f *File) Ping(context.Context) error { return nil }

func (f *File) Close() error { return nil }

func (f *File) TryAcquireLeadership(_ context.Context, name, identity string, ttl time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ttl <= 0 {
		ttl = 15 * time.Second
	}
	now := time.Now()
	// Reload so multiple processes sharing a volume observe the same lease.
	if err := f.loadLocked(); err != nil {
		return false, err
	}
	lease, ok := f.leader[name]
	if ok && lease.ExpiresAt.After(now) && lease.Holder != identity {
		return false, nil
	}
	if f.leader == nil {
		f.leader = map[string]leaderLeaseSnapshot{}
	}
	f.leader[name] = leaderLeaseSnapshot{Holder: identity, ExpiresAt: now.Add(ttl)}
	if err := f.persistLocked(); err != nil {
		return false, err
	}
	return true, nil
}

func (f *File) ReleaseLeadership(_ context.Context, name, identity string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.loadLocked(); err != nil {
		return err
	}
	lease, ok := f.leader[name]
	if !ok || lease.Holder != identity {
		return nil
	}
	delete(f.leader, name)
	return f.persistLocked()
}

func (f *File) LoadAdmissionState(context.Context) (AdmissionStateDocument, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return cloneAdmission(f.admission), nil
}

func (f *File) SaveAdmissionState(_ context.Context, doc AdmissionStateDocument) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.admission = cloneAdmission(doc)
	return f.persistLocked()
}

func (f *File) load() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loadLocked()
}

func (f *File) loadLocked() error {
	data, err := os.ReadFile(f.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read state store %s: %w", f.path, err)
	}
	if len(data) == 0 {
		return nil
	}
	var snapshot fileSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return fmt.Errorf("decode state store %s: %w", f.path, err)
	}
	if snapshot.Allocations != nil {
		f.allocations = snapshot.Allocations
	}
	if snapshot.Admission.Circuits != nil || snapshot.Admission.Limits != nil {
		f.admission = cloneAdmission(snapshot.Admission)
	}
	if snapshot.Leader != nil {
		f.leader = snapshot.Leader
	}
	return nil
}

func (f *File) persistLocked() error {
	if err := os.MkdirAll(filepath.Dir(f.path), 0o755); err != nil {
		return fmt.Errorf("create state store directory: %w", err)
	}
	data, err := json.MarshalIndent(fileSnapshot{
		Allocations: f.allocations,
		Admission:   f.admission,
		Leader:      f.leader,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state store: %w", err)
	}
	tmp := f.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write state store: %w", err)
	}
	if err := os.Rename(tmp, f.path); err != nil {
		return fmt.Errorf("replace state store: %w", err)
	}
	return nil
}
