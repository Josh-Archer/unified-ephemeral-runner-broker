package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/config"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/tier"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestTierRuleAppearsInCapacityAndMetrics(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("query") {
		case "rootfs":
			fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[{"value":[1,"0.824"]}]}}`)
		case "empty":
			fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
		default:
			t.Errorf("unexpected query")
		}
	}))
	defer server.Close()
	cfg := config.Default()
	cfg.Broker.StateStore = model.StateStoreConfig{Type: "memory"}
	cfg.Broker.TierRouting.Enabled = true
	cfg.Broker.TierRouting.Prometheus.URL = server.URL
	cfg.Pools[0].Backends = map[model.BackendName]model.BackendConfig{model.BackendARC: {
		Enabled: true, Healthy: true, MaxRunners: 2, Template: "arc-full",
		TierRules: []model.TierRuleConfig{
			{Name: "rootfs", UsageQuery: "rootfs", HardLimitRatio: 0.82, Action: tier.ActionDisable},
			{Name: "missing", UsageQuery: "empty", HardLimitRatio: 0.5, Action: tier.ActionDisable},
		},
	}}
	svc := NewService(cfg, backend.NewRegistry(testBackend{name: model.BackendARC}), nil)
	decisions, err := tier.NewConfigRefresher(cfg, nil).Refresh(context.Background())
	if err != nil || len(decisions) != 1 {
		t.Fatalf("refresh: %v %+v", err, decisions)
	}
	svc.tierMgr.SetDecision(decisions[0])
	want := "rootfs prometheus value=0.824 threshold=0.82"
	if got := svc.noCapacityCaps(cfg.Pools[0].Name); !strings.Contains(got, want) {
		t.Fatalf("caps: %s", got)
	}
	matrix := svc.BuildCapacitySnapshot(cfg.Pools[0], model.AllocationRequest{})
	if got := matrix[string(model.BackendARC)].Reason; !strings.Contains(got, want) {
		t.Fatalf("matrix: %s", got)
	}
	obs := NewPrometheusObserver(nil)
	svc.observer = obs
	svc.observeTierState()
	requireFloat(t, obs.tierRuleValue.WithLabelValues("arc", "rootfs"), 0.824)
	requireFloat(t, obs.tierRuleTripped.WithLabelValues("arc", "rootfs"), 1)
	if n := testutil.CollectAndCount(obs.tierRuleValue); n != 1 {
		t.Fatalf("expected only the measured rule, got %d", n)
	}
	got, _ := svc.tierMgr.Decision(cfg.Pools[0].Name, model.BackendARC)
	if len(got.Rules) != 2 || got.Rules[1].HasValue || !got.Rules[1].NoData {
		t.Fatalf("missing reading: %+v", got.Rules)
	}
}
