package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/config"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/scheduler"
)

// scriptedActivityBackend reports a fixed GitHub busy bit per allocation.
// A non-nil err fails the probe so the sweep must not treat runners as idle.
type scriptedActivityBackend struct {
	*testCleanupBackend
	busy map[string]bool
	err  error
}

func (b *scriptedActivityBackend) RunnerActivity(_ context.Context, allocations []model.AllocationStatus) (map[string]bool, error) {
	if b.err != nil {
		return nil, b.err
	}
	out := make(map[string]bool, len(allocations))
	for _, status := range allocations {
		if busy, ok := b.busy[status.ID]; ok {
			out[status.ID] = busy
		}
	}
	return out, nil
}

func newIdleService(t *testing.T, idle time.Duration, maxRunners int, impl backend.Backend) (*Service, *time.Time, *recordingObserver) {
	t.Helper()
	cfg := config.Default()
	cfg.Broker.IdleTimeout = idle
	cfg.Broker.RegistrationTimeout = time.Hour
	cfg.Broker.DefaultJobTimeout = time.Hour
	for i := range cfg.Pools {
		if cfg.Pools[i].Name != model.PoolFull {
			continue
		}
		for name, backendCfg := range cfg.Pools[i].Backends {
			if name == model.BackendARC {
				backendCfg.Enabled = true
				backendCfg.Healthy = true
				backendCfg.MaxRunners = maxRunners
				backendCfg.RunnerLabel = ""
			} else {
				backendCfg.Enabled = false
			}
			cfg.Pools[i].Backends[name] = backendCfg
		}
	}
	if impl == nil {
		impl = &testCleanupBackend{testBackend: testBackend{name: model.BackendARC}}
	}
	service := NewService(cfg, backend.NewRegistry(impl), nil)
	observer := &recordingObserver{}
	service.observer = observer
	now := time.Unix(1_700_000_000, 0).UTC()
	service.now = func() time.Time { return now }
	return service, &now, observer
}

func allocatePinned(t *testing.T, service *Service, pool model.PoolName, backendName model.BackendName, jobTimeout time.Duration) model.AllocationStatus {
	t.Helper()
	pinned := backendName
	allocation, err := service.Allocate(context.Background(), model.AllocationRequest{
		Pool:       pool,
		Backend:    &pinned,
		JobTimeout: jobTimeout,
	})
	if err != nil {
		t.Fatalf("allocate %s/%s: %v", pool, backendName, err)
	}
	return allocation
}

func registerAt(t *testing.T, service *Service, id string) model.AllocationStatus {
	t.Helper()
	status, ok, err := service.Register(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("register %s: ok=%v err=%v", id, ok, err)
	}
	return status
}

func lastIdleSnapshot(t *testing.T, observer *recordingObserver) []idleRunnerSnapshot {
	t.Helper()
	if len(observer.idleSnapshots) == 0 {
		t.Fatal("expected an idle-runner observation")
	}
	return observer.idleSnapshots[len(observer.idleSnapshots)-1]
}

