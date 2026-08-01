package fakecapacity

import (
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/capacity"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

// Free returns a usable capacity reading with free slots.
// freeSlots must be non-negative; ActiveRunners is max(0, max-free).
func Free(maxRunners, freeSlots int) backend.CapacityStatus {
	if maxRunners < 0 {
		maxRunners = 0
	}
	if freeSlots < 0 {
		freeSlots = 0
	}
	if freeSlots > maxRunners {
		freeSlots = maxRunners
	}
	return backend.CapacityStatus{
		MaxRunners:    maxRunners,
		ActiveRunners: maxRunners - freeSlots,
	}
}

// Full returns a capacity reading with no free slots.
func Full(maxRunners int) backend.CapacityStatus {
	if maxRunners < 0 {
		maxRunners = 0
	}
	return backend.CapacityStatus{
		MaxRunners:    maxRunners,
		ActiveRunners: maxRunners,
	}
}

// Detailed returns a capacity reading with explicit counters.
func Detailed(maxRunners, active, pending, warm int) backend.CapacityStatus {
	return backend.CapacityStatus{
		MaxRunners:     maxRunners,
		ActiveRunners:  active,
		PendingRunners: pending,
		WarmRunners:    warm,
	}
}

// InvalidMax returns a reading with MaxRunners <= 0 (unusable for routing).
func InvalidMax() backend.CapacityStatus {
	return backend.CapacityStatus{}
}

// LiveSnapshot builds a fresh live snapshot for Manager.Set.
func LiveSnapshot(name model.BackendName, status backend.CapacityStatus, now time.Time) capacity.Snapshot {
	return capacity.Snapshot{
		Backend:   name,
		Status:    status,
		UpdatedAt: now,
		Stale:     false,
		Source:    "live",
	}
}

// StaleSnapshot builds a stale (aged-out) snapshot for Manager.Set.
func StaleSnapshot(name model.BackendName, status backend.CapacityStatus, now time.Time) capacity.Snapshot {
	return capacity.Snapshot{
		Backend:   name,
		Status:    status,
		UpdatedAt: now,
		Stale:     true,
		Source:    "live",
	}
}

// ErrorSnapshot builds a failed capacity probe snapshot for Manager.Set.
// When previous holds a last-good status, counters are preserved the same way
// capacity.Refresh does on probe failure.
func ErrorSnapshot(name model.BackendName, errMsg string, previous *backend.CapacityStatus, now time.Time) capacity.Snapshot {
	snap := capacity.Snapshot{
		Backend:   name,
		UpdatedAt: now,
		Stale:     false,
		Err:       errMsg,
		Source:    "error",
	}
	if previous != nil {
		snap.Status = *previous
	}
	return snap
}

// SeedManager writes one or more snapshots into the manager.
func SeedManager(manager *capacity.Manager, snapshots ...capacity.Snapshot) {
	if manager == nil {
		return
	}
	for _, snap := range snapshots {
		manager.Set(snap)
	}
}
