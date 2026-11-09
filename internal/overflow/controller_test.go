package overflow

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/tier"
)

type fakeGitHub struct {
	private  map[string]bool
	visErr   error
	setErr   error
	sets     []string
	vars     map[string]string
	visCalls int
}

func (f *fakeGitHub) SetRepositoryVariable(_ context.Context, repo, name, value string) error {
	if f.setErr != nil {
		return f.setErr
	}
	if f.vars == nil {
		f.vars = map[string]string{}
	}
	f.vars[repo+"/"+name] = value
	f.sets = append(f.sets, repo+"="+value)
	return nil
}

func (f *fakeGitHub) RepositoryIsPrivate(_ context.Context, repo string) (bool, error) {
	f.visCalls++
	if f.visErr != nil {
		return false, f.visErr
	}
	return f.private[repo], nil
}

type fakeAllocs []model.AllocationStatus

func (f *fakeAllocs) List() []model.AllocationStatus { return *f }

type fakeBudget struct {
	decision tier.Decision
	ok       bool
}

func (f *fakeBudget) Decision(model.PoolName, model.BackendName) (tier.Decision, bool) {
	return f.decision, f.ok
}

var t0 = time.Date(2026, 11, 9, 18, 0, 0, 0, time.UTC)

func testConfig(budgetRules bool) model.BrokerConfig {
	overflow := &model.HostedOverflowConfig{
		Enabled:         true,
		Variable:        "UECB_SHARED_RUNS_ON",
		SelfHostedValue: "uecb-shared",
		HostedValue:     "ubuntu-latest",
		Repositories:    []string{"o/private-repo", "o/public-repo"},
		SaturateAfter:   2 * time.Minute,
		RecoverAfter:    5 * time.Minute,
	}
	if budgetRules {
		overflow.BudgetRules = []model.TierRuleConfig{{Name: "github-actions-minutes", UsageQuery: "x", HardLimitRatio: 1}}
	}
	return model.BrokerConfig{Pools: []model.PoolConfig{
		{Name: "shared", HostedOverflow: overflow},
		{Name: "other"},
	}}
}

func waitingAllocation(pool model.PoolName, state model.AllocationState, created time.Time) model.AllocationStatus {
	return model.AllocationStatus{ID: string(pool) + created.String(), Pool: pool, State: state, CreatedAt: created}
}

func newController(t *testing.T, budgetRules bool, budget Budget) (*Controller, *fakeGitHub, *fakeAllocs) {
	t.Helper()
	gh := &fakeGitHub{private: map[string]bool{"o/private-repo": true}}
	allocs := &fakeAllocs{}
	c := New(testConfig(budgetRules), gh, allocs, budget)
	if c == nil {
		t.Fatal("controller is nil")
	}
	c.logf = func(string, ...any) {}
	return c, gh, allocs
}

func value(gh *fakeGitHub, repo string) string { return gh.vars[repo+"/UECB_SHARED_RUNS_ON"] }

func TestNewIsNilWithoutEnabledPools(t *testing.T) {
	if New(model.BrokerConfig{Pools: []model.PoolConfig{{Name: "x"}}}, nil, nil, nil) != nil {
		t.Fatal("want nil controller")
	}
}