func TestIdleTimeoutExpiresUnclaimedRunnerAndReleasesMaxRunners(t *testing.T) {
	cleanups := 0
	impl := &testCleanupBackend{testBackend: testBackend{name: model.BackendARC}, cleanupCount: &cleanups}
	service, clock, observer := newIdleService(t, 10*time.Minute, 1, impl)

	first := allocatePinned(t, service, model.PoolFull, model.BackendARC, time.Hour)
	registered := registerAt(t, service, first.ID)
	if registered.ClaimedAt != nil {
		t.Fatal("register must not claim the job")
	}

	*clock = clock.Add(9 * time.Minute)
	if swept := service.SweepExpired(*clock); swept != 0 {
		t.Fatalf("swept %d before the idle TTL", swept)
	}
	held, ok := service.Get(first.ID)
	if !ok || held.State != model.StateReady {
		t.Fatalf("runner should still be ready, ok=%v state=%s", ok, held.State)
	}
	if held.IdleSince == nil || !held.IdleSince.Equal(*registered.RegisteredAt) {
		t.Fatalf("IdleSince=%v, want RegisteredAt %v", held.IdleSince, registered.RegisteredAt)
	}
	if service.store.CountActive(model.PoolFull, model.BackendARC) != 1 {
		t.Fatalf("active count=%d, want 1 while the idle runner is inside the TTL", service.store.CountActive(model.PoolFull, model.BackendARC))
	}
	if _, err := service.Allocate(context.Background(), model.AllocationRequest{Pool: model.PoolFull, JobTimeout: time.Hour}); !errors.Is(err, scheduler.ErrNoCapacity) {
		t.Fatalf("idle allocation must keep maxRunners full, err=%v", err)
	}
	snap := lastIdleSnapshot(t, observer)
	if len(snap) != 1 || snap[0].Pool != model.PoolFull || snap[0].Backend != model.BackendARC || snap[0].Count != 1 {
		t.Fatalf("idle snapshot=%+v", snap)
	}
	if snap[0].OldestSeconds != (9 * time.Minute).Seconds() {
		t.Fatalf("oldest idle seconds=%v, want 540", snap[0].OldestSeconds)
	}

	*clock = clock.Add(2 * time.Minute)
	if swept := service.SweepExpired(*clock); swept != 1 {
		t.Fatalf("swept %d, want 1 once the idle TTL elapses", swept)
	}
	expired, ok := service.Get(first.ID)
	if !ok || expired.State != model.StateExpired {
		t.Fatalf("expected expired, ok=%v state=%s", ok, expired.State)
	}
	if !strings.Contains(expired.Error, "runner took no job within the idle timeout") {
		t.Fatalf("error=%q", expired.Error)
	}
	if cleanups != 1 {
		t.Fatalf("cleanup calls=%d, want 1 (deregister and delete)", cleanups)
	}
	if service.store.CountActive(model.PoolFull, model.BackendARC) != 0 {
		t.Fatal("expired idle runner must stop counting toward maxRunners")
	}
	if service.registrationPenalized(model.PoolFull, model.BackendARC, *clock) {
		t.Fatal("idle expiry is not a backend failure and must not penalize the backend")
	}
	found := false
	for _, ev := range observer.expiredEvents {
		if ev == "full:arc:idle" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expired events=%v", observer.expiredEvents)
	}
	if snap := lastIdleSnapshot(t, observer); len(snap) != 0 {
		t.Fatalf("reaped runner still in idle gauge snapshot: %+v", snap)
	}

	second := allocatePinned(t, service, model.PoolFull, model.BackendARC, time.Hour)
	if second.ID == first.ID || second.SelectedBackend != model.BackendARC {
		t.Fatalf("cap should accept a new runner, got %+v", second)
	}
}

func TestIdleTimeoutDefaultIsTenMinutesWhenUnset(t *testing.T) {
	service, clock, _ := newIdleService(t, 0, 2, nil)
	allocation := allocatePinned(t, service, model.PoolFull, model.BackendARC, time.Hour)
	registerAt(t, service, allocation.ID)

	*clock = clock.Add(9 * time.Minute)
	if swept := service.SweepExpired(*clock); swept != 0 {
		t.Fatalf("default TTL should not expire at 9m, swept %d", swept)
	}
	*clock = clock.Add(2 * time.Minute)
	if swept := service.SweepExpired(*clock); swept != 1 {
		t.Fatalf("default TTL should expire at 11m, swept %d", swept)
	}
	updated, _ := service.Get(allocation.ID)
	if updated.State != model.StateExpired || !strings.Contains(updated.Error, "idle timeout") {
		t.Fatalf("state=%s error=%q", updated.State, updated.Error)
	}
}

func TestNegativeIdleTimeoutDisablesReaper(t *testing.T) {
	service, clock, observer := newIdleService(t, -time.Second, 1, nil)
	allocation := allocatePinned(t, service, model.PoolFull, model.BackendARC, time.Hour)
	registerAt(t, service, allocation.ID)

	*clock = clock.Add(30 * time.Minute)
	if swept := service.SweepExpired(*clock); swept != 0 {
		t.Fatalf("negative broker idleTimeout must disable the reaper, swept %d", swept)
	}
	if st, _ := service.Get(allocation.ID); st.State != model.StateReady || st.IdleSince != nil {
		t.Fatalf("state=%s idle_since=%v", st.State, st.IdleSince)
	}
	if service.store.CountActive(model.PoolFull, model.BackendARC) != 1 {
		t.Fatal("disabled idle reaper must leave the cap held until job timeout")
	}
	if snap := lastIdleSnapshot(t, observer); len(snap) != 0 {
		t.Fatalf("disabled reaper published idle runners: %+v", snap)
	}
}

