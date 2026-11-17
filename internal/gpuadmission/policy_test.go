package gpuadmission

import (
	"errors"
	"math"
	"testing"
	"time"
)

func ptr[T any](v T) *T { return &v }
func fixture() (Policy, Identity, Observation, time.Time) {
	n := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	id := Identity{"node", "GPU-id"}
	p := Policy{ptr(time.Minute), ptr(uint64(5)), ptr(float64(2)), ptr(uint64(0))}
	o := Observation{id, &Sample[Memory]{Memory{10, 4}, n}, &Sample[Slices]{Slices{10, 7, 1}, n}, &Sample[Health]{HealthOK, n}, &Sample[float64]{1, n}, &Sample[uint64]{0, n}, &Sample[Reservations]{Reservations{}, n}}
	return p, id, o, n
}
func TestRejections(t *testing.T) {
	cases := []struct {
		name   string
		change func(*Policy, *Observation)
		want   Reason
	}{
		{"policy", func(p *Policy, o *Observation) { p.MaxAge = nil }, PolicyInvalid},
		{"uuid", func(p *Policy, o *Observation) { o.Device.UUID = "changed" }, IdentityMismatch},
		{"node", func(p *Policy, o *Observation) { o.Device.Node = "changed" }, IdentityMismatch},
		{"missing", func(p *Policy, o *Observation) { o.Memory = nil }, MissingObservation},
		{"future", func(p *Policy, o *Observation) { o.Health.At = o.Health.At.Add(time.Second) }, StaleObservation},
		{"old", func(p *Policy, o *Observation) { o.Slices.At = o.Slices.At.Add(-2 * time.Minute) }, StaleObservation},
		{"overflow", func(p *Policy, o *Observation) { o.Slices.Value.Pending = math.MaxUint64 }, InvalidObservation},
		{"memory invalid", func(p *Policy, o *Observation) { o.Memory.Value.Used = 11 }, InvalidObservation},
		{"nan", func(p *Policy, o *Observation) { o.ProtectedLatency.Value = math.NaN() }, InvalidObservation},
		{"infinity", func(p *Policy, o *Observation) { o.ProtectedLatency.Value = math.Inf(1) }, InvalidObservation},
		{"negative", func(p *Policy, o *Observation) { o.ProtectedLatency.Value = -1 }, InvalidObservation},
		{"health", func(p *Policy, o *Observation) { o.Health.Value = HealthUnknown }, Unhealthy},
		{"memory", func(p *Policy, o *Observation) { o.Memory.Value.Used = 6 }, MemoryPressure},
		{"slices", func(p *Policy, o *Observation) { o.Slices.Value.Pending = 3 }, SlicePressure},
		{"latency", func(p *Policy, o *Observation) { o.ProtectedLatency.Value = 3 }, ProtectedPressure},
		{"errors", func(p *Policy, o *Observation) { o.ProtectedErrors.Value = 1 }, ProtectedPressure},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, id, o, n := fixture()
			tc.change(&p, &o)
			err := Evaluate(p, id, o, n)
			var r *Rejection
			if !errors.As(err, &r) || r.Reason != tc.want {
				t.Fatalf("got %v want %s", err, tc.want)
			}
		})
	}
}
func TestGlobalCountersAndExplicitZero(t *testing.T) {
	p, id, o, n := fixture()
	if err := Evaluate(p, id, o, n); err != nil {
		t.Fatal(err)
	}
	if err := Evaluate(p, id, o, n); err != nil {
		t.Fatal("comparison must not pretend to consume", err)
	}
	for _, r := range []Reservations{{Reserved: 1}, {Running: 1}, {CleanupPending: 1}, {Orphans: math.MaxUint64}} {
		o.Reservations.Value = r
		if err := Evaluate(p, id, o, n); err == nil {
			t.Fatal("busy accepted")
		}
	}
	o.Reservations.Value = Reservations{}
	p.MaxProtectedLatency = ptr(float64(0))
	o.ProtectedLatency.Value = 0
	if err := Evaluate(p, id, o, n); err != nil {
		t.Fatal("explicit zero rejected", err)
	}
	p.MaxProtectedLatency = nil
	if Evaluate(p, id, o, n) == nil {
		t.Fatal("missing policy accepted")
	}
}
func FuzzAccounting(f *testing.F) {
	f.Add(uint64(10), uint64(7), uint64(1), float64(1))
	f.Fuzz(func(t *testing.T, cap, used, pending uint64, lat float64) {
		p, id, o, n := fixture()
		o.Slices.Value = Slices{cap, used, pending}
		o.ProtectedLatency.Value = lat
		err := Evaluate(p, id, o, n)
		if err == nil && (cap == 0 || used >= cap || pending >= cap-used || !finite(lat) || lat > 2) {
			t.Fatal("invalid accounting accepted")
		}
	})
}

func TestEveryRequiredFieldAndTimestamp(t *testing.T) {
	for _, change := range []func(*Policy){func(p *Policy) { p.RequiredFreeBytes = nil }, func(p *Policy) { p.MaxProtectedErrors = nil }, func(p *Policy) { p.MaxAge = ptr(time.Duration(0)) }, func(p *Policy) { p.MaxProtectedLatency = ptr(math.NaN()) }, func(p *Policy) { p.MaxProtectedLatency = ptr(math.Inf(1)) }} {
		p, id, o, n := fixture()
		change(&p)
		if Evaluate(p, id, o, n) == nil {
			t.Fatal("invalid policy")
		}
	}
	for _, change := range []func(*Observation){func(o *Observation) { o.Slices = nil }, func(o *Observation) { o.Health = nil }, func(o *Observation) { o.ProtectedLatency = nil }, func(o *Observation) { o.ProtectedErrors = nil }, func(o *Observation) { o.Reservations = nil }} {
		p, id, o, n := fixture()
		change(&o)
		if Evaluate(p, id, o, n) == nil {
			t.Fatal("missing sample")
		}
	}
	for index := 0; index < 6; index++ {
		p, id, o, n := fixture()
		times := []*time.Time{&o.Memory.At, &o.Slices.At, &o.Health.At, &o.ProtectedLatency.At, &o.ProtectedErrors.At, &o.Reservations.At}
		*times[index] = n.Add(-*p.MaxAge)
		if err := Evaluate(p, id, o, n); err != nil {
			t.Fatal("exact freshness boundary", err)
		}
		*times[index] = times[index].Add(-time.Nanosecond)
		if Evaluate(p, id, o, n) == nil {
			t.Fatal("expired timestamp")
		}
		*times[index] = time.Time{}
		if Evaluate(p, id, o, n) == nil {
			t.Fatal("missing timestamp")
		}
	}
	p, id, o, _ := fixture()
	if Evaluate(p, id, o, time.Time{}) == nil {
		t.Fatal("zero clock")
	}
}

func TestFreshnessDoesNotSaturateDuration(t *testing.T) {
	p, id, o, now := fixture()
	p.MaxAge = ptr(time.Duration(math.MaxInt64))
	boundary := now.Add(-*p.MaxAge)
	for _, at := range []*time.Time{&o.Memory.At, &o.Slices.At, &o.Health.At, &o.ProtectedLatency.At, &o.ProtectedErrors.At, &o.Reservations.At} {
		*at = boundary
	}
	if err := Evaluate(p, id, o, now); err != nil {
		t.Fatalf("exact max-duration boundary: %v", err)
	}
	o.Memory.At = boundary.Add(-time.Nanosecond)
	if err := Evaluate(p, id, o, now); err == nil {
		t.Fatal("saturated duration accepted an older sample")
	}
}
