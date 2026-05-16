package tier

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

type staticSecretReader map[string]map[string]string

func (s staticSecretReader) ReadSecret(_ context.Context, name string) (map[string]string, error) {
	return s[name], nil
}

func TestConfigRefresherCombinesPrometheusUsageWithProviderLimit(t *testing.T) {
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	providerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"limit":100,"used":0,"updated_at":"` + now.Format(time.RFC3339) + `"}`))
	}))
	defer providerServer.Close()
	prometheusServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer prom-token" {
			t.Fatalf("unexpected prometheus auth header: %q", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"scalar","result":[1715097600,"96"]}}`))
	}))
	defer prometheusServer.Close()

	refresher := NewConfigRefresher(model.BrokerConfig{
		Broker: model.BrokerRuntimeConfig{
			TierRouting: model.TierRoutingConfig{
				Enabled: true,
				Prometheus: model.TierPrometheusConfig{
					URL:       prometheusServer.URL,
					SecretRef: "prom",
				},
				Providers: map[string]model.TierProviderConfig{
					"aws-main": {
						Provider:  "aws",
						Mode:      "free-tier",
						SecretRef: "aws",
					},
				},
			},
		},
		Pools: []model.PoolConfig{{
			Name: model.PoolLite,
			Backends: map[model.BackendName]model.BackendConfig{
				model.BackendCodeBuild: {
					TierRules: []model.TierRuleConfig{{
						Name:           "codebuild-free-tier",
						ProviderRef:    "aws-main",
						UsageQuery:     "uecb:usage",
						HardLimitRatio: 0.95,
						Action:         ActionDisable,
					}},
				},
			},
		}},
	}, staticSecretReader{
		"aws":  {"snapshot_url": providerServer.URL, "token": "aws-token"},
		"prom": {"bearer_token": "prom-token"},
	})
	refresher.now = func() time.Time { return now }

	decisions, err := refresher.Refresh(context.Background())
	if err != nil {
		t.Fatalf("refresh returned error: %v", err)
	}
	if len(decisions) != 1 {
		t.Fatalf("expected one decision, got %d", len(decisions))
	}
	if decisions[0].State != StateExceeded {
		t.Fatalf("expected exceeded decision, got %+v", decisions[0])
	}
}