func TestBackendNegativeIdleTimeoutOverridesBroker(t *testing.T) {
	service, clock, _ := newIdleService(t, time.Minute, 1, nil)
	for i := range service.cfg.Pools {
		if service.cfg.Pools[i].Name != model.PoolFull {
			continue
		}
		backendCfg := service.cfg.Pools[i].Backends[model.BackendARC]
		backendCfg.IdleTimeout = -time.Second
		service.cfg.Pools[i].Backends[model.BackendARC] = backendCfg
	}
	allocation := allocatePinned(t, service, model.PoolFull, model.BackendARC, time.Hour)
	registerAt(t, service, allocation.ID)
	*clock = clock.Add(30 * time.Minute)
	if swept := service.SweepExpired(*clock); swept != 0 {
		t.Fatalf("backend idleTimeout -1s must disable reap despite broker TTL, swept %d", swept)
	}
	if st, _ := service.Get(allocation.ID); st.State != model.StateReady {
		t.Fatalf("state=%s", st.State)
	}
}

func TestClaimedRunnerWaitsForJobTimeout(t *testing.T) {
	service, clock, observer := newIdleService(t, 10*time.Minute, 1, nil)
	allocation := allocatePinned(t, service, model.PoolFull, model.BackendARC, 20*time.Minute)
	registerAt(t, service, allocation.ID)
	if _, ok, err := service.Claim(context.Background(), allocation.ID); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if st, _ := service.Get(allocation.ID); st.IdleSince != nil {
		t.Fatal("claim must clear IdleSince")
	}

	*clock = clock.Add(11 * time.Minute)
	if swept := service.SweepExpired(*clock); swept != 0 {
		t.Fatalf("claimed runner swept at idle TTL: %d", swept)
	}
	if service.store.CountActive(model.PoolFull, model.BackendARC) != 1 {
		t.Fatal("claimed work must keep its cap until job timeout")
	}

	*clock = clock.Add(10 * time.Minute)
	if swept := service.SweepExpired(*clock); swept != 1 {
		t.Fatalf("job timeout swept %d, want 1", swept)
	}
	found := false
	for _, ev := range observer.expiredEvents {
		if ev == "full:arc:job_timeout" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expired events=%v", observer.expiredEvents)
	}
}

func TestIdleTimeoutReleasesCrossPoolCap(t *testing.T) {
	cleanups := 0
	impl := &testCleanupBackend{testBackend: testBackend{name: model.BackendARC}, cleanupCount: &cleanups}
	cfg := model.BrokerConfig{
		Broker: model.BrokerRuntimeConfig{
			DefaultPool:         "pool-a",
			IdleTimeout:         10 * time.Minute,
			RegistrationTimeout: time.Hour,
			DefaultJobTimeout:   time.Hour,
		},
		Pools: []model.PoolConfig{
			{
				Name: "pool-a",
				Backends: map[model.BackendName]model.BackendConfig{
					model.BackendARC: {Enabled: true, Healthy: true, MaxRunners: 2, CrossPoolMaxRunners: 1},
				},
			},
			{
				Name: "pool-b",
				Backends: map[model.BackendName]model.BackendConfig{
					model.BackendARC: {Enabled: true, Healthy: true, MaxRunners: 2, CrossPoolMaxRunners: 1},
				},
			},
		},
	}
	service := NewService(cfg, backend.NewRegistry(impl), nil)
	now := time.Unix(1_700_000_000, 0).UTC()
	service.now = func() time.Time { return now }

	first := allocatePinned(t, service, "pool-a", model.BackendARC, time.Hour)
	registerAt(t, service, first.ID)
	if _, err := service.Allocate(context.Background(), model.AllocationRequest{Pool: "pool-b", JobTimeout: time.Hour}); !errors.Is(err, scheduler.ErrNoCapacity) {
		t.Fatalf("idle runner must hold crossPoolMaxRunners, err=%v", err)
	}
	if service.store.CountBackendActive(model.BackendARC) != 1 {
		t.Fatalf("cross-pool active=%d", service.store.CountBackendActive(model.BackendARC))
	}

	now = now.Add(11 * time.Minute)
	if swept := service.SweepExpired(now); swept != 1 {
		t.Fatalf("swept %d, want 1", swept)
	}
	if service.store.CountBackendActive(model.BackendARC) != 0 || service.store.CountActive("pool-a", model.BackendARC) != 0 {
		t.Fatal("idle expiry must drop the runner from pool and cross-pool caps")
	}
	if cleanups != 1 {
		t.Fatalf("cleanup calls=%d, want 1", cleanups)
	}
	second := allocatePinned(t, service, "pool-b", model.BackendARC, time.Hour)
	if second.Pool != "pool-b" {
		t.Fatalf("pool-b did not get the freed cap: %+v", second)
	}
}

