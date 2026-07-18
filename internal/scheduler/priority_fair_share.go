package scheduler

import (
	"errors"
	"strings"
	"sync"

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

// ReassignTenant moves one unit of tenantActive from one tenant to another for
// the given pool/backend without changing total active capacity. Used when a
// warm allocation (reserved under the empty/"default" tenant) is handed off to
// a real consumer identity.
func (p *PriorityFairShare) ReassignTenant(pool model.PoolName, backend model.BackendName, from, to string) {
	p.state.ReassignTenant(pool, backend, from, to)
}

func (p *PriorityFairShare) TenantActive(pool model.PoolName, backend model.BackendName, tenant string) int {
	return p.state.TenantActive(pool, backend, tenant)
}

type priorityFairShareState struct {
	mu           sync.Mutex
	cursors      map[model.PoolName]int
	active       map[model.PoolName]map[model.BackendName]int
	tenantActive map[model.PoolName]map[model.BackendName]map[string]int
}

func newPriorityFairShareState() *priorityFairShareState {
	return &priorityFairShareState{
		cursors:      map[model.PoolName]int{},
		active:       map[model.PoolName]map[model.BackendName]int{},
		tenantActive: map[model.PoolName]map[model.BackendName]map[string]int{},
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

	pinned := request.Backend
	if pinned != nil {
		cfg, ok := pool.Backends[*pinned]
		if !ok {
			return "", ErrUnknownBackend
		}
		if !backendAvailable(cfg, s.active[pool.Name][*pinned]) {
			return "", ErrNoCapacity
		}
		s.active[pool.Name][*pinned]++
		tenantMap := s.tenantActive[pool.Name][*pinned]
		if tenantMap == nil {
			tenantMap = map[string]int{}
			s.tenantActive[pool.Name][*pinned] = tenantMap
		}
		tenantMap[normalizeTenant(request.Tenant)]++
		return *pinned, nil
	}

	backends := orderedBackends(pool)
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
		if !backendAvailable(cfg, active) {
			continue
		}

		normalizedTenant := s.tenantActive[pool.Name][candidate][tenant]
		score := priorityFairShareScore(active, normalizedTenant, priorityWeight)
		if bestIndex == -1 || score < bestScore || (score == bestScore && offset < bestOffset) {
			bestIndex = (start + offset) % len(backends)
			bestScore = score
			bestOffset = offset
		}
	}

	if bestIndex < 0 {
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
	return selected, nil
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

func (s *priorityFairShareState) TenantActive(pool model.PoolName, backend model.BackendName, tenant string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tenantActive[pool][backend][normalizeTenant(tenant)]
}

func (s *priorityFairShareState) ReassignTenant(pool model.PoolName, backend model.BackendName, from, to string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	fromTenant := normalizeTenant(from)
	toTenant := normalizeTenant(to)
	if fromTenant == toTenant {
		return
	}

	if _, ok := s.tenantActive[pool]; !ok {
		s.tenantActive[pool] = map[model.BackendName]map[string]int{}
	}
	fromCounts := s.tenantActive[pool][backend]
	if fromCounts == nil {
		fromCounts = map[string]int{}
		s.tenantActive[pool][backend] = fromCounts
	}
	if fromCounts[fromTenant] > 0 {
		fromCounts[fromTenant]--
		if fromCounts[fromTenant] == 0 {
			delete(fromCounts, fromTenant)
		}
	}
	toCounts := s.tenantActive[pool][backend]
	if toCounts == nil {
		toCounts = map[string]int{}
		s.tenantActive[pool][backend] = toCounts
	}
	toCounts[toTenant]++
}

func priorityFairShareScore(activeTotal, tenantActive, weight int) int {
	if weight < priorityShareClassMinWeight {
		weight = priorityShareClassMinWeight
	}
	tenantPenalty := (tenantActive * priorityShareScale) / weight
	return activeTotal*priorityShareScale + tenantPenalty - (weight-1)*priorityShareClassBoost
}

func normalizePriorityClassWeight(pool model.PoolConfig, requestPriorityClass string) int {
	normalized := strings.TrimSpace(strings.ToLower(requestPriorityClass))
	if normalized == "" {
		return defaultNormalPriorityClass
	}

	if pool.FairShare.PriorityClasses != nil {
		if weight, ok := pool.FairShare.PriorityClasses[normalized]; ok && weight > 0 {
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

func normalizeTenant(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "default"
	}
	return strings.ToLower(value)
}
