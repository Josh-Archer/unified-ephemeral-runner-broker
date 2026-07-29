// Package budget implements per-backend allocation budget and cost guardrails.
//
// Operator-set daily/monthly quotas mark backends ineligible when exceeded.
// Counters are pluggable: the default Source is a local rolling counter shared
// via the admission state document; external Sources (metrics scrapes or cloud
// billing APIs) can override reported usage without changing allocation logic.
package budget

import (
	"context"
	"sync"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/store"
)

// Source reports external daily/monthly usage for a pool/backend.
// When Usage returns ok=true, the values replace local counters for allow checks
// (local counters still advance on Record so restarts remain useful).
type Source interface {
	Usage(ctx context.Context, pool model.PoolName, backend model.BackendName) (daily, monthly int, ok bool)
}

// Decision describes whether a backend is within budget.
type Decision struct {
	Allowed      bool
	Reason       string // empty when allowed; "daily-budget-exceeded" or "monthly-budget-exceeded"
	DailyUsed    int
	DailyLimit   int
	MonthlyUsed  int
	MonthlyLimit int
}

// Tracker tracks per-backend allocation budgets.
type Tracker struct {
	mu       sync.Mutex
	counters map[string]*counter
	shared   store.AdmissionStateStore
	source   Source
}

type counter struct {
	dailyWindowStart   time.Time
	dailyUsed          int
	monthlyWindowStart time.Time
	monthlyUsed        int
}

// NewTracker builds a budget tracker. shared may be nil (process-local only).
func NewTracker(shared store.AdmissionStateStore) *Tracker {
	t := &Tracker{
		counters: map[string]*counter{},
		shared:   shared,
	}
	t.reloadShared()
	return t
}

// SetSource installs an optional external usage source (metrics/cloud APIs).
func (t *Tracker) SetSource(source Source) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.source = source
}

// Allow reports whether the backend may accept another successful allocation.
func (t *Tracker) Allow(pool model.PoolName, backend model.BackendName, cfg model.BudgetConfig, now time.Time) Decision {
	if t == nil || !cfg.Enabled {
		return Decision{Allowed: true}
	}
	t.reloadShared()
	t.mu.Lock()
	defer t.mu.Unlock()

	c := t.ensureCounterLocked(store.AdmissionKey(pool, backend), now)
	dailyUsed, monthlyUsed := c.dailyUsed, c.monthlyUsed
	if t.source != nil {
		if d, m, ok := t.source.Usage(context.Background(), pool, backend); ok {
			dailyUsed, monthlyUsed = d, m
		}
	}
	return evaluate(cfg, dailyUsed, monthlyUsed)
}

// Record increments counters after a successful ready allocation.
func (t *Tracker) Record(pool model.PoolName, backend model.BackendName, cfg model.BudgetConfig, now time.Time) {
	if t == nil || !cfg.Enabled {
		return
	}
	t.reloadShared()
	t.mu.Lock()
	defer t.mu.Unlock()

	c := t.ensureCounterLocked(store.AdmissionKey(pool, backend), now)
	if cfg.MaxAllocationsDaily > 0 {
		c.dailyUsed++
	}
	if cfg.MaxAllocationsMonthly > 0 {
		c.monthlyUsed++
	}
	t.persistSharedLocked()
}

// Snapshot returns current usage for observability.
func (t *Tracker) Snapshot(pool model.PoolName, backend model.BackendName, cfg model.BudgetConfig, now time.Time) Decision {
	if t == nil || !cfg.Enabled {
		return Decision{Allowed: true}
	}
	t.reloadShared()
	t.mu.Lock()
	defer t.mu.Unlock()
	c := t.ensureCounterLocked(store.AdmissionKey(pool, backend), now)
	return evaluate(cfg, c.dailyUsed, c.monthlyUsed)
}

