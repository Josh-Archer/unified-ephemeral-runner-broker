package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	arcbackend "github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend/arc"
	azurevmbackend "github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend/azurevm"
	ec2backend "github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend/ec2"
	gcebackend "github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend/gce"
	kubernetesbackend "github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend/kubernetes"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/config"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/github"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/runtime"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/scheduler"
)

type recordingKubeAPI struct {
	mu      sync.Mutex
	posts   []map[string]any
	deletes int
}

func (f *recordingKubeAPI) Namespace() string { return "arc-runners" }

func (f *recordingKubeAPI) Do(_ context.Context, method, path string, body any) (int, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch method {
	case http.MethodPost:
		encoded, _ := json.Marshal(body)
		var decoded map[string]any
		_ = json.Unmarshal(encoded, &decoded)
		f.posts = append(f.posts, decoded)
		return http.StatusCreated, []byte(`{}`), nil
	case http.MethodDelete:
		f.deletes++
		return http.StatusOK, []byte(`{}`), nil
	default:
		return http.StatusOK, []byte(`{"items":[]}`), nil
	}
}

func (f *recordingKubeAPI) postCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.posts)
}

type recordingRegistrar struct {
	mu      sync.Mutex
	nextID  int64
	deleted []int64
}

func (r *recordingRegistrar) GenerateJITConfig(context.Context, model.GitHubScope, string, string, []string) (github.JITConfig, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	return github.JITConfig{RunnerID: 100 + r.nextID, Encoded: "jit"}, nil
}

func (r *recordingRegistrar) DeleteRunner(context.Context, model.GitHubScope, int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deleted = append(r.deleted, 1)
	return nil
}

func (r *recordingRegistrar) registrations() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.nextID
}

func (r *recordingRegistrar) deleteCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.deleted)
}

type mapSecrets map[string]map[string]string

func (m mapSecrets) ReadSecret(_ context.Context, name string) (map[string]string, error) {
	values, ok := m[name]
	if !ok {
		return nil, errors.New("secret not found: " + name)
	}
	copyValues := make(map[string]string, len(values))
	for key, value := range values {
		copyValues[key] = value
	}
	return copyValues, nil
}

func enableOnlyBackend(cfg *model.BrokerConfig, poolName model.PoolName, name model.BackendName, backendCfg model.BackendConfig) {
	for i := range cfg.Pools {
		pool := &cfg.Pools[i]
		for existing, current := range pool.Backends {
			current.Enabled = false
			pool.Backends[existing] = current
		}
		if pool.Name == poolName {
			pool.Backends[name] = backendCfg
		}
	}
}

func setWarm(svc *Service, poolName model.PoolName, name model.BackendName, warmMin, warmMax int) {
	for i := range svc.cfg.Pools {
		if svc.cfg.Pools[i].Name != poolName {
			continue
		}
		cfg := svc.cfg.Pools[i].Backends[name]
		cfg.WarmMin = warmMin
		cfg.WarmMax = warmMax
		svc.cfg.Pools[i].Backends[name] = cfg
	}
}

func podAnnotation(pod map[string]any, key string) string {
	metadata, _ := pod["metadata"].(map[string]any)
	annotations, _ := metadata["annotations"].(map[string]any)
	value, _ := annotations[key].(string)
	return value
}

func podDeadlineSeconds(pod map[string]any) float64 {
	spec, _ := pod["spec"].(map[string]any)
	value, _ := spec["activeDeadlineSeconds"].(float64)
	return value
}

func warmStatuses(svc *Service, pool model.PoolName, name model.BackendName) []model.AllocationStatus {
	var warm []model.AllocationStatus
	for _, status := range svc.store.List() {
		if status.Pool == pool && status.SelectedBackend == name && status.State == model.StateWarm {
			warm = append(warm, status)
		}
	}
	return warm
}