func TestBusyFalseReapsRunnerRunningRegistration(t *testing.T) {
	cleanups := 0
	activity := &scriptedActivityBackend{
		testCleanupBackend: &testCleanupBackend{testBackend: testBackend{name: model.BackendARC}, cleanupCount: &cleanups},
		busy:               map[string]bool{},
	}
	service, clock, _ := newIdleService(t, 10*time.Minute, 1, activity)
	allocation := allocatePinned(t, service, model.PoolFull, model.BackendARC, time.Hour)
	registerAt(t, service, allocation.ID)
	registeredAt := clock.Add(-2 * time.Hour)
	status, _ := service.Get(allocation.ID)
	status.RegisteredAt = &registeredAt
	status.Metadata[registeredViaMetadataKey] = registeredViaRunnerRunning
	if err := service.store.Save(status); err != nil {
		t.Fatal(err)
	}
	activity.busy[allocation.ID] = false

	if swept := service.SweepExpired(*clock); swept != 1 {
		t.Fatalf("busy=false runner already past the TTL swept %d, want 1", swept)
	}
	if st, _ := service.Get(allocation.ID); st.State != model.StateExpired || !strings.Contains(st.Error, "idle timeout") {
		t.Fatalf("state=%s error=%q", st.State, st.Error)
	}
	if cleanups != 1 {
		t.Fatalf("cleanup calls=%d, want 1", cleanups)
	}
	if service.store.CountActive(model.PoolFull, model.BackendARC) != 0 {
		t.Fatal("reaped runner still counts toward the cap")
	}
}

func TestBusyTrueClaimsRunnerInsteadOfReaping(t *testing.T) {
	cleanups := 0
	activity := &scriptedActivityBackend{
		testCleanupBackend: &testCleanupBackend{testBackend: testBackend{name: model.BackendARC}, cleanupCount: &cleanups},
		busy:               map[string]bool{},
	}
	service, clock, observer := newIdleService(t, 10*time.Minute, 1, activity)
	allocation := allocatePinned(t, service, model.PoolFull, model.BackendARC, 20*time.Minute)
	registerAt(t, service, allocation.ID)
	registeredAt := clock.Add(-2 * time.Hour)
	status, _ := service.Get(allocation.ID)
	status.RegisteredAt = &registeredAt
	status.Metadata[registeredViaMetadataKey] = registeredViaRunnerRunning
	if err := service.store.Save(status); err != nil {
		t.Fatal(err)
	}
	activity.busy[allocation.ID] = true

	if swept := service.SweepExpired(*clock); swept != 0 {
		t.Fatalf("busy runner swept %d, want 0", swept)
	}
	claimed, _ := service.Get(allocation.ID)
	if claimed.State != model.StateReady || claimed.ClaimedAt == nil || claimed.IdleSince != nil {
		t.Fatalf("busy runner should be claimed and still ready: %+v", claimed)
	}
	if cleanups != 0 {
		t.Fatalf("busy runner cleanup calls=%d", cleanups)
	}

	*clock = clock.Add(21 * time.Minute)
	if swept := service.SweepExpired(*clock); swept != 1 {
		t.Fatalf("job timeout swept %d, want 1", swept)
	}
	found := false
	for _, ev := range observer.expiredEvents {
		if ev == "full:arc:job_timeout" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expired events=%v", observer.expiredEvents)
	}
}

