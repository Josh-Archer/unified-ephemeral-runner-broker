package adaptertest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/pkg/adapter"
)

// DefaultMaxProbeReservations is the capacity-exhaustion probe cap used when
// Options.MaxProbeReservations is zero. Factories used with the kit should
// report a free-slot count at or below this so the probe runs.
const DefaultMaxProbeReservations = 4

// Factory constructs the adapter under test. RunConformance calls it once per
// case so cases do not share reservations.
type Factory func(t testing.TB) adapter.Adapter

// Options configures the reservation the in-process kit sends.
// Zero values select conformance defaults.
type Options struct {
	JobTimeout   time.Duration
	Pool         adapter.PoolName
	Backend      adapter.BackendName
	Tenant       string
	Priority     string
	Labels       []string
	Capabilities []string
	Metadata     map[string]string
	// Warm is forwarded on LaunchRequest.
	Warm bool
	// MaxProbeReservations caps how many Reserve calls the exhaustion case
	// may make. Zero uses DefaultMaxProbeReservations. When the adapter has
	// more free slots than this cap, the exhaustion case skips unless
	// RequireExhaustion is set.
	MaxProbeReservations int
	// RequireExhaustion fails the exhaustion case instead of skipping it when
	// the adapter's free-slot count exceeds MaxProbeReservations.
	RequireExhaustion bool
}

type skipSignal struct {
	reason string
}

func (s skipSignal) Error() string { return s.reason }

func (o Options) normalized() Options {
	if o.JobTimeout <= 0 {
		o.JobTimeout = 15 * time.Minute
	}
	if o.Pool == "" {
		o.Pool = "lite"
	}
	if o.Backend == "" {
		o.Backend = "conformance"
	}
	if o.Tenant == "" {
		o.Tenant = "conformance"
	}
	if len(o.Labels) == 0 {
		o.Labels = []string{"self-hosted", "linux", "x64"}
	}
	if len(o.Capabilities) == 0 {
		o.Capabilities = []string{"docker"}
	}
	if o.MaxProbeReservations <= 0 {
		o.MaxProbeReservations = DefaultMaxProbeReservations
	}
	return o
}

