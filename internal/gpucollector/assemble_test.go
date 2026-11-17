package gpucollector

import (
	"errors"
	"math"
	"strconv"
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/gpuadmission"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/gputelemetry"
)

func pointer[T any](v T) *T { return &v }

var clock = time.Unix(1700000000, 0).UTC()

func fixture() (Config, Inputs) {
	c := Config{Selection: gputelemetry.Selection{NodeLabel: "node", UUIDLabel: "UUID", Node: "desktop", UUID: "GPU-one"}, Policy: gpuadmission.Policy{MaxAge: pointer(time.Minute), RequiredFreeBytes: pointer(uint64(10)), MaxProtectedLatency: pointer(0.5), MaxProtectedErrors: pointer(uint64(0))}, Units: map[gputelemetry.Metric]Unit{}, HealthMeaning: ZeroErrorsHealthy, WorkloadLabel: "workload", ProtectedWorkloads: []string{"plex", "deskquest"}}
	for k, v := range expectedUnits {
		c.Units[k] = v
	}
	sample := func(value, workload string) gputelemetry.Sample {
		labels := map[string]string{"node": "desktop", "UUID": "GPU-one"}
		if workload != "" {
			labels["workload"] = workload
		}
		return gputelemetry.Sample{Labels: labels, ValueText: value, At: clock, Timestamp: "1700000000"}
	}
	in := Inputs{Device: map[gputelemetry.Metric]gputelemetry.Sample{}, Protected: map[string]ProtectedSample{}, Reservations: &gpuadmission.Sample[gpuadmission.Reservations]{At: clock}}
	for _, m := range deviceMetrics {
		in.Device[m] = sample("0", "")
	}
	in.Device[gputelemetry.MemoryTotal] = sample("100", "")
	in.Device[gputelemetry.SliceCapacity] = sample("10", "")
	for _, w := range c.ProtectedWorkloads {
		in.Protected[w] = ProtectedSample{Latency: sample("0.1", w), Errors: sample("0", w)}
	}
	return c, in
}
func expect(t *testing.T, err error, want Reason) {
	t.Helper()
	var r *Rejection
	if !errors.As(err, &r) || r.Reason != want {
		t.Fatalf("got %v want %v", err, want)
	}
}
func TestAssembleAndEvaluate(t *testing.T) {
	c, in := fixture()
	o, err := Assemble(c, in, clock)
	if err != nil {
		t.Fatal(err)
	}
	if err := gpuadmission.Evaluate(c.Policy, o.Device, o, clock); err != nil {
		t.Fatal(err)
	}
	in.Reservations.Value.Orphans = 1
	if o.Reservations.Value.Orphans != 0 {
		t.Fatal("reservation aliases input")
	}
	o, err = Assemble(c, in, clock)
	if err != nil {
		t.Fatal(err)
	}
	if err := gpuadmission.Evaluate(c.Policy, o.Device, o, clock); err == nil {
		t.Fatal("orphan ignored")
	}
}
func TestIndependentTimestamps(t *testing.T) {
	c, in := fixture()
	s := in.Device[gputelemetry.MemoryUsed]
	s.At = clock.Add(-20 * time.Second)
	in.Device[gputelemetry.MemoryUsed] = s
	s = in.Device[gputelemetry.SlicePending]
	s.At = clock.Add(-30 * time.Second)
	in.Device[gputelemetry.SlicePending] = s
	pair := in.Protected["plex"]
	pair.Latency.At = clock.Add(-40 * time.Second)
	pair.Errors.At = clock.Add(-50 * time.Second)
	in.Protected["plex"] = pair
	o, err := Assemble(c, in, clock)
	if err != nil {
		t.Fatal(err)
	}
	if !o.Memory.At.Equal(clock.Add(-20*time.Second)) || !o.Slices.At.Equal(clock.Add(-30*time.Second)) || !o.ProtectedLatency.At.Equal(pair.Latency.At) || !o.ProtectedErrors.At.Equal(pair.Errors.At) {
		t.Fatal("lost component time")
	}
	// A future component cannot be hidden by grouping it with an older component.
	s = in.Device[gputelemetry.MemoryTotal]
	s.At = clock.Add(time.Nanosecond)
	in.Device[gputelemetry.MemoryTotal] = s
	_, err = Assemble(c, in, clock)
	expect(t, err, StaleSample)
}
func TestRejectInputs(t *testing.T) {
	cases := []struct {
		name   string
		change func(*Config, *Inputs)
		want   Reason
	}{
		{"missing units", func(c *Config, i *Inputs) { delete(c.Units, gputelemetry.MemoryTotal) }, InvalidConfig},
		{"MiB not bytes", func(c *Config, i *Inputs) { c.Units[gputelemetry.MemoryTotal] = "MiB" }, InvalidConfig},
		{"unknown health", func(c *Config, i *Inputs) { c.HealthMeaning = "" }, InvalidConfig},
		{"missing policy", func(c *Config, i *Inputs) { c.Policy.MaxAge = nil }, InvalidConfig},
		{"nan policy", func(c *Config, i *Inputs) { c.Policy.MaxProtectedLatency = pointer(math.NaN()) }, InvalidConfig},
		{"no workloads", func(c *Config, i *Inputs) { c.ProtectedWorkloads = nil }, InvalidConfig},
		{"duplicate workloads", func(c *Config, i *Inputs) { c.ProtectedWorkloads = []string{"plex", "plex"} }, InvalidConfig},
		{"missing coverage", func(c *Config, i *Inputs) { delete(i.Protected, "plex") }, MissingCoverage},
		{"unknown coverage", func(c *Config, i *Inputs) { i.Protected["other"] = i.Protected["plex"]; delete(i.Protected, "plex") }, MissingCoverage},
		{"wrong workload label", func(c *Config, i *Inputs) { i.Protected["plex"].Latency.Labels["workload"] = "other" }, IdentityMismatch},
		{"cross device workload", func(c *Config, i *Inputs) { i.Protected["plex"].Errors.Labels["UUID"] = "GPU-two" }, IdentityMismatch},
		{"host aggregate", func(c *Config, i *Inputs) { delete(i.Device[gputelemetry.MemoryUsed].Labels, "UUID") }, IdentityMismatch},
		{"per container counter", func(c *Config, i *Inputs) { i.Device[gputelemetry.MemoryUsed].Labels["workload"] = "plex" }, IdentityMismatch},
		{"missing device counter", func(c *Config, i *Inputs) { delete(i.Device, gputelemetry.SlicePending) }, InvalidSample},
		{"missing reservations", func(c *Config, i *Inputs) { i.Reservations = nil }, InvalidSample},
		{"stale reservations", func(c *Config, i *Inputs) { i.Reservations.At = clock.Add(-time.Hour) }, StaleSample},
		{"stale protected", func(c *Config, i *Inputs) {
			p := i.Protected["plex"]
			p.Errors.At = clock.Add(-time.Hour)
			i.Protected["plex"] = p
		}, StaleSample},
		{"fractional counter", func(c *Config, i *Inputs) {
			s := i.Device[gputelemetry.SliceUsed]
			s.ValueText = "1.0000000000000001"
			s.Value = 1
			i.Device[gputelemetry.SliceUsed] = s
		}, InvalidSample},
		{"overflow sum", func(c *Config, i *Inputs) {
			for w, p := range i.Protected {
				p.Errors.ValueText = "18446744073709551615"
				i.Protected[w] = p
			}
		}, InvalidSample},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, in := fixture()
			tc.change(&c, &in)
			o, err := Assemble(c, in, clock)
			expect(t, err, tc.want)
			if o.Memory != nil {
				t.Fatal("partial observation escaped")
			}
		})
	}
}
func TestExactValuesAndHealth(t *testing.T) {
	for _, tc := range []struct {
		text  string
		value uint64
	}{{"9007199254740993", 9007199254740993}, {"18446744073709551615", math.MaxUint64}, {"1.0e3", 1000}, {"0.001e3", 1}} {
		v, err := integer(tc.text)
		if err != nil || v != tc.value {
			t.Fatalf("%s %d %v", tc.text, v, err)
		}
	}
	for _, text := range []string{"18446744073709551616", "1.0000000000000001", "NaN", "Inf", "-1", "1e1000000", "1e-1000000", "0x10", "", "1/1", "1_000"} {
		if _, err := integer(text); err == nil {
			t.Fatalf("accepted %s", text)
		}
	}
	v, err := upperFloat("0.500000000000000000001")
	if err != nil || v <= 0.5 {
		t.Fatal("latency rounded down")
	}
	c, in := fixture()
	s := in.Device[gputelemetry.DeviceErrors]
	s.ValueText = "1"
	in.Device[gputelemetry.DeviceErrors] = s
	o, err := Assemble(c, in, clock)
	if err != nil || o.Health.Value != gpuadmission.HealthFault {
		t.Fatal(o, err)
	}
	if err := gpuadmission.Evaluate(c.Policy, o.Device, o, clock); err == nil {
		t.Fatal("health ignored")
	}
}
func TestFreshnessBoundary(t *testing.T) {
	c, in := fixture()
	in.Reservations.At = clock.Add(-time.Minute)
	if _, err := Assemble(c, in, clock); err != nil {
		t.Fatal(err)
	}
	in.Reservations.At = in.Reservations.At.Add(-time.Nanosecond)
	_, err := Assemble(c, in, clock)
	expect(t, err, StaleSample)
}
func FuzzInteger(f *testing.F) {
	for _, s := range []string{"0", "9007199254740993", "1.01", "1e999999"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		v, err := integer(s)
		if err == nil {
			r, e := exact(s)
			if e != nil || r.Num().String() != strconv.FormatUint(v, 10) || !r.IsInt() {
				t.Fatal("inexact integer")
			}
		}
	})
}
