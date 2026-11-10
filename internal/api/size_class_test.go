package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	kubernetesbackend "github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend/kubernetes"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/config"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/github"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	"gopkg.in/yaml.v3"
)

const sizeClassPodSpec = `
containers:
  - name: runner
    resources:
      requests: {cpu: 500m, memory: 1Gi, ephemeral-storage: 8Gi}
  - name: dind
    image: docker:dind
    resources:
      requests: {cpu: 250m, memory: 512Mi, ephemeral-storage: 8Gi}
`

type sizeClassCluster struct {
	pods []map[string]any
}

func (c *sizeClassCluster) Namespace() string { return "runners" }

func (c *sizeClassCluster) Do(_ context.Context, method, path string, body any) (int, []byte, error) {
	if method == http.MethodGet && strings.Contains(path, "/pods") {
		return http.StatusOK, []byte(`{"items":[]}`), nil
	}
	if method == http.MethodPost && strings.Contains(path, "/pods") {
		raw, err := json.Marshal(body)
		if err != nil {
			return http.StatusBadRequest, nil, err
		}
		var pod map[string]any
		if err := json.Unmarshal(raw, &pod); err != nil {
			return http.StatusBadRequest, nil, err
		}
		c.pods = append(c.pods, pod)
		return http.StatusCreated, []byte(`{}`), nil
	}
	return http.StatusNotFound, []byte(`{"message":"not found"}`), nil
}

type sizeClassRegistrar struct {
	labels [][]string
}

func (r *sizeClassRegistrar) GenerateJITConfig(_ context.Context, _ model.GitHubScope, _, _ string, labels []string) (github.JITConfig, error) {
	r.labels = append(r.labels, append([]string(nil), labels...))
	return github.JITConfig{RunnerID: int64(len(r.labels)), Encoded: "jit"}, nil
}

func (r *sizeClassRegistrar) DeleteRunner(context.Context, model.GitHubScope, int64) error {
	return nil
}

func sizeClassService(t *testing.T) (*Service, *sizeClassCluster, *sizeClassRegistrar) {
	t.Helper()
	cfg := config.Default()
	var podSpec map[string]any
	if err := yaml.Unmarshal([]byte(sizeClassPodSpec), &podSpec); err != nil {
		t.Fatal(err)
	}
	cfg.Pools[0].Backends[model.BackendKubernetes] = model.BackendConfig{
		Enabled:    true,
		Healthy:    true,
		MaxRunners: 4,
		Kubernetes: &model.KubernetesBackendConfig{Namespace: "runners", PodSpec: podSpec},
	}
	cfg.Broker.SizeClasses = map[string]model.SizeClassConfig{
		"small": {
			Runner: model.ContainerResources{Requests: model.ResourceQuantity{CPU: "500m", Memory: "1Gi", EphemeralStorage: "8Gi"}},
			Dind:   model.ContainerResources{Requests: model.ResourceQuantity{CPU: "250m", Memory: "512Mi", EphemeralStorage: "8Gi"}},
		},
		"large": {
			Runner: model.ContainerResources{
				Requests: model.ResourceQuantity{CPU: "2", Memory: "4Gi", EphemeralStorage: "16Gi"},
				Limits:   model.ResourceQuantity{CPU: "2", Memory: "4Gi", EphemeralStorage: "16Gi"},
			},
			Dind: model.ContainerResources{
				Requests: model.ResourceQuantity{CPU: "1", Memory: "2Gi", EphemeralStorage: "16Gi"},
				Limits:   model.ResourceQuantity{CPU: "2", Memory: "4Gi", EphemeralStorage: "16Gi"},
			},
			Backends: []model.BackendName{model.BackendKubernetes},
		},
	}
	cluster := &sizeClassCluster{}
	registrar := &sizeClassRegistrar{}
	kube := kubernetesbackend.New(cfg, cluster, registrar)
	service := NewService(cfg, backend.NewRegistry(testBackend{name: model.BackendARC}, kube), nil)
	return service, cluster, registrar
}

func podQuantity(pod map[string]any, container, side, key string) string {
	spec := pod["spec"].(map[string]any)
	for _, raw := range spec["containers"].([]any) {
		item := raw.(map[string]any)
		if item["name"] != container {
			continue
		}
		resources, _ := item["resources"].(map[string]any)
		section, _ := resources[side].(map[string]any)
		value, _ := section[key].(string)
		return value
	}
	return ""
}

