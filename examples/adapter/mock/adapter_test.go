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

func TestLaunchRejectsUnknownAllocation(t *testing.T) {
	impl := New(1)
	before, err := impl.Capacity(context.Background())
	if err != nil {
		t.Fatalf("Capacity: %v", err)
	}

	_, err = impl.Launch(context.Background(), adapter.LaunchRequest{
		Reservation: adapter.Reservation{AllocationID: "does-not-exist", RunnerLabel: "ghost"},
	})
	if err == nil {
		t.Fatal("Launch of unknown allocation returned nil error, want rejection")
	}

	after, err := impl.Capacity(context.Background())
	if err != nil {
		t.Fatalf("Capacity after Launch: %v", err)
	}
	if after.ActiveRunners != before.ActiveRunners || after.PendingRunners != before.PendingRunners || after.WarmRunners != before.WarmRunners {
		t.Fatalf("Launch of unknown allocation changed capacity from %+v to %+v", before, after)
	}
}

func TestLaunchRejectsEmptyAllocationID(t *testing.T) {
	impl := New(1)
	before, err := impl.Capacity(context.Background())
	if err != nil {
		t.Fatalf("Capacity: %v", err)
	}

	_, err = impl.Launch(context.Background(), adapter.LaunchRequest{
		Reservation: adapter.Reservation{AllocationID: "   ", RunnerLabel: "ghost"},
	})
	if err == nil {
		t.Fatal("Launch with empty AllocationID returned nil error, want rejection")
	}

	after, err := impl.Capacity(context.Background())
	if err != nil {
		t.Fatalf("Capacity after Launch: %v", err)
	}
	if after.ActiveRunners != before.ActiveRunners || after.PendingRunners != before.PendingRunners || after.WarmRunners != before.WarmRunners {
		t.Fatalf("Launch with empty AllocationID changed capacity from %+v to %+v", before, after)
	}
}
