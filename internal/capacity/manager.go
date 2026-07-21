package capacity

import (
	"sync"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

// Snapshot is a cached provider capacity reading for one backend.
type Snapshot struct {
	Backend   model.BackendName
	Status    backend.CapacityStatus
	UpdatedAt time.Time
	Stale     bool
	Err       string
	Source    string // "live", "error", "missing"
}

// Manager stores out-of-band capacity snapshots for allocation-time reads.
type Manager struct {
	mu        sync.RWMutex
	snapshots map[model.BackendName]Snapshot
}

func NewManager() *Manager {
	return &Manager{
		snapshots: map[model.BackendName]Snapshot{},
	}
}

func (m *Manager) Set(snapshot Snapshot) {
	if m == nil {
		return
	}
	if snapshot.Source == "" {
		if snapshot.Err != "" {
			snapshot.Source = "error"
		} else {
			snapshot.Source = "live"
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.snapshots[snapshot.Backend] = snapshot
}

func (m *Manager) Get(name model.BackendName) (Snapshot, bool) {
	if m == nil {
		return Snapshot{}, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	snapshot, ok := m.snapshots[name]
	return snapshot, ok
}

func (m *Manager) Snapshot() []Snapshot {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]Snapshot, 0, len(m.snapshots))
	for _, snapshot := range m.snapshots {
		result = append(result, snapshot)
	}
	return result
}

// MarkStale flags snapshots older than staleAfter. Allocation still reads the
// last value; failureMode decides whether stale data blocks routing.
func (m *Manager) MarkStale(staleAfter time.Duration, now time.Time) {
	if m == nil || staleAfter <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, snapshot := range m.snapshots {
		if snapshot.UpdatedAt.IsZero() || now.Sub(snapshot.UpdatedAt) > staleAfter {
			snapshot.Stale = true
			m.snapshots[name] = snapshot
		}
	}
}

// EffectiveMaxRunners combines configured maxRunners, local active reservations,
// and provider-reported free slots into a scheduler ceiling.
//
// available is false when the backend should be skipped for routing.
// When live capacity is disabled, missing, or pass-through under stale/error
// policy, the configured MaxRunners remains the only ceiling.
func EffectiveMaxRunners(cfgMax, localActive int, snap Snapshot, hasSnapshot bool, failureMode string) (max int, available bool, reason string) {
	if cfgMax <= 0 {
		return 0, false, "disabled"
	}
	if localActive >= cfgMax {
		return cfgMax, false, "local-full"
	}

	if !hasSnapshot {
		return cfgMax, true, "no-live-data"
	}

	staleOrError := snap.Stale || snap.Err != "" || snap.Source == "error" || snap.Status.MaxRunners <= 0
	if staleOrError {
		switch normalizeFailureMode(failureMode) {
		case FailureModeBlock:
			return cfgMax, false, staleBlockReason(snap)
		default:
			return cfgMax, true, "stale-pass-through"
		}
	}

	free := backend.FreeSlots(snap.Status)
	ceiling := cfgMax
	if snap.Status.MaxRunners > 0 && snap.Status.MaxRunners < ceiling {
		ceiling = snap.Status.MaxRunners
	}
	if localActive >= ceiling {
		return ceiling, false, "provider-ceiling"
	}
	if free <= 0 {
		return ceiling, false, "provider-full"
	}

	// Cap remaining local admits by provider free slots so concurrent local
	// reservations do not intentionally overrun provider-reported free capacity.
	remaining := ceiling - localActive
	if free < remaining {
		remaining = free
	}
	return localActive + remaining, true, "live"
}

func staleBlockReason(snap Snapshot) string {
	if snap.Err != "" || snap.Source == "error" {
		return "capacity-error"
	}
	if snap.Status.MaxRunners <= 0 {
		return "capacity-invalid"
	}
	return "capacity-stale"
}

const (
	FailureModePassThrough = "pass-through"
	FailureModeBlock       = "block"
)

func normalizeFailureMode(mode string) string {
	switch mode {
	case FailureModeBlock, "block-stale", "fail-closed":
		return FailureModeBlock
	default:
		return FailureModePassThrough
	}
}