func TestKeepsSelfHostedAndDoesNotRewrite(t *testing.T) {
	c, gh, allocs := newController(t, true, &fakeBudget{decision: tier.Decision{State: tier.StateHealthy}, ok: true})
	// A young wait is normal start-up, not a backlog.
	*allocs = fakeAllocs{waitingAllocation("shared", model.StateReserved, t0.Add(-time.Minute))}
	if err := c.Reconcile(context.Background(), t0); err != nil {
		t.Fatal(err)
	}
	if value(gh, "o/private-repo") != "uecb-shared" || value(gh, "o/public-repo") != "uecb-shared" {
		t.Fatalf("vars = %v", gh.vars)
	}
	if err := c.Reconcile(context.Background(), t0.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if len(gh.sets) != 2 {
		t.Fatalf("variables rewritten: %v", gh.sets)
	}
	if gh.visCalls != 0 {
		t.Fatalf("visibility read while keeping up: %d", gh.visCalls)
	}
}

func TestBackedUpSendsPublicAndBudgetedPrivateReposToHosted(t *testing.T) {
	budget := &fakeBudget{decision: tier.Decision{State: tier.StateApproaching}, ok: true}
	c, gh, allocs := newController(t, true, budget)
	*allocs = fakeAllocs{waitingAllocation("shared", model.StatePending, t0.Add(-3*time.Minute))}
	if err := c.Reconcile(context.Background(), t0); err != nil {
		t.Fatal(err)
	}
	if value(gh, "o/private-repo") != "ubuntu-latest" || value(gh, "o/public-repo") != "ubuntu-latest" {
		t.Fatalf("vars = %v", gh.vars)
	}
}

func TestPrivateRepoStaysSelfHostedWithoutBudget(t *testing.T) {
	cases := map[string]Budget{
		"exceeded": &fakeBudget{decision: tier.Decision{State: tier.StateExceeded, Reason: "minutes used"}, ok: true},
		"unknown":  &fakeBudget{decision: tier.Decision{State: tier.StateUnknown}, ok: true},
		"stale":    &fakeBudget{decision: tier.Decision{State: tier.StateHealthy, Stale: true}, ok: true},
		"no data":  &fakeBudget{},
		"no tier":  nil,
	}
	for name, budget := range cases {
		t.Run(name, func(t *testing.T) {
			c, gh, allocs := newController(t, true, budget)
			*allocs = fakeAllocs{waitingAllocation("shared", model.StateReserved, t0.Add(-10*time.Minute))}
			if err := c.Reconcile(context.Background(), t0); err != nil {
				t.Fatal(err)
			}
			if value(gh, "o/private-repo") != "uecb-shared" {
				t.Fatalf("private repo went hosted: %v", gh.vars)
			}
			if value(gh, "o/public-repo") != "ubuntu-latest" {
				t.Fatalf("public repo stayed local: %v", gh.vars)
			}
		})
	}
}

func TestNoBudgetRulesKeepsPrivateReposLocal(t *testing.T) {
	c, gh, allocs := newController(t, false, &fakeBudget{decision: tier.Decision{State: tier.StateHealthy}, ok: true})
	*allocs = fakeAllocs{waitingAllocation("shared", model.StatePending, t0.Add(-3*time.Minute))}
	if err := c.Reconcile(context.Background(), t0); err != nil {
		t.Fatal(err)
	}
	if value(gh, "o/private-repo") != "uecb-shared" {
		t.Fatalf("vars = %v", gh.vars)
	}
}

func TestRecoversAfterQuietPeriod(t *testing.T) {
	c, gh, allocs := newController(t, true, &fakeBudget{decision: tier.Decision{State: tier.StateHealthy}, ok: true})
	*allocs = fakeAllocs{waitingAllocation("shared", model.StatePending, t0.Add(-3*time.Minute))}
	_ = c.Reconcile(context.Background(), t0)
	*allocs = fakeAllocs{}
	_ = c.Reconcile(context.Background(), t0.Add(4*time.Minute))
	if value(gh, "o/public-repo") != "ubuntu-latest" {
		t.Fatal("switched back before recoverAfter")
	}
	_ = c.Reconcile(context.Background(), t0.Add(5*time.Minute))
	if value(gh, "o/public-repo") != "uecb-shared" || value(gh, "o/private-repo") != "uecb-shared" {
		t.Fatalf("did not switch back: %v", gh.vars)
	}
}

func TestVisibilityErrorFailsClosedAndRetriesWrites(t *testing.T) {
	c, gh, allocs := newController(t, true, &fakeBudget{decision: tier.Decision{State: tier.StateHealthy}, ok: true})
	gh.visErr = errors.New("boom")
	*allocs = fakeAllocs{waitingAllocation("shared", model.StatePending, t0.Add(-3*time.Minute))}
	if err := c.Reconcile(context.Background(), t0); err == nil {
		t.Fatal("want visibility error")
	}
	if value(gh, "o/public-repo") != "uecb-shared" {
		t.Fatalf("unknown visibility went hosted: %v", gh.vars)
	}

	gh.visErr = nil
	gh.setErr = errors.New("forbidden")
	if err := c.Reconcile(context.Background(), t0.Add(time.Second)); err == nil {
		t.Fatal("want write error")
	}
	gh.setErr = nil
	if err := c.Reconcile(context.Background(), t0.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if value(gh, "o/public-repo") != "ubuntu-latest" {
		t.Fatalf("failed write was not retried: %v", gh.vars)
	}
}

func TestWaitingIgnoresPickedUpFinishedAndOtherPools(t *testing.T) {
	old := t0.Add(-10 * time.Minute)
	registered := old.Add(time.Minute)
	allocations := []model.AllocationStatus{
		waitingAllocation("shared", model.StatePending, old),
		waitingAllocation("shared", model.StateReady, old),
		{Pool: "shared", State: model.StateReady, CreatedAt: old, RegisteredAt: &registered},
		{Pool: "shared", State: model.StateReserved, CreatedAt: old, ClaimedAt: &registered},
		waitingAllocation("shared", model.StateCompleted, old),
		waitingAllocation("shared", model.StatePending, t0.Add(-30*time.Second)),
		waitingAllocation("other", model.StatePending, old),
		{Pool: "shared", State: model.StatePending},
	}
	if got := Waiting(allocations, "shared", t0, 2*time.Minute); got != 2 {
		t.Fatalf("Waiting = %d, want 2", got)
	}
}
