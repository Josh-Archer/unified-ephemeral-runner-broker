package scheduler

import (
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

var ErrQuotaExceeded = errors.New("tenant quota exceeded")

const (
	defaultNormalPriorityClass  = 1
	defaultHighPriorityClass    = 2
	priorityShareScale          = 1000
	priorityShareClassBoost     = 500
	priorityShareClassMinWeight = 1
)

// nowFunc is the time source for the scheduler. Tests may replace it.
var nowFunc = time.Now

type PriorityFairShare struct {
	state *priorityFairShareState
}

func NewPriorityFairShare() *PriorityFairShare {
	return &PriorityFairShare{
		state: newPriorityFairShareState(),
	}
}

func (p *PriorityFairShare) Reserve(pool model.PoolConfig, request model.AllocationRequest) (model.BackendName, error) {
	return p.state.Reserve(pool, request)
}

func (p *PriorityFairShare) Release(pool model.PoolName, backend model.BackendName, allocation model.AllocationStatus) {
	p.state.Release(pool, backend, allocation)
}

func (p *PriorityFairShare) Active(pool model.PoolName, backend model.BackendName) int {
	return p.state.Active(pool, backend)
}

// tenantReservation records when a tenant reservation was made so windowed
// fair-share scoring can age out old entries.
type tenantReservation struct {
	tenant    string
	backend   model.BackendName
	reserveAt time.Time
}

type priorityFairShareState struct {
	mu           sync.Mutex
	cursors      map[model.PoolName]int
	active       map[model.PoolName]map[model.BackendName]int
	tenantActive map[model.PoolName]map[model.BackendName]map[string]int

	// tenantLog records per-pool reservation timestamps for UsageWindow scoring.
	// Only populated when UsageWindow > 0 on the pool's FairShareConfig.
	tenantLog map[model.PoolName][]tenantReservation

	// starvationStart tracks when a (pool, priorityClass) first failed to
	// reserve due to capacity. Cleared on successful reservation.
	starvationStart map[starvationKey]time.Time
}

type starvationKey struct {
	pool          model.PoolName
	priorityClass string
}

func newPriorityFairShareState() *priorityFairShareState {
	return &priorityFairShareState{
		cursors:         map[model.PoolName]int{},
		active:          map[model.PoolName]map[model.BackendName]int{},
		tenantActive:    map[model.PoolName]map[model.BackendName]map[string]int{},
		tenantLog:       map[model.PoolName][]tenantReservation{},
		starvationStart: map[starvationKey]time.Time{},
	}
}

func (s *priorityFairShareState) Reserve(pool model.PoolConfig, request model.AllocationRequest) (model.BackendName, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.active[pool.Name]; !ok {
		s.active[pool.Name] = map[model.BackendName]int{}
	}
	if _, ok := s.tenantActive[pool.Name]; !ok {
		s.tenantActive[pool.Name] = map[model.BackendName]map[string]int{}
	}

	tenant := normalizeTenant(request.Tenant)
	if quota, ok := pool.FairShare.Quotas[tenant]; ok {
		tenantTotal := 0
		for _, counts := range s.tenantActive[pool.Name] {
			tenantTotal += counts[tenant]
		}
		if tenantTotal >= quota {
			return "", ErrQuotaExceeded
		}
	}

	now := nowFunc()
	starving := s.isStarving(pool, request.PriorityClass, now)
	sk := starvationKey{pool: pool.Name, priorityClass: normalizePriorityClassString(request.PriorityClass)}

	pinned := request.Backend
	if pinned != nil {
		cfg, ok := pool.Backends[*pinned]
		if !ok {
			return "", ErrUnknownBackend
		}
		if !backendAvailableForPriorityWithStarvation(cfg, s.active[pool.Name][*pinned], pool, request.PriorityClass, starving) {
			s.recordStarvation(sk, now)
			return "", ErrNoCapacity
		}
		s.active[pool.Name][*pinned]++
		tenantMap := s.tenantActive[pool.Name][*pinned]
		if tenantMap == nil {
			tenantMap = map[string]int{}
			s.tenantActive[pool.Name][*pinned] = tenantMap
		}
		tenantMap[normalizeTenant(request.Tenant)]++
		s.recordReservation(pool, *pinned, tenant, now)
		delete(s.starvationStart, sk)
		return *pinned, nil
	}

	// Compose with the pool backend scheduler: fair-share ranks by tenant/priority
	// load, then WRR/RR order breaks ties among equal-score backends with capacity.
	// When scheduler is weighted-round-robin, candidate order is weight-expanded so
	// backend weights still influence selection when fair-share scores match.
	backends := fairShareCandidateBackends(pool)
	if len(backends) == 0 {
		return "", ErrNoCapacity
	}

	var (
		bestIndex  = -1
		bestScore  = 0
		bestOffset = 0
	)

	priorityWeight := normalizePriorityClassWeight(pool, request.PriorityClass)
	start := s.cursors[pool.Name] % len(backends)

	for offset := 0; offset < len(backends); offset++ {
		candidate := backends[(start+offset)%len(backends)]
		cfg := pool.Backends[candidate]
		active := s.active[pool.Name][candidate]
		if !backendAvailableForPriorityWithStarvation(cfg, active, pool, request.PriorityClass, starving) {
			continue
		}

		tenantUsage := s.tenantUsage(pool, candidate, tenant, now)
		score := priorityFairShareScore(active, tenantUsage, priorityWeight)
		if bestIndex == -1 || score < bestScore || (score == bestScore && offset < bestOffset) {
			bestIndex = (start + offset) % len(backends)
			bestScore = score
			bestOffset = offset
		}
	}

	if bestIndex < 0 {
		s.recordStarvation(sk, now)
		return "", ErrNoCapacity
	}

	selected := backends[bestIndex]
	s.active[pool.Name][selected]++
	tenantMap := s.tenantActive[pool.Name][selected]
	if tenantMap == nil {
		tenantMap = map[string]int{}
		s.tenantActive[pool.Name][selected] = tenantMap
	}
	s.tenantActive[pool.Name][selected][tenant]++
	s.cursors[pool.Name] = (bestIndex + 1) % len(backends)
	s.recordReservation(pool, selected, tenant, now)
	delete(s.starvationStart, sk)
	return selected, nil
}

// tenantUsage returns the tenant's active count on a backend for fair-share
// scoring. When UsageWindow is configured, only reservations within the window
// are counted instead of the all-time active count.
func (s *priorityFairShareState) tenantUsage(pool model.PoolConfig, backend model.BackendName, tenant string, now time.Time) int {
	if pool.FairShare.UsageWindow <= 0 {
		return s.tenantActive[pool.Name][backend][tenant]
	}
	cutoff := now.Add(-pool.FairShare.UsageWindow)
	count := 0
	for _, entry := range s.tenantLog[pool.Name] {
		if entry.backend == backend && entry.tenant == tenant && !entry.reserveAt.Before(cutoff) {
			count++
		}
	}
	return count
}

// recordReservation appends a timestamped entry to the tenant log when
// UsageWindow is configured and prunes entries older than the window.
func (s *priorityFairShareState) recordReservation(pool model.PoolConfig, backend model.BackendName, tenant string, now time.Time) {
	if pool.FairShare.UsageWindow <= 0 {
		return
	}
	s.tenantLog[pool.Name] = append(s.tenantLog[pool.Name], tenantReservation{
		tenant:    tenant,
		backend:   backend,
		reserveAt: now,
	})
	s.pruneLog(pool.Name, now.Add(-pool.FairShare.UsageWindow))
}

// pruneLog removes entries older than cutoff from the tenant log.
func (s *priorityFairShareState) pruneLog(pool model.PoolName, cutoff time.Time) {
	log := s.tenantLog[pool]
	kept := 0
	for _, entry := range log {
		if !entry.reserveAt.Before(cutoff) {
			log[kept] = entry
			kept++
		}
	}
	s.tenantLog[pool] = log[:kept]
}

// isStarving reports whether the given priority class has been continuously
// denied for longer than StarvationAfter. Caller must hold s.mu.
func (s *priorityFairShareState) isStarving(pool model.PoolConfig, priorityClass string, now time.Time) bool {
	if pool.FairShare.StarvationAfter <= 0 {
		return false
	}
	sk := starvationKey{pool: pool.Name, priorityClass: normalizePriorityClassString(priorityClass)}
	start, ok := s.starvationStart[sk]
	if !ok {
		return false
	}
	return now.Sub(start) >= pool.FairShare.StarvationAfter
}

// recordStarvation marks the start of a starvation period for a priority class
// if one hasn't started already. Caller must hold s.mu.
func (s *priorityFairShareState) recordStarvation(sk starvationKey, now time.Time) {
	if _, ok := s.starvationStart[sk]; !ok {
		s.starvationStart[sk] = now
	}
}

// fairShareCandidateBackends returns the ordered candidate ring used when
// fair-share is selecting a backend. weighted-round-robin expands each backend
// by its weight so weights apply as a tie-breaker among equal fair-share scores.
// Other schedulers (round-robin, priority-fair-share) use one slot per backend.
func fairShareCandidateBackends(pool model.PoolConfig) []model.BackendName {
	switch normalizeSchedulerName(pool.Scheduler) {
	case NameWeightedRoundRobin:
		return weightedBackends(pool)
	default:
		return orderedBackends(pool)
	}
}

func (s *priorityFairShareState) Release(pool model.PoolName, backend model.BackendName, allocation model.AllocationStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()

	poolActive, ok := s.active[pool]
	if !ok {
		return
	}

	if poolActive[backend] > 0 {
		poolActive[backend]--
	}

	tenant := normalizeTenant(allocation.Tenant)
	if tenantBackends, ok := s.tenantActive[pool]; ok {
		tenantCounts := tenantBackends[backend]
		if tenantCounts == nil {
			return
		}
		if tenantCounts[tenant] > 0 {
			tenantCounts[tenant]--
			if tenantCounts[tenant] == 0 {
				delete(tenantCounts, tenant)
			}
		}
	}
}

func (s *priorityFairShareState) Active(pool model.PoolName, backend model.BackendName) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active[pool][backend]
}

