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
	// starvationIdleGapFactor is how many StarvationAfter intervals a class may
	// go without a soft-reserve denial before its streak resets. A retry about
	// one interval later still counts as continuous, so the request that crosses
	// StarvationAfter can bypass soft reserves. A longer gap means the class
	// went idle and must accumulate a new streak.
	starvationIdleGapFactor = 2
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
	// usageWindows remembers each pool's UsageWindow so a reservation or
	// release in any pool can prune idle pools, not only the pool just touched.
	usageWindows map[model.PoolName]time.Duration

	// starvation tracks a continuous soft-reserve denial streak per known
	// priority class. Hard maxRunners denials do not create or refresh entries.
	// Entries are removed on admission, when StarvationAfter is zero, and when
	// the class is idle longer than starvationIdleGap.
	starvation map[starvationKey]starvationTrack
}

type starvationKey struct {
	pool          model.PoolName
	priorityClass string
}

type starvationTrack struct {
	start      time.Time
	lastDenied time.Time
}

func newPriorityFairShareState() *priorityFairShareState {
	return &priorityFairShareState{
		cursors:      map[model.PoolName]int{},
		active:       map[model.PoolName]map[model.BackendName]int{},
		tenantActive: map[model.PoolName]map[model.BackendName]map[string]int{},
		tenantLog:    map[model.PoolName][]tenantReservation{},
		usageWindows: map[model.PoolName]time.Duration{},
		starvation:   map[starvationKey]starvationTrack{},
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

	now := nowFunc()
	s.noteUsageWindow(pool)
	s.pruneAllTenantLogs(now)
	if pool.FairShare.StarvationAfter <= 0 {
		s.dropPoolStarvation(pool.Name)
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

	starving := s.isStarving(pool, request.PriorityClass, now)

	pinned := request.Backend
	if pinned != nil {
		cfg, ok := pool.Backends[*pinned]
		if !ok {
			return "", ErrUnknownBackend
		}
		active := s.active[pool.Name][*pinned]
		if !backendAvailableForPriorityWithStarvation(cfg, active, pool, request.PriorityClass, starving) {
			if backendDeniedBySoftReserve(cfg, active, pool, request.PriorityClass) {
				s.recordStarvation(pool, request.PriorityClass, now)
			}
			return "", ErrNoCapacity
		}
		s.active[pool.Name][*pinned]++
		tenantMap := s.tenantActive[pool.Name][*pinned]
		if tenantMap == nil {
			tenantMap = map[string]int{}
			s.tenantActive[pool.Name][*pinned] = tenantMap
		}
		tenantMap[tenant]++
		s.recordReservation(pool, *pinned, tenant, now)
		s.clearStarvation(pool, request.PriorityClass)
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
		if s.anyBackendDeniedBySoftReserve(pool, request.PriorityClass) {
			s.recordStarvation(pool, request.PriorityClass, now)
		}
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
	s.clearStarvation(pool, request.PriorityClass)
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

// noteUsageWindow remembers the pool's scoring window. A non-positive window
// drops that pool's log so disabling UsageWindow cannot leave stale entries.
func (s *priorityFairShareState) noteUsageWindow(pool model.PoolConfig) {
	if pool.FairShare.UsageWindow <= 0 {
		delete(s.usageWindows, pool.Name)
		delete(s.tenantLog, pool.Name)
		return
	}
	s.usageWindows[pool.Name] = pool.FairShare.UsageWindow
}

// recordReservation appends a timestamped entry to the tenant log when
// UsageWindow is configured. Pruning of every pool happens in Reserve/Release.
func (s *priorityFairShareState) recordReservation(pool model.PoolConfig, backend model.BackendName, tenant string, now time.Time) {
	if pool.FairShare.UsageWindow <= 0 {
		return
	}
	s.usageWindows[pool.Name] = pool.FairShare.UsageWindow
	s.tenantLog[pool.Name] = append(s.tenantLog[pool.Name], tenantReservation{
		tenant:    tenant,
		backend:   backend,
		reserveAt: now,
	})
}

// pruneAllTenantLogs drops usage-window entries that have aged out of every
// pool, including pools that are not on the request currently being served.
func (s *priorityFairShareState) pruneAllTenantLogs(now time.Time) {
	for pool, window := range s.usageWindows {
		if window <= 0 {
			delete(s.tenantLog, pool)
			delete(s.usageWindows, pool)
			continue
		}
		s.pruneLog(pool, now.Add(-window))
	}
}

// pruneLog removes entries older than cutoff from one pool's tenant log.
func (s *priorityFairShareState) pruneLog(pool model.PoolName, cutoff time.Time) {
	log := s.tenantLog[pool]
	kept := 0
	for _, entry := range log {
		if !entry.reserveAt.Before(cutoff) {
			log[kept] = entry
			kept++
		}
	}
	if kept == 0 {
		delete(s.tenantLog, pool)
		return
	}
	s.tenantLog[pool] = log[:kept]
}

// isStarving reports whether the priority class has been denied by soft
// reserves, without an idle gap, for at least StarvationAfter. An idle gap
// clears the streak. Caller must hold s.mu.
func (s *priorityFairShareState) isStarving(pool model.PoolConfig, priorityClass string, now time.Time) bool {
	after := pool.FairShare.StarvationAfter
	if after <= 0 {
		return false
	}
	key := starvationKeyFor(pool, priorityClass)
	track, ok := s.starvation[key]
	if !ok {
		return false
	}
	if now.Sub(track.lastDenied) > starvationIdleGap(after) {
		delete(s.starvation, key)
		return false
	}
	return now.Sub(track.start) >= after
}

// recordStarvation continues, or starts, a soft-reserve denial streak.
// StarvationAfter == 0 records nothing. Caller must hold s.mu.
func (s *priorityFairShareState) recordStarvation(pool model.PoolConfig, priorityClass string, now time.Time) {
	after := pool.FairShare.StarvationAfter
	if after <= 0 {
		return
	}
	key := starvationKeyFor(pool, priorityClass)
	track, ok := s.starvation[key]
	if !ok || now.Sub(track.lastDenied) > starvationIdleGap(after) {
		s.starvation[key] = starvationTrack{start: now, lastDenied: now}
		return
	}
	track.lastDenied = now
	s.starvation[key] = track
}

func (s *priorityFairShareState) clearStarvation(pool model.PoolConfig, priorityClass string) {
	delete(s.starvation, starvationKeyFor(pool, priorityClass))
}

func (s *priorityFairShareState) dropPoolStarvation(pool model.PoolName) {
	for key := range s.starvation {
		if key.pool == pool {
			delete(s.starvation, key)
		}
	}
}

func starvationIdleGap(starvationAfter time.Duration) time.Duration {
	return starvationAfter * starvationIdleGapFactor
}

func starvationKeyFor(pool model.PoolConfig, priorityClass string) starvationKey {
	return starvationKey{
		pool:          pool.Name,
		priorityClass: canonicalPriorityClass(pool.FairShare, priorityClass),
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
	// Sweep every pool's usage window, including pools this release does not touch.
	defer func() { s.pruneAllTenantLogs(nowFunc()) }()

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

// backendDeniedBySoftReserve reports a denial caused by higher-lane soft
// reserves while hard maxRunners still has room. A backend already at
// maxRunners is a hard-capacity denial and must not arm starvation.
func backendDeniedBySoftReserve(cfg model.BackendConfig, active int, pool model.PoolConfig, priorityClass string) bool {
	if !cfg.Enabled || !cfg.Healthy || cfg.MaxRunners <= 0 || active >= cfg.MaxRunners {
		return false
	}
	return active >= EffectiveMaxRunners(cfg.MaxRunners, pool.FairShare, priorityClass)
}

func (s *priorityFairShareState) anyBackendDeniedBySoftReserve(pool model.PoolConfig, priorityClass string) bool {
	active := s.active[pool.Name]
	for name, cfg := range pool.Backends {
		if backendDeniedBySoftReserve(cfg, active[name], pool, priorityClass) {
			return true
		}
	}
	return false
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

// canonicalPriorityClass folds a request class onto a known lane so starvation
// state is bounded by configuration plus the built-in high/normal classes.
// Unknown names share normal, matching priorityClassWeight's fallback.
func canonicalPriorityClass(fairShare model.FairShareConfig, priorityClass string) string {
	normalized := normalizePriorityClassString(priorityClass)
	if weight, ok := fairShare.PriorityClasses[normalized]; ok && weight > 0 {
		return normalized
	}
	switch model.PriorityClass(normalized) {
	case model.PriorityClassHigh, model.PriorityClassNormal:
		return normalized
	}
	return string(model.PriorityClassNormal)
}

func normalizeTenant(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "default"
	}
	return strings.ToLower(value)
}
