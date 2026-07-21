package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

func TestLoadParsesOIDCPolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broker.yaml")
	if err := os.WriteFile(path, []byte(`
broker:
  api:
    oidcAudience: custom-aud
    oidcPolicy:
      allowedRepositories:
        - acme/widgets
        - acme/*
      allowedOwners:
        - acme
`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.Broker.API.OIDCAudience != "custom-aud" {
		t.Fatalf("unexpected audience %q", cfg.Broker.API.OIDCAudience)
	}
	if len(cfg.Broker.API.OIDCPolicy.AllowedRepositories) != 2 {
		t.Fatalf("expected 2 allowed repositories, got %#v", cfg.Broker.API.OIDCPolicy.AllowedRepositories)
	}
	if cfg.Broker.API.OIDCPolicy.AllowedRepositories[0] != "acme/widgets" {
		t.Fatalf("unexpected repository entry %#v", cfg.Broker.API.OIDCPolicy.AllowedRepositories)
	}
	if len(cfg.Broker.API.OIDCPolicy.AllowedOwners) != 1 || cfg.Broker.API.OIDCPolicy.AllowedOwners[0] != "acme" {
		t.Fatalf("unexpected allowed owners %#v", cfg.Broker.API.OIDCPolicy.AllowedOwners)
	}
}

func TestDefaultIncludesSeparateCodeBuildAndLambdaBackends(t *testing.T) {
	cfg := Default()
	pool := cfg.Pools[1]

	codebuildCfg, ok := pool.Backends["codebuild"]
	if !ok {
		t.Fatal("expected lite pool to include codebuild backend")
	}
	if codebuildCfg.SecretRef != "uecb-codebuild" {
		t.Fatalf("expected codebuild secretRef uecb-codebuild, got %q", codebuildCfg.SecretRef)
	}

	lambdaCfg, ok := pool.Backends["lambda"]
	if !ok {
		t.Fatal("expected lite pool to include lambda backend")
	}
	if lambdaCfg.SecretRef != "uecb-lambda" {
		t.Fatalf("expected lambda secretRef uecb-lambda, got %q", lambdaCfg.SecretRef)
	}
}

func TestLoadParsesBackendAdmissionPolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broker.yaml")
	if err := os.WriteFile(path, []byte(`
pools:
  - name: lite
    backends:
      codebuild:
        circuitBreaker:
          enabled: true
          failureThreshold: 2
          evaluationWindow: 3m
          openDuration: 45s
          probeInterval: 15s
          probeTimeout: 5s
          recoverySuccessThreshold: 2
          halfOpenMaxRequests: 1
          tripReasons:
            - timeout
            - throttled
        rateLimit:
          enabled: true
          permits: 4
          interval: 1m
          burst: 2
`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	var codebuildCfg model.BackendConfig
	for _, pool := range cfg.Pools {
		if pool.Name == model.PoolLite {
			codebuildCfg = pool.Backends[model.BackendCodeBuild]
			break
		}
	}
	if !codebuildCfg.CircuitBreaker.Enabled {
		t.Fatal("expected circuit breaker to be enabled")
	}
	if codebuildCfg.CircuitBreaker.FailureThreshold != 2 {
		t.Fatalf("unexpected failure threshold: %d", codebuildCfg.CircuitBreaker.FailureThreshold)
	}
	if codebuildCfg.CircuitBreaker.EvaluationWindow != 3*time.Minute {
		t.Fatalf("unexpected evaluation window: %s", codebuildCfg.CircuitBreaker.EvaluationWindow)
	}
	if !codebuildCfg.RateLimit.Enabled || codebuildCfg.RateLimit.Permits != 4 || codebuildCfg.RateLimit.Interval != time.Minute {
		t.Fatalf("unexpected rate limit config: %+v", codebuildCfg.RateLimit)
	}
}

func TestLoadParsesTierRoutingConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broker.yaml")
	if err := os.WriteFile(path, []byte(`
broker:
  tierRouting:
    enabled: true
    refreshInterval: 2m
    staleAfter: 6m
    failureMode: fallback-backends
    fallbackBackends:
      - codebuild
    prometheus:
      url: https://prometheus.example.invalid
      timeout: 3s
      secretRef: uecb-prometheus
    providers:
      aws-main:
        provider: aws
        mode: free-tier
        secretRef: uecb-aws-billing
    providerRules:
      - name: aws-free-tier
        providerRef: aws-main
        hardLimitRatio: 0.9
        action: disable
pools:
  - name: lite
    backends:
      codebuild:
        tierRules:
          - name: codebuild-free-tier
            providerRef: aws-main
            usageQuery: uecb:backend_usage:ratio
            softLimitRatio: 0.75
            hardLimitRatio: 0.9
            minRemainingCredit: 2
            action: disable
`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if !cfg.Broker.TierRouting.Enabled {
		t.Fatal("expected tier routing to be enabled")
	}
	if cfg.Broker.TierRouting.RefreshInterval != 2*time.Minute {
		t.Fatalf("unexpected refresh interval: %s", cfg.Broker.TierRouting.RefreshInterval)
	}
	if cfg.Broker.TierRouting.Prometheus.SecretRef != "uecb-prometheus" {
		t.Fatalf("unexpected prometheus secret ref: %q", cfg.Broker.TierRouting.Prometheus.SecretRef)
	}
	provider := cfg.Broker.TierRouting.Providers["aws-main"]
	if provider.Provider != "aws" || provider.Mode != "free-tier" || provider.SecretRef != "uecb-aws-billing" {
		t.Fatalf("unexpected provider config: %+v", provider)
	}
	providerRule := cfg.Broker.TierRouting.ProviderRules[0]
	if providerRule.ProviderRef != "aws-main" || providerRule.Action != "disable" || providerRule.HardLimitRatio != 0.9 {
		t.Fatalf("unexpected provider tier rule: %+v", providerRule)
	}
	rule := cfg.Pools[0].Backends[model.BackendCodeBuild].TierRules[0]
	if rule.ProviderRef != "aws-main" || rule.Action != "disable" || rule.HardLimitRatio != 0.9 {
		t.Fatalf("unexpected tier rule: %+v", rule)
	}
}

func TestLoadRejectsInvalidTierRoutingConfig(t *testing.T) {
	cases := map[string]string{
		"invalid provider ref": `
broker:
  tierRouting:
    enabled: true
    prometheus:
      url: https://prometheus.example.invalid
pools:
  - name: lite
    backends:
      codebuild:
        tierRules:
          - providerRef: missing-provider
`,
		"invalid provider rule ref": `
broker:
  tierRouting:
    enabled: true
    providerRules:
      - providerRef: missing-provider
        hardLimitRatio: 0.9
`,
		"invalid threshold order": `
broker:
  tierRouting:
    enabled: true
    prometheus:
      url: https://prometheus.example.invalid
pools:
  - name: lite
    backends:
      codebuild:
        tierRules:
          - usageQuery: uecb:usage
            softLimitRatio: 0.95
            hardLimitRatio: 0.8
`,
		"fallback without backends": `
broker:
  tierRouting:
    enabled: true
    failureMode: fallback-backends
`,
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "broker.yaml")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			if _, err := Load(path); err == nil {
				t.Fatal("expected invalid tier routing config to fail")
			}
		})
	}
}