func TestKubernetesFamilyWarmLaunchesRegisteredPod(t *testing.T) {
	cases := []struct {
		name   model.BackendName
		driver string
	}{
		{name: model.BackendKubernetes},
		{name: model.BackendName("kubernetes-primary"), driver: "kubernetes"},
		{name: model.BackendARC, driver: "kubernetes"},
	}
	for _, tc := range cases {
		t.Run(string(tc.name), func(t *testing.T) {
			cfg := config.Default()
			enableOnlyBackend(&cfg, model.PoolLite, tc.name, model.BackendConfig{
				Enabled: true, Healthy: true, Driver: tc.driver,
				MaxRunners: 3, CrossPoolMaxRunners: 3,
				WarmMin: 1, WarmMax: 1,
				Kubernetes: &model.KubernetesBackendConfig{Namespace: "arc-runners"},
			})
			kube := &recordingKubeAPI{}
			reg := &recordingRegistrar{}
			svc := NewService(cfg, backend.NewRegistry(kubernetesbackend.NewNamed(cfg, kube, reg, tc.name)), nil)

			svc.ReconcileWarmPools()
			if kube.postCount() != 1 || reg.registrations() != 1 {
				t.Fatalf("posts=%d registrations=%d", kube.postCount(), reg.registrations())
			}
			warm := warmStatuses(svc, model.PoolLite, tc.name)
			if len(warm) != 1 {
				t.Fatalf("warm = %+v", svc.store.List())
			}
			if warm[0].RegisteredAt == nil || warm[0].Metadata["github_runner_id"] == "" {
				t.Fatalf("warm runner was not registered: %+v", warm[0])
			}
			if warm[0].Metadata[backend.MetadataLaunchModeKey] != launchModeWarm {
				t.Fatalf("launch mode = %q", warm[0].Metadata[backend.MetadataLaunchModeKey])
			}
			kube.mu.Lock()
			pod := kube.posts[0]
			kube.mu.Unlock()
			if podAnnotation(pod, "uecb.josh-archer.dev/launch-mode") != launchModeWarm {
				t.Fatalf("pod annotations missing warm launch mode: %+v", pod["metadata"])
			}
			// Default warm TTL 15m plus the default 15m job budget. The pod
			// deadline also adds grace, so it must be at least that sum.
			if got := podDeadlineSeconds(pod); got < (30 * time.Minute).Seconds() {
				t.Fatalf("activeDeadlineSeconds = %v, want at least 30m", got)
			}
			if svc.store.CountActive(model.PoolLite, tc.name) != 1 {
				t.Fatalf("warm runner did not count toward maxRunners")
			}

			allocation, err := svc.Allocate(context.Background(), model.AllocationRequest{
				Pool: model.PoolLite, JobTimeout: 5 * time.Minute,
			})
			if err != nil {
				t.Fatal(err)
			}
			if allocation.ID != warm[0].ID || allocation.Metadata[backend.MetadataLaunchModeKey] != launchModeWarm {
				t.Fatalf("consume = %+v, want warm id %s", allocation, warm[0].ID)
			}
			if kube.postCount() != 1 {
				t.Fatal("consuming a warm runner started another pod")
			}

			svc.ReconcileWarmPools()
			if kube.postCount() != 2 || svc.store.CountActive(model.PoolLite, tc.name) != 2 {
				t.Fatalf("replenish posts=%d active=%d", kube.postCount(), svc.store.CountActive(model.PoolLite, tc.name))
			}

			setWarm(svc, model.PoolLite, tc.name, 0, 0)
			svc.ReconcileWarmPools()
			if reg.deleteCount() == 0 || kube.deletes == 0 {
				t.Fatalf("warm 0 did not recycle the idle pod (deletes=%d registrar=%d)", kube.deletes, reg.deleteCount())
			}
			if left := warmStatuses(svc, model.PoolLite, tc.name); len(left) != 0 {
				t.Fatalf("idle warm runners left = %+v", left)
			}
			if svc.store.CountActive(model.PoolLite, tc.name) != 1 {
				t.Fatalf("consumed runner should still hold its slot, active=%d", svc.store.CountActive(model.PoolLite, tc.name))
			}
		})
	}
}

