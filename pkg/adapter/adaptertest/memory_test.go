package adaptertest

import (
	"context"
	"testing"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/pkg/adapter"
)

func TestMemoryLaunchRejectsUnknownAllocation(t *testing.T) {
	impl := NewMemory(1)
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

func TestMemoryLaunchRejectsEmptyAllocationID(t *testing.T) {
	impl := NewMemory(1)
	before, err := impl.Capacity(context.Background())
	if err != nil {
		t.Fatalf("Capacity: %v", err)
	}

	_, err = impl.Launch(context.Background(), adapter.LaunchRequest{
		Reservation: adapter.Reservation{AllocationID: "", RunnerLabel: "ghost"},
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