func TestDefaultIncludesDockerAndVMBackends(t *testing.T) {
	cfg := Default()
	pool := cfg.Pools[1]

	codebuildCfg, ok := pool.Backends["codebuild"]
	if !ok {
		t.Fatal("expected lite pool to include codebuild backend")
	}
	if !contains(codebuildCfg.Capabilities, "docker") {
		t.Fatalf("expected codebuild to advertise docker capability, got %v", codebuildCfg.Capabilities)
	}

	azureVMCfg, ok := pool.Backends["azure-vm"]
	if !ok {
		t.Fatal("expected lite pool to include azure-vm backend")
	}
	if azureVMCfg.RunnerLabel != "replace-with-private-azure-vm-runner-label" {
		t.Fatalf("expected default azure-vm runner label replace-with-private-azure-vm-runner-label, got %q", azureVMCfg.RunnerLabel)
	}

	for _, name := range []model.BackendName{model.BackendEC2, model.BackendGCE} {
		cfg, ok := pool.Backends[name]
		if !ok {
			t.Fatalf("expected lite pool to include %s backend", name)
		}
		if cfg.SecretRef == "" {
			t.Fatalf("expected %s backend to have a secretRef", name)
		}
		if !contains(cfg.Capabilities, "docker") || !contains(cfg.Capabilities, "vm") {
			t.Fatalf("expected %s backend to advertise docker and vm capabilities, got %v", name, cfg.Capabilities)
		}
	}
}

func contains(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func TestValidateLiveCapacity(t *testing.T) {
	cfg := Default()
	cfg.Broker.LiveCapacity.Enabled = true
	cfg.Broker.LiveCapacity.FailureMode = "not-a-mode"
	if err := Validate(cfg); err == nil {
		t.Fatal("expected invalid failureMode to fail validation")
	}
	cfg.Broker.LiveCapacity.FailureMode = "block"
	if err := Validate(cfg); err != nil {
		t.Fatalf("expected valid live capacity config: %v", err)
	}
}

func TestLoadParsesLiveCapacity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broker.yaml")
	if err := os.WriteFile(path, []byte(`
broker:
  liveCapacity:
    enabled: true
    refreshInterval: 15s
    staleAfter: 1m
    probeTimeout: 1s
    failureMode: block
    refreshOnStartup: false
`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !cfg.Broker.LiveCapacity.Enabled {
		t.Fatal("expected live capacity enabled")
	}
	if cfg.Broker.LiveCapacity.RefreshInterval != 15*time.Second {
		t.Fatalf("unexpected refresh interval %s", cfg.Broker.LiveCapacity.RefreshInterval)
	}
	if cfg.Broker.LiveCapacity.FailureMode != "block" {
		t.Fatalf("unexpected failure mode %q", cfg.Broker.LiveCapacity.FailureMode)
	}
	if cfg.Broker.LiveCapacity.RefreshOnStartup {
		t.Fatal("expected refreshOnStartup false")
	}
}
