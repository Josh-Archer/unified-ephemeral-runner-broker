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

func TestValidateWebhooks(t *testing.T) {
	cfg := model.WebhooksConfig{Enabled: false}
	if err := validateWebhooks(cfg); err != nil {
		t.Fatalf("disabled webhooks should validate: %v", err)
	}

	cfg.Enabled = true
	if err := validateWebhooks(cfg); err == nil {
		t.Fatal("enabled without endpoints should fail")
	}

	cfg.Endpoints = []model.WebhookEndpointConfig{{
		URL:           "https://hooks.example.com/uecb",
		SigningSecret: "secret",
		Events:        []string{"ready", "allocation.completed", "cancelled"},
	}}
	if err := validateWebhooks(cfg); err != nil {
		t.Fatalf("valid webhooks should pass: %v", err)
	}

	cfg.Endpoints[0].SigningSecret = ""
	if err := validateWebhooks(cfg); err == nil {
		t.Fatal("missing signing secret should fail")
	}

	cfg.Endpoints[0].SigningSecretRef = "uecb-webhook"
	cfg.Endpoints[0].Events = []string{"bogus"}
	if err := validateWebhooks(cfg); err == nil {
		t.Fatal("bogus event should fail")
	}
}