func priorityFairShareScore(activeTotal, tenantActive, weight int) int {
	if weight < priorityShareClassMinWeight {
		weight = priorityShareClassMinWeight
	}
	tenantPenalty := (tenantActive * priorityShareScale) / weight
	return activeTotal*priorityShareScale + tenantPenalty - (weight-1)*priorityShareClassBoost
}

// backendAvailableForPriority reports whether a backend has free capacity for
// the given priority class after soft reserves for higher lanes are held back.
func backendAvailableForPriority(cfg model.BackendConfig, active int, pool model.PoolConfig, priorityClass string) bool {
	return backendAvailableForPriorityWithStarvation(cfg, active, pool, priorityClass, false)
}

// backendAvailableForPriorityWithStarvation is like backendAvailableForPriority
// but when starving is true, soft reserves are bypassed so the class can use
// the full maxRunners budget.
func backendAvailableForPriorityWithStarvation(cfg model.BackendConfig, active int, pool model.PoolConfig, priorityClass string, starving bool) bool {
	if !cfg.Enabled || !cfg.Healthy {
		return false
	}
	if starving {
		return active < cfg.MaxRunners
	}
	return active < EffectiveMaxRunners(cfg.MaxRunners, pool.FairShare, priorityClass)
}

// SoftReserveHeld returns how many slots on each backend are soft-reserved for
// priority classes (lanes) with weight strictly greater than the request.
// Lower-weight traffic cannot consume those slots; higher-weight traffic sees
// the full maxRunners budget.
func SoftReserveHeld(fairShare model.FairShareConfig, requestPriorityClass string) int {
	if !fairShare.Enabled || len(fairShare.SoftReserves) == 0 {
		return 0
	}
	requestWeight := priorityClassWeight(fairShare, requestPriorityClass)
	held := 0
	for class, slots := range fairShare.SoftReserves {
		if slots <= 0 {
			continue
		}
		if priorityClassWeight(fairShare, class) > requestWeight {
			held += slots
		}
	}
	return held
}

