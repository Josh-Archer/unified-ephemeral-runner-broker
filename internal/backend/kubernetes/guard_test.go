package kubernetes

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/config"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

const desktopName model.BackendName = "kubernetes-desktop"
const admittedNode = `{"kind":"Node","metadata":{"name":"desktop","labels":{"interactive/schedulable":"true"}},"spec":{},"status":{"conditions":[{"type":"Ready","status":"True"}]}}`

func namedConfig(t *testing.T) model.BrokerConfig {
	t.Helper()
	cfg := newConfig(t)
	target := model.BackendConfig{Enabled: true, Healthy: true, Driver: "kubernetes", MaxRunners: 5, CrossPoolMaxRunners: 5, Kubernetes: &model.KubernetesBackendConfig{
		Namespace: "arc-runners", NodeGuard: &model.KubernetesNodeGuardConfig{NodeName: "desktop", RequiredLabels: map[string]string{"interactive/schedulable": "true"}},
	}}
	for i := range cfg.Pools {
		cfg.Pools[i].Backends[desktopName] = target
	}
	return cfg
}

func TestGuardUnavailableFailsClosedBeforeRegistration(t *testing.T) {
	cases := []struct {
		name      string
		node      response
		inventory response
	}{
		{"missing", response{404, `{}`}, response{200, `{"items":[]}`}},
		{"forbidden", response{403, `{}`}, response{200, `{"items":[]}`}},
		{"malformed", response{200, `{"kind":"Node"}`}, response{200, `{"items":[]}`}},
		{"not-ready", response{200, strings.Replace(admittedNode, `"status":"True"`, `"status":"False"`, 1)}, response{200, `{"items":[]}`}},
		{"revoked", response{200, strings.Replace(admittedNode, `"interactive/schedulable":"true"`, `"interactive/schedulable":"false"`, 1)}, response{200, `{"items":[]}`}},
		{"cordoned", response{200, strings.Replace(admittedNode, `"spec":{}`, `"spec":{"unschedulable":true}`, 1)}, response{200, `{"items":[]}`}},
		{"tainted", response{200, strings.Replace(admittedNode, `"spec":{}`, `"spec":{"taints":[{"effect":"NoSchedule"}]}`, 1)}, response{200, `{"items":[]}`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := &fakeAPI{responses: map[string]response{"GET /api/v1/nodes/desktop": tc.node, "GET /api/v1/namespaces/arc-runners/pods": tc.inventory}}
			reg := &fakeRegistrar{}
			b := NewNamed(namedConfig(t), api, reg, desktopName)
			status, err := b.Capacity(context.Background())
			if err != nil || backend.FreeSlots(status) != 0 || status.MaxRunners != 5 {
				t.Fatalf("capacity=%+v error=%v", status, err)
			}
			_, err = b.Provision(context.Background(), model.AllocationRequest{}, model.AllocationStatus{ID: "a", Pool: model.PoolLite, Repository: "example-owner/home"})
			if !backend.IsCapacityExhausted(err) {
				t.Fatalf("want capacity fallback, got %v", err)
			}
			if len(reg.registered) != 0 {
				t.Fatal("unavailable node registered a runner")
			}
		})
	}
}

func TestGuardUnknownInventoryFailsClosed(t *testing.T) {
	for _, inventory := range []response{{403, `{}`}, {200, `{}`}, {200, `not json`}} {
		api := &fakeAPI{responses: map[string]response{"GET /api/v1/nodes/desktop": {200, admittedNode}, "GET /api/v1/namespaces/arc-runners/pods": inventory}}
		status, err := NewNamed(namedConfig(t), api, &fakeRegistrar{}, desktopName).Capacity(context.Background())
		if err != nil || backend.FreeSlots(status) != 0 {
			t.Fatalf("inventory=%+v capacity=%+v err=%v", inventory, status, err)
		}
	}
	status, err := NewNamed(namedConfig(t), nil, nil, desktopName).Capacity(context.Background())
	if err != nil || backend.FreeSlots(status) != 0 {
		t.Fatalf("missing API capacity=%+v err=%v", status, err)
	}
}