// RunConformance checks the frozen v1 Adapter lifecycle.
// The factory must return a healthy adapter. Keep its free-slot count at or
// below MaxProbeReservations so capacity exhaustion is actually exercised.
func RunConformance(t *testing.T, factory Factory, options Options) {
	t.Helper()
	options = options.normalized()

	t.Run("health", func(t *testing.T) {
		t.Helper()
		if err := checkHealth(context.Background(), factory(t)); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("capacity", func(t *testing.T) {
		t.Helper()
		if err := checkCapacity(context.Background(), factory(t)); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("reserve-launch-cleanup", func(t *testing.T) {
		t.Helper()
		if err := checkReserveLaunchCleanup(context.Background(), factory(t), options); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("capacity-accounting", func(t *testing.T) {
		t.Helper()
		if err := checkCapacityAccounting(context.Background(), factory(t), options); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("idempotent-reserve", func(t *testing.T) {
		t.Helper()
		if err := checkIdempotentReserve(context.Background(), factory(t), options); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("cleanup-unknown", func(t *testing.T) {
		t.Helper()
		if err := checkCleanupUnknown(context.Background(), factory(t)); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("cleanup-terminal-states", func(t *testing.T) {
		t.Helper()
		if err := checkCleanupTerminalStates(context.Background(), factory(t), options); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("capacity-exhausted", func(t *testing.T) {
		t.Helper()
		err := checkExhaustion(context.Background(), factory(t), options)
		var skip skipSignal
		if errors.As(err, &skip) {
			t.Skip(skip.Error())
		}
		if err != nil {
			t.Fatal(err)
		}
	})
	t.Run("canceled-context", func(t *testing.T) {
		t.Helper()
		if err := checkCanceledContext(factory(t), options); err != nil {
			t.Fatal(err)
		}
	})
}

func checkHealth(ctx context.Context, impl adapter.Adapter) error {
	if impl == nil {
		return errors.New("factory returned a nil adapter")
	}
	got, err := impl.Health(ctx)
	if err != nil {
		return fmt.Errorf("Health returned error: %w", err)
	}
	if !got.Healthy {
		return fmt.Errorf("Health returned unhealthy status: %+v", got)
	}
	if got.Checked.IsZero() {
		return fmt.Errorf("Health returned a zero Checked time: %+v", got)
	}
	return nil
}

func checkCapacity(ctx context.Context, impl adapter.Adapter) error {
	if impl == nil {
		return errors.New("factory returned a nil adapter")
	}
	got, err := impl.Capacity(ctx)
	if err != nil {
		return fmt.Errorf("Capacity returned error: %w", err)
	}
	return validateCapacity(got)
}

func checkReserveLaunchCleanup(ctx context.Context, impl adapter.Adapter, opts Options) error {
	if impl == nil {
		return errors.New("factory returned a nil adapter")
	}
	request := reservationRequest(opts, "conformance-1")
	reservation, err := impl.Reserve(ctx, request)
	if err != nil {
		return fmt.Errorf("Reserve returned error: %w", err)
	}
	if err := validateReservation(request, reservation); err != nil {
		return err
	}

	launch, err := impl.Launch(ctx, adapter.LaunchRequest{Reservation: reservation, Warm: opts.Warm})
	if err != nil {
		return fmt.Errorf("Launch returned error: %w", err)
	}
	if strings.TrimSpace(launch.RunnerLabel) == "" {
		return fmt.Errorf("Launch must return a runner label: %+v", launch)
	}

	cleanup := adapter.CleanupRequest{
		Reservation: reservation,
		State:       adapter.StateCompleted,
		Reason:      "conformance complete",
	}
	if err := impl.Cleanup(ctx, cleanup); err != nil {
		return fmt.Errorf("Cleanup returned error: %w", err)
	}
	if err := impl.Cleanup(ctx, cleanup); err != nil {
		return fmt.Errorf("Cleanup must be idempotent: %w", err)
	}
	return nil
}

func checkCapacityAccounting(ctx context.Context, impl adapter.Adapter, opts Options) error {
	if impl == nil {
		return errors.New("factory returned a nil adapter")
	}
	before, err := impl.Capacity(ctx)
	if err != nil {
		return fmt.Errorf("Capacity before reserve: %w", err)
	}
	if err := validateCapacity(before); err != nil {
		return err
	}
	request := reservationRequest(opts, "acct-1")
	reservation, err := impl.Reserve(ctx, request)
	if err != nil {
		return fmt.Errorf("Reserve: %w", err)
	}
	if err := validateReservation(request, reservation); err != nil {
		return err
	}
	reserved, err := impl.Capacity(ctx)
	if err != nil {
		return fmt.Errorf("Capacity after reserve: %w", err)
	}
	if got, want := usedSlots(reserved), usedSlots(before)+1; got != want {
		return fmt.Errorf("capacity used = %d after Reserve, want %d; counters must include the new slot before Reserve returns", got, want)
	}
	if _, err := impl.Launch(ctx, adapter.LaunchRequest{Reservation: reservation, Warm: opts.Warm}); err != nil {
		return fmt.Errorf("Launch: %w", err)
	}
	launched, err := impl.Capacity(ctx)
	if err != nil {
		return fmt.Errorf("Capacity after launch: %w", err)
	}
	if got, want := usedSlots(launched), usedSlots(before)+1; got != want {
		return fmt.Errorf("capacity used = %d after Launch, want %d", got, want)
	}
	if err := impl.Cleanup(ctx, adapter.CleanupRequest{
		Reservation: reservation,
		State:       adapter.StateCompleted,
		Reason:      "conformance complete",
	}); err != nil {
		return fmt.Errorf("Cleanup: %w", err)
	}
	after, err := impl.Capacity(ctx)
	if err != nil {
		return fmt.Errorf("Capacity after cleanup: %w", err)
	}
	if got, want := usedSlots(after), usedSlots(before); got != want {
		return fmt.Errorf("capacity used = %d after Cleanup, want %d", got, want)
	}
	return nil
}

func checkIdempotentReserve(ctx context.Context, impl adapter.Adapter, opts Options) error {
	if impl == nil {
		return errors.New("factory returned a nil adapter")
	}
	before, err := impl.Capacity(ctx)
	if err != nil {
		return fmt.Errorf("Capacity: %w", err)
	}
	request := reservationRequest(opts, "idem-1")
	first, err := impl.Reserve(ctx, request)
	if err != nil {
		return fmt.Errorf("first Reserve: %w", err)
	}
	second, err := impl.Reserve(ctx, request)
	if err != nil {
		return fmt.Errorf("second Reserve of %q: %w", request.AllocationID, err)
	}
	if second.AllocationID != first.AllocationID || second.RunnerLabel != first.RunnerLabel {
		return fmt.Errorf("repeat Reserve returned %+v, want the original %+v", second, first)
	}
	mid, err := impl.Capacity(ctx)
	if err != nil {
		return fmt.Errorf("Capacity after repeat Reserve: %w", err)
	}
	if got, want := usedSlots(mid), usedSlots(before)+1; got != want {
		return fmt.Errorf("repeat Reserve consumed %d slots, want 1", got-usedSlots(before))
	}
	return impl.Cleanup(ctx, adapter.CleanupRequest{Reservation: first, State: adapter.StateCanceled, Reason: "conformance release"})
}

func checkCleanupUnknown(ctx context.Context, impl adapter.Adapter) error {
	if impl == nil {
		return errors.New("factory returned a nil adapter")
	}
	before, err := impl.Capacity(ctx)
	if err != nil {
		return fmt.Errorf("Capacity: %w", err)
	}
	err = impl.Cleanup(ctx, adapter.CleanupRequest{
		Reservation: adapter.Reservation{AllocationID: "does-not-exist"},
		State:       adapter.StateCanceled,
		Reason:      "unknown allocation",
	})
	if err != nil {
		return fmt.Errorf("Cleanup of an unknown allocation: %w", err)
	}
	after, err := impl.Capacity(ctx)
	if err != nil {
		return fmt.Errorf("Capacity after unknown cleanup: %w", err)
	}
	if usedSlots(after) != usedSlots(before) {
		return fmt.Errorf("unknown Cleanup changed used slots from %d to %d", usedSlots(before), usedSlots(after))
	}
	return nil
}

func checkCleanupTerminalStates(ctx context.Context, impl adapter.Adapter, opts Options) error {
	if impl == nil {
		return errors.New("factory returned a nil adapter")
	}
	states := []adapter.AllocationState{
		adapter.StateCanceled,
		adapter.StateExpired,
		adapter.StateFailed,
		adapter.StateCompleted,
	}
	for _, state := range states {
		before, err := impl.Capacity(ctx)
		if err != nil {
			return fmt.Errorf("Capacity before %s: %w", state, err)
		}
		request := reservationRequest(opts, "terminal-"+string(state))
		reservation, err := impl.Reserve(ctx, request)
		if err != nil {
			return fmt.Errorf("Reserve for %s: %w", state, err)
		}
		if err := impl.Cleanup(ctx, adapter.CleanupRequest{
			Reservation: reservation,
			State:       state,
			Reason:      "conformance " + string(state),
		}); err != nil {
			return fmt.Errorf("Cleanup state %s: %w", state, err)
		}
		after, err := impl.Capacity(ctx)
		if err != nil {
			return fmt.Errorf("Capacity after %s: %w", state, err)
		}
		if usedSlots(after) != usedSlots(before) {
			return fmt.Errorf("Cleanup state %s left used slots at %d, want %d", state, usedSlots(after), usedSlots(before))
		}
	}
	return nil
}

func checkExhaustion(ctx context.Context, impl adapter.Adapter, opts Options) error {
	if impl == nil {
		return errors.New("factory returned a nil adapter")
	}
	status, err := impl.Capacity(ctx)
	if err != nil {
		return fmt.Errorf("Capacity: %w", err)
	}
	if err := validateCapacity(status); err != nil {
		return err
	}
	free := adapter.FreeSlots(status)
	if free > opts.MaxProbeReservations {
		message := fmt.Sprintf("free slots %d exceed MaxProbeReservations %d; shrink the test adapter or raise the cap", free, opts.MaxProbeReservations)
		if opts.RequireExhaustion {
			return errors.New(message)
		}
		return skipSignal{reason: message}
	}

	held := make([]adapter.Reservation, 0, free)
	for i := 0; i < free; i++ {
		reservation, err := impl.Reserve(ctx, reservationRequest(opts, fmt.Sprintf("fill-%d", i)))
		if err != nil {
			return fmt.Errorf("Reserve %d of %d free slots: %w", i+1, free, err)
		}
		held = append(held, reservation)
	}
	if _, err := impl.Reserve(ctx, reservationRequest(opts, "overflow")); !adapter.IsCapacityExhausted(err) {
		return fmt.Errorf("Reserve past capacity returned %v, want ErrCapacityExhausted", err)
	}
	filled, err := impl.Capacity(ctx)
	if err != nil {
		return fmt.Errorf("Capacity after filling free slots: %w", err)
	}
	if got, want := usedSlots(filled), usedSlots(status)+free; got != want {
		return fmt.Errorf("used slots = %d after filling %d free slots, want %d", got, free, want)
	}
	for _, reservation := range held {
		if err := impl.Cleanup(ctx, adapter.CleanupRequest{
			Reservation: reservation,
			State:       adapter.StateCanceled,
			Reason:      "conformance release",
		}); err != nil {
			return fmt.Errorf("Cleanup %s: %w", reservation.AllocationID, err)
		}
	}
	return nil
}

func checkCanceledContext(impl adapter.Adapter, opts Options) error {
	if impl == nil {
		return errors.New("factory returned a nil adapter")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := impl.Health(ctx); !errors.Is(err, context.Canceled) {
		return fmt.Errorf("Health with a canceled context returned %v, want context.Canceled", err)
	}
	if _, err := impl.Capacity(ctx); !errors.Is(err, context.Canceled) {
		return fmt.Errorf("Capacity with a canceled context returned %v, want context.Canceled", err)
	}

	before, err := impl.Capacity(context.Background())
	if err != nil {
		return fmt.Errorf("Capacity: %w", err)
	}
	if _, err := impl.Reserve(ctx, reservationRequest(opts, "canceled-1")); !errors.Is(err, context.Canceled) {
		return fmt.Errorf("Reserve with a canceled context returned %v, want context.Canceled", err)
	}
	after, err := impl.Capacity(context.Background())
	if err != nil {
		return fmt.Errorf("Capacity after canceled Reserve: %w", err)
	}
	if usedSlots(after) != usedSlots(before) {
		return fmt.Errorf("canceled Reserve changed used slots from %d to %d", usedSlots(before), usedSlots(after))
	}
	if _, err := impl.Launch(ctx, adapter.LaunchRequest{
		Reservation: adapter.Reservation{AllocationID: "canceled-1", RunnerLabel: "canceled"},
	}); !errors.Is(err, context.Canceled) {
		return fmt.Errorf("Launch with a canceled context returned %v, want context.Canceled", err)
	}
	if err := impl.Cleanup(ctx, adapter.CleanupRequest{
		Reservation: adapter.Reservation{AllocationID: "canceled-1"},
		State:       adapter.StateCanceled,
	}); !errors.Is(err, context.Canceled) {
		return fmt.Errorf("Cleanup with a canceled context returned %v, want context.Canceled", err)
	}
	return nil
}

func reservationRequest(opts Options, id string) adapter.ReservationRequest {
	return adapter.ReservationRequest{
		AllocationID: id,
		Pool:         opts.Pool,
		Backend:      opts.Backend,
		Tenant:       opts.Tenant,
		Priority:     opts.Priority,
		Labels:       append([]string(nil), opts.Labels...),
		Capabilities: append([]string(nil), opts.Capabilities...),
		JobTimeout:   opts.JobTimeout,
		Metadata:     cloneMetadata(opts.Metadata),
	}
}

func validateCapacity(status adapter.CapacityStatus) error {
	if status.MaxRunners <= 0 {
		return fmt.Errorf("Capacity MaxRunners must be positive, got %+v", status)
	}
	if status.ActiveRunners < 0 || status.PendingRunners < 0 || status.WarmRunners < 0 {
		return fmt.Errorf("Capacity counters must not be negative, got %+v", status)
	}
	return nil
}

func validateReservation(request adapter.ReservationRequest, reservation adapter.Reservation) error {
	if strings.TrimSpace(reservation.AllocationID) == "" {
		return fmt.Errorf("Reserve must return an allocation id: %+v", reservation)
	}
	if request.AllocationID != "" && reservation.AllocationID != request.AllocationID {
		return fmt.Errorf("Reserve allocation id = %q, want %q", reservation.AllocationID, request.AllocationID)
	}
	if strings.TrimSpace(reservation.RunnerLabel) == "" {
		return fmt.Errorf("Reserve must return a runner label: %+v", reservation)
	}
	if !reservation.ExpiresAt.IsZero() && !reservation.ExpiresAt.After(time.Now()) {
		return fmt.Errorf("Reserve returned an already-expired reservation: %+v", reservation)
	}
	return nil
}

func usedSlots(status adapter.CapacityStatus) int {
	return status.ActiveRunners + status.PendingRunners + status.WarmRunners
}
