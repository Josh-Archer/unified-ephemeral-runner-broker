package config

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

func overflowConfig(mutate func(*model.PoolConfig, *model.BrokerConfig)) model.BrokerConfig {
	cfg := Default()
	cfg.Broker.TierRouting.Enabled = true
	cfg.Broker.TierRouting.Prometheus.URL = "http://prometheus:9090"
	cfg.Broker.GitHubWebhook.SecretRef = "uecb-github-webhook"
	pool := cfg.Pools[0]
	pool.WebhookLabels = []string{"self-hosted", "uecb-shared"}
	pool.HostedOverflow = &model.HostedOverflowConfig{
		Enabled:         true,
		Variable:        "UECB_SHARED_RUNS_ON",
		SelfHostedValue: "uecb-shared",
		HostedValue:     "ubuntu-latest",
		Repositories:    []string{"Josh-Archer/DeskQuest", "Josh-Archer/agent-setup"},
		SaturateAfter:   2 * time.Minute,
		RecoverAfter:    5 * time.Minute,
		BudgetRules: []model.TierRuleConfig{{
			Name:           "github-actions-minutes",
			UsageQuery:     "github_minutes_ratio",
			HardLimitRatio: 0.95,
		}},
	}
	if mutate != nil {
		mutate(&pool, &cfg)
	}
	cfg.Pools[0] = pool
	return cfg
}

func TestValidateHostedOverflow(t *testing.T) {
	if err := Validate(overflowConfig(nil)); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	cases := map[string]struct {
		mutate func(*model.PoolConfig, *model.BrokerConfig)
		want   string
	}{
		"no webhook labels": {func(p *model.PoolConfig, _ *model.BrokerConfig) { p.WebhookLabels = nil }, "webhookLabels"},
		"bad variable":      {func(p *model.PoolConfig, _ *model.BrokerConfig) { p.HostedOverflow.Variable = "1BAD-NAME" }, "variable"},
		"github prefix":     {func(p *model.PoolConfig, _ *model.BrokerConfig) { p.HostedOverflow.Variable = "GITHUB_RUNS_ON" }, "variable"},
		"missing value":     {func(p *model.PoolConfig, _ *model.BrokerConfig) { p.HostedOverflow.HostedValue = "" }, "hostedValue"},
		"same values":       {func(p *model.PoolConfig, _ *model.BrokerConfig) { p.HostedOverflow.HostedValue = "uecb-shared" }, "differ"},
		"no repositories":   {func(p *model.PoolConfig, _ *model.BrokerConfig) { p.HostedOverflow.Repositories = nil }, "repositories"},
		"bad repository": {func(p *model.PoolConfig, _ *model.BrokerConfig) {
			p.HostedOverflow.Repositories = []string{"DeskQuest"}
		}, "owner/repo"},
		"duplicate": {func(p *model.PoolConfig, _ *model.BrokerConfig) {
			p.HostedOverflow.Repositories = []string{"o/r", "O/R"}
		}, "twice"},
		"saturate too short":  {func(p *model.PoolConfig, _ *model.BrokerConfig) { p.HostedOverflow.SaturateAfter = 5 * time.Second }, "saturateAfter"},
		"budget without tier": {func(_ *model.PoolConfig, c *model.BrokerConfig) { c.Broker.TierRouting.Enabled = false }, "tierRouting"},
		"bad budget rule":     {func(p *model.PoolConfig, _ *model.BrokerConfig) { p.HostedOverflow.BudgetRules[0].HardLimitRatio = 2 }, "github-hosted"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := Validate(overflowConfig(tc.mutate))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want mention of %q", err, tc.want)
			}
		})
	}
	disabled := overflowConfig(func(p *model.PoolConfig, _ *model.BrokerConfig) {
		p.HostedOverflow.Enabled = false
		p.HostedOverflow.Repositories = nil
	})
	if err := Validate(disabled); err != nil {
		t.Fatalf("disabled overflow should not be validated: %v", err)
	}
}

func TestLoadParsesHostedOverflow(t *testing.T) {
	path := t.TempDir() + "/broker.yaml"
	if err := os.WriteFile(path, []byte(`
broker:
  githubWebhook:
    secretRef: uecb-github-webhook
  tierRouting:
    enabled: true
    prometheus:
      url: http://prometheus:9090
pools:
  - name: shared
    labels: [self-hosted, linux, x64, uecb-shared]
    webhookLabels: [self-hosted, uecb-shared]
    scheduler: priority-fair-share
    capabilityProfile: full
    hostedOverflow:
      enabled: true
      variable: UECB_SHARED_RUNS_ON
      selfHostedValue: uecb-shared
      hostedValue: ubuntu-latest
      repositories: [Josh-Archer/DeskQuest]
      saturateAfter: 3m
      budgetRules:
        - name: github-actions-minutes
          usageQuery: github_minutes_ratio
          hardLimitRatio: 0.95
          action: disable
    backends:
      kubernetes-shared:
        driver: kubernetes
        enabled: true
        healthy: true
        maxRunners: 2
        crossPoolMaxRunners: 2
        kubernetes:
          namespace: arc-runners
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	var overflow *model.HostedOverflowConfig
	for _, pool := range cfg.Pools {
		if pool.Name == "shared" {
			overflow = pool.HostedOverflow
		}
	}
	if overflow == nil || !overflow.Enabled || overflow.SaturateAfter != 3*time.Minute || len(overflow.BudgetRules) != 1 {
		t.Fatalf("hostedOverflow = %+v", overflow)
	}
}
