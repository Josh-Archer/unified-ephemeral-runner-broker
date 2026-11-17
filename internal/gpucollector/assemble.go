// Package gpucollector assembles server-collected samples into GPU observations.
// It provides no transport, authentication, scheduling, or reservation authority.
package gpucollector

import (
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/gpuadmission"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/gputelemetry"
)

type Unit string

const (
	Bytes   Unit = "bytes"
	Count   Unit = "count"
	Seconds Unit = "seconds"
)

type HealthMeaning string

const ZeroErrorsHealthy HealthMeaning = "zero-device-errors-means-healthy"

// Config must be authored by the server operator, not allocation/PR metadata.
// Units describe actual query results; this version deliberately performs no
// implicit scaling. ProtectedWorkloads names the independently resolved complete
// set that must be measured. This configuration does not prove that resolution.
type Config struct {
	Selection          gputelemetry.Selection
	Policy             gpuadmission.Policy
	Units              map[gputelemetry.Metric]Unit
	HealthMeaning      HealthMeaning
	WorkloadLabel      string
	ProtectedWorkloads []string
}
type ProtectedSample struct{ Latency, Errors gputelemetry.Sample }
type Inputs struct {
	Device    map[gputelemetry.Metric]gputelemetry.Sample
	Protected map[string]ProtectedSample
	// Reservations must originate from the authoritative durable global ledger.
	// A decoder, caller boolean, or Prometheus gauge cannot supply this authority.
	Reservations *gpuadmission.Sample[gpuadmission.Reservations]
}
type Reason string

const (
	InvalidConfig    Reason = "invalid-collector-contract"
	InvalidSample    Reason = "invalid-collector-sample"
	MissingCoverage  Reason = "incomplete-protected-coverage"
	IdentityMismatch Reason = "collector-identity-mismatch"
	StaleSample      Reason = "stale-collector-sample"
)

type Rejection struct{ Reason Reason }

func (r *Rejection) Error() string { return string(r.Reason) }
func reject(r Reason) error        { return &Rejection{r} }

var deviceMetrics = []gputelemetry.Metric{gputelemetry.MemoryTotal, gputelemetry.MemoryUsed, gputelemetry.SliceCapacity, gputelemetry.SliceUsed, gputelemetry.SlicePending, gputelemetry.DeviceErrors}
var expectedUnits = map[gputelemetry.Metric]Unit{
	gputelemetry.MemoryTotal: Bytes, gputelemetry.MemoryUsed: Bytes,
	gputelemetry.SliceCapacity: Count, gputelemetry.SliceUsed: Count, gputelemetry.SlicePending: Count,
	gputelemetry.DeviceErrors: Count, gputelemetry.ProtectedLatency: Seconds, gputelemetry.ProtectedErrors: Count,
}