func TestWarmReplenishStopsAtMaxRunners(t *testing.T) {
	name := model.BackendName("kubernetes-primary")
	cfg := config.Default()
	enableOnlyBackend(&cfg, model.PoolLite, name, model.BackendConfig{
		Enabled: true, Healthy: true, Driver: "kubernetes",
		MaxRunners: 3, CrossPoolMaxRunners: 3,
		WarmMin: 1, WarmMax: 1,
		Kubernetes: &model.KubernetesBackendConfig{Namespace: "arc-runners"},
	})
	kube := &recordingKubeAPI{}
	svc := NewService(cfg, backend.NewRegistry(kubernetesbackend.NewNamed(cfg, kube, &recordingRegistrar{}, name)), nil)

	svc.ReconcileWarmPools()
	if _, err := svc.Allocate(context.Background(), model.AllocationRequest{Pool: model.PoolLite, JobTimeout: time.Minute}); err != nil {
		t.Fatalf("consume warm: %v", err)
	}
	svc.ReconcileWarmPools()
	if _, err := svc.Allocate(context.Background(), model.AllocationRequest{Pool: model.PoolLite, JobTimeout: time.Minute}); err != nil {
		t.Fatalf("consume replenished warm: %v", err)
	}
	if _, err := svc.Allocate(context.Background(), model.AllocationRequest{Pool: model.PoolLite, JobTimeout: time.Minute}); err != nil {
		t.Fatalf("cold fill: %v", err)
	}
	if svc.store.CountActive(model.PoolLite, name) != 3 {
		t.Fatalf("active = %d, want 3", svc.store.CountActive(model.PoolLite, name))
	}
	posts := kube.postCount()
	svc.ReconcileWarmPools()
	if kube.postCount() != posts {
		t.Fatalf("reconcile started a runner past maxRunners: posts %d -> %d", posts, kube.postCount())
	}
	if _, err := svc.Allocate(context.Background(), model.AllocationRequest{Pool: model.PoolLite, JobTimeout: time.Minute}); !errors.Is(err, scheduler.ErrNoCapacity) {
		t.Fatalf("expected ErrNoCapacity, got %v", err)
	}
}

