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
	"strings"
	"sync"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/tier"
)

const (
	defaultSaturateAfter = 2 * time.Minute
	defaultRecoverAfter  = 5 * time.Minute
	// visibilityTTL bounds how long a repository's public/private answer is reused.
	visibilityTTL = time.Hour
)

// GitHub is the part of the GitHub client the controller uses.
type GitHub interface {
	SetRepositoryVariable(ctx context.Context, repository, name, value string) error
	RepositoryIsPrivate(ctx context.Context, repository string) (bool, error)
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

	mu         sync.Mutex
	state      map[model.PoolName]*poolState
	applied    map[string]string
	visibility map[string]visibility
}

type poolState struct {
	backedUp    bool
	lastWaiting time.Time
}

type visibility struct {
	private   bool
	checkedAt time.Time
}

// New returns nil when no pool enables hostedOverflow. budget may be nil, in
// which case private repositories never switch to hosted runners.
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
		pools:      pools,
		github:     github,
		allocs:     allocs,
		budget:     budget,
		logf:       log.Printf,
		state:      map[model.PoolName]*poolState{},
		applied:    map[string]string{},
		visibility: map[string]visibility{},
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
		budgetOK, budgetReason := c.budgetAvailable(pool.Name, cfg)
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
				case budgetOK:
					want = strings.TrimSpace(cfg.HostedValue)
					reason = "pool backed up, " + budgetReason
				default:
					reason = "pool backed up, " + budgetReason
				}
			}
			key := string(pool.Name) + "|" + strings.ToLower(repository)
			if c.applied[key] == want {
				continue
			}
			if err := c.github.SetRepositoryVariable(ctx, repository, strings.TrimSpace(cfg.Variable), want); err != nil {
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

func (c *Controller) budgetAvailable(pool model.PoolName, cfg *model.HostedOverflowConfig) (bool, string) {
	if len(cfg.BudgetRules) == 0 {
		return false, "no budget rules, private repositories stay self-hosted"
	}
	if c.budget == nil {
		return false, "tier routing is off, budget unknown"
	}
	decision, ok := c.budget.Decision(pool, model.HostedOverflowBudgetBackend)
	if !ok {
		return false, "budget not read yet"
	}
	if decision.Stale {
		return false, "budget reading is stale"
	}
	switch decision.State {
	case tier.StateHealthy, tier.StateApproaching:
		return true, "GitHub minutes " + decision.State
	default:
		reason := decision.Reason
		if reason == "" {
			reason = decision.State
		}
		return false, "GitHub minutes unavailable: " + reason
	}
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
