// Package gpuadmission compares server observations with explicit GPU policy.
// It does not authorize work, reserve resources, or guarantee launch-time safety.
package gpuadmission

import (
	"context"
	"math"
	"time"
)

// Policy fields are pointers so explicit zero limits differ from missing policy.
// No operational thresholds are provided by this package.
type Policy struct {
	MaxAge              *time.Duration
	RequiredFreeBytes   *uint64
	MaxProtectedLatency *float64
	MaxProtectedErrors  *uint64
}
type Identity struct{ Node, UUID string }

// Sample must describe one whole physical device, including non-Kubernetes use.
// Each observation is independently timestamped; collection time is not sample time.
type Sample[T any] struct {
	Value T
	At    time.Time
}
type Memory struct{ Total, Used uint64 }
type Slices struct{ Capacity, Used, Pending uint64 }
type Health uint8

const (
	HealthUnknown Health = iota
	HealthOK
	HealthFault
)

// Reservations includes all GPU lanes/nodes, not just the selected backend.
// Categories must be disjoint; launching is included in Reserved.
type Reservations struct{ Reserved, Running, CleanupPending, Orphans uint64 }
type Observation struct {
	Device           Identity
	Memory           *Sample[Memory]
	Slices           *Sample[Slices]
	Health           *Sample[Health]
	ProtectedLatency *Sample[float64]
	ProtectedErrors  *Sample[uint64]
	Reservations     *Sample[Reservations]
}

// Source is a server-side integration boundary with no implementation here.
// Implementations must authenticate their upstream, resolve node/UUID bindings,
// reject ambiguous series, and preserve individual sample timestamps. Never
// populate these observations from PR content or allocation-request metadata.
// This interface alone cannot prove provenance or detect a dishonest source.
type Source interface {
	Observe(context.Context, Identity) (Observation, error)
}

type Reason string

const (
	PolicyInvalid      Reason = "policy-invalid"
	IdentityMismatch   Reason = "identity-mismatch"
	MissingObservation Reason = "missing-observation"
	StaleObservation   Reason = "stale-or-future-observation"
	InvalidObservation Reason = "invalid-observation"
	Unhealthy          Reason = "device-unhealthy"
	MemoryPressure     Reason = "memory-pressure"
	SlicePressure      Reason = "slice-pressure"
	ProtectedPressure  Reason = "protected-workload-pressure"
	GlobalBusy         Reason = "global-gpu-lane-busy"
)

type Rejection struct{ Reason Reason }

func (e *Rejection) Error() string { return string(e.Reason) }
func reject(r Reason) error        { return &Rejection{Reason: r} }
func finite(v float64) bool        { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 }

// Evaluate returns nil only for a successful snapshot comparison. It does not
// consume a reservation; concurrent/replayed evaluations can both succeed.
// The caller must separately enforce eligibility, trust and atomic fenced launch.
func Evaluate(p Policy, expected Identity, o Observation, now time.Time) error {
	if p.MaxAge == nil || *p.MaxAge <= 0 || p.RequiredFreeBytes == nil || p.MaxProtectedLatency == nil || !finite(*p.MaxProtectedLatency) || p.MaxProtectedErrors == nil {
		return reject(PolicyInvalid)
	}
	if expected.Node == "" || expected.UUID == "" || o.Device != expected {
		return reject(IdentityMismatch)
	}
	if o.Memory == nil || o.Slices == nil || o.Health == nil || o.ProtectedLatency == nil || o.ProtectedErrors == nil || o.Reservations == nil {
		return reject(MissingObservation)
	}
	for _, at := range []time.Time{o.Memory.At, o.Slices.At, o.Health.At, o.ProtectedLatency.At, o.ProtectedErrors.At, o.Reservations.At} {
		if now.IsZero() || at.IsZero() || at.After(now) || at.Before(now.Add(-*p.MaxAge)) {
			return reject(StaleObservation)
		}
	}
	m, s := o.Memory.Value, o.Slices.Value
	if m.Total == 0 || m.Used > m.Total || s.Capacity == 0 || s.Used > s.Capacity || s.Pending > s.Capacity-s.Used || !finite(o.ProtectedLatency.Value) {
		return reject(InvalidObservation)
	}
	if o.Health.Value != HealthOK {
		return reject(Unhealthy)
	}
	if m.Total-m.Used < *p.RequiredFreeBytes {
		return reject(MemoryPressure)
	}
	// Subtraction after bounds checks avoids uint64 addition overflow.
	if s.Capacity-s.Used-s.Pending < 1 {
		return reject(SlicePressure)
	}
	if o.ProtectedLatency.Value > *p.MaxProtectedLatency || o.ProtectedErrors.Value > *p.MaxProtectedErrors {
		return reject(ProtectedPressure)
	}
	r := o.Reservations.Value
	// Fixed initial global concurrency one; never sum untrusted counters.
	if r.Reserved != 0 || r.Running != 0 || r.CleanupPending != 0 || r.Orphans != 0 {
		return reject(GlobalBusy)
	}
	return nil
}