func TestWarmCountsTowardCrossPoolMaxRunners(t *testing.T) {
	alias := model.BackendName("kubernetes-primary")
	counting := &countingBackend{testBackend: testBackend{name: alias}}
	cfg := config.Default()
	for i := range cfg.Pools {
		pool := &cfg.Pools[i]
		for name, backendCfg := range pool.Backends {
			backendCfg.Enabled = false
			pool.Backends[name] = backendCfg
		}
		pool.Backends[alias] = model.BackendConfig{
			Enabled: true, Healthy: true, Driver: "kubernetes",
			MaxRunners: 4, CrossPoolMaxRunners: 3,
			WarmMin: 1, WarmMax: 1,
		}
	}
	svc := NewService(cfg, backend.NewRegistry(counting), nil)
	svc.ReconcileWarmPools()
	if counting.provisionCount != 2 || svc.store.CountBackendActive(alias) != 2 {
		t.Fatalf("provisions=%d active=%d, want one warm runner in each pool", counting.provisionCount, svc.store.CountBackendActive(alias))
	}
	if _, err := svc.Allocate(context.Background(), model.AllocationRequest{Pool: model.PoolFull, JobTimeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	// The consumed runner still counts. Raising this pool's warm target asks
	// for two idle runners, which would pass its own maxRunners but not the
	// shared cap (ready + other pool's warm + two new warm).
	setWarm(svc, model.PoolFull, alias, 2, 2)
	svc.ReconcileWarmPools()
	if counting.provisionCount != 3 || svc.store.CountBackendActive(alias) != 3 {
		t.Fatalf("replenish provisions=%d active=%d, want the shared cap of 3", counting.provisionCount, svc.store.CountBackendActive(alias))
	}
	if got := len(warmStatuses(svc, model.PoolFull, alias)); got != 1 {
		t.Fatalf("full warm = %d, want 1 held back by crossPoolMaxRunners", got)
	}
}

func TestFileStoreRestoresWarmSetWithoutDoubling(t *testing.T) {
	counting := &countingBackend{testBackend: testBackend{name: model.BackendEC2}}
	cfg := config.Default()
	cfg.Broker.StateStore = model.StateStoreConfig{Type: "file", Path: t.TempDir() + "/allocations.json"}
	enableOnlyBackend(&cfg, model.PoolLite, model.BackendEC2, model.BackendConfig{
		Enabled: true, Healthy: true, MaxRunners: 2, WarmMin: 1, WarmMax: 1,
	})
	first := NewService(cfg, backend.NewRegistry(counting), nil)
	first.ReconcileWarmPools()
	warm := warmStatuses(first, model.PoolLite, model.BackendEC2)
	if len(warm) != 1 || counting.provisionCount != 1 {
		t.Fatalf("warm=%d provisions=%d", len(warm), counting.provisionCount)
	}

	restarted := NewService(cfg, backend.NewRegistry(counting), nil)
	got, ok := restarted.Get(warm[0].ID)
	if !ok || got.State != model.StateWarm {
		t.Fatalf("restored = %+v ok=%v", got, ok)
	}
	restarted.ReconcileWarmPools()
	if counting.provisionCount != 1 {
		t.Fatalf("restart doubled the warm set, provisions=%d", counting.provisionCount)
	}
	allocation, err := restarted.Allocate(context.Background(), model.AllocationRequest{Pool: model.PoolLite, JobTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if allocation.ID != warm[0].ID || allocation.Metadata[backend.MetadataLaunchModeKey] != launchModeWarm {
		t.Fatalf("consume after restart = %+v", allocation)
	}
}

func TestRestartRebuildsIdleWarmRunnerAsWarm(t *testing.T) {
	cfg := config.Default()
	cfg.Pools[0].Backends[model.BackendKubernetes] = model.BackendConfig{
		Enabled: true, Healthy: true, MaxRunners: 2, WarmMin: 1, WarmMax: 1,
		Kubernetes: &model.KubernetesBackendConfig{Namespace: "arc-runners"},
	}
	impl := inventoryBackend{
		testBackend: testBackend{name: model.BackendKubernetes},
		runners: []backend.LiveRunner{
			{
				AllocationID: "warm-kept",
				Pool:         model.PoolFull,
				Metadata: map[string]string{
					"launch_mode":      "warm",
					"github_runner_id": "77",
					"runner_label":     "uecb-kubernetes-warm-kept",
				},
			},
			{
				AllocationID:  "warm-consumed",
				Pool:          model.PoolFull,
				WorkflowJobID: 99,
				Metadata: map[string]string{
					"launch_mode":      "warm",
					"github_runner_id": "78",
				},
			},
		},
	}
	svc := NewService(cfg, backend.NewRegistry(impl), nil)
	idle, ok := svc.Get("warm-kept")
	if !ok || idle.State != model.StateWarm || idle.RegisteredAt == nil || idle.Metadata["github_runner_id"] != "77" {
		t.Fatalf("idle rebuild = %+v ok=%v", idle, ok)
	}
	consumed, ok := svc.Get("warm-consumed")
	if !ok || consumed.State != model.StateReady {
		t.Fatalf("consumed rebuild = %+v ok=%v", consumed, ok)
	}
	before := len(svc.store.List())
	svc.ReconcileWarmPools()
	if len(svc.store.List()) != before || len(warmStatuses(svc, model.PoolFull, model.BackendKubernetes)) != 1 {
		t.Fatalf("reconcile changed the restored warm set: %+v", svc.store.List())
	}
}

func TestBuiltinARCWarmHoldsLabelWithoutStartingPod(t *testing.T) {
	cfg := config.Default()
	enableOnlyBackend(&cfg, model.PoolLite, model.BackendARC, model.BackendConfig{
		Enabled: true, Healthy: true, MaxRunners: 2, WarmMin: 1, WarmMax: 1,
	})
	svc := NewService(cfg, backend.NewRegistry(arcbackend.New(cfg, nil)), nil)
	svc.ReconcileWarmPools()
	warm := warmStatuses(svc, model.PoolLite, model.BackendARC)
	if len(warm) != 1 {
		t.Fatalf("warm = %+v", svc.store.List())
	}
	if warm[0].Metadata["provisioner"] != "arc-job" || warm[0].RunnerLabel == "" {
		t.Fatalf("label-only arc warm = %+v", warm[0])
	}
	if warm[0].Metadata["github_runner_id"] != "" || warm[0].RegisteredAt != nil {
		t.Fatalf("label-only arc must not claim a GitHub registration: %+v", warm[0])
	}
}

func TestServerlessWarmReconcileDoesNotProvision(t *testing.T) {
	for _, name := range []model.BackendName{model.BackendLambda, model.BackendCodeBuild, model.BackendCloudRun, model.BackendAzureFunctions} {
		t.Run(string(name), func(t *testing.T) {
			counting := &countingBackend{testBackend: testBackend{name: name}}
			cfg := config.Default()
			enableOnlyBackend(&cfg, model.PoolLite, name, model.BackendConfig{
				Enabled: true, Healthy: true, MaxRunners: 2, WarmMin: 1, WarmMax: 1,
			})
			svc := NewService(cfg, backend.NewRegistry(counting), nil)
			svc.ReconcileWarmPools()
			if counting.provisionCount != 0 || len(warmStatuses(svc, model.PoolLite, name)) != 0 {
				t.Fatalf("serverless warm provisioned %d times: %+v", counting.provisionCount, svc.store.List())
			}
		})
	}
}

func TestWarmRecyclesWhenBackendDisabled(t *testing.T) {
	counting := &countingBackend{testBackend: testBackend{name: model.BackendEC2}}
	cfg := config.Default()
	enableOnlyBackend(&cfg, model.PoolLite, model.BackendEC2, model.BackendConfig{
		Enabled: true, Healthy: true, MaxRunners: 2, WarmMin: 1, WarmMax: 1,
	})
	svc := NewService(cfg, backend.NewRegistry(counting), nil)
	svc.ReconcileWarmPools()
	if counting.provisionCount != 1 {
		t.Fatalf("provisions = %d", counting.provisionCount)
	}
	for i := range svc.cfg.Pools {
		if svc.cfg.Pools[i].Name != model.PoolLite {
			continue
		}
		backendCfg := svc.cfg.Pools[i].Backends[model.BackendEC2]
		backendCfg.Healthy = false
		svc.cfg.Pools[i].Backends[model.BackendEC2] = backendCfg
	}
	svc.ReconcileWarmPools()
	if left := warmStatuses(svc, model.PoolLite, model.BackendEC2); len(left) != 0 {
		t.Fatalf("unhealthy backend left warm runners: %+v", left)
	}
	if svc.store.CountActive(model.PoolLite, model.BackendEC2) != 0 {
		t.Fatal("recycled warm runner still counts as active")
	}
}

func TestExternalVMWarmDispatchesAndHolds(t *testing.T) {
	cases := []struct {
		name model.BackendName
		new  func(model.BrokerConfig, runtime.SecretReader) backend.Backend
	}{
		{model.BackendEC2, func(cfg model.BrokerConfig, secrets runtime.SecretReader) backend.Backend {
			return ec2backend.New(cfg, secrets)
		}},
		{model.BackendGCE, func(cfg model.BrokerConfig, secrets runtime.SecretReader) backend.Backend {
			return gcebackend.New(cfg, secrets)
		}},
		{model.BackendAzureVM, func(cfg model.BrokerConfig, secrets runtime.SecretReader) backend.Backend {
			return azurevmbackend.NewFromRuntime(cfg, secrets)
		}},
	}
	for _, tc := range cases {
		t.Run(string(tc.name), func(t *testing.T) {
			var mu sync.Mutex
			var modes []string
			var timeout int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var payload struct {
					LaunchMode        string `json:"launch_mode"`
					JobTimeoutSeconds int64  `json:"job_timeout_seconds"`
				}
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				mu.Lock()
				modes = append(modes, payload.LaunchMode)
				timeout = payload.JobTimeoutSeconds
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"execution_id":"run-1"}`))
			}))
			t.Cleanup(server.Close)

			cfg := config.Default()
			enableOnlyBackend(&cfg, model.PoolLite, tc.name, model.BackendConfig{
				Enabled: true, Healthy: true, MaxRunners: 3,
				WarmMin: 1, WarmMax: 1, SecretRef: "uecb-vm",
			})
			svc := NewService(cfg, backend.NewRegistry(tc.new(cfg, mapSecrets{
				"uecb-vm": {"dispatch_url": server.URL},
			})), nil)
			svc.ReconcileWarmPools()
			mu.Lock()
			gotModes := append([]string(nil), modes...)
			gotTimeout := timeout
			mu.Unlock()
			if len(gotModes) != 1 || gotModes[0] != launchModeWarm {
				t.Fatalf("launch modes = %v", gotModes)
			}
			if gotTimeout < int64((30*time.Minute)/time.Second) {
				t.Fatalf("job_timeout_seconds = %d, want idle warm hold plus a job", gotTimeout)
			}
			warm := warmStatuses(svc, model.PoolLite, tc.name)
			if len(warm) != 1 {
				t.Fatalf("warm = %+v", svc.store.List())
			}
			allocation, err := svc.Allocate(context.Background(), model.AllocationRequest{Pool: model.PoolLite, JobTimeout: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			if allocation.ID != warm[0].ID || allocation.Metadata[backend.MetadataLaunchModeKey] != launchModeWarm {
				t.Fatalf("consume = %+v", allocation)
			}
			mu.Lock()
			if len(modes) != 1 {
				t.Fatalf("consume dispatched again: %v", modes)
			}
			mu.Unlock()
			svc.ReconcileWarmPools()
			mu.Lock()
			defer mu.Unlock()
			if len(modes) != 2 || modes[1] != launchModeWarm {
				t.Fatalf("replenish modes = %v", modes)
			}
		})
	}
}
