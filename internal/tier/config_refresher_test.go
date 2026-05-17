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

func TestConfigRefresherAppliesProviderRuleToMatchingBackends(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	providerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"limit":100,"used":96,"updated_at":"` + now.Format(time.RFC3339) + `"}`))
	}))
	defer providerServer.Close()

	refresher := NewConfigRefresher(model.BrokerConfig{
		Broker: model.BrokerRuntimeConfig{
			TierRouting: model.TierRoutingConfig{
				Enabled: true,
				Providers: map[string]model.TierProviderConfig{
					"aws-main": {
						Provider:  "aws",
						Mode:      "budget",
						SecretRef: "aws",
					},
				},
				ProviderRules: []model.ProviderTierRuleConfig{{
					Name:           "aws-budget",
					ProviderRef:    "aws-main",
					HardLimitRatio: 0.95,
					Action:         ActionDisable,
				}},
			},
		},
		Pools: []model.PoolConfig{{
			Name: model.PoolLite,
			Backends: map[model.BackendName]model.BackendConfig{
				model.BackendCodeBuild: {},
				model.BackendEC2: {
					Capabilities: []string{"cloud:aws"},
				},
				model.BackendAzureVM: {
					Capabilities: []string{"cloud:azure"},
				},
				model.BackendGCE: {
					Capabilities: []string{"cloud:gcp"},
				},
			},
		}},
	}, staticSecretReader{
		"aws": {"snapshot_url": providerServer.URL},
	})
	refresher.now = func() time.Time { return now }

	decisions, err := refresher.Refresh(context.Background())
	if err != nil {
		t.Fatalf("refresh returned error: %v", err)
	}
	got := decisionsByBackend(decisions)
	for _, backendName := range []model.BackendName{model.BackendCodeBuild, model.BackendEC2} {
		decision, ok := got[backendName]
		if !ok {
			t.Fatalf("expected decision for %s", backendName)
		}
		if decision.State != StateExceeded || decision.Action != ActionDisable {
			t.Fatalf("expected exceeded disable for %s, got %+v", backendName, decision)
		}
	}
	for _, backendName := range []model.BackendName{model.BackendAzureVM, model.BackendGCE} {
		if _, ok := got[backendName]; ok {
			t.Fatalf("did not expect AWS provider rule to affect %s", backendName)
		}
	}
}

func TestConfigRefresherMergesProviderAndBackendRulesMostRestrictively(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	providerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"limit":100,"used":96,"updated_at":"` + now.Format(time.RFC3339) + `"}`))
	}))
	defer providerServer.Close()

	refresher := NewConfigRefresher(model.BrokerConfig{
		Broker: model.BrokerRuntimeConfig{
			TierRouting: model.TierRoutingConfig{
				Enabled: true,
				Providers: map[string]model.TierProviderConfig{
					"aws-main": {
						Provider:  "aws",
						Mode:      "budget",
						SecretRef: "aws",
					},
				},
				ProviderRules: []model.ProviderTierRuleConfig{{
					Name:           "aws-budget",
					ProviderRef:    "aws-main",
					HardLimitRatio: 0.95,
					Action:         ActionDisable,
				}},
			},
		},
		Pools: []model.PoolConfig{{
			Name: model.PoolLite,
			Backends: map[model.BackendName]model.BackendConfig{
				model.BackendCodeBuild: {
					TierRules: []model.TierRuleConfig{{
						Name:           "codebuild-observe",
						ProviderRef:    "aws-main",
						HardLimitRatio: 0.99,
						Action:         ActionObserveOnly,
					}},
				},
			},
		}},
	}, staticSecretReader{
		"aws": {"snapshot_url": providerServer.URL},
	})
	refresher.now = func() time.Time { return now }

	decisions, err := refresher.Refresh(context.Background())
	if err != nil {
		t.Fatalf("refresh returned error: %v", err)
	}
	got := decisionsByBackend(decisions)
	decision := got[model.BackendCodeBuild]
	if decision.State != StateExceeded || decision.Action != ActionDisable {
		t.Fatalf("expected provider disable decision to win, got %+v", decision)
	}
}

func decisionsByBackend(decisions []Decision) map[model.BackendName]Decision {
	result := map[model.BackendName]Decision{}
	for _, decision := range decisions {
		result[decision.Backend] = decision
	}
	return result
}
