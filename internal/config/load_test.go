package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

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
