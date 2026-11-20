package api

import (
	"context"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

const (
	defaultIdleTimeout = 10 * time.Minute

	// registeredViaMetadataKey records how RegisteredAt was set.
	// runner_running means the pod was up and the in_progress webhook had not
	// arrived yet (home#4340). That registration is not proof the runner is
	// idle: it may be mid-job. Idle TTL reaps it only after GitHub reports
	// busy=false.
	registeredViaMetadataKey   = "registered_via"
	registeredViaRunnerRunning = "runner_running"

	idleExpiryReason = "idle"
)

type idleActivityMode int

const (
	// idleActivityNone means the backend cannot report GitHub busy state.
	// Explicit registrations still use RegisteredAt. runner_running
	// registrations are left alone so a lost webhook cannot delete a live job.
	idleActivityNone idleActivityMode = iota
	// idleActivityFailed means a probe exists but this cycle failed. Reap
	// nothing: a missing busy reading must not delete a runner that has a job.
	idleActivityFailed
	// idleActivityOK means busy is authoritative for IDs present in the map.
	idleActivityOK
)

type idleActivity struct {
	mode idleActivityMode
	busy map[string]bool
}

func (a idleActivity) lookup(id string) (busy bool, known bool) {
	if a.mode != idleActivityOK {
		return false, false
	}
	busy, known = a.busy[id]
	return busy, known
}

// effectiveIdleTimeout resolves the idle TTL for an allocation.
// A non-positive result disables the reaper for that allocation.
func (s *Service) effectiveIdleTimeout(status model.AllocationStatus) time.Duration {
	if s == nil {
		return defaultIdleTimeout
	}
	if pool, err := s.resolvePool(status.Pool); err == nil {
		if cfg, ok := pool.Backends[status.SelectedBackend]; ok && cfg.IdleTimeout != 0 {
			if cfg.IdleTimeout < 0 {
				return 0
			}
			return cfg.IdleTimeout
		}
	}
	if s.cfg.Broker.IdleTimeout < 0 {
		return 0
	}
	if s.cfg.Broker.IdleTimeout > 0 {
		return s.cfg.Broker.IdleTimeout
	}
	return defaultIdleTimeout
}

// loadIdleActivity asks each backend that can see GitHub whether the given
// unclaimed runners are busy. Backends without the probe are recorded as
// idleActivityNone. A probe error is recorded as idleActivityFailed.
func (s *Service) loadIdleActivity(ctx context.Context, statuses []model.AllocationStatus) map[model.BackendName]idleActivity {
	out := map[model.BackendName]idleActivity{}
	if s == nil || s.registry == nil {
		return out
	}
	byBackend := map[model.BackendName][]model.AllocationStatus{}
	for _, status := range statuses {
		if !isActiveAllocationState(status.State) || status.IsClaimed() {
			continue
		}
		if s.effectiveIdleTimeout(status) <= 0 || s.isStaticLabelBackend(status.Pool, status.SelectedBackend) {
			continue
		}
		byBackend[status.SelectedBackend] = append(byBackend[status.SelectedBackend], status)
	}
	for name, allocs := range byBackend {
		impl, ok := s.registry.Get(name)
		if !ok {
			continue
		}
		probe, ok := impl.(backend.RunnerActivityBackend)
		if !ok {
			out[name] = idleActivity{mode: idleActivityNone}
			continue
		}
		probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		busy, err := probe.RunnerActivity(probeCtx, allocs)
		cancel()
		if err != nil {
			logAllocationEvent(ctx, "idle_runner_activity_failed", map[string]string{
				"backend": string(name),
				"error":   err.Error(),
			})
			out[name] = idleActivity{mode: idleActivityFailed}
			continue
		}
		if busy == nil {
			busy = map[string]bool{}
		}
		out[name] = idleActivity{mode: idleActivityOK, busy: busy}
	}
	return out
}

// expireIdleAllocation reaps one allocation that took no job within the idle
// TTL. It returns true when the allocation was expired. A busy runner is
// claimed instead so a lost in_progress webhook cannot delete a live job
// (home#4340). Static labels and warm runners are not handled here.
func (s *Service) expireIdleAllocation(status model.AllocationStatus, now time.Time, activity idleActivity) bool {
	ttl := s.effectiveIdleTimeout(status)
	if ttl <= 0 || !isActiveAllocationState(status.State) || status.IsClaimed() {
		return false
	}
	if s.isStaticLabelBackend(status.Pool, status.SelectedBackend) {
		return false
	}
	if activity.mode == idleActivityFailed {
		return false
	}

	if busy, known := activity.lookup(status.ID); known && busy {
		s.claimBusyRunner(context.Background(), status)
		return false
	}

	if !s.idleConfirmed(status, activity) {
		return false
	}

	start := idleClock(status, now)
	if status.IdleSince == nil || status.IdleSince.IsZero() {
		saved, ok := s.stampIdleSince(status.ID, start)
		if !ok {
			return false
		}
		status = saved
	}
	if status.IdleSince == nil || status.IdleSince.IsZero() || now.Before(status.IdleSince.Add(ttl)) {
		return false
	}
	return s.expireIdle(status, now)
}

// idleConfirmed reports whether status is known to be waiting with no job.
// A runner_running registration is confirmed only by busy=false. An explicit
// registration (POST /register) is idle when the backend has no busy probe,
// or when a successful probe does not say the runner is busy.
func (s *Service) idleConfirmed(status model.AllocationStatus, activity idleActivity) bool {
	if busy, known := activity.lookup(status.ID); known {
		return !busy
	}
	if activity.mode == idleActivityOK && status.Metadata[registeredViaMetadataKey] == registeredViaRunnerRunning {
		// Listed as unknown, or not listed. The container is up but GitHub has
		// not shown busy=false, so this may still be a job with a lost webhook.
		return false
	}
	if activity.mode == idleActivityNone && status.Metadata[registeredViaMetadataKey] == registeredViaRunnerRunning {
		return false
	}
	return status.IsRegistered()
}

func idleClock(status model.AllocationStatus, now time.Time) time.Time {
	if status.IdleSince != nil && !status.IdleSince.IsZero() {
		return *status.IdleSince
	}
	if status.RegisteredAt != nil && !status.RegisteredAt.IsZero() && !status.RegisteredAt.After(now) {
		return *status.RegisteredAt
	}
	return now
}

func (s *Service) stampIdleSince(id string, start time.Time) (model.AllocationStatus, bool) {
	status, ok := s.store.Get(id)
	if !ok || !isActiveAllocationState(status.State) || status.IsClaimed() {
		return status, false
	}
	if status.IdleSince != nil && !status.IdleSince.IsZero() {
		return status, true
	}
	stamp := start
	status.IdleSince = &stamp
	saved, err := s.store.SaveIfState(status, status.State)
	if err != nil || !saved {
		if err != nil {
			logAllocationEvent(context.Background(), "idle_since_save_failed", map[string]string{
				"allocation_id": allocationIDLabel(id),
				"error":         err.Error(),
			})
		}
		return status, false
	}
	return status, true
}

func (s *Service) claimBusyRunner(ctx context.Context, status model.AllocationStatus) {
	current, ok := s.store.Get(status.ID)
	if !ok || current.IsClaimed() || !isActiveAllocationState(current.State) {
		return
	}
	claimed, _, err := s.Claim(ctx, status.ID)
	if err != nil {
		logAllocationEvent(ctx, "idle_runner_claim_failed", map[string]string{
			"allocation_id": allocationIDLabel(status.ID),
			"pool":          string(status.Pool),
			"backend":       string(status.SelectedBackend),
			"error":         err.Error(),
		})
		return
	}
	logAllocationEvent(ctx, "idle_runner_busy", map[string]string{
		"allocation_id": allocationIDLabel(claimed.ID),
		"pool":          string(claimed.Pool),
		"backend":       string(claimed.SelectedBackend),
	})
}

func (s *Service) expireIdle(status model.AllocationStatus, now time.Time) bool {
	current, ok := s.store.Get(status.ID)
	if !ok || !isActiveAllocationState(current.State) || current.IsClaimed() {
		return false
	}
	const message = "allocation expired: runner took no job within the idle timeout"
	updated, ok := s.markState(current.ID, model.StateExpired, now, message)
	if !ok {
		return false
	}
	// Release the scheduler slot and tear the runner down (pod delete and
	// GitHub deregistration for Kubernetes). Do not record a backend failure:
	// the runner was healthy and the job was missing, stolen, or cancelled.
	if pool, err := s.resolvePool(current.Pool); err == nil {
		s.releaseStaleLabel(context.Background(), pool, current.SelectedBackend, updated)
	} else {
		s.reclaimStaleRunnerLabel(context.Background(), updated)
	}
	s.observer.ObserveAllocationExpired(current.Pool, current.SelectedBackend, idleExpiryReason)
	logAllocationEvent(context.Background(), "allocation_expired", map[string]string{
		"allocation_id": allocationIDLabel(current.ID),
		"pool":          string(current.Pool),
		"backend":       string(current.SelectedBackend),
		"runner_label":  current.RunnerLabel,
		"reason":        idleExpiryReason,
	})
	return true
}

// idleRunnerSnapshots counts allocated runners that are registered, unclaimed,
// and confirmed idle. They still hold maxRunners and crossPoolMaxRunners.
func idleRunnerSnapshots(statuses []model.AllocationStatus, now time.Time) []idleRunnerSnapshot {
	type key struct {
		pool    model.PoolName
		backend model.BackendName
	}
	type agg struct {
		count  int
		oldest float64
	}
	grouped := map[key]*agg{}
	for _, status := range statuses {
		if !isActiveAllocationState(status.State) || status.IsClaimed() {
			continue
		}
		if status.IdleSince == nil || status.IdleSince.IsZero() {
			continue
		}
		k := key{pool: status.Pool, backend: status.SelectedBackend}
		item := grouped[k]
		if item == nil {
			item = &agg{}
			grouped[k] = item
		}
		item.count++
		age := now.Sub(*status.IdleSince).Seconds()
		if age < 0 {
			age = 0
		}
		if age > item.oldest {
			item.oldest = age
		}
	}
	out := make([]idleRunnerSnapshot, 0, len(grouped))
	for k, item := range grouped {
		out = append(out, idleRunnerSnapshot{
			Pool:          k.pool,
			Backend:       k.backend,
			Count:         item.count,
			OldestSeconds: item.oldest,
		})
	}
	return out
}
