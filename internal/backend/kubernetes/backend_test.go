package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/config"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/github"
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

type registration struct {
	scope  model.GitHubScope
	group  string
	name   string
	labels []string
}

type fakeRegistrar struct {
	registered []registration
	deleted    []int64
	nextID     int64
	err        error
	deleteErr  error
}

func (f *fakeRegistrar) GenerateJITConfig(_ context.Context, scope model.GitHubScope, group, name string, labels []string) (github.JITConfig, error) {
	if f.err != nil {
		return github.JITConfig{}, f.err
	}
	f.nextID++
	f.registered = append(f.registered, registration{scope, group, name, labels})
	return github.JITConfig{RunnerID: 100 + f.nextID, Encoded: "jit-" + name}, nil
}

func (f *fakeRegistrar) DeleteRunner(_ context.Context, _ model.GitHubScope, id int64) error {
	f.deleted = append(f.deleted, id)
	return f.deleteErr
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
      - name: ACTIONS_RUNNER_INPUT_JITCONFIG
        value: must-be-replaced
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
				PodSpec:   podSpec,
			},
		}
	}
	return cfg
}

func envMap(container map[string]any) map[string]any {
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

func TestProvisionRegistersRunnerAndPassesOnlyJITConfig(t *testing.T) {
	api := &fakeAPI{}
	reg := &fakeRegistrar{}
	b := New(newConfig(t), api, reg)
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

	if len(reg.registered) != 1 {
		t.Fatalf("registrations = %+v", reg.registered)
	}
	got := reg.registered[0]
	if got.scope.Type != model.GitHubScopeRepository || got.scope.Owner != "example-owner" || got.scope.Repository != "questkit" {
		t.Fatalf("runner registered on %+v, want example-owner/questkit", got.scope)
	}
	if got.name != "uecb-kubernetes-Alloc_123" || strings.Join(got.labels, ",") != "self-hosted,linux,x64,uecb,uecb-full,uecb-kubernetes-Alloc_123" {
		t.Fatalf("registration = %+v", got)
	}
	if provisioned.RunnerLabel != "uecb-kubernetes-Alloc_123" || provisioned.Metadata[metadataRunnerID] != "101" {
		t.Fatalf("provisioned = %+v", provisioned)
	}
	if provisioned.Metadata["target_url"] != "https://github.com/example-owner/questkit" || provisioned.Metadata["namespace"] != "arc-runners" {
		t.Fatalf("metadata = %+v", provisioned.Metadata)
	}

	if len(api.calls) != 1 || api.calls[0].method != http.MethodPost || api.calls[0].path != "/api/v1/namespaces/arc-runners/pods" {
		t.Fatalf("calls = %+v", api.calls)
	}
	pod := api.calls[0].body
	metadata := pod["metadata"].(map[string]any)
	if metadata["name"] != "uecb-runner-alloc-123" || metadata["annotations"].(map[string]any)[annotationRepository] != "example-owner/questkit" {
		t.Fatalf("metadata = %+v", metadata)
	}
	spec := pod["spec"].(map[string]any)
	if spec["restartPolicy"] != "Never" || spec["activeDeadlineSeconds"].(float64) != 30*60 {
		t.Fatalf("spec = %+v", spec)
	}
	containers := spec["containers"].([]any)
	runner := containers[0].(map[string]any)
	if runner["image"] != defaultRunnerImage || runner["command"].([]any)[0] != defaultRunnerCommand {
		t.Fatalf("runner = %+v", runner)
	}
	env := envMap(runner)
	if env[envJITConfig] != "jit-uecb-kubernetes-Alloc_123" || env["DOCKER_HOST"] == nil || len(env) != 2 {
		t.Fatalf("env = %+v", env)
	}
	encoded, _ := json.Marshal(pod)
	for _, leak := range []string{"GITHUB_TOKEN", "secretKeyRef", "uecb-github-app", "must-be-replaced"} {
		if strings.Contains(string(encoded), leak) {
			t.Errorf("pod must not carry %q: %s", leak, encoded)
		}
	}
	if containers[1].(map[string]any)["image"] != "docker:dind" {
		t.Fatalf("sidecar changed: %+v", containers[1])
	}
	if len(reg.deleted) != 0 {
		t.Fatalf("a successful provision must keep its registration: %v", reg.deleted)
	}
}

func TestProvisionFailuresRemoveTheRegistration(t *testing.T) {
	owned := model.AllocationStatus{ID: "y", Pool: model.PoolFull, Repository: "example-owner/deskquest"}

	for _, tc := range []struct {
		name     string
		resp     response
		capacity bool
	}{
		{"conflict", response{http.StatusConflict, `{}`}, false},
		{"quota", response{http.StatusForbidden, `{"message":"pods \"x\" is forbidden: exceeded quota: runners"}`}, true},
		{"server error", response{http.StatusInternalServerError, `boom`}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &fakeAPI{responses: map[string]response{"POST /api/v1/namespaces/arc-runners/pods": tc.resp}}
			reg := &fakeRegistrar{}
			_, err := New(newConfig(t), api, reg).Provision(context.Background(), model.AllocationRequest{Pool: model.PoolFull}, owned)
			if err == nil || backend.IsCapacityExhausted(err) != tc.capacity {
				t.Fatalf("err = %v, capacity exhausted want %v", err, tc.capacity)
			}
			if len(reg.deleted) != 1 || reg.deleted[0] != 101 {
				t.Fatalf("registration not removed: %v", reg.deleted)
			}
		})
	}

	api := &fakeAPI{}
	reg := &fakeRegistrar{}
	b := New(newConfig(t), api, reg)
	if _, err := b.Provision(context.Background(), model.AllocationRequest{Pool: model.PoolFull}, model.AllocationStatus{ID: "x", Pool: model.PoolFull, Repository: "someone-else/repo"}); err == nil || len(reg.registered) != 0 || len(api.calls) != 0 {
		t.Fatalf("a foreign repository must get neither a registration nor a pod: err=%v", err)
	}
	reg.err = errors.New("github down")
	if _, err := b.Provision(context.Background(), model.AllocationRequest{Pool: model.PoolFull}, owned); err == nil || len(api.calls) != 0 {
		t.Fatalf("no pod without a registration: err=%v calls=%d", err, len(api.calls))
	}
	if _, err := New(newConfig(t), api, nil).Provision(context.Background(), model.AllocationRequest{Pool: model.PoolFull}, owned); err == nil {
		t.Fatal("no registrar should fail")
	}
	if _, err := New(newConfig(t), nil, reg).Provision(context.Background(), model.AllocationRequest{Pool: model.PoolFull}, owned); err == nil {
		t.Fatal("no Kubernetes API should fail")
	}
}

