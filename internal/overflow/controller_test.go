package overflow

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	ghclient "github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/github"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/tier"
)

type fakeGitHub struct {
	private      map[string]bool
	visErr       error
	setErr       error
	sets         []string
	vars         map[string]string
	visCalls     int
	billingGross float64
	billingNet   float64
	billingErr   error
	billingCalls int
	hostedMins   map[string]float64
	hostedErr    error
	hostedCalls  int
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

func (f *fakeGitHub) GetActionsBillingUsage(_ context.Context, _ string, _, _ int, _ string) (float64, float64, error) {
	f.billingCalls++
	if f.billingErr != nil {
		return 0, 0, f.billingErr
	}
	return f.billingGross, f.billingNet, nil
}

func (f *fakeGitHub) MonthlyHostedMinutes(_ context.Context, repo string, _, _ time.Time) (float64, error) {
	f.hostedCalls++
	if f.hostedErr != nil {
		return 0, f.hostedErr
	}
	return f.hostedMins[repo], nil
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

func TestPrivateRepoStaysSelfHostedWhenPrometheusExceeded(t *testing.T) {
	c, gh, allocs := newController(t, true, &fakeBudget{decision: tier.Decision{State: tier.StateExceeded, Reason: "minutes used"}, ok: true})
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
}

func TestFallbackOrderAndWorstCaseFailOpen(t *testing.T) {
	// No Prometheus data -> Tier 2 (Billing API) -> Tier 3 (GitHub App job estimate) -> Fail open.
	c, gh, allocs := newController(t, true, &fakeBudget{})
	c.getenv = func(string) string { return "pat-token" }
	gh.billingErr = errors.New("billing api down")
	gh.hostedMins = map[string]float64{"o/private-repo": 100}
	*allocs = fakeAllocs{waitingAllocation("shared", model.StatePending, t0.Add(-3*time.Minute))}

	if err := c.Reconcile(context.Background(), t0); err != nil {
		t.Fatal(err)
	}
	if gh.billingCalls != 1 || gh.hostedCalls != 1 {
		t.Fatalf("expected Tier 2 then Tier 3 calls, got billing=%d hosted=%d", gh.billingCalls, gh.hostedCalls)
	}
	if value(gh, "o/private-repo") != "ubuntu-latest" {
		t.Fatalf("expected private repo to overflow on Tier 3 under limit, got %v", gh.vars)
	}

	// Worst case: all three sources fail -> still fail open.
	c2, gh2, allocs2 := newController(t, false, nil)
	c2.getenv = func(string) string { return "pat-token" }
	gh2.billingErr = errors.New("billing api down")
	gh2.hostedErr = errors.New("runs api down")
	*allocs2 = fakeAllocs{waitingAllocation("shared", model.StatePending, t0.Add(-3*time.Minute))}

	if err := c2.Reconcile(context.Background(), t0); err != nil {
		t.Fatal(err)
	}
	if value(gh2, "o/private-repo") != "ubuntu-latest" {
		t.Fatalf("expected worst-case fail open, got %v", gh2.vars)
	}
}

func TestBillingErrorLatchesUntilNextMonthReset(t *testing.T) {
	c, gh, allocs := newController(t, true, &fakeBudget{})
	c.getenv = func(string) string { return "pat-token" }
	gh.billingErr = &ghclient.StatusError{Code: http.StatusPaymentRequired, Message: "spending limit reached"}
	*allocs = fakeAllocs{waitingAllocation("shared", model.StatePending, t0.Add(-3*time.Minute))}

	if err := c.Reconcile(context.Background(), t0); err != nil {
		t.Fatal(err)
	}
	if value(gh, "o/private-repo") != "uecb-shared" {
		t.Fatalf("expected private repo blocked on billing error, got %v", gh.vars)
	}
	if value(gh, "o/public-repo") != "ubuntu-latest" {
		t.Fatalf("expected public repo still hosted, got %v", gh.vars)
	}

	// Still in November: latch holds without re-querying billing API.
	gh.billingErr = nil
	gh.billingGross = 0
	laterSameMonth := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	*allocs = fakeAllocs{waitingAllocation("shared", model.StatePending, laterSameMonth.Add(-3*time.Minute))}
	if err := c.Reconcile(context.Background(), laterSameMonth); err != nil {
		t.Fatal(err)
	}
	if value(gh, "o/private-repo") != "uecb-shared" {
		t.Fatalf("expected latch to hold until month reset, got %v", gh.vars)
	}
	if gh.billingCalls != 1 {
		t.Fatalf("expected latch to skip billing API until month reset, calls=%d", gh.billingCalls)
	}

	// December 1st UTC: month resets, latch clears and private repo can overflow again.
	nextMonth := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	*allocs = fakeAllocs{waitingAllocation("shared", model.StatePending, nextMonth.Add(-3*time.Minute))}
	if err := c.Reconcile(context.Background(), nextMonth); err != nil {
		t.Fatal(err)
	}
	if value(gh, "o/private-repo") != "ubuntu-latest" {
		t.Fatalf("expected private repo allowed after month reset, got %v", gh.vars)
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

func TestSelfHostedOnlyRestoresSelectedRepositoryUnderBacklog(t *testing.T) {
	cfg := testConfig(false)
	cfg.Pools[0].HostedOverflow.SelfHostedOnlyRepositories = []string{" O/PRIVATE-REPO "}
	gh := &fakeGitHub{private: map[string]bool{"o/private-repo": true}, vars: map[string]string{"o/private-repo/UECB_SHARED_RUNS_ON": "ubuntu-latest"}}
	allocs := fakeAllocs{waitingAllocation("shared", model.StatePending, t0.Add(-3*time.Minute))}
	c := New(cfg, gh, &allocs, nil)
	if err := c.Reconcile(context.Background(), t0); err != nil {
		t.Fatal(err)
	}
	if value(gh, "o/private-repo") != "uecb-shared" || value(gh, "o/public-repo") != "ubuntu-latest" {
		t.Fatalf("vars = %v", gh.vars)
	}
	if gh.visCalls != 1 || gh.billingCalls != 0 || gh.hostedCalls != 0 {
		t.Fatalf("unexpected lookups: %+v", gh)
	}
	if err := c.Reconcile(context.Background(), t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(gh.sets) != 2 {
		t.Fatalf("variables rewritten: %v", gh.sets)
	}
}

func TestSelfHostedOnlyRetriesFailedRestoreWithoutVisibility(t *testing.T) {
	cfg := testConfig(false)
	cfg.Pools[0].HostedOverflow.Repositories = []string{"o/public-repo"}
	cfg.Pools[0].HostedOverflow.SelfHostedOnlyRepositories = []string{"o/public-repo"}
	gh := &fakeGitHub{visErr: errors.New("visibility unavailable"), setErr: errors.New("write failed")}
	allocs := fakeAllocs{waitingAllocation("shared", model.StatePending, t0.Add(-3*time.Minute))}
	c := New(cfg, gh, &allocs, nil)
	if err := c.Reconcile(context.Background(), t0); err == nil {
		t.Fatal("want restore error")
	}
	gh.setErr = nil
	if err := c.Reconcile(context.Background(), t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if value(gh, "o/public-repo") != "uecb-shared" || gh.visCalls != 0 {
		t.Fatalf("restore failed: %+v", gh)
	}
}
