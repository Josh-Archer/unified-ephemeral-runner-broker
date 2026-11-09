package mock

import (
	"context"
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/pkg/adapter"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/pkg/adapter/adaptertest"
)

func TestAdapterConformance(t *testing.T) {
	adaptertest.RunConformance(t, func(testing.TB) adapter.Adapter {
		return New(1)
	}, adaptertest.Options{
		Backend: "mock",
		Pool:    "lite",
	})
}

func TestReserveCountsPendingUntilLaunch(t *testing.T) {
	impl := New(1)
	reservation, err := impl.Reserve(context.Background(), adapter.ReservationRequest{
		AllocationID: "pending-1",
		JobTimeout:   time.Minute,
	})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	status, err := impl.Capacity(context.Background())
	if err != nil {
		t.Fatalf("Capacity: %v", err)
	}
	if status.PendingRunners != 1 || status.ActiveRunners != 0 || status.WarmRunners != 0 {
		t.Fatalf("capacity after reserve = %+v, want 1 pending", status)
	}

	if _, err := impl.Launch(context.Background(), adapter.LaunchRequest{Reservation: reservation, Warm: true}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	status, err = impl.Capacity(context.Background())
	if err != nil {
		t.Fatalf("Capacity after launch: %v", err)
	}
	if status.WarmRunners != 1 || status.ActiveRunners != 0 || status.PendingRunners != 0 {
		t.Fatalf("capacity after warm launch = %+v, want 1 warm", status)
	}
}
