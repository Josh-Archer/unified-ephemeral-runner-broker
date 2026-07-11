package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

const (
	TypeMemory = "memory"
	TypeFile   = "file"
)

type File struct {
	mu          sync.RWMutex
	path        string
	allocations map[string]model.AllocationStatus
}

type fileSnapshot struct {
	Allocations map[string]model.AllocationStatus `json:"allocations"`
}

func NewFromConfig(cfg model.StateStoreConfig) (Store, error) {
	switch cfg.Type {
	case "", TypeMemory:
		return NewMemory(), nil
	case TypeFile:
		return NewFile(cfg.Path)
	default:
		return nil, fmt.Errorf("unsupported broker.stateStore.type %q", cfg.Type)
	}
}

func NewFile(path string) (*File, error) {
	if path == "" {
		return nil, fmt.Errorf("broker.stateStore.path is required when type is file")
	}
	store := &File{
		path:        path,
		allocations: map[string]model.AllocationStatus{},
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

	status.State = state
	if message != "" {
		status.Error = message
	}
	if state == model.StateExpired || state == model.StateQuarantined {
		status.ExpiresAt = now
	}

	f.allocations[id] = status
	if err := f.persistLocked(); err != nil {
		status.Error = err.Error()
		f.allocations[id] = status
	}
	return status, true
}

func (f *File) load() error {
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
	return nil
}

func (f *File) persistLocked() error {
	if err := os.MkdirAll(filepath.Dir(f.path), 0o755); err != nil {
		return fmt.Errorf("create state store directory: %w", err)
	}
	data, err := json.MarshalIndent(fileSnapshot{Allocations: f.allocations}, "", "  ")
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
