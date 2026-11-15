// Package overflow sends a pool's jobs to GitHub-hosted runners while the
// pool is backed up. GitHub cannot move a job that is already queued for
// self-hosted labels, so the controller steers jobs before they are queued:
// it sets a repository Actions variable that workflows read in runs-on.
package overflow

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	ghclient "github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/github"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/tier"
)

const (
	defaultSaturateAfter   = 2 * time.Minute
	defaultRecoverAfter    = 5 * time.Minute
	defaultBillingTokenEnv = "GITHUB_BILLING_TOKEN"
	defaultIncludedUSD     = 12.0
	defaultHardLimitRatio  = 0.9
	defaultIncludedMinutes = 2000.0
	// visibilityTTL bounds how long a repository's public/private answer is reused.
	visibilityTTL = time.Hour
	// billingCacheTTL bounds how often direct GitHub billing/job fallbacks run.
	billingCacheTTL = 15 * time.Minute
)

// GitHub is the part of the GitHub client the controller uses.
type GitHub interface {
	SetRepositoryVariable(ctx context.Context, repository, name, value string) error
	RepositoryIsPrivate(ctx context.Context, repository string) (bool, error)
	GetActionsBillingUsage(ctx context.Context, user string, year, month int, token string) (grossUSD, netUSD float64, err error)
	MonthlyHostedMinutes(ctx context.Context, repository string, monthStart, now time.Time) (float64, error)
}

// Allocations lists the broker's current allocations.
type Allocations interface {
	List() []model.AllocationStatus
}

// Budget returns cached tier decisions; *tier.Manager satisfies it.
type Budget interface {
	Decision(pool model.PoolName, backend model.BackendName) (tier.Decision, bool)
}

// Controller keeps each managed repository variable in step with its pool.
type Controller struct {
	pools  []model.PoolConfig
	github GitHub
	allocs Allocations
	budget Budget
	logf   func(string, ...any)

	mu           sync.Mutex
	getenv       func(string) string
	state        map[model.PoolName]*poolState
	applied      map[string]string
	visibility   map[string]visibility
	blockedUntil map[model.PoolName]time.Time
	directCache  map[model.PoolName]cachedBudget
}

type poolState struct {
	backedUp    bool
	lastWaiting time.Time
}

type visibility struct {
	private   bool
	checkedAt time.Time
}

type cachedBudget struct {
	allowed   bool
	reason    string
	checkedAt time.Time
}

// New returns nil when no pool enables hostedOverflow. budget may be nil, in
// which case the controller falls back to direct GitHub queries and fails open
// if all budget sources are unavailable.
func New(cfg model.BrokerConfig, github GitHub, allocs Allocations, budget Budget) *Controller {
	var pools []model.PoolConfig
	for _, pool := range cfg.Pools {
		if pool.HostedOverflow != nil && pool.HostedOverflow.Enabled {
			pools = append(pools, pool)
		}
	}
	if len(pools) == 0 {
		return nil
	}
	return &Controller{
		pools:        pools,
		github:       github,
		allocs:       allocs,
		budget:       budget,
		logf:         log.Printf,
		getenv:       os.Getenv,
		state:        map[model.PoolName]*poolState{},
		applied:      map[string]string{},
		visibility:   map[string]visibility{},
		blockedUntil: map[model.PoolName]time.Time{},
		directCache:  map[model.PoolName]cachedBudget{},
	}
}

// Reconcile updates every managed variable that is out of step. Variables
// already set to the wanted value are not written again.
func (c *Controller) Reconcile(ctx context.Context, now time.Time) error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	allocations := c.allocs.List()
	var errs []error
	for _, pool := range c.pools {
		cfg := pool.HostedOverflow
		backedUp := c.updatePoolState(pool.Name, cfg, allocations, now)
		var budgetChecked, budgetOK bool
		var budgetReason string
		for _, repository := range cfg.Repositories {
			repository = strings.TrimSpace(repository)
			want := strings.TrimSpace(cfg.SelfHostedValue)
			reason := "pool keeping up"
			if backedUp {
				private, err := c.isPrivate(ctx, repository, now)
				switch {
				case err != nil:
					errs = append(errs, fmt.Errorf("read visibility of %s: %w", repository, err))
					reason = "pool backed up, visibility unknown"
				case !private:
					want = strings.TrimSpace(cfg.HostedValue)
					reason = "pool backed up, public repository"
				default:
					if !budgetChecked {
						budgetOK, budgetReason = c.budgetAvailable(ctx, pool.Name, cfg, now)
						budgetChecked = true
					}
					if budgetOK {
						want = strings.TrimSpace(cfg.HostedValue)
					}
					reason = "pool backed up, " + budgetReason
				}
			}
			key := string(pool.Name) + "|" + strings.ToLower(repository)
			if c.applied[key] == want {
				continue
			}
			if err := c.github.SetRepositoryVariable(ctx, repository, strings.TrimSpace(cfg.Variable), want); err != nil {
				if ghclient.IsBillingExhaustedError(err) {
					c.latchBlockedUntilNextMonth(pool.Name, now, err.Error())
				}
				errs = append(errs, fmt.Errorf("set %s on %s: %w", cfg.Variable, repository, err))
				continue
			}
			c.applied[key] = want
			c.logf("hosted overflow: set %s=%s on %s (pool %s: %s)", cfg.Variable, want, repository, pool.Name, reason)
		}
	}
	return errors.Join(errs...)
}

