// Package kubernetes launches one ephemeral GitHub Actions runner pod per
// allocation in the broker's cluster. The broker registers the runner itself
// (a single-use just-in-time config on the scope the allocation resolves to)
// and the pod only receives that config, never the broker's GitHub token. With
// github.scope.type owner one pool serves every repository.
package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/github"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

const (
	defaultRunnerContainer = "runner"
	defaultRunnerImage     = "ghcr.io/actions/actions-runner:latest"
	defaultRunnerCommand   = "/home/runner/run.sh"
	defaultDeadlineGrace   = 10 * time.Minute

	// envJITConfig is read by the actions runner as its --jitconfig argument.
	envJITConfig = "ACTIONS_RUNNER_INPUT_JITCONFIG"

	metadataRunnerID = "github_runner_id"

	labelManagedBy    = "app.kubernetes.io/managed-by"
	labelManagedByVal = "unified-ephemeral-runner-broker"
	labelBackend      = "uecb.josh-archer.dev/backend"
	labelAllocation   = "uecb.josh-archer.dev/allocation-id"
	labelPool         = "uecb.josh-archer.dev/pool"

	annotationRepository = "uecb.josh-archer.dev/repository"
)

// API is the subset of runtime.KubernetesAPI the backend uses.
type API interface {
	Namespace() string
	Do(ctx context.Context, method, path string, body any) (int, []byte, error)
}

// Registrar registers and removes runners on GitHub with the broker's token.
type Registrar interface {
	GenerateJITConfig(ctx context.Context, scope model.GitHubScope, runnerGroup, name string, labels []string) (github.JITConfig, error)
	DeleteRunner(ctx context.Context, scope model.GitHubScope, runnerID int64) error
}

type Backend struct {
	cfg       model.BrokerConfig
	api       API
	registrar Registrar
}

// New returns the backend. api may be nil when the broker runs outside
// Kubernetes; Provision then fails and Capacity reports an error.
func New(cfg model.BrokerConfig, api API, registrar Registrar) *Backend {
	return &Backend{cfg: cfg, api: api, registrar: registrar}
}

func (b *Backend) Name() model.BackendName {
	return model.BackendKubernetes
}

func (b *Backend) Provision(ctx context.Context, request model.AllocationRequest, allocation model.AllocationStatus) (backend.ProvisionedRunner, error) {
	if b.api == nil {
		return backend.ProvisionedRunner{}, fmt.Errorf("backend %s needs the broker to run in Kubernetes", model.BackendKubernetes)
	}
	pool, ok := b.pool(allocation.Pool)
	if !ok {
		return backend.ProvisionedRunner{}, fmt.Errorf("backend %s is not configured for pool %s", model.BackendKubernetes, allocation.Pool)
	}
	backendCfg := pool.Backends[model.BackendKubernetes]

	scope, err := b.cfg.GitHub.Scope.ForRepository(allocation.TargetRepository())
	if err != nil {
		return backend.ProvisionedRunner{}, err
	}
	targetURL, err := scope.TargetURL()
	if err != nil {
		return backend.ProvisionedRunner{}, err
	}

	if b.registrar == nil {
		return backend.ProvisionedRunner{}, fmt.Errorf("backend %s needs a GitHub client to register runners", model.BackendKubernetes)
	}

	runnerLabel := backend.DefaultRunnerLabel(model.BackendKubernetes, allocation.ID)
	timeout := request.JobTimeout
	if timeout <= 0 {
		timeout = b.cfg.Broker.DefaultJobTimeout
	}
	labels := runnerLabels(pool.Labels, allocation.RequestedLabels, runnerLabel)
	jit, err := b.registrar.GenerateJITConfig(ctx, scope, scope.RunnerGroup(pool.Name), runnerLabel, labels)
	if err != nil {
		return backend.ProvisionedRunner{}, err
	}
	// The registration is useless without its pod, so remove it on any failure.
	created := false
	defer func() {
		if !created {
			_ = b.registrar.DeleteRunner(context.WithoutCancel(ctx), scope, jit.RunnerID)
		}
	}()

	pod, err := b.buildPod(pool, backendCfg, allocation, jit.Encoded, timeout)
	if err != nil {
		return backend.ProvisionedRunner{}, err
	}

	namespace := b.namespace(backendCfg)
	status, body, err := b.api.Do(ctx, http.MethodPost, podsPath(namespace, ""), pod)
	if err != nil {
		return backend.ProvisionedRunner{}, err
	}
	switch {
	case status == http.StatusForbidden && strings.Contains(strings.ToLower(kubeMessage(body)), "exceeded quota"):
		// A ResourceQuota rejection means the namespace has no room for the pod.
		return backend.ProvisionedRunner{}, backend.NewAllocationError(
			fmt.Errorf("create runner pod in %s: %s", namespace, kubeMessage(body)),
			backend.ErrBackendCapacityExhausted,
			true,
		)
	case status < 200 || status >= 300:
		// 409 included: a pod left over for this allocation holds a stale
		// registration, so fail rather than adopt it.
		return backend.ProvisionedRunner{}, fmt.Errorf("create runner pod in %s: status %d: %s", namespace, status, kubeMessage(body))
	}
	created = true

	metadata := backend.WithCapabilitiesMetadata(backendCfg, map[string]string{
		"provisioner":    "kubernetes-pod",
		"lifecycle":      "ephemeral",
		"namespace":      namespace,
		"pod_name":       podName(allocation.ID),
		"runner_label":   runnerLabel,
		"target_url":     targetURL,
		"scope_type":     scope.Type,
		metadataRunnerID: strconv.FormatInt(jit.RunnerID, 10),
	})
	return backend.ProvisionedRunner{RunnerLabel: runnerLabel, Metadata: metadata}, nil
}

