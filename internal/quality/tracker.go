// Package quality tracks rolling backend outcome stats and scores candidates
// for quality-aware auto backend selection.
package quality

import (
	"math"
	"sort"
	"sync"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

const (
	defaultWindow     = 15 * time.Minute
	defaultMinSamples = 3
	defaultMaxEvents  = 512

	DefaultWeightFreeSlots      = 1.0
	DefaultWeightSuccessRate    = 1.0
	DefaultWeightLatency        = 1.0
	DefaultWeightCapacityErrors = 1.0
)

// Event is one observed allocation outcome for a backend.
type Event struct {
	At            time.Time
	Success       bool
	ReadyLatency  time.Duration
	CapacityError bool
}

// Snapshot is a rolling window summary used for scoring.
type Snapshot struct {
	Pool            model.PoolName
	Backend         model.BackendName
	Samples         int
	Successes       int
	Failures        int
	CapacityErrors  int
	SuccessRate     float64
	P95Ready        time.Duration
	InsufficientData bool
}

// Weights control the composite score. Zero weight disables a component.
type Weights struct {
	FreeSlots      float64
	SuccessRate    float64
	Latency        float64
	CapacityErrors float64
}

// Normalize fills defaults for zero weights so an empty config still scores.
func (w Weights) Normalize() Weights {
	out := w
	if out.FreeSlots == 0 && out.SuccessRate == 0 && out.Latency == 0 && out.CapacityErrors == 0 {
		out.FreeSlots = DefaultWeightFreeSlots
		out.SuccessRate = DefaultWeightSuccessRate
		out.Latency = DefaultWeightLatency
		out.CapacityErrors = DefaultWeightCapacityErrors
	}
	return out
}

// Candidate is a schedulable backend with free slot information for scoring.
type Candidate struct {
	Backend   model.BackendName
	FreeSlots int
}

// ScoreResult is the scored ranking of one candidate.
type ScoreResult struct {
	Backend        model.BackendName
	Score          float64
	FreeSlots      int
	SuccessRate    float64
	P95Ready       time.Duration
	CapacityErrors int
	Samples        int
	Reason         string
}

// Tracker stores process-local rolling outcome history per pool/backend.
type Tracker struct {
	mu         sync.Mutex
	window     time.Duration
	minSamples int
	maxEvents  int
	events     map[model.PoolName]map[model.BackendName][]Event
}

// NewTracker builds a tracker with the given rolling window and sample floor.
func NewTracker(window time.Duration, minSamples int) *Tracker {
	if window <= 0 {
		window = defaultWindow
	}
	if minSamples <= 0 {
		minSamples = defaultMinSamples
	}
	return &Tracker{
		window:     window,
		minSamples: minSamples,
		maxEvents:  defaultMaxEvents,
		events:     map[model.PoolName]map[model.BackendName][]Event{},
	}
}

// RecordSuccess records a successful provision/ready outcome.
func (t *Tracker) RecordSuccess(pool model.PoolName, backend model.BackendName, readyLatency time.Duration, now time.Time) {
	if t == nil {
		return
	}
	if readyLatency < 0 {
		readyLatency = 0
	}
	t.record(pool, backend, Event{
		At:           now,
		Success:      true,
		ReadyLatency: readyLatency,
	}, now)
}

// RecordFailure records a failed provision or completion outcome.
func (t *Tracker) RecordFailure(pool model.PoolName, backend model.BackendName, capacityError bool, now time.Time) {
	if t == nil {
		return
	}
	t.record(pool, backend, Event{
		At:            now,
		Success:       false,
		CapacityError: capacityError,
	}, now)
}

func (t *Tracker) record(pool model.PoolName, backend model.BackendName, event Event, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.events[pool] == nil {
		t.events[pool] = map[model.BackendName][]Event{}
	}
	events := append(t.events[pool][backend], event)
	events = pruneEvents(events, now, t.window, t.maxEvents)
	t.events[pool][backend] = events
}

// Snapshot returns the rolling stats for one backend.
func (t *Tracker) Snapshot(pool model.PoolName, backend model.BackendName, now time.Time) Snapshot {
	if t == nil {
		return Snapshot{Pool: pool, Backend: backend, SuccessRate: 1, InsufficientData: true}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	events := pruneEvents(t.events[pool][backend], now, t.window, t.maxEvents)
	if t.events[pool] != nil {
		t.events[pool][backend] = events
	}
	return summarize(pool, backend, events, t.minSamples)
}

// Snapshots returns rolling stats for every backend with history in a pool.
func (t *Tracker) Snapshots(pool model.PoolName, now time.Time) []Snapshot {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	backends := t.events[pool]
	if len(backends) == 0 {
		return nil
	}
	out := make([]Snapshot, 0, len(backends))
	for backend, events := range backends {
		events = pruneEvents(events, now, t.window, t.maxEvents)
		backends[backend] = events
		out = append(out, summarize(pool, backend, events, t.minSamples))
	}
	return out
}

func summarize(pool model.PoolName, backend model.BackendName, events []Event, minSamples int) Snapshot {
	snap := Snapshot{
		Pool:    pool,
		Backend: backend,
		Samples: len(events),
	}
	if len(events) == 0 {
		snap.SuccessRate = 1
		snap.InsufficientData = true
		return snap
	}

	latencies := make([]time.Duration, 0, len(events))
	for _, event := range events {
		if event.Success {
			snap.Successes++
			latencies = append(latencies, event.ReadyLatency)
		} else {
			snap.Failures++
			if event.CapacityError {
				snap.CapacityErrors++
			}
		}
	}
	snap.SuccessRate = float64(snap.Successes) / float64(len(events))
	snap.P95Ready = percentileDuration(latencies, 0.95)
	snap.InsufficientData = len(events) < minSamples
	return snap
}

func pruneEvents(events []Event, now time.Time, window time.Duration, maxEvents int) []Event {
	if len(events) == 0 {
		return events
	}
	cutoff := now.Add(-window)
	kept := events[:0]
	for _, event := range events {
		if event.At.Before(cutoff) {
			continue
		}
		kept = append(kept, event)
	}
	if maxEvents > 0 && len(kept) > maxEvents {
		kept = kept[len(kept)-maxEvents:]
	}
	// Copy to avoid retaining large underlying arrays.
	out := make([]Event, len(kept))
	copy(out, kept)
	return out
}

func percentileDuration(values []time.Duration, p float64) time.Duration {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	if p <= 0 {
		return sorted[0]
	}
	if p >= 1 {
		return sorted[len(sorted)-1]
	}
	// Nearest-rank method.
	rank := int(math.Ceil(p*float64(len(sorted)))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}
	return sorted[rank]
}

// Score ranks candidates by free slots, success rate, p95 ready latency, and
// recent capacity errors. Higher score is better. When samples are below the
// configured floor, success/latency/error components are treated as neutral so
// free slots still drive preference without thrashing on sparse data.
func Score(candidates []Candidate, snapshots map[model.BackendName]Snapshot, weights Weights) []ScoreResult {
	if len(candidates) == 0 {
		return nil
	}
	weights = weights.Normalize()

	maxFree := 0
	for _, candidate := range candidates {
		if candidate.FreeSlots > maxFree {
			maxFree = candidate.FreeSlots
		}
	}
	if maxFree < 1 {
		maxFree = 1
	}

	results := make([]ScoreResult, 0, len(candidates))
	for _, candidate := range candidates {
		snap := snapshots[candidate.Backend]
		if snap.Backend == "" {
			snap.Backend = candidate.Backend
			snap.SuccessRate = 1
			snap.InsufficientData = true
		}

		freeComponent := float64(candidate.FreeSlots) / float64(maxFree)
		successComponent := 1.0
		latencyComponent := 1.0
		capacityComponent := 1.0
		reason := "highest-score"

		if snap.InsufficientData {
			reason = "insufficient-samples"
		} else {
			successComponent = snap.SuccessRate
			// Map p95 ready latency into (0,1]: 0s -> 1, grows toward 0 as latency rises.
			latencyComponent = 1.0 / (1.0 + snap.P95Ready.Seconds())
			capacityComponent = 1.0 / (1.0 + float64(snap.CapacityErrors))
		}

		score := weights.FreeSlots*freeComponent +
			weights.SuccessRate*successComponent +
			weights.Latency*latencyComponent +
			weights.CapacityErrors*capacityComponent

		results = append(results, ScoreResult{
			Backend:        candidate.Backend,
			Score:          score,
			FreeSlots:      candidate.FreeSlots,
			SuccessRate:    snap.SuccessRate,
			P95Ready:       snap.P95Ready,
			CapacityErrors: snap.CapacityErrors,
			Samples:        snap.Samples,
			Reason:         reason,
		})
	}

	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Score == results[j].Score {
			// Deterministic tie-break: more free slots, then name.
			if results[i].FreeSlots == results[j].FreeSlots {
				return results[i].Backend < results[j].Backend
			}
			return results[i].FreeSlots > results[j].FreeSlots
		}
		return results[i].Score > results[j].Score
	})

	if len(results) == 1 {
		results[0].Reason = "single-candidate"
	}
	return results
}

// SelectBest returns the highest-scoring candidate, or false when none exist.
func SelectBest(candidates []Candidate, snapshots map[model.BackendName]Snapshot, weights Weights) (ScoreResult, bool) {
	ranked := Score(candidates, snapshots, weights)
	if len(ranked) == 0 {
		return ScoreResult{}, false
	}
	return ranked[0], true
}
