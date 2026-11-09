package adapter

import (
	"context"
	"time"
)

type BackendName string
type PoolName string

// AllocationState is the lifecycle state a caller passes to Cleanup.
// The string values are part of adapter API v1.
type AllocationState string

const (
	StateReserved  AllocationState = "reserved"
	StateReady     AllocationState = "ready"
	StatePending   AllocationState = "pending"
	StateCanceled  AllocationState = "canceled"
	StateExpired   AllocationState = "expired"
	StateFailed    AllocationState = "failed"
	StateCompleted AllocationState = "completed"
)

type HealthStatus struct {
	Healthy bool
	Reason  string
	Checked time.Time
}

type CapacityStatus struct {
	MaxRunners     int
	ActiveRunners  int
	PendingRunners int
	WarmRunners    int
}

type ReservationRequest struct {
	AllocationID string
	Pool         PoolName
	Backend      BackendName
	Tenant       string
	Priority     string
	Labels       []string
	Capabilities []string
	JobTimeout   time.Duration
	Metadata     map[string]string
}

type Reservation struct {
	AllocationID string
	RunnerName   string
	RunnerLabel  string
	ExpiresAt    time.Time
	Metadata     map[string]string
}

type LaunchRequest struct {
	Reservation Reservation
	Warm        bool
	Metadata    map[string]string
}

type LaunchResult struct {
	RunnerLabel string
	StatusURL   string
	DetailsURL  string
	Metadata    map[string]string
}

type CleanupRequest struct {
	Reservation Reservation
	State       AllocationState
	Reason      string
	Metadata    map[string]string
}

// Adapter is the v1 runner-controller lifecycle.
//
// The method set is frozen while APIVersion is "v1". Every method honors
// context cancellation: a context that is already canceled or whose deadline
// has already passed returns that context error and leaves provider state
// unchanged.
//
// Reserve preserves a non-empty AllocationID, returns a non-empty RunnerLabel,
// and returns a zero or future ExpiresAt. Reserving an AllocationID that is
// already held returns the original reservation and does not consume another
// slot. When no free slot remains, Reserve returns an error for which
// IsCapacityExhausted is true.
//
// Capacity counts every reserved, launching, warm, and running slot before
// the corresponding call returns. Cleanup releases the slot, succeeds for the
// terminal states canceled, expired, failed, and completed, and succeeds
// again when the allocation is already gone.
type Adapter interface {
	Health(context.Context) (HealthStatus, error)
	Capacity(context.Context) (CapacityStatus, error)
	Reserve(context.Context, ReservationRequest) (Reservation, error)
	Launch(context.Context, LaunchRequest) (LaunchResult, error)
	Cleanup(context.Context, CleanupRequest) error
}