func TestCleanupDeletesPodAndRegistration(t *testing.T) {
	api := &fakeAPI{responses: map[string]response{
		"DELETE /api/v1/namespaces/arc-runners/pods/uecb-runner-gone": {http.StatusNotFound, `{}`},
		"DELETE /api/v1/namespaces/arc-runners/pods/uecb-runner-bad":  {http.StatusInternalServerError, `{"message":"etcd down"}`},
	}}
	reg := &fakeRegistrar{}
	b := New(newConfig(t), api, reg)
	status := func(id, runnerID string) model.AllocationStatus {
		return model.AllocationStatus{ID: id, Pool: model.PoolFull, Repository: "example-owner/questkit",
			Metadata: map[string]string{"namespace": "arc-runners", metadataRunnerID: runnerID}}
	}
	if err := b.Cleanup(context.Background(), status("live", "7")); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if err := b.Cleanup(context.Background(), status("gone", "8")); err != nil {
		t.Fatalf("missing pod: %v", err)
	}
	if strings.Join([]string{api.calls[0].method, api.calls[0].path}, " ") != "DELETE /api/v1/namespaces/arc-runners/pods/uecb-runner-live" {
		t.Fatalf("calls = %+v", api.calls)
	}
	if len(reg.deleted) != 2 || reg.deleted[0] != 7 || reg.deleted[1] != 8 {
		t.Fatalf("registrations deleted = %v", reg.deleted)
	}

	// A runner still running its job (422) is left for GitHub to remove.
	reg.deleteErr = &github.StatusError{Code: http.StatusUnprocessableEntity, Message: "runner busy"}
	if err := b.Cleanup(context.Background(), status("busy", "9")); err != nil {
		t.Fatalf("busy runner: %v", err)
	}
	reg.deleteErr = errors.New("github down")
	if err := b.Cleanup(context.Background(), status("bad", "10")); err == nil || !strings.Contains(err.Error(), "etcd down") || !strings.Contains(err.Error(), "github down") {
		t.Fatalf("expected both failures, got %v", err)
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
	status, err := New(newConfig(t), api, &fakeRegistrar{}).Capacity(context.Background())
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
	cfg.GitHub.Auth.SecretRef = ""
	if err := config.Validate(cfg); err == nil || !strings.Contains(err.Error(), "secretRef") {
		t.Fatalf("missing github.auth.secretRef should be rejected, got %v", err)
	}
}

func TestLiveRunnersRebuildsIdentityFromPods(t *testing.T) {
	cfg := newConfig(t)
	api := &fakeAPI{responses: map[string]response{
		"GET /api/v1/namespaces/arc-runners/pods": {status: http.StatusOK, body: `{"items":[
 {"metadata":{"name":"uecb-runner-alloc-1","labels":{"uecb.josh-archer.dev/pool":"full","uecb.josh-archer.dev/allocation-id":"alloc-1"},
  "annotations":{"uecb.josh-archer.dev/allocation-id":"alloc-1","uecb.josh-archer.dev/github-runner-id":"9","uecb.josh-archer.dev/workflow-job-id":"55","uecb.josh-archer.dev/repository":"example/project"}},"status":{"phase":"Running"}},
 {"metadata":{"name":"uecb-runner-alloc-2","labels":{"uecb.josh-archer.dev/pool":"full","uecb.josh-archer.dev/allocation-id":"alloc-2"}},"status":{"phase":"Succeeded"}},
 {"metadata":{"name":"uecb-runner-alloc-3","deletionTimestamp":"2026-10-31T00:00:00Z","labels":{"uecb.josh-archer.dev/allocation-id":"alloc-3"}},"status":{"phase":"Running"}},
 {"metadata":{"name":"something-else","labels":{"uecb.josh-archer.dev/allocation-id":"alloc-4"}},"status":{"phase":"Running"}}
]}`},
	}}
	b := New(cfg, api, &fakeRegistrar{})
	runners, err := b.LiveRunners(context.Background())
	if err != nil {
		t.Fatalf("LiveRunners: %v", err)
	}
	if len(runners) != 1 {
		t.Fatalf("expected 1 live runner, got %+v", runners)
	}
	r := runners[0]
	if r.AllocationID != "alloc-1" || r.Pool != "full" || r.WorkflowJobID != 55 || r.Repository != "example/project" || r.Metadata[metadataRunnerID] != "9" || r.Metadata["pod_name"] != "uecb-runner-alloc-1" {
		t.Fatalf("unexpected live runner: %+v", r)
	}
}

func TestProvisionAnnotatesPodWithAllocationIdentity(t *testing.T) {
	api := &fakeAPI{}
	b := New(newConfig(t), api, &fakeRegistrar{})
	request := model.AllocationRequest{Pool: model.PoolFull, WorkflowJobID: 31, TargetRepository: "example-owner/questkit"}
	if _, err := b.Provision(context.Background(), request, model.AllocationStatus{ID: "Alloc_9", Pool: model.PoolFull, Request: &request}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	var annotations map[string]any
	for _, c := range api.calls {
		if c.method == http.MethodPost {
			annotations = c.body["metadata"].(map[string]any)["annotations"].(map[string]any)
		}
	}
	if annotations[annotationAllocationID] != "Alloc_9" || annotations[annotationRunnerID] != "101" || annotations[annotationWorkflowJob] != "31" {
		t.Fatalf("annotations = %+v", annotations)
	}
}

func TestFinishedRunnersListsExitedPods(t *testing.T) {
	cfg := newConfig(t)
	api := &fakeAPI{responses: map[string]response{
		"GET /api/v1/namespaces/arc-runners/pods": {status: http.StatusOK, body: `{"items":[
 {"metadata":{"name":"uecb-runner-alloc-1","labels":{"uecb.josh-archer.dev/pool":"full","uecb.josh-archer.dev/allocation-id":"alloc-1"}},"status":{"phase":"Running"}},
 {"metadata":{"name":"uecb-runner-alloc-2","labels":{"uecb.josh-archer.dev/pool":"full","uecb.josh-archer.dev/allocation-id":"alloc-2"},
  "annotations":{"uecb.josh-archer.dev/allocation-id":"alloc-2","uecb.josh-archer.dev/github-runner-id":"12","uecb.josh-archer.dev/repository":"example/project"}},"status":{"phase":"Succeeded"}},
 {"metadata":{"name":"uecb-runner-alloc-3","labels":{"uecb.josh-archer.dev/pool":"full","uecb.josh-archer.dev/allocation-id":"alloc-3"}},"status":{"phase":"Failed"}},
 {"metadata":{"name":"uecb-runner-alloc-4","deletionTimestamp":"2026-10-31T00:00:00Z","labels":{"uecb.josh-archer.dev/allocation-id":"alloc-4"}},"status":{"phase":"Succeeded"}},
 {"metadata":{"name":"something-else","labels":{"uecb.josh-archer.dev/allocation-id":"alloc-5"}},"status":{"phase":"Succeeded"}}
]}`},
	}}
	runners, err := New(cfg, api, &fakeRegistrar{}).FinishedRunners(context.Background())
	if err != nil {
		t.Fatalf("FinishedRunners: %v", err)
	}
	if len(runners) != 2 {
		t.Fatalf("expected the succeeded and failed pods, got %+v", runners)
	}
	byID := map[string]bool{}
	for _, r := range runners {
		byID[r.AllocationID] = r.Failed
		if r.AllocationID == "alloc-2" && (r.Repository != "example/project" || r.Metadata[metadataRunnerID] != "12" || r.Metadata["namespace"] != "arc-runners") {
			t.Fatalf("unexpected identity: %+v", r)
		}
	}
	if failed, ok := byID["alloc-2"]; !ok || failed {
		t.Fatalf("alloc-2 should be a succeeded runner: %+v", runners)
	}
	if failed, ok := byID["alloc-3"]; !ok || !failed {
		t.Fatalf("alloc-3 should be a failed runner: %+v", runners)
	}
}

const notReadyNode = `{"kind":"Node","metadata":{"name":"desktop-node"},"status":{"conditions":[{"type":"Ready","status":"Unknown"}]}}`
const readyNode = `{"kind":"Node","metadata":{"name":"worker-node"},"status":{"conditions":[{"type":"Ready","status":"True"}]}}`

// Regression: Terminating pods and pods on a NotReady node
// must not count as used slots.
func TestCapacityExcludesTerminatingAndNotReadyNodePods(t *testing.T) {
	api := &fakeAPI{responses: map[string]response{
		"GET /api/v1/nodes/desktop-node": {http.StatusOK, notReadyNode},
		"GET /api/v1/nodes/worker-node":      {http.StatusOK, readyNode},
		"GET /api/v1/nodes/gone":           {http.StatusNotFound, `{}`},
		"GET /api/v1/namespaces/arc-runners/pods": {http.StatusOK, `{"items":[
			{"spec":{"nodeName":"worker-node"},"status":{"phase":"Running"}},
			{"spec":{"nodeName":"worker-node"},"status":{"phase":"Pending"}},
			{"status":{"phase":"Pending"}},
			{"metadata":{"deletionTimestamp":"2026-11-01T00:00:00Z"},"spec":{"nodeName":"worker-node"},"status":{"phase":"Running"}},
			{"spec":{"nodeName":"desktop-node"},"status":{"phase":"Running"}},
			{"spec":{"nodeName":"desktop-node"},"status":{"phase":"Pending"}},
			{"spec":{"nodeName":"gone"},"status":{"phase":"Running"}}
		]}`},
	}}
	status, err := New(newConfig(t), api, &fakeRegistrar{}).Capacity(context.Background())
	if err != nil {
		t.Fatalf("Capacity: %v", err)
	}
	if status.ActiveRunners != 1 || status.PendingRunners != 2 {
		t.Fatalf("status = %+v", status)
	}
	nodeLookups := 0
	for _, c := range api.calls {
		if strings.HasPrefix(c.path, "/api/v1/nodes/desktop-node") {
			nodeLookups++
		}
	}
	if nodeLookups != 1 {
		t.Fatalf("node readiness should be cached per pass, got %d lookups", nodeLookups)
	}
}

func TestLiveRunnersExcludesPodsOnNotReadyNode(t *testing.T) {
	api := &fakeAPI{responses: map[string]response{
		"GET /api/v1/nodes/desktop-node": {http.StatusOK, notReadyNode},
		"GET /api/v1/namespaces/arc-runners/pods": {http.StatusOK, `{"items":[
 {"metadata":{"name":"uecb-runner-alloc-1","labels":{"uecb.josh-archer.dev/pool":"full","uecb.josh-archer.dev/allocation-id":"alloc-1"}},"spec":{"nodeName":"desktop-node"},"status":{"phase":"Running"}}
]}`},
	}}
	runners, err := New(newConfig(t), api, &fakeRegistrar{}).LiveRunners(context.Background())
	if err != nil {
		t.Fatalf("LiveRunners: %v", err)
	}
	if len(runners) != 0 {
		t.Fatalf("pod on NotReady node must not be live: %+v", runners)
	}
}