func (c *Controller) updatePoolState(pool model.PoolName, cfg *model.HostedOverflowConfig, allocations []model.AllocationStatus, now time.Time) bool {
	saturateAfter := cfg.SaturateAfter
	if saturateAfter <= 0 {
		saturateAfter = defaultSaturateAfter
	}
	recoverAfter := cfg.RecoverAfter
	if recoverAfter <= 0 {
		recoverAfter = defaultRecoverAfter
	}
	st := c.state[pool]
	if st == nil {
		st = &poolState{}
		c.state[pool] = st
	}
	if n := Waiting(allocations, pool, now, saturateAfter); n > 0 {
		if !st.backedUp {
			c.logf("hosted overflow: pool %s backed up (%d jobs waiting over %s)", pool, n, saturateAfter)
		}
		st.backedUp = true
		st.lastWaiting = now
	} else if st.backedUp && now.Sub(st.lastWaiting) >= recoverAfter {
		c.logf("hosted overflow: pool %s caught up (no long waits for %s)", pool, recoverAfter)
		st.backedUp = false
	}
	return st.backedUp
}

// Waiting counts the pool's allocations that have waited at least after
// without a runner picking up their job: queued for capacity, or given a
// backend whose runner has not registered (for example a Pending pod).
func Waiting(allocations []model.AllocationStatus, pool model.PoolName, now time.Time, after time.Duration) int {
	n := 0
	for _, allocation := range allocations {
		if allocation.Pool != pool {
			continue
		}
		switch allocation.State {
		case model.StatePending, model.StateReserved, model.StateReady:
		default:
			continue
		}
		if allocation.IsRegistered() || allocation.IsClaimed() {
			continue
		}
		if allocation.CreatedAt.IsZero() || now.Sub(allocation.CreatedAt) < after {
			continue
		}
		n++
	}
	return n
}

