package config

import (
	"testing"

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
