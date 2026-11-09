package kubernetes

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	"gopkg.in/yaml.v3"
)

const sizedPodSpecYAML = `
containers:
  - name: runner
    resources:
      requests: {cpu: 500m, memory: 1Gi, ephemeral-storage: 8Gi}
    env:
      - name: DOCKER_HOST
        value: unix:///var/run/docker/docker.sock
  - name: dind
    image: docker:dind
    resources:
      requests: {cpu: 250m, memory: 512Mi}
initContainers:
  - name: dind
    image: docker:dind
    restartPolicy: Always
    resources:
      requests: {cpu: 250m, memory: 512Mi}
`

func sizedConfig(t *testing.T, classes map[string]model.SizeClassConfig) model.BrokerConfig {
	t.Helper()
	cfg := newConfig(t)
	var podSpec map[string]any
	if err := yaml.Unmarshal([]byte(sizedPodSpecYAML), &podSpec); err != nil {
		t.Fatal(err)
	}
	for i := range cfg.Pools {
		if cfg.Pools[i].Name != model.PoolFull {
			continue
		}
		backendCfg := cfg.Pools[i].Backends[model.BackendKubernetes]
		backendCfg.Kubernetes.PodSpec = podSpec
		cfg.Pools[i].Backends[model.BackendKubernetes] = backendCfg
	}
	cfg.Broker.SizeClasses = classes
	return cfg
}

func containerResources(pod map[string]any, field, name, side, key string) string {
	spec := pod["spec"].(map[string]any)
	for _, raw := range spec[field].([]any) {
		container := raw.(map[string]any)
		if container["name"] != name {
			continue
		}
		resources, _ := container["resources"].(map[string]any)
		section, _ := resources[side].(map[string]any)
		value, _ := section[key].(string)
		return value
	}
	return ""
}

