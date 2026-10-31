package kubernetes

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/config"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	"gopkg.in/yaml.v3"
)

type call struct {
	method string
	path   string
	body   map[string]any
}

type fakeAPI struct {
	calls     []call
	responses map[string]response
}

type response struct {
	status int
	body   string
}

func (f *fakeAPI) Namespace() string { return "arc-systems" }

func (f *fakeAPI) Do(_ context.Context, method, path string, body any) (int, []byte, error) {
	var decoded map[string]any
	if body != nil {
		encoded, _ := json.Marshal(body)
		_ = json.Unmarshal(encoded, &decoded)
	}
	f.calls = append(f.calls, call{method: method, path: path, body: decoded})
	key := method + " " + strings.SplitN(path, "?", 2)[0]
	if r, ok := f.responses[key]; ok {
		return r.status, []byte(r.body), nil
	}
	return http.StatusCreated, []byte(`{}`), nil
}

const podSpecYAML = `
nodeSelector:
  kubernetes.io/arch: amd64
containers:
  - name: runner
    resources:
      requests: {cpu: 500m, memory: 1Gi}
    env:
      - name: DOCKER_HOST
        value: unix:///var/run/docker/docker.sock
  - name: dind
    image: docker:dind
    securityContext: {privileged: true}
`

func newConfig(t *testing.T) model.BrokerConfig {
	t.Helper()
	cfg := config.Default()
	cfg.GitHub.Scope = model.GitHubScope{Type: model.GitHubScopeOwner, Owner: "example-owner"}
	cfg.GitHub.Auth.SecretRef = "uecb-github-app"
	var podSpec map[string]any
	if err := yaml.Unmarshal([]byte(podSpecYAML), &podSpec); err != nil {
		t.Fatal(err)
	}
	for i := range cfg.Pools {
		if cfg.Pools[i].Name != model.PoolFull {
			continue
		}
		cfg.Pools[i].Backends[model.BackendKubernetes] = model.BackendConfig{
			Enabled:    true,
			Healthy:    true,
			MaxRunners: 6,
			Kubernetes: &model.KubernetesBackendConfig{
				Namespace: "arc-runners",
				Image:     "ghcr.io/example/launcher:v1",
				PodSpec:   podSpec,
			},
		}
	}
	return cfg
}

func envMap(t *testing.T, container map[string]any) map[string]any {
	t.Helper()
	result := map[string]any{}
	for _, raw := range container["env"].([]any) {
		entry := raw.(map[string]any)
		if value, ok := entry["value"]; ok {
			result[entry["name"].(string)] = value
		} else {
			result[entry["name"].(string)] = entry["valueFrom"]
		}
	}
	return result
}