func TestNamedTargetSharesCapAndKeepsIndependentIdentity(t *testing.T) {
	api := &fakeAPI{responses: map[string]response{"GET /api/v1/nodes/desktop": {200, admittedNode}, "GET /api/v1/namespaces/arc-runners/pods": {200, `{"items":[{"status":{"phase":"Running"}},{"status":{"phase":"Pending"},"metadata":{"deletionTimestamp":"now"}}]}`}}}
	reg := &fakeRegistrar{}
	cfg := namedConfig(t)
	if err := config.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	b := NewNamed(cfg, api, reg, desktopName)
	status, err := b.Capacity(context.Background())
	if err != nil || status.MaxRunners != 5 || status.ActiveRunners != 1 || status.PendingRunners != 1 {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	selector, _ := url.QueryUnescape(api.calls[1].path)
	if !strings.Contains(selector, labelBackend+"=kubernetes-desktop") {
		t.Fatalf("selector=%s", selector)
	}
	got, err := b.Provision(context.Background(), model.AllocationRequest{}, model.AllocationStatus{ID: "one", Pool: model.PoolLite, Repository: "example-owner/home"})
	if err != nil {
		t.Fatal(err)
	}
	if got.RunnerLabel != "uecb-kubernetesdesktop-one" {
		t.Fatalf("runner=%+v", got)
	}
	pod := api.calls[len(api.calls)-1].body
	if pod["metadata"].(map[string]any)["labels"].(map[string]any)[labelBackend] != string(desktopName) {
		t.Fatal("target label wrong")
	}
	spec := pod["spec"].(map[string]any)
	if spec["nodeSelector"].(map[string]any)["interactive/schedulable"] != "true" {
		t.Fatal("missing admission selector")
	}
	encoded, _ := json.Marshal(spec)
	if !strings.Contains(string(encoded), `"matchFields":[{"key":"metadata.name","operator":"In","values":["desktop"]}]`) {
		t.Fatalf("missing exact node constraint: %s", encoded)
	}
	if err := b.Cleanup(context.Background(), model.AllocationStatus{ID: "one", Pool: model.PoolLite, Repository: "example-owner/home", Metadata: got.Metadata}); err != nil {
		t.Fatal(err)
	}
	if api.calls[len(api.calls)-1].method != http.MethodDelete || len(reg.deleted) != 1 {
		t.Fatal("named target not cleaned up")
	}
	builtin := New(cfg, api, reg)
	if _, err := builtin.Capacity(context.Background()); err != nil {
		t.Fatal(err)
	}
	selector, _ = url.QueryUnescape(api.calls[len(api.calls)-1].path)
	if !strings.Contains(selector, labelBackend+"=kubernetes") || strings.Contains(selector, "kubernetes-desktop") {
		t.Fatalf("builtin selector=%s", selector)
	}
}

func TestNodeGuardPreservesEveryAffinityAlternative(t *testing.T) {
	spec := map[string]any{"affinity": map[string]any{"nodeAffinity": map[string]any{"requiredDuringSchedulingIgnoredDuringExecution": map[string]any{"nodeSelectorTerms": []any{map[string]any{}, map[string]any{"matchFields": []any{map[string]any{"key": "metadata.name", "operator": "NotIn", "values": []any{"excluded"}}}}}}}}}
	if err := enforceNodeGuard(spec, &model.KubernetesNodeGuardConfig{NodeName: "desktop", RequiredLabels: map[string]string{"admitted": "true"}}); err != nil {
		t.Fatal(err)
	}
	terms := spec["affinity"].(map[string]any)["nodeAffinity"].(map[string]any)["requiredDuringSchedulingIgnoredDuringExecution"].(map[string]any)["nodeSelectorTerms"].([]any)
	for _, raw := range terms {
		fields := raw.(map[string]any)["matchFields"].([]any)
		last := fields[len(fields)-1].(map[string]any)
		if last["values"].([]any)[0] != "desktop" {
			t.Fatal("node guard not ANDed into every alternative")
		}
	}
}

func TestNamedTargetValidationRejectsUnsafeConfiguration(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*model.BrokerConfig)
	}{
		{"builtin-collision", func(cfg *model.BrokerConfig) {
			cfg.Pools[0].Backends[model.BackendARC] = cfg.Pools[0].Backends[desktopName]
		}},
		{"missing-cap", func(cfg *model.BrokerConfig) {
			c := cfg.Pools[0].Backends[desktopName]
			c.CrossPoolMaxRunners = 0
			cfg.Pools[0].Backends[desktopName] = c
		}},
		{"different-cap", func(cfg *model.BrokerConfig) {
			c := cfg.Pools[0].Backends[desktopName]
			c.CrossPoolMaxRunners = 6
			cfg.Pools[0].Backends[desktopName] = c
		}},
		{"different-target", func(cfg *model.BrokerConfig) {
			target := cfg.Pools[0].Backends[desktopName]
			copy := *target.Kubernetes
			copy.Namespace = "other-runners"
			target.Kubernetes = &copy
			cfg.Pools[0].Backends[desktopName] = target
		}},
		{"different-driver", func(cfg *model.BrokerConfig) {
			c := cfg.Pools[0].Backends[desktopName]
			c.Driver = ""
			cfg.Pools[0].Backends[desktopName] = c
		}},
		{"node-name-bypass", func(cfg *model.BrokerConfig) {
			cfg.Pools[0].Backends[desktopName].Kubernetes.PodSpec = map[string]any{"nodeName": "desktop"}
		}},
		{"conflicting-label", func(cfg *model.BrokerConfig) {
			cfg.Pools[0].Backends[desktopName].Kubernetes.PodSpec = map[string]any{"nodeSelector": map[string]any{"interactive/schedulable": "false"}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := namedConfig(t)
			tc.mutate(&cfg)
			if err := config.Validate(cfg); err == nil {
				t.Fatal("unsafe config accepted")
			}
		})
	}
}

