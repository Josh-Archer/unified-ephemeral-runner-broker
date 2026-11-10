package kubernetes

import (
	"context"
	"net/http"
	"testing"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/config"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	"gopkg.in/yaml.v3"
)

func TestCapacityCountsSiblingPodsInSharedAntiAffinityGroup(t *testing.T) {
	cfg := groupedHomelab2Config(t)
	api := &fakeAPI{responses: map[string]response{
		"GET /api/v1/namespaces/arc-runners/pods": {http.StatusOK, `{"items":[
			{"metadata":{"labels":{"uecb.josh-archer.dev/backend":"arc"}},"status":{"phase":"Pending","conditions":[{"type":"PodScheduled","status":"False","reason":"Unschedulable"}]}},
			{"metadata":{"labels":{"uecb.josh-archer.dev/backend":"arc-small-nodes"}},"status":{"phase":"Running"}},
			{"metadata":{"labels":{"uecb.josh-archer.dev/backend":"kubernetes-desktop"}},"status":{"phase":"Running"}},
			{"status":{"phase":"Running"}}
		]}`},
	}}
	status, err := NewNamed(cfg, api, &fakeRegistrar{}, "arc").Capacity(context.Background())
	if err != nil {
		t.Fatalf("Capacity: %v", err)
	}
	if status.MaxRunners != 1 || status.ActiveRunners != 1 || status.PendingRunners != 0 || backend.FreeSlots(status) != 0 {
		t.Fatalf("status = %+v, want max=1 active=1 pending=0 free=0", status)
	}
}

func TestCapacityGroupDoesNotCountUnschedulableSibling(t *testing.T) {
	cfg := groupedHomelab2Config(t)
	api := &fakeAPI{responses: map[string]response{
		"GET /api/v1/namespaces/arc-runners/pods": {http.StatusOK, `{"items":[
			{"metadata":{"labels":{"uecb.josh-archer.dev/backend":"arc-small-nodes"}},"status":{"phase":"Pending","conditions":[{"type":"PodScheduled","status":"False","reason":"Unschedulable"}]}},
			{"metadata":{"labels":{"uecb.josh-archer.dev/backend":"arc"}},"status":{"phase":"Pending","conditions":[{"type":"PodScheduled","status":"True"}]}}
		]}`},
	}}
	status, err := NewNamed(cfg, api, &fakeRegistrar{}, "arc").Capacity(context.Background())
	if err != nil {
		t.Fatalf("Capacity: %v", err)
	}
	if status.MaxRunners != 1 || status.ActiveRunners != 0 || status.PendingRunners != 1 || backend.FreeSlots(status) != 0 {
		t.Fatalf("status = %+v, want the schedulable pending pod to occupy the only shared slot", status)
	}
}

func groupedHomelab2Config(t *testing.T) model.BrokerConfig {
	t.Helper()
	cfg := config.Default()
	cfg.GitHub.Scope = model.GitHubScope{Type: model.GitHubScopeOwner, Owner: "example-owner"}
	cfg.GitHub.Auth.SecretRef = "uecb-github-app"
	var spec map[string]any
	raw := `
affinity:
  nodeAffinity:
    requiredDuringSchedulingIgnoredDuringExecution:
      nodeSelectorTerms:
        - matchExpressions:
          - key: kubernetes.io/hostname
            operator: In
            values: [homelab2]
          - key: kubernetes.io/arch
            operator: In
            values: [amd64]
  podAntiAffinity:
    requiredDuringSchedulingIgnoredDuringExecution:
      - topologyKey: kubernetes.io/hostname
`
	if err := yaml.Unmarshal([]byte(raw), &spec); err != nil {
		t.Fatal(err)
	}
	for i := range cfg.Pools {
		if cfg.Pools[i].Name != model.PoolFull {
			continue
		}
		backend := model.BackendConfig{
			Driver:              "kubernetes",
			Enabled:             true,
			Healthy:             true,
			MaxRunners:          4,
			CrossPoolMaxRunners: 4,
			Kubernetes:          &model.KubernetesBackendConfig{Namespace: "arc-runners", PodSpec: spec},
		}
		peer := backend
		cfg.Pools[i].Backends = map[model.BackendName]model.BackendConfig{
			"arc":                backend,
			"arc-small-nodes":    peer,
			"kubernetes-desktop": {Driver: "kubernetes", Enabled: true, Healthy: true, MaxRunners: 10, CrossPoolMaxRunners: 10},
		}
	}
	return cfg
}
