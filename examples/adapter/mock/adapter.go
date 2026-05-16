package mock

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/pkg/adapter"
)

type Adapter struct {
	mu           sync.Mutex
	maxRunners   int
	reservations map[string]adapter.Reservation
}

func New(maxRunners int) *Adapter {
	if maxRunners <= 0 {
		maxRunners = 1
	}
	return &Adapter{
		maxRunners:   maxRunners,
		reservations: map[string]adapter.Reservation{},
	}
}

func (a *Adapter) Health(context.Context) (adapter.HealthStatus, error) {
	return adapter.HealthStatus{Healthy: true, Checked: time.Now().UTC()}, nil
}

func (a *Adapter) Capacity(context.Context) (adapter.CapacityStatus, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return adapter.CapacityStatus{
		MaxRunners:    a.maxRunners,
		ActiveRunners: len(a.reservations),
	}, nil
}

func (a *Adapter) Reserve(_ context.Context, request adapter.ReservationRequest) (adapter.Reservation, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if len(a.reservations) >= a.maxRunners {
		return adapter.Reservation{}, fmt.Errorf("mock adapter capacity exhausted")
	}

	id := request.AllocationID
	if id == "" {
		id = fmt.Sprintf("mock-%d", len(a.reservations)+1)
	}
	reservation := adapter.Reservation{
		AllocationID: id,
		RunnerName:   "uecb-mock-" + id,
		RunnerLabel:  "uecb-mock-" + id,
		ExpiresAt:    time.Now().UTC().Add(request.JobTimeout),
		Metadata: map[string]string{
			"adapter": "mock",
		},
	}
	a.reservations[id] = reservation
	return reservation, nil
}

func (a *Adapter) Launch(_ context.Context, request adapter.LaunchRequest) (adapter.LaunchResult, error) {
	return adapter.LaunchResult{
		RunnerLabel: request.Reservation.RunnerLabel,
		Metadata: map[string]string{
			"launch_mode": "cold",
		},
	}, nil
}

func (a *Adapter) Cleanup(_ context.Context, request adapter.CleanupRequest) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.reservations, request.Reservation.AllocationID)
	return nil
}