func TestGuardRejectsFullOrUnknownInventoryBeforeRegistration(t *testing.T) {
	for _, inventory := range []response{{403, `{}`}, {200, `{"items":[{},{},{},{},{}]}`}} {
		api := &fakeAPI{responses: map[string]response{"GET /api/v1/nodes/desktop": {200, admittedNode}, "GET /api/v1/namespaces/arc-runners/pods": inventory}}
		registrar := &fakeRegistrar{}
		_, err := NewNamed(namedConfig(t), api, registrar, desktopName).Provision(context.Background(), model.AllocationRequest{}, model.AllocationStatus{ID: "a", Pool: model.PoolLite, Repository: "example-owner/home"})
		if !backend.IsCapacityExhausted(err) || len(registrar.registered) != 0 {
			t.Fatalf("full/unknown target must fall back before JIT: error=%v registrations=%v", err, registrar.registered)
		}
	}
}

func TestConfiguredPodMetadataCannotReplaceBrokerOwnership(t *testing.T) {
	cfg := newConfig(t)
	for _, pool := range cfg.Pools {
		if target, ok := pool.Backends[model.BackendKubernetes]; ok {
			target.Kubernetes.PodLabels = map[string]string{"interactive-controller/evictable": "true", labelManagedBy: "spoof", labelBackend: "spoof", labelPool: "spoof", labelAllocation: "spoof"}
			target.Kubernetes.PodAnnotations = map[string]string{"interactive-controller/eviction-policy": "lossy", annotationRepository: "spoof"}
		}
	}
	api := &fakeAPI{}
	_, err := New(cfg, api, &fakeRegistrar{}).Provision(context.Background(), model.AllocationRequest{}, model.AllocationStatus{ID: "one", Pool: model.PoolFull, Repository: "example-owner/home"})
	if err != nil {
		t.Fatal(err)
	}
	metadata := api.calls[0].body["metadata"].(map[string]any)
	labels := metadata["labels"].(map[string]any)
	if labels[labelManagedBy] != labelManagedByVal || labels[labelBackend] != string(model.BackendKubernetes) || labels[labelPool] != "full" || labels[labelAllocation] != "one" || labels["interactive-controller/evictable"] != "true" {
		t.Fatalf("ownership labels=%v", labels)
	}
	annotations := metadata["annotations"].(map[string]any)
	if annotations[annotationRepository] != "example-owner/home" || annotations["interactive-controller/eviction-policy"] != "lossy" {
		t.Fatalf("annotations=%v", annotations)
	}
}
