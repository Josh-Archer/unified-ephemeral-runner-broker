package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/config"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/scheduler"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/store"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/tier"
)

func TestPressureLimitsScaleSharedAdmissionAndRecoverWithHysteresis(t *testing.T) {
	ctx := context.Background()
	const name model.BackendName = "kubernetes-local"
	var iowait atomic.Int64
	var requests atomic.Int64
	prometheus := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requests.Add(1)
		if req.URL.Query().Get("query") != "node_iowait" {
			http.Error(w, "unexpected query", http.StatusBadRequest)
			return
		}
		sample := iowait.Load()
		if sample < 0 {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintf(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1234,"%g"]}]}}`, float64(sample)/1000)
	}))
	t.Cleanup(prometheus.Close)

	cfg := config.Default()
	cfg.Broker.StateStore = model.StateStoreConfig{Type: "memory"}
	cfg.Broker.TierRouting = model.TierRoutingConfig{
		Enabled:    true,
		Prometheus: model.TierPrometheusConfig{URL: prometheus.URL},
		RunnerLimits: map[model.BackendName]model.TierRunnerLimitConfig{
			name: {
				UsageQuery: "node_iowait",
				Steps: []model.TierRunnerLimitStep{
					{At: .20, RecoverBelow: .15, MaxRunners: 4},
					{At: .30, RecoverBelow: .25, MaxRunners: 3},
					{At: .40, RecoverBelow: .35, MaxRunners: 2},
					{At: .50, RecoverBelow: .45, MaxRunners: 1},
				},
			},
		},
	}
	local := model.BackendConfig{Driver: "kubernetes", Enabled: true, Healthy: true, MaxRunners: 5, CrossPoolMaxRunners: 5}
	cfg.Pools = []model.PoolConfig{
		{Name: model.PoolFull, Backends: map[model.BackendName]model.BackendConfig{name: local}},
		{Name: model.PoolLite, Backends: map[model.BackendName]model.BackendConfig{name: local}},
	}
	service := NewService(cfg, backend.NewRegistry(testBackend{name: name}), nil)
	refresher := tier.NewConfigRefresher(cfg, nil)
	refresh := func(sample int64, want int) {
		t.Helper()
		iowait.Store(sample)
		before := requests.Load()
		decisions, err := refresher.Refresh(ctx)
		if sample < 0 {
			if err == nil || len(decisions) != 0 {
				t.Fatalf("missing sample: decisions = %+v, err = %v", decisions, err)
			}
		} else {
			if err != nil || len(decisions) != 1 || decisions[0].Pool != "" {
				t.Fatalf("sample %d: decisions = %+v, err = %v", sample, decisions, err)
			}
			service.tierMgr.SetDecision(decisions[0])
		}
		if requests.Load()-before != 1 || service.pressureRunnerLimit(name) != want {
			t.Fatalf("sample %d: pressure limit = %d, want %d with one query across both pools", sample, service.pressureRunnerLimit(name), want)
		}
	}
	allocate := func(pool model.PoolName, pin *model.BackendName) model.AllocationStatus {
		t.Helper()
		status, err := service.Allocate(ctx, model.AllocationRequest{Pool: pool, Backend: pin, JobTimeout: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		return status
	}
	complete := func(status model.AllocationStatus) {
		t.Helper()
		if _, ok, err := service.Complete(ctx, status.ID, completionRequest{State: "completed"}); err != nil || !ok {
			t.Fatalf("complete %s: ok = %v, err = %v", status.ID, ok, err)
		}
	}

	refresh(46, 0)
	first := allocate(model.PoolFull, nil)
	second := allocate(model.PoolLite, backendNamePtr(name))
	third := allocate(model.PoolFull, nil)
	fourth := allocate(model.PoolLite, nil)
	fifth := allocate(model.PoolFull, nil)

	refresh(205, 4)
	refresh(198, 4)
	refresh(320, 3)
	refresh(410, 2)
	refresh(656, 1)
	refresh(-1, 1)

	if active := service.store.CountBackendActive(name); active != 5 {
		t.Fatalf("pressure preempted runners: %d still active, want 5", active)
	}
	if _, err := service.Allocate(ctx, model.AllocationRequest{Pool: model.PoolLite, Backend: backendNamePtr(name)}); !errors.Is(err, scheduler.ErrNoCapacity) {
		t.Fatalf("pinned request at shared cap: %v", err)
	}
	if _, err := service.Allocate(ctx, model.AllocationRequest{Pool: model.PoolLite}); !errors.Is(err, scheduler.ErrNoCapacity) {
		t.Fatalf("automatic request at shared cap: %v", err)
	}
	if caps := service.noCapacityCaps(model.PoolLite); !strings.Contains(caps, "pressureLimit(5/1)") {
		t.Fatalf("capacity diagnostics = %q", caps)
	}
	if snapshot := service.BuildCapacitySnapshot(cfg.Pools[1], model.AllocationRequest{})[string(name)]; snapshot.AvailableSlots != 0 || snapshot.Reason != "pressureLimit(5/1)" {
		t.Fatalf("capacity snapshot = %+v", snapshot)
	}

	for _, status := range []model.AllocationStatus{first, second, third, fourth, fifth} {
		complete(status)
	}
	if snapshot := service.BuildCapacitySnapshot(cfg.Pools[1], model.AllocationRequest{})[string(name)]; snapshot.AvailableSlots != 1 {
		t.Fatalf("pressure floor capacity = %+v, want one available slot", snapshot)
	}
	allocate(model.PoolLite, nil)
	// Check the transactional cap directly as well. It must hold if two
	// requests pass the preliminary filter before either saves a reservation.
	err := service.saveReservedAllocation(model.AllocationStatus{
		ID: "racing-reservation", Pool: model.PoolFull, SelectedBackend: name, State: model.StateReserved,
	}, cfg.Pools[0])
	if !errors.Is(err, store.ErrNoCapacity) {
		t.Fatalf("transactional shared cap: %v", err)
	}

	refresh(449, 2)
	refresh(340, 3)
	refresh(240, 4)
	refresh(150, 4)
	refresh(149, 0)
	for range 4 {
		allocate(model.PoolFull, nil)
	}
	if active := service.store.CountBackendActive(name); active != 5 {
		t.Fatalf("recovered capacity = %d, want 5", active)
	}
}

func TestPressureLimitRecyclesOnlyUnusedWarmAllocations(t *testing.T) {
	const name model.BackendName = "kubernetes-local"
	cfg := config.Default()
	cfg.Broker.StateStore = model.StateStoreConfig{Type: "memory"}
	cfg.Broker.TierRouting.Enabled = true
	cfg.Pools = []model.PoolConfig{
		{Name: model.PoolFull, Backends: map[model.BackendName]model.BackendConfig{
			name: {Driver: "kubernetes", Enabled: true, Healthy: true, MaxRunners: 5, CrossPoolMaxRunners: 5, WarmMin: 1, WarmMax: 1},
		}},
	}
	service := NewService(cfg, backend.NewRegistry(testBackend{name: name}), nil)
	service.ReconcileWarmPools()
	warm := service.store.List()
	if len(warm) != 1 || warm[0].State != model.StateWarm {
		t.Fatalf("expected one warmed runner, got %+v", warm)
	}
	ready, err := service.Allocate(context.Background(), model.AllocationRequest{Pool: model.PoolFull, JobTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	service.ReconcileWarmPools()
	service.tierMgr.SetDecision(tier.Decision{Backend: name, RunnerLimit: 1, UpdatedAt: time.Now()})
	// Simulate a stale warm snapshot after this runner was assigned to a job.
	service.recycleWarmAllocation(context.Background(), cfg.Pools[0], warm[0], "stale warm snapshot")
	service.ReconcileWarmPools()
	service.ReconcileWarmPools()

	if got, ok := service.Get(ready.ID); !ok || got.State != model.StateReady {
		t.Fatalf("claimed runner was affected: %+v", got)
	}
	if active := service.store.CountBackendActive(name); active != 1 {
		t.Fatalf("expected only the ready runner, active = %d", active)
	}
}