func evaluate(cfg model.BudgetConfig, dailyUsed, monthlyUsed int) Decision {
	decision := Decision{
		Allowed:      true,
		DailyUsed:    dailyUsed,
		DailyLimit:   cfg.MaxAllocationsDaily,
		MonthlyUsed:  monthlyUsed,
		MonthlyLimit: cfg.MaxAllocationsMonthly,
	}
	if cfg.MaxAllocationsDaily > 0 && dailyUsed >= cfg.MaxAllocationsDaily {
		decision.Allowed = false
		decision.Reason = "daily-budget-exceeded"
		return decision
	}
	if cfg.MaxAllocationsMonthly > 0 && monthlyUsed >= cfg.MaxAllocationsMonthly {
		decision.Allowed = false
		decision.Reason = "monthly-budget-exceeded"
		return decision
	}
	return decision
}

func (t *Tracker) ensureCounterLocked(key string, now time.Time) *counter {
	c := t.counters[key]
	if c == nil {
		c = &counter{}
		t.counters[key] = c
	}
	dayStart := utcDayStart(now)
	monthStart := utcMonthStart(now)
	if c.dailyWindowStart.IsZero() || c.dailyWindowStart.Before(dayStart) {
		c.dailyWindowStart = dayStart
		c.dailyUsed = 0
	}
	if c.monthlyWindowStart.IsZero() || c.monthlyWindowStart.Before(monthStart) {
		c.monthlyWindowStart = monthStart
		c.monthlyUsed = 0
	}
	return c
}

func (t *Tracker) reloadShared() {
	if t == nil || t.shared == nil {
		return
	}
	doc, err := t.shared.LoadAdmissionState(context.Background())
	if err != nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for key, state := range doc.Budgets {
		c := t.counters[key]
		if c == nil {
			c = &counter{}
			t.counters[key] = c
		}
		c.dailyWindowStart = state.DailyWindowStart
		c.dailyUsed = state.DailyUsed
		c.monthlyWindowStart = state.MonthlyWindowStart
		c.monthlyUsed = state.MonthlyUsed
	}
}

func (t *Tracker) persistSharedLocked() {
	if t == nil || t.shared == nil {
		return
	}
	// Preserve circuit/rate-limit state written by backend admission.
	doc, err := t.shared.LoadAdmissionState(context.Background())
	if err != nil {
		doc = store.AdmissionStateDocument{}
	}
	if doc.Circuits == nil {
		doc.Circuits = map[string]store.AdmissionCircuitState{}
	}
	if doc.Limits == nil {
		doc.Limits = map[string]store.AdmissionRateLimit{}
	}
	budgets := map[string]store.AdmissionBudgetState{}
	for key, c := range t.counters {
		budgets[key] = store.AdmissionBudgetState{
			DailyWindowStart:   c.dailyWindowStart,
			DailyUsed:          c.dailyUsed,
			MonthlyWindowStart: c.monthlyWindowStart,
			MonthlyUsed:        c.monthlyUsed,
		}
	}
	doc.Budgets = budgets
	_ = t.shared.SaveAdmissionState(context.Background(), doc)
}

func utcDayStart(now time.Time) time.Time {
	u := now.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

func utcMonthStart(now time.Time) time.Time {
	u := now.UTC()
	return time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// CostClass returns the effective cost class for scheduler tie-breaking.
// Lower is cheaper. Explicit cfg.CostClass > 0 wins; otherwise built-in defaults
// prefer cluster-local and free-tier cloud backends over VM backends.
func CostClass(name model.BackendName, cfg model.BackendConfig) int {
	if cfg.CostClass > 0 {
		return cfg.CostClass
	}
	switch name {
	case model.BackendARC, model.BackendDesktop:
		return 10
	case model.BackendAzureFunctions:
		return 30
	case model.BackendLambda:
		return 40
	case model.BackendCodeBuild, model.BackendCloudRun:
		return 50
	case model.BackendAzureVM:
		return 80
	case model.BackendEC2, model.BackendGCE:
		return 90
	default:
		return 100
	}
}

// FreeSlots returns remaining local scheduler capacity for a backend.
func FreeSlots(cfg model.BackendConfig, active int) int {
	if !cfg.Enabled || !cfg.Healthy || cfg.MaxRunners <= 0 {
		return 0
	}
	free := cfg.MaxRunners - active
	if free < 0 {
		return 0
	}
	return free
}
