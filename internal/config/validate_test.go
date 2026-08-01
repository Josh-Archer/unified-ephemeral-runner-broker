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

func TestValidateFairShareSoftReserves(t *testing.T) {
	cfg := Default()
	for i := range cfg.Pools {
		if cfg.Pools[i].Name != model.PoolLite {
			continue
		}
		cfg.Pools[i].FairShare.Enabled = true
		cfg.Pools[i].FairShare.PriorityClasses = map[string]int{
			"smoke": 1, "deploy": 3,
		}
		cfg.Pools[i].FairShare.SoftReserves = map[string]int{"deploy": 1}
	}
	if err := Validate(cfg); err != nil {
		t.Fatalf("valid soft reserves should pass: %v", err)
	}

	for i := range cfg.Pools {
		if cfg.Pools[i].Name != model.PoolLite {
			continue
		}
		cfg.Pools[i].FairShare.Enabled = false
	}
	if err := Validate(cfg); err == nil {
		t.Fatal("softReserves without fairShare.enabled should fail")
	}

	for i := range cfg.Pools {
		if cfg.Pools[i].Name != model.PoolLite {
			continue
		}
		cfg.Pools[i].FairShare.Enabled = true
		cfg.Pools[i].FairShare.SoftReserves = map[string]int{"deploy": -1}
	}
	if err := Validate(cfg); err == nil {
		t.Fatal("negative softReserves should fail")
	}
}
