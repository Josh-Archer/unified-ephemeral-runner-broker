package config

import (
	"strings"
	"testing"
	"time"

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

func TestARCFirstWaitDefaultsAndValidation(t *testing.T) {
	cfg := Default()
	if cfg.Broker.ARCFirst.SaturationWait != 2*time.Second || cfg.Broker.ARCFirst.PollInterval != 100*time.Millisecond {
		t.Fatalf("unexpected ARC-first defaults: %+v", cfg.Broker.ARCFirst)
	}
	cfg.Broker.ARCFirst.SaturationWait = 0
	cfg.Broker.ARCFirst.PollInterval = 0
	if err := Validate(cfg); err != nil {
		t.Fatalf("zero wait should allow immediate fallback: %v", err)
	}
	cfg.Broker.ARCFirst.SaturationWait = -time.Second
	if err := Validate(cfg); err == nil {
		t.Fatal("negative ARC-first saturation wait should be rejected")
	}
}

func TestValidateWebhooksAcceptsQuarantinedEvent(t *testing.T) {
	cfg := Default()
	cfg.Broker.Webhooks = model.WebhooksConfig{
		Enabled: true,
		Endpoints: []model.WebhookEndpointConfig{{
			URL:           "https://hooks.example.com/uecb",
			SigningSecret: "test-secret",
			Events:        []string{"quarantined"},
		}},
	}
	if err := Validate(cfg); err != nil {
		t.Fatalf("quarantined webhook event should be valid: %v", err)
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

func TestValidateBackendBudgets(t *testing.T) {
	cfg := Default()
	for i := range cfg.Pools {
		if cfg.Pools[i].Name != model.PoolLite {
			continue
		}
		backendCfg := cfg.Pools[i].Backends[model.BackendLambda]
		backendCfg.Budget = model.BudgetConfig{Enabled: true}
		cfg.Pools[i].Backends[model.BackendLambda] = backendCfg
	}
	if err := Validate(cfg); err == nil {
		t.Fatal("enabled budget without limits should fail")
	}

	for i := range cfg.Pools {
		if cfg.Pools[i].Name != model.PoolLite {
			continue
		}
		backendCfg := cfg.Pools[i].Backends[model.BackendLambda]
		backendCfg.Budget = model.BudgetConfig{Enabled: true, MaxAllocationsDaily: 10}
		backendCfg.CostClass = -1
		cfg.Pools[i].Backends[model.BackendLambda] = backendCfg
	}
	if err := Validate(cfg); err == nil {
		t.Fatal("negative costClass should fail")
	}

	for i := range cfg.Pools {
		if cfg.Pools[i].Name != model.PoolLite {
			continue
		}
		backendCfg := cfg.Pools[i].Backends[model.BackendLambda]
		backendCfg.CostClass = 40
		backendCfg.EstimatedCostPerAllocation = -0.01
		cfg.Pools[i].Backends[model.BackendLambda] = backendCfg
	}
	if err := Validate(cfg); err == nil {
		t.Fatal("negative estimatedCostPerAllocation should fail")
	}

	for i := range cfg.Pools {
		if cfg.Pools[i].Name != model.PoolLite {
			continue
		}
		backendCfg := cfg.Pools[i].Backends[model.BackendLambda]
		backendCfg.Budget = model.BudgetConfig{Enabled: true, MaxAllocationsDaily: 10, MaxAllocationsMonthly: 100}
		backendCfg.CostClass = 40
		backendCfg.EstimatedCostPerAllocation = 0.02
		cfg.Pools[i].Backends[model.BackendLambda] = backendCfg
	}
	if err := Validate(cfg); err != nil {
		t.Fatalf("valid budget config should pass: %v", err)
	}
}

func TestValidateFairShareNegativeUsageWindow(t *testing.T) {
	cfg := Default()
	for i := range cfg.Pools {
		if cfg.Pools[i].Name != model.PoolLite {
			continue
		}
		cfg.Pools[i].FairShare = model.FairShareConfig{
			Enabled:     true,
			UsageWindow: -1 * time.Minute,
		}
	}
	if err := Validate(cfg); err == nil {
		t.Fatal("negative usageWindow should fail validation")
	}
}

func TestValidateFairShareNegativeStarvationAfter(t *testing.T) {
	cfg := Default()
	for i := range cfg.Pools {
		if cfg.Pools[i].Name != model.PoolLite {
			continue
		}
		cfg.Pools[i].FairShare = model.FairShareConfig{
			Enabled:         true,
			StarvationAfter: -30 * time.Second,
		}
	}
	if err := Validate(cfg); err == nil {
		t.Fatal("negative starvationAfter should fail validation")
	}
}

func TestValidateFairSharePositiveValues(t *testing.T) {
	cfg := Default()
	for i := range cfg.Pools {
		if cfg.Pools[i].Name != model.PoolLite {
			continue
		}
		cfg.Pools[i].FairShare = model.FairShareConfig{
			Enabled:         true,
			UsageWindow:     10 * time.Minute,
			StarvationAfter: 30 * time.Second,
		}
	}
	if err := Validate(cfg); err != nil {
		t.Fatalf("valid fair share config should pass: %v", err)
	}
}

func TestValidateWebhookLabels(t *testing.T) {
	valid := func() model.BrokerConfig {
		cfg := Default()
		cfg.Pools[0].WebhookLabels = []string{"self-hosted", "uecb-full"}
		cfg.Broker.GitHubWebhook.Secret = "s"
		return cfg
	}
	if err := Validate(valid()); err != nil {
		t.Fatalf("valid webhook config rejected: %v", err)
	}

	noSecret := valid()
	noSecret.Broker.GitHubWebhook = model.GitHubWebhookConfig{}
	if err := Validate(noSecret); err == nil {
		t.Fatal("webhookLabels without githubWebhook secret should be rejected")
	}

	empty := valid()
	empty.Pools[0].WebhookLabels = []string{"self-hosted", " "}
	if err := Validate(empty); err == nil {
		t.Fatal("blank webhook label should be rejected")
	}

	dup := valid()
	dup.Pools[1].WebhookLabels = []string{"UECB-full", "self-hosted"}
	if err := Validate(dup); err == nil {
		t.Fatal("identical label sets across pools should be rejected")
	}

	off := Default()
	if err := Validate(off); err != nil {
		t.Fatalf("pools without webhookLabels need no secret: %v", err)
	}

	sized := valid()
	sized.Pools[0].WebhookLabels = []string{"self-hosted", "size-large"}
	if err := Validate(sized); err == nil || !strings.Contains(err.Error(), "reserved for runner size classes") {
		t.Fatalf("size label in webhookLabels = %v", err)
	}
}

func TestValidateSizeClasses(t *testing.T) {
	cfg := Default()
	cfg.Broker.SizeClasses = map[string]model.SizeClassConfig{
		"small": {
			Runner: model.ContainerResources{Requests: model.ResourceQuantity{CPU: "500m", Memory: "1Gi", EphemeralStorage: "8Gi"}},
			Dind:   model.ContainerResources{Limits: model.ResourceQuantity{CPU: "1", Memory: "1Gi", EphemeralStorage: "8Gi"}},
		},
		"large": {
			Runner:   model.ContainerResources{Requests: model.ResourceQuantity{CPU: "2", Memory: "4Gi", EphemeralStorage: "16Gi"}},
			Dind:     model.ContainerResources{Requests: model.ResourceQuantity{CPU: "1", Memory: "2Gi", EphemeralStorage: "16Gi"}},
			Backends: []model.BackendName{model.BackendARC},
		},
		"default": {},
	}
	if err := Validate(cfg); err != nil {
		t.Fatalf("valid size classes: %v", err)
	}

	unknown := cfg
	unknown.Broker.SizeClasses = map[string]model.SizeClassConfig{
		"large": {
			Runner:   model.ContainerResources{Requests: model.ResourceQuantity{CPU: "2"}},
			Backends: []model.BackendName{"missing"},
		},
	}
	if err := Validate(unknown); err == nil || !strings.Contains(err.Error(), "unknown backend") {
		t.Fatalf("unknown backend = %v", err)
	}

	emptyShape := cfg
	emptyShape.Broker.SizeClasses = map[string]model.SizeClassConfig{"build": {}}
	if err := Validate(emptyShape); err == nil || !strings.Contains(err.Error(), "must set runner or dind") {
		t.Fatalf("empty shape = %v", err)
	}

	badQty := cfg
	badQty.Broker.SizeClasses = map[string]model.SizeClassConfig{
		"small": {Runner: model.ContainerResources{Requests: model.ResourceQuantity{Memory: "2 GB"}}},
	}
	if err := Validate(badQty); err == nil || !strings.Contains(err.Error(), "invalid quantity") {
		t.Fatalf("bad quantity = %v", err)
	}

	badName := cfg
	badName.Broker.SizeClasses = map[string]model.SizeClassConfig{
		"Size-Large": {Runner: model.ContainerResources{Requests: model.ResourceQuantity{CPU: "1"}}},
	}
	if err := Validate(badName); err == nil {
		t.Fatal("uppercase class name should be rejected")
	}

	reserved := Default()
	reserved.Pools[0].Labels = append(reserved.Pools[0].Labels, "size-small")
	if err := Validate(reserved); err == nil || !strings.Contains(err.Error(), "reserved for runner size classes") {
		t.Fatalf("pool label = %v", err)
	}
}

func TestValidateScaleSetListener(t *testing.T) {
	cfg := Default()
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Broker.ScaleSetListener.Enabled || cfg.Broker.ScaleSetListener.EffectiveMode() != model.ScaleSetModeObserve {
		t.Fatalf("default listener = %+v", cfg.Broker.ScaleSetListener)
	}

	cfg.Broker.ScaleSetListener.Enabled = true
	cfg.Broker.ScaleSetListener.SessionSecretRef = "uecb-scaleset-sessions"
	if err := Validate(cfg); err == nil {
		t.Fatal("enabled listener without a scale set should fail")
	}

	cfg.Pools[0].ScaleSet = model.PoolScaleSetConfig{Name: "uecb-shared", ID: 4}
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}

	cfg.Broker.ScaleSetListener.Mode = "watch"
	if err := Validate(cfg); err == nil {
		t.Fatal("unknown mode should fail")
	}
	cfg.Broker.ScaleSetListener.Mode = model.ScaleSetModeAllocate
	cfg.Broker.ScaleSetListener.SessionSecretRef = " "
	if err := Validate(cfg); err == nil {
		t.Fatal("missing session secret should fail")
	}
	cfg.Broker.ScaleSetListener.SessionSecretRef = "uecb-scaleset-sessions"

	cfg.Pools[1].ScaleSet = model.PoolScaleSetConfig{Name: "uecb-shared", ID: 5}
	if err := Validate(cfg); err == nil {
		t.Fatal("duplicate scale set name should fail")
	}
	cfg.Pools[1].ScaleSet = model.PoolScaleSetConfig{Name: "uecb-lite", ID: 4}
	if err := Validate(cfg); err == nil {
		t.Fatal("duplicate scale set id should fail")
	}
	cfg.Pools[1].ScaleSet = model.PoolScaleSetConfig{Name: "uecb-lite"}
	if err := Validate(cfg); err == nil {
		t.Fatal("name without id should fail")
	}
}

func TestValidateRegistrationTimeoutAndBackendLimits(t *testing.T) {
	cfg := Default()
	if err := Validate(cfg); err != nil {
		t.Fatalf("default config should be valid: %v", err)
	}

	negReg := cfg
	negReg.Broker.RegistrationTimeout = -1
	if err := Validate(negReg); err == nil {
		t.Fatal("negative broker.registrationTimeout should be rejected")
	}

	negLimit := cfg
	negLimit.Broker.BackendLimits = map[model.BackendName]int{model.BackendAzureVM: -1}
	if err := Validate(negLimit); err == nil {
		t.Fatal("negative broker.backendLimits should be rejected")
	}

	negCrossPool := cfg
	b := negCrossPool.Pools[0].Backends[model.BackendARC]
	b.CrossPoolMaxRunners = -1
	negCrossPool.Pools[0].Backends[model.BackendARC] = b
	if err := Validate(negCrossPool); err == nil {
		t.Fatal("negative backend.crossPoolMaxRunners should be rejected")
	}

	negBackendReg := cfg
	b2 := negBackendReg.Pools[0].Backends[model.BackendARC]
	b2.RegistrationTimeout = -1
	negBackendReg.Pools[0].Backends[model.BackendARC] = b2
	if err := Validate(negBackendReg); err == nil {
		t.Fatal("negative backend.registrationTimeout should be rejected")
	}
}

func TestValidatePerRepositoryScope(t *testing.T) {
	cfg := Default()
	cfg.GitHub.Scope = model.GitHubScope{Type: model.GitHubScopeOwner, Owner: "example-owner"}
	for i := range cfg.Pools {
		for name, backendCfg := range cfg.Pools[i].Backends {
			backendCfg.WarmMin, backendCfg.WarmMax, backendCfg.WarmSchedule = 0, 0, nil
			cfg.Pools[i].Backends[name] = backendCfg
		}
	}
	if err := Validate(cfg); err != nil {
		t.Fatalf("owner scope without warm runners should validate: %v", err)
	}

	withRepo := cfg
	withRepo.GitHub.Scope.Repository = "owner/fixed"
	if err := Validate(withRepo); err == nil {
		t.Fatal("owner scope with an owner/repo default should be rejected")
	}

	noOwner := cfg
	noOwner.GitHub.Scope.Owner = ""
	if err := Validate(noOwner); err == nil {
		t.Fatal("owner scope without an owner should be rejected")
	}

	pool := cfg.Pools[0]
	for name, backendCfg := range pool.Backends {
		backendCfg.Enabled = true
		backendCfg.WarmMax = 1
		pool.Backends[name] = backendCfg
		break
	}
	if err := Validate(cfg); err == nil {
		t.Fatal("owner scope with warm runners and no default repository should be rejected")
	}
	cfg.GitHub.Scope.Repository = "home"
	if err := Validate(cfg); err != nil {
		t.Fatalf("warm runners with a default repository should validate: %v", err)
	}
}

func TestValidateWebhookJobTimeout(t *testing.T) {
	cfg := Default()
	for _, timeout := range []time.Duration{0, 6 * time.Hour} {
		cfg.Pools[0].WebhookJobTimeout = timeout
		if err := Validate(cfg); err != nil {
			t.Fatalf("timeout=%v rejected: %v", timeout, err)
		}
	}
	cfg.Pools[0].WebhookJobTimeout = -time.Second
	if err := Validate(cfg); err == nil {
		t.Fatal("negative webhookJobTimeout accepted")
	}
}

func TestValidateReconcileRepositories(t *testing.T) {
	cfg := Default()
	cfg.Broker.GitHubWebhook.ReconcileRepositories = []string{"example-owner/home", "owner/repo"}
	if err := Validate(cfg); err != nil {
		t.Fatalf("valid allowlist: %v", err)
	}
	for _, entry := range []string{"repo", "owner/re*po", "owner/a/b", "owner/*"} {
		cfg.Broker.GitHubWebhook.ReconcileRepositories = []string{entry}
		if err := Validate(cfg); err == nil {
			t.Errorf("entry %q should be rejected", entry)
		}
	}
}

func TestValidateWebhookRepositories(t *testing.T) {
	base := Default()
	base.Broker.GitHubWebhook.Secret = "s"
	for _, tc := range []struct {
		labels, repos []string
		ok            bool
	}{
		{[]string{"self-hosted", "x"}, []string{"owner/repo", "owner/*"}, true},
		{[]string{"self-hosted", "x"}, []string{"repo"}, false},
		{[]string{"self-hosted", "x"}, []string{"owner/re*po"}, false},
		{[]string{"self-hosted", "x"}, []string{"owner/a/b"}, false},
		{nil, []string{"owner/repo"}, false},
	} {
		cfg := base
		cfg.Pools = append([]model.PoolConfig(nil), base.Pools...)
		cfg.Pools[0].WebhookLabels = tc.labels
		cfg.Pools[0].WebhookRepositories = tc.repos
		if err := Validate(cfg); (err == nil) != tc.ok {
			t.Errorf("labels %v repos %v: err=%v want ok=%v", tc.labels, tc.repos, err, tc.ok)
		}
	}
}

func TestManagedAzureVMValidation(t *testing.T) {
	valid := func() model.BrokerConfig {
		c := Default()
		c.GitHub.Scope = model.GitHubScope{Type: model.GitHubScopeRepository, Owner: "example", Repository: "project"}
		c.GitHub.Auth.SecretRef = "github"
		c.Pools = []model.PoolConfig{{Name: model.PoolFull, Backends: map[model.BackendName]model.BackendConfig{model.BackendAzureVM: {Enabled: true, Healthy: true, MaxRunners: 2, CrossPoolMaxRunners: 2, AzureVM: &model.AzureVMBackendConfig{Repository: "example/project", RunnerNames: []string{"worker-1", "worker-2"}}}}}}
		return c
	}
	if err := Validate(valid()); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(*model.BrokerConfig)
	}{
		{"ha", func(c *model.BrokerConfig) { v := true; c.Broker.HA.Enabled = &v }},
		{"shared-overflow", func(c *model.BrokerConfig) {
			b := c.Pools[0].Backends[model.BackendAzureVM]
			b.CrossPoolMaxRunners = 3
			c.Pools[0].Backends[model.BackendAzureVM] = b
		}},
		{"duplicate-workers", func(c *model.BrokerConfig) {
			c.Pools[0].Backends[model.BackendAzureVM].AzureVM.RunnerNames = []string{"worker-1", "WORKER-1"}
		}},
		{"foreign-repo", func(c *model.BrokerConfig) {
			c.Pools[0].Backends[model.BackendAzureVM].AzureVM.Repository = "attacker/project"
		}},
		{"mixed-mode", func(c *model.BrokerConfig) {
			c.Pools = append(c.Pools, model.PoolConfig{Name: model.PoolLite, Backends: map[model.BackendName]model.BackendConfig{model.BackendAzureVM: {Enabled: true, Healthy: true, MaxRunners: 1}}})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := valid()
			tc.change(&c)
			if err := Validate(c); err == nil {
				t.Fatal("unsafe managed configuration accepted")
			}
		})
	}
}

func TestManagedAzureVMReplicaCountCannotBypassGuard(t *testing.T) {
	c := Default()
	c.Broker.StateStore = model.StateStoreConfig{Type: "postgres"}
	c.Pools = []model.PoolConfig{{Backends: map[model.BackendName]model.BackendConfig{model.BackendAzureVM: {AzureVM: &model.AzureVMBackendConfig{Repository: "example/project", RunnerNames: []string{"worker-1"}}}}}}
	if err := ValidateReplicaSafety(c, 2); err == nil {
		t.Fatal("managed physical claims cannot use multiple replicas")
	}
}

func TestAlwaysOnWarmPolicy(t *testing.T) {
	one := 1
	base := func(name model.BackendName, backend model.BackendConfig) model.BrokerConfig {
		cfg := Default()
		cfg.Pools = []model.PoolConfig{{
			Name:     model.PoolFull,
			Backends: map[model.BackendName]model.BackendConfig{name: backend},
		}}
		return cfg
	}
	k8s := model.BackendConfig{
		Enabled: true, Healthy: true, MaxRunners: 3, WarmMin: 1, WarmMax: 1,
		Kubernetes: &model.KubernetesBackendConfig{Namespace: "arc-runners"},
	}
	alias := k8s
	alias.Driver = "kubernetes"
	alias.CrossPoolMaxRunners = 3
	arc := model.BackendConfig{Enabled: true, Healthy: true, MaxRunners: 2, WarmMin: 1, WarmMax: 1}
	arcDriver := arc
	arcDriver.Driver = "kubernetes"
	arcDriver.CrossPoolMaxRunners = 2
	arcDriver.Kubernetes = &model.KubernetesBackendConfig{Namespace: "arc-runners"}

	for _, tc := range []struct {
		label   string
		name    model.BackendName
		backend model.BackendConfig
	}{
		{"arc", model.BackendARC, arc},
		{"arc-kubernetes-driver", model.BackendARC, arcDriver},
		{"kubernetes", model.BackendKubernetes, k8s},
		{"kubernetes-alias", model.BackendName("kubernetes-primary"), alias},
		{"ec2", model.BackendEC2, model.BackendConfig{Enabled: true, Healthy: true, MaxRunners: 2, WarmMin: 1, WarmMax: 1, SecretRef: "uecb-ec2"}},
		{"gce", model.BackendGCE, model.BackendConfig{Enabled: true, Healthy: true, MaxRunners: 2, WarmMin: 1, WarmMax: 1, SecretRef: "uecb-gce"}},
		{"azure-vm-ephemeral", model.BackendAzureVM, model.BackendConfig{Enabled: true, Healthy: true, MaxRunners: 2, WarmMin: 1, WarmMax: 1, SecretRef: "uecb-azure-vm"}},
		{"desktop-unused-field", model.BackendDesktop, model.BackendConfig{Enabled: true, Healthy: true, MaxRunners: 1, WarmMin: 1, WarmMax: 1}},
	} {
		t.Run("allow-"+tc.label, func(t *testing.T) {
			if err := Validate(base(tc.name, tc.backend)); err != nil {
				t.Fatal(err)
			}
		})
	}

	for _, tc := range []struct {
		pool    model.PoolName
		name    model.BackendName
		backend model.BackendConfig
		want    string
	}{
		{model.PoolFull, model.BackendLambda, model.BackendConfig{Enabled: true, Healthy: true, MaxRunners: 1, WarmMin: 1, WarmMax: 1}, "serverless backend lambda"},
		{model.PoolLite, model.BackendCodeBuild, model.BackendConfig{Enabled: true, Healthy: true, MaxRunners: 1, WarmMin: 1, WarmMax: 1}, "serverless backend codebuild"},
		{"ci", model.BackendCloudRun, model.BackendConfig{Enabled: true, Healthy: true, MaxRunners: 1, WarmMin: 1, WarmMax: 1}, "serverless backend cloud-run"},
		{"jobs", model.BackendAzureFunctions, model.BackendConfig{Enabled: true, Healthy: true, MaxRunners: 1, WarmMin: 1, WarmMax: 1}, "serverless backend azure-functions"},
		{"vms", model.BackendAzureVM, model.BackendConfig{Enabled: true, Healthy: true, MaxRunners: 1, CrossPoolMaxRunners: 1, WarmMin: 1, WarmMax: 1, AzureVM: &model.AzureVMBackendConfig{Repository: "example/project", RunnerNames: []string{"worker-1"}}}, "managed azure-vm"},
		{"labels", model.BackendAzureVM, model.BackendConfig{Enabled: true, Healthy: true, MaxRunners: 1, WarmMin: 1, WarmMax: 1, RunnerLabel: "persistent"}, "static-label azure-vm"},
	} {
		t.Run("refuse-"+string(tc.pool), func(t *testing.T) {
			cfg := Default()
			cfg.Pools = []model.PoolConfig{{
				Name:     tc.pool,
				Backends: map[model.BackendName]model.BackendConfig{tc.name: tc.backend},
			}}
			err := Validate(cfg)
			if err == nil {
				t.Fatal("expected warm policy rejection")
			}
			for _, needle := range []string{
				"pools[" + string(tc.pool) + "].backends[" + string(tc.name) + "]",
				"warm runners are not supported",
				tc.want,
			} {
				if !strings.Contains(err.Error(), needle) {
					t.Fatalf("error %q missing %q", err.Error(), needle)
				}
			}
		})
	}

	scheduled := base(model.BackendLambda, model.BackendConfig{
		Enabled: true, Healthy: true, MaxRunners: 1,
		WarmSchedule: &model.WarmScheduleConfig{
			Timezone: "UTC",
			Windows:  []model.WarmWindowConfig{{Start: "08:00", End: "18:00", WarmMin: &one, WarmMax: &one}},
		},
	})
	if err := Validate(scheduled); err != nil {
		t.Fatalf("serverless schedule with warmMin 0 should load: %v", err)
	}
}