// EffectiveMaxRunners is the per-backend concurrency limit visible to a request
// after higher-lane soft reserves are subtracted. Never returns a negative value.
// When fair-share is disabled or softReserves is empty, returns maxRunners unchanged.
func EffectiveMaxRunners(maxRunners int, fairShare model.FairShareConfig, requestPriorityClass string) int {
	if maxRunners <= 0 {
		return 0
	}
	held := SoftReserveHeld(fairShare, requestPriorityClass)
	if held <= 0 {
		return maxRunners
	}
	effective := maxRunners - held
	if effective < 0 {
		return 0
	}
	return effective
}

func normalizePriorityClassWeight(pool model.PoolConfig, requestPriorityClass string) int {
	return priorityClassWeight(pool.FairShare, requestPriorityClass)
}

func priorityClassWeight(fairShare model.FairShareConfig, requestPriorityClass string) int {
	normalized := strings.TrimSpace(strings.ToLower(requestPriorityClass))
	if normalized == "" {
		return defaultNormalPriorityClass
	}

	if fairShare.PriorityClasses != nil {
		if weight, ok := fairShare.PriorityClasses[normalized]; ok && weight > 0 {
			return weight
		}
	}

	switch model.PriorityClass(normalized) {
	case model.PriorityClassHigh:
		return defaultHighPriorityClass
	case model.PriorityClassNormal:
		return defaultNormalPriorityClass
	}
	return defaultNormalPriorityClass
}

func normalizePriorityClassString(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		return string(model.PriorityClassNormal)
	}
	return value
}

func normalizeTenant(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "default"
	}
	return strings.ToLower(value)
}