func TestRunnerActivityErrorDoesNotReap(t *testing.T) {
	activity := &scriptedActivityBackend{
		testCleanupBackend: &testCleanupBackend{testBackend: testBackend{name: model.BackendARC}},
		err:                errors.New("github down"),
	}
	service, clock, _ := newIdleService(t, 10*time.Minute, 1, activity)
	allocation := allocatePinned(t, service, model.PoolFull, model.BackendARC, time.Hour)
	registerAt(t, service, allocation.ID)
	registeredAt := clock.Add(-2 * time.Hour)
	status, _ := service.Get(allocation.ID)
	status.RegisteredAt = &registeredAt
	status.Metadata[registeredViaMetadataKey] = registeredViaRunnerRunning
	if err := service.store.Save(status); err != nil {
		t.Fatal(err)
	}

	*clock = clock.Add(time.Minute)
	if swept := service.SweepExpired(*clock); swept != 0 {
		t.Fatalf("activity error swept %d, want 0", swept)
	}
	if st, _ := service.Get(allocation.ID); st.State != model.StateReady || st.IdleSince != nil {
		t.Fatalf("state=%s idle_since=%v", st.State, st.IdleSince)
	}
	if service.store.CountActive(model.PoolFull, model.BackendARC) != 1 {
		t.Fatal("unknown activity must keep the cap")
	}
}

func TestRunnerRunningWithoutActivityProbeIsNotIdle(t *testing.T) {
	service, clock, _ := newIdleService(t, 10*time.Minute, 1, nil)
	allocation := allocatePinned(t, service, model.PoolFull, model.BackendARC, time.Hour)
	registerAt(t, service, allocation.ID)
	registeredAt := clock.Add(-2 * time.Hour)
	status, _ := service.Get(allocation.ID)
	status.RegisteredAt = &registeredAt
	status.Metadata[registeredViaMetadataKey] = registeredViaRunnerRunning
	if err := service.store.Save(status); err != nil {
		t.Fatal(err)
	}

	*clock = clock.Add(11 * time.Minute)
	if swept := service.SweepExpired(*clock); swept != 0 {
		t.Fatalf("runner_running without busy=false swept %d, want 0", swept)
	}
	if st, _ := service.Get(allocation.ID); st.State != model.StateReady {
		t.Fatalf("state=%s", st.State)
	}
}

func TestStaticLabelIgnoresIdleTimeout(t *testing.T) {
	cfg := config.Default()
	cfg.Broker.IdleTimeout = time.Minute
	cfg.Broker.RegistrationTimeout = time.Hour
	for i := range cfg.Pools {
		if cfg.Pools[i].Name != model.PoolLite {
			continue
		}
		backendCfg := cfg.Pools[i].Backends[model.BackendAzureVM]
		backendCfg.Enabled = true
		backendCfg.Healthy = true
		backendCfg.MaxRunners = 1
		backendCfg.RunnerLabel = "az-vm-static"
		cfg.Pools[i].Backends[model.BackendAzureVM] = backendCfg
	}
	service := NewService(cfg, backend.NewRegistry(testBackend{name: model.BackendAzureVM}), nil)
	now := time.Unix(1_700_000_000, 0).UTC()
	service.now = func() time.Time { return now }

	allocation := allocatePinned(t, service, model.PoolLite, model.BackendAzureVM, 2*time.Hour)
	if allocation.RegisteredAt == nil {
		t.Fatal("static label should be registered at allocate")
	}
	now = now.Add(30 * time.Minute)
	if swept := service.SweepExpired(now); swept != 0 {
		t.Fatalf("static runner swept by idle TTL: %d", swept)
	}
	if st, _ := service.Get(allocation.ID); st.State != model.StateReady {
		t.Fatalf("state=%s error=%q", st.State, st.Error)
	}
}