// Assemble validates input identity, completeness, exact numeric evidence and
// independent freshness. It does NOT evaluate pressure thresholds, authorize a
// job or reserve capacity. Call gpuadmission.Evaluate with the same server policy
// and enforce atomic fenced reservation separately. Every error returns no data.
func Assemble(c Config, in Inputs, now time.Time) (gpuadmission.Observation, error) {
	fail := func(r Reason) (gpuadmission.Observation, error) { return gpuadmission.Observation{}, reject(r) }
	p, s := c.Policy, c.Selection
	if p.MaxAge == nil || *p.MaxAge <= 0 || p.RequiredFreeBytes == nil || p.MaxProtectedErrors == nil || p.MaxProtectedLatency == nil || !finite(*p.MaxProtectedLatency) || s.Node == "" || s.UUID == "" || s.NodeLabel == "" || s.UUIDLabel == "" || s.NodeLabel == s.UUIDLabel || c.WorkloadLabel == "" || c.WorkloadLabel == s.NodeLabel || c.WorkloadLabel == s.UUIDLabel || c.HealthMeaning != ZeroErrorsHealthy || len(c.ProtectedWorkloads) == 0 || len(c.Units) != len(expectedUnits) || now.IsZero() {
		return fail(InvalidConfig)
	}
	for metric, unit := range expectedUnits {
		if c.Units[metric] != unit {
			return fail(InvalidConfig)
		}
	}
	workloads := map[string]bool{}
	for _, w := range c.ProtectedWorkloads {
		if w == "" || workloads[w] {
			return fail(InvalidConfig)
		}
		workloads[w] = true
	}
	if len(in.Device) != len(deviceMetrics) || in.Reservations == nil {
		return fail(InvalidSample)
	}
	fresh := func(at time.Time) bool { return !at.IsZero() && !at.After(now) && !at.Before(now.Add(-*p.MaxAge)) }
	validate := func(sample gputelemetry.Sample) error {
		if sample.Labels[s.NodeLabel] != s.Node || sample.Labels[s.UUIDLabel] != s.UUID {
			return reject(IdentityMismatch)
		}
		if !fresh(sample.At) {
			return reject(StaleSample)
		}
		return nil
	}
	values := map[gputelemetry.Metric]uint64{}
	for _, metric := range deviceMetrics {
		sample, ok := in.Device[metric]
		if !ok {
			return fail(InvalidSample)
		}
		if err := validate(sample); err != nil {
			return gpuadmission.Observation{}, err
		}
		// A per-workload device counter is not a whole-device observation.
		if _, ok := sample.Labels[c.WorkloadLabel]; ok {
			return fail(IdentityMismatch)
		}
		value, err := integer(sample.ValueText)
		if err != nil {
			return fail(InvalidSample)
		}
		values[metric] = value
	}
	if len(in.Protected) != len(workloads) {
		return fail(MissingCoverage)
	}
	var latency float64
	var errors uint64
	latencyAt, errorsAt := now, now
	for workload := range workloads {
		pair, ok := in.Protected[workload]
		if !ok {
			return fail(MissingCoverage)
		}
		for _, sample := range []gputelemetry.Sample{pair.Latency, pair.Errors} {
			if err := validate(sample); err != nil {
				return gpuadmission.Observation{}, err
			}
			if sample.Labels[c.WorkloadLabel] != workload {
				return fail(IdentityMismatch)
			}
		}
		v, err := upperFloat(pair.Latency.ValueText)
		if err != nil {
			return fail(InvalidSample)
		}
		n, err := integer(pair.Errors.ValueText)
		if err != nil || n > math.MaxUint64-errors {
			return fail(InvalidSample)
		}
		latency = math.Max(latency, v)
		errors += n
		latencyAt = oldest(latencyAt, pair.Latency.At)
		errorsAt = oldest(errorsAt, pair.Errors.At)
	}
	if !fresh(in.Reservations.At) {
		return fail(StaleSample)
	}
	memory := gpuadmission.Memory{Total: values[gputelemetry.MemoryTotal], Used: values[gputelemetry.MemoryUsed]}
	slices := gpuadmission.Slices{Capacity: values[gputelemetry.SliceCapacity], Used: values[gputelemetry.SliceUsed], Pending: values[gputelemetry.SlicePending]}
	if memory.Total == 0 || memory.Used > memory.Total || slices.Capacity == 0 || slices.Used > slices.Capacity || slices.Pending > slices.Capacity-slices.Used {
		return fail(InvalidSample)
	}
	health := gpuadmission.HealthOK
	if values[gputelemetry.DeviceErrors] != 0 {
		health = gpuadmission.HealthFault
	}
	reservations := *in.Reservations // Copy; no mutable caller-owned observation pointers escape.
	return gpuadmission.Observation{
		Device:           gpuadmission.Identity{Node: s.Node, UUID: s.UUID},
		Memory:           &gpuadmission.Sample[gpuadmission.Memory]{Value: memory, At: oldest(in.Device[gputelemetry.MemoryTotal].At, in.Device[gputelemetry.MemoryUsed].At)},
		Slices:           &gpuadmission.Sample[gpuadmission.Slices]{Value: slices, At: oldest(in.Device[gputelemetry.SliceCapacity].At, oldest(in.Device[gputelemetry.SliceUsed].At, in.Device[gputelemetry.SlicePending].At))},
		Health:           &gpuadmission.Sample[gpuadmission.Health]{Value: health, At: in.Device[gputelemetry.DeviceErrors].At},
		ProtectedLatency: &gpuadmission.Sample[float64]{Value: latency, At: latencyAt},
		ProtectedErrors:  &gpuadmission.Sample[uint64]{Value: errors, At: errorsAt},
		Reservations:     &reservations,
	}, nil
}
func oldest(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 }

var decimal = regexp.MustCompile(`^(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$`)

func exact(text string) (*big.Rat, error) {
	if len(text) == 0 || len(text) > 128 || !decimal.MatchString(text) {
		return nil, reject(InvalidSample)
	}
	if i := strings.IndexAny(text, "eE"); i >= 0 {
		e, err := strconv.Atoi(text[i+1:])
		if err != nil || e < -128 || e > 128 {
			return nil, reject(InvalidSample)
		}
	}
	r, ok := new(big.Rat).SetString(text)
	if !ok || r.Sign() < 0 {
		return nil, reject(InvalidSample)
	}
	return r, nil
}
func integer(text string) (uint64, error) {
	r, err := exact(text)
	if err != nil || !r.IsInt() || !r.Num().IsUint64() {
		return 0, reject(InvalidSample)
	}
	return r.Num().Uint64(), nil
}
func upperFloat(text string) (float64, error) {
	r, err := exact(text)
	if err != nil {
		return 0, err
	}
	v, _ := r.Float64()
	if !finite(v) {
		return 0, reject(InvalidSample)
	}
	// Round upward if needed: exact latency above a float policy limit must not
	// become equal to it through nearest-even conversion and accidentally pass.
	if new(big.Rat).SetFloat64(v).Cmp(r) < 0 {
		v = math.Nextafter(v, math.Inf(1))
	}
	if !finite(v) {
		return 0, reject(InvalidSample)
	}
	return v, nil
}
