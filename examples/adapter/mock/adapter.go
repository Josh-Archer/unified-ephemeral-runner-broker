package mock

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/pkg/adapter"
)

type slot struct {
	reservation adapter.Reservation
	phase       string
}

const (
	phasePending = "pending"
	phaseActive  = "active"
	phaseWarm    = "warm"
)

// Adapter is a minimal v1 reference implementation.
// maxRunners is the provider ceiling reported by Capacity.
type Adapter struct {
	mu           sync.Mutex
	maxRunners   int
	next         int
	reservations map[string]slot
}

func New(maxRunners int) *Adapter {
	if maxRunners <= 0 {
		maxRunners = 1
	}
	return &Adapter{
		maxRunners:   maxRunners,
		reservations: map[string]slot{},
	}
}

func (a *Adapter) Health(ctx context.Context) (adapter.HealthStatus, error) {
	if err := ctx.Err(); err != nil {
		return adapter.HealthStatus{}, err
	}
	return adapter.HealthStatus{Healthy: true, Checked: time.Now().UTC()}, nil
}

func (a *Adapter) Capacity(ctx context.Context) (adapter.CapacityStatus, error) {
	if err := ctx.Err(); err != nil {
		return adapter.CapacityStatus{}, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.capacityLocked(), nil
}

func (a *Adapter) Reserve(ctx context.Context, request adapter.ReservationRequest) (adapter.Reservation, error) {
	if err := ctx.Err(); err != nil {
		return adapter.Reservation{}, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	id := strings.TrimSpace(request.AllocationID)
	if id == "" {
		a.next++
		id = fmt.Sprintf("mock-%d", a.next)
	}
	if existing, ok := a.reservations[id]; ok {
		return cloneReservation(existing.reservation), nil
	}
	if len(a.reservations) >= a.maxRunners {
		return adapter.Reservation{}, fmt.Errorf("mock adapter: %w", adapter.ErrCapacityExhausted)
	}

	label := strings.TrimSpace(request.Metadata[adapter.MetadataRunnerLabel])
	if label == "" {
		label = "uecb-mock-" + id
	}
	name := strings.TrimSpace(request.Metadata[adapter.MetadataRunnerName])
	if name == "" {
		name = label
	}
	var expires time.Time
	if request.JobTimeout > 0 {
		expires = time.Now().UTC().Add(request.JobTimeout)
	}
	metadata := cloneMetadata(request.Metadata)
	if metadata == nil {
		metadata = map[string]string{}
	}
	metadata["adapter"] = "mock"
	reservation := adapter.Reservation{
		AllocationID: id,
		RunnerName:   name,
		RunnerLabel:  label,
		ExpiresAt:    expires,
		Metadata:     metadata,
	}
	a.reservations[id] = slot{reservation: cloneReservation(reservation), phase: phasePending}
	return cloneReservation(reservation), nil
}

func (a *Adapter) Launch(ctx context.Context, request adapter.LaunchRequest) (adapter.LaunchResult, error) {
	if err := ctx.Err(); err != nil {
		return adapter.LaunchResult{}, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	if slot, ok := a.reservations[request.Reservation.AllocationID]; ok {
		if request.Warm {
			slot.phase = phaseWarm
		} else {
			slot.phase = phaseActive
		}
		a.reservations[request.Reservation.AllocationID] = slot
	}
	label := strings.TrimSpace(request.Reservation.RunnerLabel)
	if label == "" {
		label = "uecb-mock-" + request.Reservation.AllocationID
	}
	mode := "cold"
	if request.Warm {
		mode = "warm"
	}
	return adapter.LaunchResult{
		RunnerLabel: label,
		Metadata: map[string]string{
			adapter.MetadataLaunchMode: mode,
		},
	}, nil
}

func (a *Adapter) Cleanup(ctx context.Context, request adapter.CleanupRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.reservations, request.Reservation.AllocationID)
	return nil
}

func (a *Adapter) capacityLocked() adapter.CapacityStatus {
	status := adapter.CapacityStatus{MaxRunners: a.maxRunners}
	for _, current := range a.reservations {
		switch current.phase {
		case phaseWarm:
			status.WarmRunners++
		case phaseActive:
			status.ActiveRunners++
		default:
			status.PendingRunners++
		}
	}
	return status
}

func cloneReservation(in adapter.Reservation) adapter.Reservation {
	out := in
	out.Metadata = cloneMetadata(in.Metadata)
	return out
}

func cloneMetadata(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
