package config

import (
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

func TestValidateReplicaSafety(t *testing.T) {
	cfg := model.BrokerConfig{
		Broker: model.BrokerRuntimeConfig{
			StateStore: model.StateStoreConfig{Type: "memory"},
		},
	}
	if err := ValidateReplicaSafety(cfg, 1); err != nil {
		t.Fatalf("replicas=1 should be safe: %v", err)
	}
	if err := ValidateReplicaSafety(cfg, 2); err == nil {
		t.Fatal("replicas=2 with memory should be rejected")
	}

	cfg.Broker.StateStore.Type = "file"
	cfg.Broker.StateStore.Path = "/tmp/x"
	if err := ValidateReplicaSafety(cfg, 2); err == nil {
		t.Fatal("replicas=2 with file should be rejected")
	}

	cfg.Broker.StateStore = model.StateStoreConfig{Type: "postgres"}
	if err := ValidateReplicaSafety(cfg, 3); err != nil {
		t.Fatalf("replicas=3 with postgres should be safe: %v", err)
	}
}

func TestARCFirstWaitDefaultsAndValidation(t *testing.T) {
	cfg := Default()
	if cfg.Broker.ARCFirst.SaturationWait != 2*time.Second || cfg.Broker.ARCFirst.PollInterval != 100*time.Millisecond {
		t.Fatalf("unexpected ARC-first defaults: %+v", cfg.Broker.ARCFirst)
	}
	cfg.Broker.ARCFirst.SaturationWait = 0
	cfg.Broker.ARCFirst.PollInterval = 0
	if err := Validate(cfg); err != nil {
		t.Fatalf("zero wait should allow immediate fallback: %v", err)
	}
	cfg.Broker.ARCFirst.SaturationWait = -time.Second
	if err := Validate(cfg); err == nil {
		t.Fatal("negative ARC-first saturation wait should be rejected")
	}
}

func TestValidateWebhooksAcceptsQuarantinedEvent(t *testing.T) {
	cfg := Default()
	cfg.Broker.Webhooks = model.WebhooksConfig{
		Enabled: true,
		Endpoints: []model.WebhookEndpointConfig{{
			URL:           "https://hooks.example.com/uecb",
			SigningSecret: "test-secret",
			Events:        []string{"quarantined"},
		}},
	}
	if err := Validate(cfg); err != nil {
		t.Fatalf("quarantined webhook event should be valid: %v", err)
	}
}

func TestHAEnabled(t *testing.T) {
	cfg := model.BrokerRuntimeConfig{StateStore: model.StateStoreConfig{Type: "memory"}}
	if HAEnabled(cfg) {
		t.Fatal("memory should not enable HA by default")
	}
	cfg.StateStore.Type = "postgres"
	if !HAEnabled(cfg) {
		t.Fatal("postgres should enable HA by default")
	}
	disabled := false
	cfg.HA.Enabled = &disabled
	if HAEnabled(cfg) {
		t.Fatal("explicit false should disable HA")
	}
}

func TestValidateBackendBudgets(t *testing.T) {
	cfg := Default()
	for i := range cfg.Pools {
		if cfg.Pools[i].Name != model.PoolLite {
			continue
		}
		backendCfg := cfg.Pools[i].Backends[model.BackendLambda]
		backendCfg.Budget = model.BudgetConfig{Enabled: true}
		cfg.Pools[i].Backends[model.BackendLambda] = backendCfg
	}
	if err := Validate(cfg); err == nil {
		t.Fatal("enabled budget without limits should fail")
	}

	for i := range cfg.Pools {
		if cfg.Pools[i].Name != model.PoolLite {
			continue
		}
		backendCfg := cfg.Pools[i].Backends[model.BackendLambda]
		backendCfg.Budget = model.BudgetConfig{Enabled: true, MaxAllocationsDaily: 10}
		backendCfg.CostClass = -1
		cfg.Pools[i].Backends[model.BackendLambda] = backendCfg
	}
	if err := Validate(cfg); err == nil {
		t.Fatal("negative costClass should fail")
	}

	for i := range cfg.Pools {
		if cfg.Pools[i].Name != model.PoolLite {
			continue
		}
		backendCfg := cfg.Pools[i].Backends[model.BackendLambda]
		backendCfg.Budget = model.BudgetConfig{Enabled: true, MaxAllocationsDaily: 10, MaxAllocationsMonthly: 100}
		backendCfg.CostClass = 40
		cfg.Pools[i].Backends[model.BackendLambda] = backendCfg
	}
	if err := Validate(cfg); err != nil {
		t.Fatalf("valid budget config should pass: %v", err)
	}
}

func TestValidateFairShareNegativeUsageWindow(t *testing.T) {
	cfg := Default()
	for i := range cfg.Pools {
		if cfg.Pools[i].Name != model.PoolLite {
			continue
		}
		cfg.Pools[i].FairShare = model.FairShareConfig{
			Enabled:     true,
			UsageWindow: -1 * time.Minute,
		}
	}
	if err := Validate(cfg); err == nil {
		t.Fatal("negative usageWindow should fail validation")
	}
}

func TestValidateFairShareNegativeStarvationAfter(t *testing.T) {
	cfg := Default()
	for i := range cfg.Pools {
		if cfg.Pools[i].Name != model.PoolLite {
			continue
		}
		cfg.Pools[i].FairShare = model.FairShareConfig{
			Enabled:         true,
			StarvationAfter: -30 * time.Second,
		}
	}
	if err := Validate(cfg); err == nil {
		t.Fatal("negative starvationAfter should fail validation")
	}
}

func TestValidateFairSharePositiveValues(t *testing.T) {
	cfg := Default()
	for i := range cfg.Pools {
		if cfg.Pools[i].Name != model.PoolLite {
			continue
		}
		cfg.Pools[i].FairShare = model.FairShareConfig{
			Enabled:         true,
			UsageWindow:     10 * time.Minute,
			StarvationAfter: 30 * time.Second,
		}
	}
	if err := Validate(cfg); err != nil {
		t.Fatalf("valid fair share config should pass: %v", err)
	}
}

func TestValidateWebhookLabels(t *testing.T) {
	valid := func() model.BrokerConfig {
		cfg := Default()
		cfg.Pools[0].WebhookLabels = []string{"self-hosted", "uecb-full"}
		cfg.Broker.GitHubWebhook.Secret = "s"
		return cfg
	}
	if err := Validate(valid()); err != nil {
		t.Fatalf("valid webhook config rejected: %v", err)
	}

	noSecret := valid()
	noSecret.Broker.GitHubWebhook = model.GitHubWebhookConfig{}
	if err := Validate(noSecret); err == nil {
		t.Fatal("webhookLabels without githubWebhook secret should be rejected")
	}

	empty := valid()
	empty.Pools[0].WebhookLabels = []string{"self-hosted", " "}
	if err := Validate(empty); err == nil {
		t.Fatal("blank webhook label should be rejected")
	}

	dup := valid()
	dup.Pools[1].WebhookLabels = []string{"UECB-full", "self-hosted"}
	if err := Validate(dup); err == nil {
		t.Fatal("identical label sets across pools should be rejected")
	}

	off := Default()
	if err := Validate(off); err != nil {
		t.Fatalf("pools without webhookLabels need no secret: %v", err)
	}
}

func TestValidateRegistrationTimeoutAndBackendLimits(t *testing.T) {
	cfg := Default()
	if err := Validate(cfg); err != nil {
		t.Fatalf("default config should be valid: %v", err)
	}

	negReg := cfg
	negReg.Broker.RegistrationTimeout = -1
	if err := Validate(negReg); err == nil {
		t.Fatal("negative broker.registrationTimeout should be rejected")
	}

	negLimit := cfg
	negLimit.Broker.BackendLimits = map[model.BackendName]int{model.BackendAzureVM: -1}
	if err := Validate(negLimit); err == nil {
		t.Fatal("negative broker.backendLimits should be rejected")
	}

	negCrossPool := cfg
	b := negCrossPool.Pools[0].Backends[model.BackendARC]
	b.CrossPoolMaxRunners = -1
	negCrossPool.Pools[0].Backends[model.BackendARC] = b
	if err := Validate(negCrossPool); err == nil {
		t.Fatal("negative backend.crossPoolMaxRunners should be rejected")
	}

	negBackendReg := cfg
	b2 := negBackendReg.Pools[0].Backends[model.BackendARC]
	b2.RegistrationTimeout = -1
	negBackendReg.Pools[0].Backends[model.BackendARC] = b2
	if err := Validate(negBackendReg); err == nil {
		t.Fatal("negative backend.registrationTimeout should be rejected")
	}
}
