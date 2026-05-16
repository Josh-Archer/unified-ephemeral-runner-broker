package adapter

import (
	"context"
	"time"
)

type BackendName string
type PoolName string

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

type Adapter interface {
	Health(context.Context) (HealthStatus, error)
	Capacity(context.Context) (CapacityStatus, error)
	Reserve(context.Context, ReservationRequest) (Reservation, error)
	Launch(context.Context, LaunchRequest) (LaunchResult, error)
	Cleanup(context.Context, CleanupRequest) error
}