// Cleanup deletes the allocation's runner pod and its runner registration.
// Either being gone already is not an error. An ephemeral runner that ran its
// job is removed by GitHub; one that never got a job is removed here.
func (b *Backend) Cleanup(ctx context.Context, status model.AllocationStatus) error {
	var errs []error
	if b.api != nil {
		namespace := strings.TrimSpace(status.Metadata["namespace"])
		if namespace == "" {
			if pool, ok := b.pool(status.Pool); ok {
				namespace = b.namespace(pool.Backends[model.BackendKubernetes])
			}
		}
		if namespace != "" {
			code, body, err := b.api.Do(ctx, http.MethodDelete, podsPath(namespace, podName(status.ID)), nil)
			switch {
			case err != nil:
				errs = append(errs, err)
			case code != http.StatusNotFound && (code < 200 || code >= 300):
				errs = append(errs, fmt.Errorf("delete runner pod %s/%s: status %d: %s", namespace, podName(status.ID), code, kubeMessage(body)))
			}
		}
	}
	if runnerID, err := strconv.ParseInt(status.Metadata[metadataRunnerID], 10, 64); err == nil && runnerID > 0 && b.registrar != nil {
		scope, err := b.cfg.GitHub.Scope.ForRepository(status.TargetRepository())
		if err == nil {
			err = b.registrar.DeleteRunner(ctx, scope, runnerID)
		}
		var apiErr *github.StatusError
		// 422: the runner is running a job; GitHub removes it when the job ends.
		if err != nil && !(errors.As(err, &apiErr) && apiErr.Code == http.StatusUnprocessableEntity) {
			errs = append(errs, fmt.Errorf("delete runner %d: %w", runnerID, err))
		}
	}
	return errors.Join(errs...)
}

// Capacity counts this backend's live runner pods against the summed
// maxRunners of its enabled pools. Pods that cannot be scheduled count as
// pending, so a full cluster reports no free slots and allocations fall back
// to other backends.
func (b *Backend) Capacity(ctx context.Context) (backend.CapacityStatus, error) {
	maxRunners := 0
	namespaces := map[string]struct{}{}
	for _, pool := range b.cfg.Pools {
		cfg, ok := pool.Backends[model.BackendKubernetes]
		if !ok || !cfg.Enabled {
			continue
		}
		maxRunners += cfg.MaxRunners
		namespaces[b.namespace(cfg)] = struct{}{}
	}
	if maxRunners <= 0 {
		return backend.CapacityStatus{}, fmt.Errorf("backend %s has no configured maxRunners", model.BackendKubernetes)
	}
	if b.api == nil {
		return backend.CapacityStatus{}, fmt.Errorf("backend %s needs the broker to run in Kubernetes", model.BackendKubernetes)
	}

	status := backend.CapacityStatus{MaxRunners: maxRunners}
	selector := url.QueryEscape(fmt.Sprintf("%s=%s,%s=%s", labelManagedBy, labelManagedByVal, labelBackend, model.BackendKubernetes))
	for namespace := range namespaces {
		code, body, err := b.api.Do(ctx, http.MethodGet, podsPath(namespace, "")+"?labelSelector="+selector, nil)
		if err != nil {
			return backend.CapacityStatus{}, err
		}
		if code < 200 || code >= 300 {
			return backend.CapacityStatus{}, fmt.Errorf("list runner pods in %s: status %d: %s", namespace, code, kubeMessage(body))
		}
		var list podList
		if err := json.Unmarshal(body, &list); err != nil {
			return backend.CapacityStatus{}, fmt.Errorf("decode runner pods in %s: %w", namespace, err)
		}
		for _, item := range list.Items {
			if item.Metadata.DeletionTimestamp != "" {
				continue
			}
			switch item.Status.Phase {
			case "Running":
				status.ActiveRunners++
			case "Pending", "":
				status.PendingRunners++
			}
		}
	}
	return status, nil
}

func (b *Backend) pool(name model.PoolName) (model.PoolConfig, bool) {
	for _, pool := range b.cfg.Pools {
		if pool.Name != name {
			continue
		}
		if _, ok := pool.Backends[model.BackendKubernetes]; ok {
			return pool, true
		}
	}
	return model.PoolConfig{}, false
}