func TestAllocateMapsSizeClassOntoTheRunnerPod(t *testing.T) {
	service, cluster, registrar := sizeClassService(t)
	ctx := context.Background()

	large, err := service.Allocate(ctx, model.AllocationRequest{
		Pool:       model.PoolFull,
		JobTimeout: time.Minute,
		Labels:     []string{"self-hosted", "Size-Large"},
	})
	if err != nil {
		t.Fatalf("size-large: %v", err)
	}
	if large.SelectedBackend != model.BackendKubernetes {
		t.Fatalf("size-large backend = %s, want kubernetes (the class allow-list)", large.SelectedBackend)
	}
	if !strings.Contains(strings.Join(large.RequestedLabels, ","), "size-large") {
		t.Fatalf("requested labels = %#v", large.RequestedLabels)
	}
	if large.Metadata["size_class"] != "large" {
		t.Fatalf("metadata = %#v", large.Metadata)
	}
	if len(cluster.pods) != 1 {
		t.Fatalf("pods = %d", len(cluster.pods))
	}
	pod := cluster.pods[0]
	if podQuantity(pod, "runner", "requests", "cpu") != "2" ||
		podQuantity(pod, "runner", "requests", "memory") != "4Gi" ||
		podQuantity(pod, "runner", "requests", "ephemeral-storage") != "16Gi" ||
		podQuantity(pod, "runner", "limits", "memory") != "4Gi" ||
		podQuantity(pod, "dind", "requests", "cpu") != "1" ||
		podQuantity(pod, "dind", "requests", "memory") != "2Gi" ||
		podQuantity(pod, "dind", "limits", "ephemeral-storage") != "16Gi" {
		t.Fatalf("pod spec = %+v", pod["spec"])
	}
	if joined := strings.Join(registrar.labels[0], ","); !strings.Contains(joined, "size-large") {
		t.Fatalf("runner registration labels = %s", joined)
	}

	pinned := model.BackendKubernetes
	plain, err := service.Allocate(ctx, model.AllocationRequest{
		Pool:       model.PoolFull,
		Backend:    &pinned,
		JobTimeout: time.Minute,
		Labels:     []string{"self-hosted"},
	})
	if err != nil {
		t.Fatalf("unlabeled: %v", err)
	}
	if plain.Metadata["size_class"] != "" {
		t.Fatalf("unlabeled job stored a size class: %#v", plain.Metadata)
	}
	pod = cluster.pods[1]
	if podQuantity(pod, "runner", "requests", "cpu") != "500m" ||
		podQuantity(pod, "runner", "requests", "memory") != "1Gi" ||
		podQuantity(pod, "runner", "requests", "ephemeral-storage") != "8Gi" ||
		podQuantity(pod, "dind", "requests", "cpu") != "250m" ||
		podQuantity(pod, "dind", "requests", "memory") != "512Mi" {
		t.Fatalf("default pod changed the template: %+v", pod["spec"])
	}
	for _, label := range registrar.labels[1] {
		if strings.HasPrefix(label, "size-") {
			t.Fatalf("unlabeled runner registered %q", label)
		}
	}

	before := len(cluster.pods)
	_, err = service.Allocate(ctx, model.AllocationRequest{Pool: model.PoolFull, JobTimeout: time.Minute, Labels: []string{"size-huge"}})
	if err == nil || !errors.Is(err, model.ErrUnknownSizeClass) || !strings.Contains(err.Error(), `unknown runner size class "huge"`) {
		t.Fatalf("unknown class = %v", err)
	}
	var allocErr *AllocationError
	if !errors.As(err, &allocErr) || allocErr.ErrorCode != "unknown_size_class" {
		t.Fatalf("error code = %#v", err)
	}
	if len(cluster.pods) != before {
		t.Fatal("unknown class created a pod")
	}

	_, err = service.Allocate(ctx, model.AllocationRequest{Pool: model.PoolFull, JobTimeout: time.Minute, Labels: []string{"size-small", "size-large"}})
	if err == nil || !strings.Contains(err.Error(), "more than one runner size class") {
		t.Fatalf("multiple = %v", err)
	}

	arc := model.BackendARC
	_, err = service.Allocate(ctx, model.AllocationRequest{Pool: model.PoolFull, Backend: &arc, JobTimeout: time.Minute, Labels: []string{"size-large"}})
	if err == nil || !strings.Contains(err.Error(), `pinned backend "arc" cannot serve runner size class "large"`) {
		t.Fatalf("pinned arc = %v", err)
	}
}