func TestProvisionSizeClassOverlaysRunnerAndDind(t *testing.T) {
	classes := map[string]model.SizeClassConfig{
		"large": {
			Runner: model.ContainerResources{
				Requests: model.ResourceQuantity{CPU: "2", Memory: "4Gi", EphemeralStorage: "16Gi"},
				Limits:   model.ResourceQuantity{CPU: "2", Memory: "4Gi", EphemeralStorage: "16Gi"},
			},
			Dind: model.ContainerResources{
				Requests: model.ResourceQuantity{CPU: "1", Memory: "2Gi", EphemeralStorage: "16Gi"},
				Limits:   model.ResourceQuantity{CPU: "2", Memory: "4Gi", EphemeralStorage: "16Gi"},
			},
		},
		"small": {
			Runner: model.ContainerResources{Requests: model.ResourceQuantity{Memory: "512Mi"}},
		},
	}
	api := &fakeAPI{}
	reg := &fakeRegistrar{}
	b := New(sizedConfig(t, classes), api, reg)
	request := model.AllocationRequest{
		Pool:             model.PoolFull,
		JobTimeout:       time.Minute,
		Labels:           []string{"self-hosted", "size-large"},
		TargetRepository: "example-owner/questkit",
	}
	if _, err := b.Provision(context.Background(), request, model.AllocationStatus{
		ID:              "Alloc_large",
		Pool:            model.PoolFull,
		RequestedLabels: request.Labels,
		Request:         &request,
	}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if len(reg.registered) != 1 || !containsLabel(reg.registered[0].labels, "size-large") {
		t.Fatalf("registration labels = %#v", reg.registered[0].labels)
	}
	pod := api.calls[0].body
	if containerResources(pod, "containers", "runner", "requests", "cpu") != "2" ||
		containerResources(pod, "containers", "runner", "requests", "memory") != "4Gi" ||
		containerResources(pod, "containers", "runner", "requests", "ephemeral-storage") != "16Gi" ||
		containerResources(pod, "containers", "runner", "limits", "cpu") != "2" {
		t.Fatalf("runner resources = %+v", pod["spec"].(map[string]any)["containers"])
	}
	if containerResources(pod, "containers", "dind", "requests", "cpu") != "1" ||
		containerResources(pod, "containers", "dind", "limits", "memory") != "4Gi" ||
		containerResources(pod, "initContainers", "dind", "requests", "ephemeral-storage") != "16Gi" ||
		containerResources(pod, "initContainers", "dind", "requests", "cpu") != "1" {
		t.Fatalf("dind resources were not overlaid: %+v", pod["spec"])
	}
	meta := pod["metadata"].(map[string]any)
	if meta["labels"].(map[string]any)[labelSizeClass] != "large" || meta["annotations"].(map[string]any)[annotationSizeClass] != "large" {
		t.Fatalf("size class not recorded: %+v", meta)
	}
	env := envMap(pod["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any))
	if env["DOCKER_HOST"] == nil || env[envJITConfig] == nil {
		t.Fatalf("runner env was dropped: %+v", env)
	}

	// A class that sets only memory must keep the template CPU and disk.
	api = &fakeAPI{}
	reg = &fakeRegistrar{}
	b = New(sizedConfig(t, classes), api, reg)
	small := model.AllocationRequest{Pool: model.PoolFull, Labels: []string{"size-small"}, TargetRepository: "example-owner/questkit"}
	if _, err := b.Provision(context.Background(), small, model.AllocationStatus{ID: "Alloc_small", Pool: model.PoolFull, RequestedLabels: small.Labels, Request: &small}); err != nil {
		t.Fatalf("small: %v", err)
	}
	pod = api.calls[0].body
	if containerResources(pod, "containers", "runner", "requests", "memory") != "512Mi" ||
		containerResources(pod, "containers", "runner", "requests", "cpu") != "500m" ||
		containerResources(pod, "containers", "runner", "requests", "ephemeral-storage") != "8Gi" ||
		containerResources(pod, "containers", "dind", "requests", "cpu") != "250m" {
		t.Fatalf("partial overlay = %+v", pod["spec"].(map[string]any)["containers"])
	}
}

func TestProvisionWithoutSizeClassKeepsThePodTemplate(t *testing.T) {
	api := &fakeAPI{}
	reg := &fakeRegistrar{}
	b := New(sizedConfig(t, map[string]model.SizeClassConfig{
		"large": {Runner: model.ContainerResources{Requests: model.ResourceQuantity{CPU: "2", Memory: "4Gi"}}},
	}), api, reg)
	request := model.AllocationRequest{Pool: model.PoolFull, Labels: []string{"self-hosted", "uecb-full"}, TargetRepository: "example-owner/questkit"}
	if _, err := b.Provision(context.Background(), request, model.AllocationStatus{
		ID:              "Alloc_default",
		Pool:            model.PoolFull,
		RequestedLabels: request.Labels,
		Request:         &request,
	}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	pod := api.calls[0].body
	if containerResources(pod, "containers", "runner", "requests", "cpu") != "500m" ||
		containerResources(pod, "containers", "runner", "requests", "memory") != "1Gi" ||
		containerResources(pod, "containers", "runner", "requests", "ephemeral-storage") != "8Gi" ||
		containerResources(pod, "containers", "dind", "requests", "cpu") != "250m" ||
		containerResources(pod, "containers", "dind", "requests", "memory") != "512Mi" {
		t.Fatalf("default changed the template: %+v", pod["spec"])
	}
	meta := pod["metadata"].(map[string]any)
	if _, ok := meta["labels"].(map[string]any)[labelSizeClass]; ok {
		t.Fatal("implicit default must not add a size-class pod label")
	}
	if _, ok := meta["annotations"].(map[string]any)[annotationSizeClass]; ok {
		t.Fatal("implicit default must not add a size-class annotation")
	}
	for _, label := range reg.registered[0].labels {
		if label == "size-default" || label == "size-large" {
			t.Fatalf("default registration gained a size label: %#v", reg.registered[0].labels)
		}
	}

	// An explicit default entry is today's shape when the operator writes it down,
	// and it does replace the template fields it sets.
	api = &fakeAPI{}
	b = New(sizedConfig(t, map[string]model.SizeClassConfig{
		"default": {Runner: model.ContainerResources{Requests: model.ResourceQuantity{CPU: "1", Memory: "2Gi", EphemeralStorage: "16Gi"}}},
	}), api, reg)
	configured := model.AllocationRequest{Pool: model.PoolFull, TargetRepository: "example-owner/questkit"}
	if _, err := b.Provision(context.Background(), configured, model.AllocationStatus{ID: "Alloc_cfgdefault", Pool: model.PoolFull, Request: &configured}); err != nil {
		t.Fatalf("configured default: %v", err)
	}
	pod = api.calls[0].body
	if containerResources(pod, "containers", "runner", "requests", "cpu") != "1" ||
		containerResources(pod, "containers", "runner", "requests", "memory") != "2Gi" {
		t.Fatalf("configured default did not apply: %+v", pod["spec"].(map[string]any)["containers"])
	}
}

func TestProvisionUnknownSizeClassDoesNotRegister(t *testing.T) {
	api := &fakeAPI{}
	reg := &fakeRegistrar{}
	b := New(sizedConfig(t, nil), api, reg)
	_, err := b.Provision(context.Background(), model.AllocationRequest{Pool: model.PoolFull, Labels: []string{"size-huge"}, TargetRepository: "example-owner/questkit"}, model.AllocationStatus{ID: "Alloc_bad", Pool: model.PoolFull})
	if err == nil || !errors.Is(err, model.ErrUnknownSizeClass) || err.Error() != `unknown runner size class "huge"` {
		t.Fatalf("err = %v", err)
	}
	if len(reg.registered) != 0 || len(api.calls) != 0 {
		t.Fatalf("unknown class must not register or create a pod: labels=%v calls=%d", reg.registered, len(api.calls))
	}
}

func containsLabel(labels []string, want string) bool {
	for _, label := range labels {
		if label == want {
			return true
		}
	}
	return false
}