func (c *Controller) budgetAvailable(ctx context.Context, pool model.PoolName, cfg *model.HostedOverflowConfig, now time.Time) (bool, string) {
	if until := c.blockedUntil[pool]; !until.IsZero() && now.Before(until) {
		return false, "GitHub minutes exhausted until " + until.UTC().Format("2006-01-02")
	}

	// Tier 1 (Primary): Prometheus tierRouting budgetRules.
	if c.budget != nil && len(cfg.BudgetRules) > 0 {
		if decision, ok := c.budget.Decision(pool, model.HostedOverflowBudgetBackend); ok && !decision.Stale && decision.State != tier.StateUnknown {
			switch decision.State {
			case tier.StateHealthy, tier.StateApproaching:
				return true, "GitHub minutes " + decision.State
			default:
				reason := decision.Reason
				if reason == "" {
					reason = decision.State
				}
				c.latchBlockedUntilNextMonth(pool, now, "Prometheus budget "+reason)
				return false, "GitHub minutes unavailable: " + reason
			}
		}
	}

	// Reuse recent direct GitHub fallback check within billingCacheTTL.
	if cached, ok := c.directCache[pool]; ok && now.Sub(cached.checkedAt) < billingCacheTTL {
		return cached.allowed, cached.reason
	}

	ratio := cfg.HardLimitRatio
	if ratio <= 0 {
		ratio = defaultHardLimitRatio
	}

	// Tier 2 (Backup 1): Direct GitHub Billing Usage API (requires PAT).
	if user := resolveBillingUser(cfg); user != "" {
		tokenEnv := strings.TrimSpace(cfg.BillingTokenEnv)
		if tokenEnv == "" {
			tokenEnv = defaultBillingTokenEnv
		}
		if token := strings.TrimSpace(c.getenv(tokenEnv)); token != "" {
			utc := now.UTC()
			grossUSD, netUSD, err := c.github.GetActionsBillingUsage(ctx, user, utc.Year(), int(utc.Month()), token)
			if ghclient.IsBillingExhaustedError(err) {
				c.latchBlockedUntilNextMonth(pool, now, err.Error())
				return false, "GitHub billing exhausted until " + c.blockedUntil[pool].UTC().Format("2006-01-02")
			}
			if err == nil {
				includedUSD := cfg.IncludedUSD
				if includedUSD <= 0 {
					includedUSD = defaultIncludedUSD
				}
				limitUSD := includedUSD * ratio
				if netUSD > 0 || grossUSD >= limitUSD {
					reason := fmt.Sprintf("GitHub billing API exhausted ($%.2f/$%.2f, net $%.2f)", grossUSD, limitUSD, netUSD)
					c.latchBlockedUntilNextMonth(pool, now, reason)
					return false, reason
				}
				reason := fmt.Sprintf("GitHub billing API under limit ($%.2f/$%.2f)", grossUSD, limitUSD)
				c.directCache[pool] = cachedBudget{allowed: true, reason: reason, checkedAt: now}
				return true, reason
			}
		}
	}

	// Tier 3 (Backup 2, least accurate): GitHub App workflow-job duration estimate.
	monthStart := time.Date(now.UTC().Year(), now.UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
	var totalMinutes float64
	var anyOK bool
	for _, repo := range cfg.Repositories {
		repo = strings.TrimSpace(repo)
		private, err := c.isPrivate(ctx, repo, now)
		if err != nil || !private {
			continue
		}
		mins, err := c.github.MonthlyHostedMinutes(ctx, repo, monthStart, now)
		if ghclient.IsBillingExhaustedError(err) {
			c.latchBlockedUntilNextMonth(pool, now, err.Error())
			return false, "GitHub billing exhausted until " + c.blockedUntil[pool].UTC().Format("2006-01-02")
		}
		if err == nil {
			anyOK = true
			totalMinutes += mins
		}
	}
	if anyOK {
		includedMin := cfg.IncludedMinutes
		if includedMin <= 0 {
			includedMin = defaultIncludedMinutes
		}
		limitMin := includedMin * ratio
		if totalMinutes >= limitMin {
			reason := fmt.Sprintf("GitHub App job estimate exhausted (%.1f/%.0f min)", totalMinutes, limitMin)
			c.latchBlockedUntilNextMonth(pool, now, reason)
			return false, reason
		}
		reason := fmt.Sprintf("GitHub App job estimate under limit (%.1f/%.0f min)", totalMinutes, limitMin)
		c.directCache[pool] = cachedBudget{allowed: true, reason: reason, checkedAt: now}
		return true, reason
	}

	// Worst case: all budget sources unavailable -> fail open.
	reason := "budget sources unavailable, failing open"
	c.directCache[pool] = cachedBudget{allowed: true, reason: reason, checkedAt: now}
	return true, reason
}

func (c *Controller) latchBlockedUntilNextMonth(pool model.PoolName, now time.Time, reason string) {
	utc := now.UTC()
	until := time.Date(utc.Year(), utc.Month()+1, 1, 0, 0, 0, 0, time.UTC)
	if !c.blockedUntil[pool].Equal(until) {
		c.logf("hosted overflow: pool %s blocking private-repo overflow until %s (%s)", pool, until.Format(time.RFC3339), reason)
	}
	c.blockedUntil[pool] = until
	delete(c.directCache, pool)
}

func resolveBillingUser(cfg *model.HostedOverflowConfig) string {
	if u := strings.TrimSpace(cfg.BillingUser); u != "" {
		return u
	}
	for _, repo := range cfg.Repositories {
		if owner, _, ok := strings.Cut(strings.TrimSpace(repo), "/"); ok && owner != "" {
			return owner
		}
	}
	return ""
}

func (c *Controller) isPrivate(ctx context.Context, repository string, now time.Time) (bool, error) {
	key := strings.ToLower(repository)
	if cached, ok := c.visibility[key]; ok && now.Sub(cached.checkedAt) < visibilityTTL {
		return cached.private, nil
	}
	private, err := c.github.RepositoryIsPrivate(ctx, repository)
	if err != nil {
		return true, err
	}
	c.visibility[key] = visibility{private: private, checkedAt: now}
	return private, nil
}
