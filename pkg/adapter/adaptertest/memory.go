package adaptertest

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/pkg/adapter"
)

type memorySlot struct {
	reservation adapter.Reservation
	phase       string
}

const (
	memoryPending = "pending"
	memoryActive  = "active"
	memoryWarm    = "warm"
)

// Memory is an in-memory v1 Adapter for conformance fixtures and local tests.
type Memory struct {
	mu           sync.Mutex
	maxRunners   int
	next         int
	reservations map[string]memorySlot
}

// NewMemory returns a fixture whose Capacity.MaxRunners is maxRunners.
// A non-positive ceiling becomes 1 so conformance can exercise exhaustion.
func NewMemory(maxRunners int) *Memory {
	if maxRunners <= 0 {
		maxRunners = 1
	}
	return &Memory{
		maxRunners:   maxRunners,
		reservations: map[string]memorySlot{},
	}
}

func (m *Memory) Health(ctx context.Context) (adapter.HealthStatus, error) {
	if err := ctx.Err(); err != nil {
		return adapter.HealthStatus{}, err
	}
	return adapter.HealthStatus{Healthy: true, Checked: time.Now().UTC()}, nil
}

func (m *Memory) Capacity(ctx context.Context) (adapter.CapacityStatus, error) {
	if err := ctx.Err(); err != nil {
		return adapter.CapacityStatus{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.capacityLocked(), nil
}

func (m *Memory) Reserve(ctx context.Context, request adapter.ReservationRequest) (adapter.Reservation, error) {
	if err := ctx.Err(); err != nil {
		return adapter.Reservation{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	id := strings.TrimSpace(request.AllocationID)
	if id == "" {
		m.next++
		id = fmt.Sprintf("mem-%d", m.next)
	}
	if existing, ok := m.reservations[id]; ok {
		return cloneReservation(existing.reservation), nil
	}
	if len(m.reservations) >= m.maxRunners {
		return adapter.Reservation{}, fmt.Errorf("memory adapter: %w", adapter.ErrCapacityExhausted)
	}

	label := strings.TrimSpace(request.Metadata[adapter.MetadataRunnerLabel])
	if label == "" {
		label = "uecb-mem-" + id
	}
	name := strings.TrimSpace(request.Metadata[adapter.MetadataRunnerName])
	if name == "" {
		name = label
	}
	var expires time.Time
	if request.JobTimeout > 0 {
		expires = time.Now().UTC().Add(request.JobTimeout)
	}
	reservation := adapter.Reservation{
		AllocationID: id,
		RunnerName:   name,
		RunnerLabel:  label,
		ExpiresAt:    expires,
		Metadata:     cloneMetadata(request.Metadata),
	}
	if reservation.Metadata == nil {
		reservation.Metadata = map[string]string{}
	}
	reservation.Metadata["adapter"] = "memory"
	m.reservations[id] = memorySlot{reservation: cloneReservation(reservation), phase: memoryPending}
	return cloneReservation(reservation), nil
}

func (m *Memory) Launch(ctx context.Context, request adapter.LaunchRequest) (adapter.LaunchResult, error) {
	if err := ctx.Err(); err != nil {
		return adapter.LaunchResult{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	id := strings.TrimSpace(request.Reservation.AllocationID)
	slot, ok := m.reservations[id]
	if !ok {
		return adapter.LaunchResult{}, fmt.Errorf("memory adapter: unknown allocation %q", id)
	}
	if request.Warm {
		slot.phase = memoryWarm
	} else {
		slot.phase = memoryActive
	}
	m.reservations[id] = slot
	label := strings.TrimSpace(request.Reservation.RunnerLabel)
	if label == "" {
		label = strings.TrimSpace(slot.reservation.RunnerLabel)
	}
	if label == "" {
		label = "uecb-mem-" + id
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

func (m *Memory) Cleanup(ctx context.Context, request adapter.CleanupRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.reservations, request.Reservation.AllocationID)
	return nil
}

func (m *Memory) capacityLocked() adapter.CapacityStatus {
	status := adapter.CapacityStatus{MaxRunners: m.maxRunners}
	for _, slot := range m.reservations {
		switch slot.phase {
		case memoryWarm:
			status.WarmRunners++
		case memoryActive:
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
