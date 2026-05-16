package adaptertest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/pkg/adapter"
)

type Factory func(t testing.TB) adapter.Adapter

type Options struct {
	JobTimeout time.Duration
}

func RunConformance(t *testing.T, factory Factory, options Options) {
	t.Helper()
	if options.JobTimeout <= 0 {
		options.JobTimeout = 15 * time.Minute
	}

	t.Run("health", func(t *testing.T) {
		got, err := factory(t).Health(context.Background())
		if err != nil {
			t.Fatalf("Health returned error: %v", err)
		}
		if !got.Healthy {
			t.Fatalf("Health returned unhealthy status: %+v", got)
		}
	})

	t.Run("capacity", func(t *testing.T) {
		got, err := factory(t).Capacity(context.Background())
		if err != nil {
			t.Fatalf("Capacity returned error: %v", err)
		}
		if got.MaxRunners <= 0 {
			t.Fatalf("Capacity MaxRunners must be positive, got %+v", got)
		}
		if got.ActiveRunners < 0 || got.PendingRunners < 0 || got.WarmRunners < 0 {
			t.Fatalf("Capacity counters must not be negative, got %+v", got)
		}
	})

	t.Run("reserve-launch-cleanup", func(t *testing.T) {
		adapterUnderTest := factory(t)
		reservation, err := adapterUnderTest.Reserve(context.Background(), adapter.ReservationRequest{
			AllocationID: "conformance-1",
			Pool:         "lite",
			Backend:      "mock",
			Tenant:       "conformance",
			Labels:       []string{"self-hosted", "linux", "x64"},
			Capabilities: []string{"docker"},
			JobTimeout:   options.JobTimeout,
		})
		if err != nil {
			t.Fatalf("Reserve returned error: %v", err)
		}
		if strings.TrimSpace(reservation.AllocationID) == "" {
			t.Fatalf("Reserve must preserve or generate an allocation id: %+v", reservation)
		}
		if strings.TrimSpace(reservation.RunnerLabel) == "" {
			t.Fatalf("Reserve must return a runner label: %+v", reservation)
		}
		if !reservation.ExpiresAt.IsZero() && time.Until(reservation.ExpiresAt) <= 0 {
			t.Fatalf("Reserve returned an already-expired reservation: %+v", reservation)
		}

		launch, err := adapterUnderTest.Launch(context.Background(), adapter.LaunchRequest{Reservation: reservation})
		if err != nil {
			t.Fatalf("Launch returned error: %v", err)
		}
		if strings.TrimSpace(launch.RunnerLabel) == "" {
			t.Fatalf("Launch must return a runner label: %+v", launch)
		}

		if err := adapterUnderTest.Cleanup(context.Background(), adapter.CleanupRequest{
			Reservation: reservation,
			State:       adapter.StateCompleted,
			Reason:      "conformance complete",
		}); err != nil {
			t.Fatalf("Cleanup returned error: %v", err)
		}
	})
}
