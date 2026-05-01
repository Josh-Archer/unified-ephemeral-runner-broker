package config

import (
	"testing"

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
	if azureVMCfg.RunnerLabel != "az-vm-gha" {
		t.Fatalf("expected default azure-vm runner label az-vm-gha, got %q", azureVMCfg.RunnerLabel)
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