func TestWarmRunnerIgnoresIdleTimeout(t *testing.T) {
	service, clock, _ := newIdleService(t, time.Minute, 2, nil)
	registeredAt := clock.Add(-time.Hour)
	if err := service.store.Save(model.AllocationStatus{
		ID:              "warm-1",
		Pool:            model.PoolFull,
		SelectedBackend: model.BackendARC,
		State:           model.StateWarm,
		CreatedAt:       registeredAt,
		RegisteredAt:    &registeredAt,
		ExpiresAt:       clock.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	*clock = clock.Add(30 * time.Minute)
	if swept := service.SweepExpired(*clock); swept != 0 {
		t.Fatalf("warm runner swept %d", swept)
	}
	if st, _ := service.Get("warm-1"); st.State != model.StateWarm {
		t.Fatalf("state=%s", st.State)
	}
}

func TestHTTPIdleTTLFreesCapacityAndExportsMetrics(t *testing.T) {
	cleanups := 0
	impl := &testCleanupBackend{testBackend: testBackend{name: model.BackendARC}, cleanupCount: &cleanups}
	service, clock, _ := newIdleService(t, 10*time.Minute, 1, impl)
	server := newTestServer(t, service)
	handler := server.Handler()

	allocation := allocatePinned(t, service, model.PoolFull, model.BackendARC, time.Hour)
	regReq := httptest.NewRequest(http.MethodPost, "/v1/allocations/"+allocation.ID+"/register", nil)
	regRec := httptest.NewRecorder()
	handler.ServeHTTP(regRec, regReq)
	if regRec.Code != http.StatusOK {
		t.Fatalf("register status=%d body=%s", regRec.Code, regRec.Body.String())
	}

	*clock = clock.Add(time.Minute)
	if swept := service.SweepExpired(*clock); swept != 0 {
		t.Fatalf("swept %d inside TTL", swept)
	}
	metrics := scrapeTestMetrics(t, handler)
	if !strings.Contains(metrics, `uecb_idle_allocated_runners{backend="arc",pool="full"} 1`) {
		t.Fatalf("metrics missing idle gauge:\n%s", metrics)
	}
	if !strings.Contains(metrics, `uecb_idle_allocated_runner_oldest_seconds{backend="arc",pool="full"} 60`) {
		t.Fatalf("metrics missing idle age:\n%s", metrics)
	}

	blocked := httptest.NewRequest(http.MethodPost, "/v1/allocations", strings.NewReader(`{"pool":"full","job_timeout":"1h"}`))
	blocked.Header.Set("Content-Type", "application/json")
	blockedRec := httptest.NewRecorder()
	handler.ServeHTTP(blockedRec, blocked)
	if blockedRec.Code == http.StatusCreated {
		t.Fatalf("second allocate succeeded while the idle runner held the only slot: %s", blockedRec.Body.String())
	}

	*clock = clock.Add(10 * time.Minute)
	if swept := service.SweepExpired(*clock); swept != 1 {
		t.Fatalf("swept %d, want 1", swept)
	}
	if cleanups != 1 {
		t.Fatalf("cleanup calls=%d, want 1", cleanups)
	}
	after := scrapeTestMetrics(t, handler)
	if !strings.Contains(after, `uecb_allocation_expired_total{backend="arc",pool="full",reason="idle"} 1`) {
		t.Fatalf("metrics missing idle expiry:\n%s", after)
	}
	if strings.Contains(after, "uecb_idle_allocated_runners{") {
		t.Fatalf("idle gauge still published after reap:\n%s", after)
	}
	if service.store.CountActive(model.PoolFull, model.BackendARC) != 0 {
		t.Fatal("idle runner still counts after expiry")
	}

	again := httptest.NewRequest(http.MethodPost, "/v1/allocations", strings.NewReader(`{"pool":"full","job_timeout":"1h"}`))
	again.Header.Set("Content-Type", "application/json")
	againRec := httptest.NewRecorder()
	handler.ServeHTTP(againRec, again)
	if againRec.Code != http.StatusCreated {
		t.Fatalf("allocate after idle reap = %d: %s", againRec.Code, againRec.Body.String())
	}
}

func scrapeTestMetrics(t *testing.T, handler http.Handler) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics = %d", rec.Code)
	}
	return rec.Body.String()
}