func (b *Backend) namespace(cfg model.BackendConfig) string {
	if cfg.Kubernetes != nil {
		if ns := strings.TrimSpace(cfg.Kubernetes.Namespace); ns != "" {
			return ns
		}
	}
	if b.api != nil {
		return b.api.Namespace()
	}
	return ""
}

func (b *Backend) buildPod(pool model.PoolConfig, cfg model.BackendConfig, allocation model.AllocationStatus, jitConfig string, timeout time.Duration) (map[string]any, error) {
	kcfg := model.KubernetesBackendConfig{}
	if cfg.Kubernetes != nil {
		kcfg = *cfg.Kubernetes
	}

	spec, err := deepCopy(kcfg.PodSpec)
	if err != nil {
		return nil, fmt.Errorf("copy podSpec: %w", err)
	}
	if spec == nil {
		spec = map[string]any{}
	}
	spec["restartPolicy"] = "Never"
	grace := kcfg.DeadlineGrace
	if grace <= 0 {
		grace = defaultDeadlineGrace
	}
	spec["activeDeadlineSeconds"] = int64((timeout + grace) / time.Second)

	containerName := strings.TrimSpace(kcfg.RunnerContainer)
	if containerName == "" {
		containerName = defaultRunnerContainer
	}
	containers, _ := spec["containers"].([]any)
	runnerIndex := -1
	for i, raw := range containers {
		container, ok := raw.(map[string]any)
		if ok && container["name"] == containerName {
			runnerIndex = i
			break
		}
	}
	if runnerIndex < 0 {
		containers = append(containers, map[string]any{"name": containerName})
		runnerIndex = len(containers) - 1
	}
	runner := containers[runnerIndex].(map[string]any)
	if image, _ := runner["image"].(string); strings.TrimSpace(image) == "" {
		image = strings.TrimSpace(kcfg.Image)
		if image == "" {
			image = defaultRunnerImage
		}
		runner["image"] = image
	}
	if _, ok := runner["command"]; !ok {
		runner["command"] = []any{defaultRunnerCommand}
	}

	env, _ := runner["env"].([]any)
	filtered := make([]any, 0, len(env)+1)
	for _, raw := range env {
		if entry, ok := raw.(map[string]any); ok && entry["name"] == envJITConfig {
			continue
		}
		filtered = append(filtered, raw)
	}
	runner["env"] = append(filtered, map[string]any{"name": envJITConfig, "value": jitConfig})
	containers[runnerIndex] = runner
	spec["containers"] = containers

	annotations := map[string]any{}
	if repo := allocation.TargetRepository(); repo != "" {
		annotations[annotationRepository] = repo
	}
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]any{
			"name": podName(allocation.ID),
			"labels": map[string]any{
				labelManagedBy:  labelManagedByVal,
				labelBackend:    string(model.BackendKubernetes),
				labelAllocation: labelValue(allocation.ID),
				labelPool:       labelValue(string(pool.Name)),
			},
			"annotations": annotations,
		},
		"spec": spec,
	}, nil
}

type podList struct {
	Items []struct {
		Metadata struct {
			DeletionTimestamp string `json:"deletionTimestamp"`
		} `json:"metadata"`
		Status struct {
			Phase string `json:"phase"`
		} `json:"status"`
	} `json:"items"`
}

func runnerLabels(base, requested []string, runnerLabel string) []string {
	seen := map[string]struct{}{}
	labels := make([]string, 0, len(base)+len(requested)+1)
	for _, value := range append(append(append([]string(nil), base...), requested...), runnerLabel) {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[strings.ToLower(value)]; ok {
			continue
		}
		seen[strings.ToLower(value)] = struct{}{}
		labels = append(labels, value)
	}
	return labels
}

var invalidNameChars = regexp.MustCompile(`[^a-z0-9-]+`)

// podName derives a DNS-1123 pod name from an allocation id.
func podName(allocationID string) string {
	name := invalidNameChars.ReplaceAllString(strings.ToLower(allocationID), "-")
	name = strings.Trim(name, "-")
	if len(name) > 50 {
		name = name[:50]
	}
	return "uecb-runner-" + name
}

var invalidLabelChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func labelValue(value string) string {
	value = invalidLabelChars.ReplaceAllString(value, "-")
	if len(value) > 63 {
		value = value[:63]
	}
	return strings.Trim(value, "-._")
}

func podsPath(namespace, name string) string {
	path := fmt.Sprintf("/api/v1/namespaces/%s/pods", url.PathEscape(namespace))
	if name != "" {
		path += "/" + url.PathEscape(name)
	}
	return path
}

func deepCopy(value map[string]any) (map[string]any, error) {
	if value == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var copied map[string]any
	if err := json.Unmarshal(encoded, &copied); err != nil {
		return nil, err
	}
	return copied, nil
}

func kubeMessage(body []byte) string {
	var status struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &status) == nil && status.Message != "" {
		return status.Message
	}
	text := strings.TrimSpace(string(body))
	if len(text) > 512 {
		text = text[:512]
	}
	return text
}