func TestWebhookSizeLabelIsNotAPoolLabel(t *testing.T) {
	h := newWebhookHarness(t, true)
	h.service.cfg.Broker.SizeClasses = map[string]model.SizeClassConfig{
		"large": {Runner: model.ContainerResources{Requests: model.ResourceQuantity{CPU: "2", Memory: "4Gi", EphemeralStorage: "16Gi"}}},
	}

	if _, ok := h.service.MatchWebhookPool([]string{"self-hosted", "uecb-full", "size-large"}); !ok {
		t.Fatal("size-large must not keep a job out of a pool that covers its other labels")
	}
	if _, ok := h.service.MatchWebhookPool([]string{"size-large"}); ok {
		t.Fatal("a size label alone must not match a pool")
	}
	if _, ok := h.service.MatchWebhookPool([]string{"self-hosted", "size-small"}); ok {
		t.Fatal("size-small must not satisfy the specific-label requirement by itself")
	}

	rec := h.send("workflow_job", "size-large", jobBody("queued", 77, "", "self-hosted", "uecb-full", "size-large"))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("queued: %d %s", rec.Code, rec.Body)
	}
	allocation, ok := h.service.findByWorkflowJobID(77)
	if !ok || allocation.State != model.StateReady {
		t.Fatalf("allocation = %+v ok=%v", allocation, ok)
	}
	if strings.Join(allocation.RequestedLabels, ",") != "self-hosted,uecb-full,size-large" {
		t.Fatalf("requested labels = %#v", allocation.RequestedLabels)
	}
	if allocation.Metadata["size_class"] != "large" {
		t.Fatalf("metadata = %#v", allocation.Metadata)
	}

	before := counterValue(t, "uecb_github_webhook_events_ignored_total", map[string]string{"reason": "unknown_size_class"})
	rec = h.send("workflow_job", "size-huge", jobBody("queued", 78, "", "self-hosted", "uecb-full", "size-huge"))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("unknown queued delivery: %d %s", rec.Code, rec.Body)
	}
	if _, ok := h.service.findByWorkflowJobID(78); ok {
		t.Fatal("unknown size class must not allocate")
	}
	if got := counterValue(t, "uecb_github_webhook_events_ignored_total", map[string]string{"reason": "unknown_size_class"}); got != before+1 {
		t.Fatalf("unknown_size_class metric = %v, want %v", got, before+1)
	}

	_, _, err := h.service.AllocateForWorkflowJob(context.Background(), h.service.cfg.Pools[0], 79, "my-org/home", []string{"self-hosted", "uecb-full", "size-huge"})
	if err == nil || !errors.Is(err, model.ErrUnknownSizeClass) || !strings.Contains(err.Error(), `unknown runner size class "huge"`) {
		t.Fatalf("direct webhook allocate = %v", err)
	}
}

func TestWarmCandidateServesSize(t *testing.T) {
	if !warmCandidateServesSize(nil, nil) {
		t.Fatal("unlabeled job should take an unlabeled warm runner")
	}
	if !warmCandidateServesSize(nil, []string{"size-large"}) {
		t.Fatal("unlabeled job may take a sized warm runner")
	}
	if warmCandidateServesSize([]string{"size-large"}, nil) {
		t.Fatal("sized job must not take an unlabeled warm runner")
	}
	if warmCandidateServesSize([]string{"Size-Large"}, []string{"size-small"}) {
		t.Fatal("size-large must not take size-small")
	}
	if !warmCandidateServesSize([]string{"uecb-full", "Size-Large"}, []string{"size-large"}) {
		t.Fatal("canonical size-large should match")
	}
}

func TestAllocateSizeClassDoesNotConsumeUnlabeledWarmRunner(t *testing.T) {
	now := time.Unix(1000, 0)
	counting := &countingBackend{testBackend: testBackend{name: model.BackendEC2}}
	service := newServiceWithCountingBackend(func(pool *model.PoolConfig) {
		if pool.Name != model.PoolLite {
			return
		}
		for name, backendCfg := range pool.Backends {
			backendCfg.Enabled = false
			pool.Backends[name] = backendCfg
		}
		codebuildCfg := pool.Backends[model.BackendEC2]
		codebuildCfg.Enabled = true
		codebuildCfg.Healthy = true
		codebuildCfg.MaxRunners = 2
		codebuildCfg.WarmMin = 1
		codebuildCfg.WarmMax = 1
		pool.Backends[model.BackendEC2] = codebuildCfg
	}, counting)
	service.cfg.Broker.SizeClasses = map[string]model.SizeClassConfig{
		"large": {
			Runner: model.ContainerResources{Requests: model.ResourceQuantity{CPU: "2", Memory: "4Gi"}},
		},
	}
	service.now = func() time.Time { return now }
	service.ReconcileWarmPools()

	allocation, err := service.Allocate(context.Background(), model.AllocationRequest{
		Pool:       model.PoolLite,
		JobTimeout: 5 * time.Minute,
		Labels:     []string{"size-large"},
	})
	if err != nil {
		t.Fatalf("allocation failed: %v", err)
	}
	if allocation.Metadata[backend.MetadataLaunchModeKey] != launchModeCold {
		t.Fatalf("launch mode = %q, want cold", allocation.Metadata[backend.MetadataLaunchModeKey])
	}
	if allocation.Metadata["size_class"] != "large" {
		t.Fatalf("size_class = %q", allocation.Metadata["size_class"])
	}
	if !strings.Contains(strings.Join(allocation.RequestedLabels, ","), "size-large") {
		t.Fatalf("requested labels = %#v", allocation.RequestedLabels)
	}
	warmLeft := 0
	for _, status := range service.store.List() {
		if status.State == model.StateWarm {
			warmLeft++
		}
	}
	if warmLeft != 1 {
		t.Fatalf("warm runners left = %d, want 1", warmLeft)
	}
	if counting.provisionCount != 2 {
		t.Fatalf("provisions = %d, want warm prewarm plus cold sized launch", counting.provisionCount)
	}
}
