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

func TestValidateWarmSchedule(t *testing.T) {
	cfg := model.BrokerConfig{
		Pools: []model.PoolConfig{{
			Name: model.PoolLite,
			Backends: map[model.BackendName]model.BackendConfig{
				model.BackendLambda: {
					Enabled: true,
					WarmMin: 1,
					WarmMax: 2,
					WarmSchedule: &model.WarmScheduleConfig{
						Timezone: "America/New_York",
						Windows: []model.WarmWindowConfig{{
							Days:  []string{"mon", "fri"},
							Start: "08:00",
							End:   "18:00",
						}},
					},
				},
			},
		}},
	}
	if err := Validate(cfg); err != nil {
		t.Fatalf("valid warm schedule rejected: %v", err)
	}

	cfg.Pools[0].Backends[model.BackendLambda] = model.BackendConfig{
		WarmSchedule: &model.WarmScheduleConfig{
			Timezone: "Not/AZone",
			Windows:  []model.WarmWindowConfig{{Start: "08:00", End: "18:00"}},
		},
	}
	if err := Validate(cfg); err == nil {
		t.Fatal("expected invalid timezone to fail validation")
	}

	cfg.Pools[0].Backends[model.BackendLambda] = model.BackendConfig{
		WarmSchedule: &model.WarmScheduleConfig{
			Windows: []model.WarmWindowConfig{{Start: "25:00", End: "18:00"}},
		},
	}
	if err := Validate(cfg); err == nil {
		t.Fatal("expected invalid clock to fail validation")
	}
}