func TestProvisionCreatesRunnerPodForRequestingRepository(t *testing.T) {
	api := &fakeAPI{}
	b := New(newConfig(t), api)
	request := model.AllocationRequest{
		Pool:             model.PoolFull,
		JobTimeout:       20 * time.Minute,
		Labels:           []string{"self-hosted", "uecb-full"},
		TargetRepository: "example-owner/questkit",
	}
	provisioned, err := b.Provision(context.Background(), request, model.AllocationStatus{
		ID:              "Alloc_123",
		Pool:            model.PoolFull,
		RequestedLabels: request.Labels,
		Request:         &request,
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if provisioned.RunnerLabel != "uecb-kubernetes-Alloc_123" {
		t.Fatalf("runner label = %q", provisioned.RunnerLabel)
	}
	if provisioned.Metadata["namespace"] != "arc-runners" || provisioned.Metadata["target_url"] != "https://github.com/example-owner/questkit" {
		t.Fatalf("metadata = %+v", provisioned.Metadata)
	}

	if len(api.calls) != 1 || api.calls[0].method != http.MethodPost || api.calls[0].path != "/api/v1/namespaces/arc-runners/pods" {
		t.Fatalf("calls = %+v", api.calls)
	}
	pod := api.calls[0].body
	metadata := pod["metadata"].(map[string]any)
	if metadata["name"] != "uecb-runner-alloc-123" {
		t.Fatalf("pod name = %v", metadata["name"])
	}
	if metadata["annotations"].(map[string]any)[annotationRepository] != "example-owner/questkit" {
		t.Fatalf("annotations = %v", metadata["annotations"])
	}
	spec := pod["spec"].(map[string]any)
	if spec["restartPolicy"] != "Never" || spec["activeDeadlineSeconds"].(float64) != 30*60 {
		t.Fatalf("spec = %+v", spec)
	}
	if spec["nodeSelector"].(map[string]any)["kubernetes.io/arch"] != "amd64" {
		t.Fatalf("podSpec template not kept: %+v", spec)
	}
	containers := spec["containers"].([]any)
	if len(containers) != 2 {
		t.Fatalf("containers = %+v", containers)
	}
	runner := containers[0].(map[string]any)
	if runner["image"] != "ghcr.io/example/launcher:v1" {
		t.Fatalf("runner image = %v", runner["image"])
	}
	env := envMap(t, runner)
	want := map[string]string{
		"DOCKER_HOST":       "unix:///var/run/docker/docker.sock",
		"GITHUB_SCOPE_TYPE": "repository",
		"GITHUB_OWNER":      "example-owner",
		"GITHUB_REPOSITORY": "questkit",
		"GITHUB_TARGET_URL": "https://github.com/example-owner/questkit",
		"RUNNER_NAME":       "uecb-kubernetes-Alloc_123",
	}
	for name, value := range want {
		if env[name] != value {
			t.Errorf("env %s = %v, want %q", name, env[name], value)
		}
	}
	if labels := env["RUNNER_LABELS"].(string); !strings.Contains(labels, "uecb-full") || !strings.HasSuffix(labels, "uecb-kubernetes-Alloc_123") || strings.Count(labels, "self-hosted") != 1 {
		t.Errorf("RUNNER_LABELS = %q", labels)
	}
	token := env["GITHUB_TOKEN"].(map[string]any)["secretKeyRef"].(map[string]any)
	if token["name"] != "uecb-github-app" || token["key"] != "github_token" {
		t.Errorf("GITHUB_TOKEN = %+v", token)
	}
	if containers[1].(map[string]any)["image"] != "docker:dind" {
		t.Fatalf("sidecar changed: %+v", containers[1])
	}

	// The template is copied, not mutated, so the next pod gets clean env.
	if _, err := b.Provision(context.Background(), request, model.AllocationStatus{ID: "second", Pool: model.PoolFull, Request: &request}); err != nil {
		t.Fatal(err)
	}
	second := api.calls[1].body["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
	if got := len(second["env"].([]any)); got != len(runner["env"].([]any)) {
		t.Fatalf("env grew across pods: %d vs %d", got, len(runner["env"].([]any)))
	}
}

func TestProvisionRejectsForeignRepositoryAndHandlesAPIErrors(t *testing.T) {
	api := &fakeAPI{}
	b := New(newConfig(t), api)
	foreign := model.AllocationStatus{ID: "x", Pool: model.PoolFull, Repository: "someone-else/repo"}
	if _, err := b.Provision(context.Background(), model.AllocationRequest{Pool: model.PoolFull}, foreign); err == nil || len(api.calls) != 0 {
		t.Fatalf("foreign repository must not create a pod: err=%v calls=%d", err, len(api.calls))
	}

	owned := model.AllocationStatus{ID: "y", Pool: model.PoolFull, Repository: "example-owner/deskquest"}
	api.responses = map[string]response{"POST /api/v1/namespaces/arc-runners/pods": {http.StatusConflict, `{}`}}
	if _, err := b.Provision(context.Background(), model.AllocationRequest{Pool: model.PoolFull}, owned); err != nil {
		t.Fatalf("an existing pod (retry) should be accepted: %v", err)
	}

	api.responses["POST /api/v1/namespaces/arc-runners/pods"] = response{http.StatusForbidden, `{"message":"pods \"x\" is forbidden: exceeded quota: runners"}`}
	_, err := b.Provision(context.Background(), model.AllocationRequest{Pool: model.PoolFull}, owned)
	if !backend.IsCapacityExhausted(err) {
		t.Fatalf("quota rejection should be capacity exhaustion, got %v", err)
	}

	api.responses["POST /api/v1/namespaces/arc-runners/pods"] = response{http.StatusInternalServerError, `boom`}
	if _, err := b.Provision(context.Background(), model.AllocationRequest{Pool: model.PoolFull}, owned); err == nil || backend.IsCapacityExhausted(err) {
		t.Fatalf("server error should fail without capacity exhaustion, got %v", err)
	}

	if _, err := New(newConfig(t), nil).Provision(context.Background(), model.AllocationRequest{Pool: model.PoolFull}, owned); err == nil {
		t.Fatal("no Kubernetes API should fail")
	}
}

func TestCleanupDeletesPodAndToleratesMissing(t *testing.T) {
	api := &fakeAPI{responses: map[string]response{
		"DELETE /api/v1/namespaces/arc-runners/pods/uecb-runner-gone": {http.StatusNotFound, `{}`},
		"DELETE /api/v1/namespaces/arc-runners/pods/uecb-runner-bad":  {http.StatusInternalServerError, `{"message":"etcd down"}`},
	}}
	b := New(newConfig(t), api)
	for _, id := range []string{"live", "gone"} {
		if err := b.Cleanup(context.Background(), model.AllocationStatus{ID: id, Pool: model.PoolFull, Metadata: map[string]string{"namespace": "arc-runners"}}); err != nil {
			t.Fatalf("Cleanup(%s): %v", id, err)
		}
	}
	if err := b.Cleanup(context.Background(), model.AllocationStatus{ID: "bad", Pool: model.PoolFull}); err == nil || !strings.Contains(err.Error(), "etcd down") {
		t.Fatalf("expected delete failure, got %v", err)
	}
	if api.calls[0].method != http.MethodDelete || api.calls[0].path != "/api/v1/namespaces/arc-runners/pods/uecb-runner-live" {
		t.Fatalf("calls = %+v", api.calls)
	}
}

func TestCapacityCountsLiveRunnerPods(t *testing.T) {
	api := &fakeAPI{responses: map[string]response{
		"GET /api/v1/namespaces/arc-runners/pods": {http.StatusOK, `{"items":[
			{"status":{"phase":"Running"}},
			{"status":{"phase":"Pending"}},
			{"status":{"phase":"Succeeded"}},
			{"status":{"phase":"Failed"}},
			{"metadata":{"deletionTimestamp":"2026-10-31T00:00:00Z"},"status":{"phase":"Running"}}
		]}`},
	}}
	status, err := New(newConfig(t), api).Capacity(context.Background())
	if err != nil {
		t.Fatalf("Capacity: %v", err)
	}
	if status.MaxRunners != 6 || status.ActiveRunners != 1 || status.PendingRunners != 1 || backend.FreeSlots(status) != 4 {
		t.Fatalf("status = %+v", status)
	}
	if !strings.Contains(api.calls[0].path, "labelSelector=") {
		t.Fatalf("capacity must list only broker pods: %s", api.calls[0].path)
	}
}

func TestValidateKubernetesBackend(t *testing.T) {
	cfg := newConfig(t)
	if err := config.Validate(cfg); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	for i := range cfg.Pools {
		if cfg.Pools[i].Name == model.PoolFull {
			backendCfg := cfg.Pools[i].Backends[model.BackendKubernetes]
			backendCfg.Kubernetes = &model.KubernetesBackendConfig{}
			cfg.Pools[i].Backends[model.BackendKubernetes] = backendCfg
		}
	}
	if err := config.Validate(cfg); err == nil || !strings.Contains(err.Error(), "image") {
		t.Fatalf("missing image should be rejected, got %v", err)
	}
}
