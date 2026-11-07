package api

import (
	"sync"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

// Re-allocation of a job GitHub still reports as queued after its previous
// allocation reached a terminal state (failed, expired, unregistered,
// unclaimed, canceled, completed). Bounded so a job that can never run does
// not churn runners forever.
const (
	maxWorkflowJobAllocations = 5
	workflowJobRetryBase      = 30 * time.Second
	workflowJobRetryMax       = 10 * time.Minute

	// A backend whose runners repeatedly never register (GCE "unregistered",
	// azure-vm "unclaimed") is skipped for a cooldown while other backends in
	// the pool remain, so queued jobs fall to backends that actually work.
	registrationExpiryThreshold = 2
	registrationExpiryCooldown  = 30 * time.Minute
)

type workflowJobRetries struct {
	mu   sync.Mutex
	next map[int64]time.Time
}

func workflowJobRetryBackoff(prior int) time.Duration {
	d := workflowJobRetryBase
	for i := 1; i < prior; i++ {
		d *= 2
		if d >= workflowJobRetryMax {
			return workflowJobRetryMax
		}
	}
	return d
}

// countWorkflowJobAllocations counts every allocation recorded for jobID.
func (s *Service) countWorkflowJobAllocations(jobID int64) int {
	n := 0
	for _, status := range s.store.List() {
		if status.Request != nil && status.Request.WorkflowJobID == jobID {
			n++
		}
	}
	return n
}

// mayReallocateWorkflowJob reports whether a terminal allocation for a job
// still queued on GitHub may be replaced now. Callers hold workflowJobMu.
func (s *Service) mayReallocateWorkflowJob(jobID int64, now time.Time) bool {
	prior := s.countWorkflowJobAllocations(jobID)
	if prior >= maxWorkflowJobAllocations {
		return false
	}
	s.jobRetries.mu.Lock()
	defer s.jobRetries.mu.Unlock()
	if next, ok := s.jobRetries.next[jobID]; ok && now.Before(next) {
		return false
	}
	return true
}

func (s *Service) noteWorkflowJobReallocated(jobID int64, now time.Time) {
	prior := s.countWorkflowJobAllocations(jobID)
	s.jobRetries.mu.Lock()
	defer s.jobRetries.mu.Unlock()
	if s.jobRetries.next == nil {
		s.jobRetries.next = map[int64]time.Time{}
	}
	s.jobRetries.next[jobID] = now.Add(workflowJobRetryBackoff(prior))
	// Drop stale entries so the map stays bounded.
	for id, next := range s.jobRetries.next {
		if now.Sub(next) > time.Hour {
			delete(s.jobRetries.next, id)
		}
	}
}

type registrationPenalty struct {
	consecutive int
	skipUntil   time.Time
}

type registrationPenalties struct {
	mu sync.Mutex
	m  map[backendAdmissionKey]*registrationPenalty
}

// recordRegistrationExpiry counts an allocation that expired because its
// runner never registered or claimed the job.
func (s *Service) recordRegistrationExpiry(pool model.PoolName, name model.BackendName, now time.Time) {
	p := &s.regPenalties
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.m == nil {
		p.m = map[backendAdmissionKey]*registrationPenalty{}
	}
	key := backendAdmissionKey{pool: pool, backend: name}
	entry := p.m[key]
	if entry == nil {
		entry = &registrationPenalty{}
		p.m[key] = entry
	}
	entry.consecutive++
	if entry.consecutive >= registrationExpiryThreshold {
		entry.skipUntil = now.Add(registrationExpiryCooldown)
	}
}

// recordRegistrationSuccess clears the penalty once a runner from the backend
// registers or claims work.
func (s *Service) recordRegistrationSuccess(pool model.PoolName, name model.BackendName) {
	p := &s.regPenalties
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.m, backendAdmissionKey{pool: pool, backend: name})
}

func (s *Service) registrationPenalized(pool model.PoolName, name model.BackendName, now time.Time) bool {
	p := &s.regPenalties
	p.mu.Lock()
	defer p.mu.Unlock()
	entry := p.m[backendAdmissionKey{pool: pool, backend: name}]
	return entry != nil && now.Before(entry.skipUntil)
}

// filterRegistrationPenalized drops penalized backends from an unpinned
// request as long as at least one other backend remains.
func (s *Service) filterRegistrationPenalized(pool model.PoolConfig, request model.AllocationRequest) model.PoolConfig {
	if request.Backend != nil || len(pool.Backends) < 2 {
		return pool
	}
	now := s.now()
	kept := make(map[model.BackendName]model.BackendConfig, len(pool.Backends))
	usableKept := 0
	for name, cfg := range pool.Backends {
		if !s.registrationPenalized(pool.Name, name, now) {
			kept[name] = cfg
			// Disabled, unhealthy or zero-capacity entries cannot take the job, so
			// they must not count as the alternative that justifies skipping the
			// penalized backend. Otherwise a pool with one real backend plus
			// disabled cloud entries strands every job until the cooldown ends.
			if cfg.Enabled && cfg.Healthy && cfg.MaxRunners > 0 {
				usableKept++
			}
		}
	}
	if usableKept == 0 || len(kept) == len(pool.Backends) {
		return pool
	}
	filtered := pool
	filtered.Backends = kept
	return filtered
}
