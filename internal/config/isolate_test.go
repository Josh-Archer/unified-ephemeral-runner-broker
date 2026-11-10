package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

const mixedPoolConfig = `
broker:
  allowUnauthenticated: true
  defaultPool: lite
  stateStore:
    type: memory
pools:
  - name: full
    scheduler: round-robin
    backends:
      arc:
        enabled: true
        healthy: true
        maxRunners: 2
        weight: 1
      kubernetes-homelabai:
        driver: kubernetes
        enabled: true
        healthy: true
        maxRunners: 3
        crossPoolMaxRunners: 3
        warmMin: 1
        warmMax: 1
        kubernetes:
          namespace: arc-runners
  - name: lite
    scheduler: round-robin
    backends:
      arc:
        enabled: true
        healthy: true
        maxRunners: 2
        weight: 1
`

func TestLoadServingKeepsValidPoolsWhenOnePoolIsInvalid(t *testing.T) {
	path := writeConfig(t, mixedPoolConfig)

	cfg, disabled, err := LoadServing(path)
	if err != nil {
		t.Fatalf("load serving: %v", err)
	}
	if len(cfg.Pools) != 1 || cfg.Pools[0].Name != model.PoolLite {
		t.Fatalf("serving pools = %+v, want only lite", poolNames(cfg))
	}
	if _, ok := cfg.Pools[0].Backends[model.BackendARC]; !ok {
		t.Fatal("lite pool lost its arc backend")
	}
	if len(disabled) != 1 {
		t.Fatalf("disabled = %+v, want one pool", disabled)
	}
	fault := disabled[0]
	if fault.Pool != model.PoolFull || fault.Backend != "kubernetes-homelabai" {
		t.Fatalf("fault pool=%s backend=%s, want full/kubernetes-homelabai", fault.Pool, fault.Backend)
	}
	if !strings.Contains(fault.Error(), "pools[full].backends[kubernetes-homelabai]") || !strings.Contains(fault.Error(), "warm runners are not supported") {
		t.Fatalf("fault error = %q", fault.Error())
	}

	strict, err := Load(path)
	if err == nil {
		t.Fatal("strict Load accepted a config with an invalid pool")
	}
	if !strings.Contains(err.Error(), "pools[full].backends[kubernetes-homelabai]") || !strings.Contains(err.Error(), "warm runners are not supported") {
		t.Fatalf("strict error = %q", err.Error())
	}
	if len(strict.Pools) != 0 {
		t.Fatal("strict Load returned pools with the error")
	}
	var parsed PoolBackendErrors
	if !errors.As(err, &parsed) || len(parsed) != 1 || parsed[0].Pool != model.PoolFull || parsed[0].Backend != "kubernetes-homelabai" {
		t.Fatalf("strict error type = %T %v", err, err)
	}
}

func TestLoadServingSizeClassReferencingDisabledBackendStillStarts(t *testing.T) {
	body := `
broker:
  allowUnauthenticated: true
  defaultPool: lite
  stateStore:
    type: memory
  sizeClasses:
    large:
      runner:
        requests:
          cpu: "1"
          memory: 1Gi
      backends: [kubernetes-homelabai, arc]
    onlybad:
      runner:
        requests:
          cpu: "1"
          memory: 1Gi
      backends: [kubernetes-homelabai]
pools:
  - name: full
    scheduler: round-robin
    backends:
      arc:
        enabled: true
        healthy: true
        maxRunners: 2
        weight: 1
      kubernetes-homelabai:
        driver: kubernetes
        enabled: true
        healthy: true
        maxRunners: 3
        crossPoolMaxRunners: 3
        warmMin: 1
        warmMax: 1
        kubernetes:
          namespace: arc-runners
  - name: lite
    scheduler: round-robin
    backends:
      arc:
        enabled: true
        healthy: true
        maxRunners: 2
        weight: 1
`

	cfg, disabled, err := LoadServing(writeConfig(t, body))
	if err != nil {
		t.Fatalf("load serving: %v", err)
	}
	if len(disabled) != 1 || disabled[0].Pool != model.PoolFull {
		t.Fatalf("disabled = %+v", disabled)
	}
	if names := poolNames(cfg); len(names) != 1 || names[0] != model.PoolLite {
		t.Fatalf("serving pools = %v", names)
	}
	large, ok := cfg.Broker.SizeClasses["large"]
	if !ok || len(large.Backends) != 1 || large.Backends[0] != model.BackendARC {
		t.Fatalf("large backends = %+v", large.Backends)
	}
	if _, ok := cfg.Broker.SizeClasses["onlybad"]; ok {
		t.Fatal("size class that only named the disabled backend should be removed")
	}
}

func TestPartitionDisablesPoolScopedFailure(t *testing.T) {
	cfg := Default()
	full := cfg.Pools[0]
	full.FairShare.Enabled = true
	full.FairShare.UsageWindow = -1
	cfg.Pools[0] = full

	serving, disabled, err := Partition(cfg)
	if err != nil {
		t.Fatalf("partition: %v", err)
	}
	if len(disabled) != 1 || disabled[0].Pool != model.PoolFull || disabled[0].Backend != "" {
		t.Fatalf("disabled = %+v", disabled)
	}
	if !strings.Contains(disabled[0].Error(), "pools[full].fairShare") {
		t.Fatalf("error = %q", disabled[0].Error())
	}
	if names := poolNames(serving); len(names) != 1 || names[0] != model.PoolLite {
		t.Fatalf("serving pools = %v", names)
	}
}

func TestLoadServingAllPoolsInvalidIsFatal(t *testing.T) {
	body := `
github:
  auth:
    secretRef: uecb-github-app
pools:
  - name: full
    backends:
      kubernetes-homelabai:
        driver: kubernetes
        enabled: true
        healthy: true
        maxRunners: 3
        crossPoolMaxRunners: 3
        warmMin: 1
        warmMax: 1
        kubernetes:
          namespace: arc-runners
  - name: lite
    backends:
      kubernetes-other:
        driver: kubernetes
        enabled: true
        healthy: true
        maxRunners: 2
        crossPoolMaxRunners: 2
        warmMin: 1
        warmMax: 1
        kubernetes:
          namespace: arc-runners
`
	cfg, disabled, err := LoadServing(writeConfig(t, body))
	if err == nil {
		t.Fatal("all pools invalid should be fatal")
	}
	if len(cfg.Pools) != 0 {
		t.Fatalf("serving pools = %v, want none", poolNames(cfg))
	}
	if !strings.Contains(err.Error(), "all pools failed validation") {
		t.Fatalf("error = %q", err.Error())
	}
	for _, needle := range []string{
		"pools[full].backends[kubernetes-homelabai]",
		"pools[lite].backends[kubernetes-other]",
		"warm runners are not supported",
	} {
		if !strings.Contains(err.Error(), needle) {
			t.Fatalf("error %q missing %q", err.Error(), needle)
		}
	}
	if len(disabled) != 2 {
		t.Fatalf("disabled = %+v, want both pools", disabled)
	}
	var faults PoolBackendErrors
	if !errors.As(err, &faults) || len(faults) != 2 {
		t.Fatalf("errors.As = %v (%d)", err, len(faults))
	}
}

func TestLoadServingProcessWideErrorsAreFatal(t *testing.T) {
	t.Run("unreadable file", func(t *testing.T) {
		_, disabled, err := LoadServing(filepath.Join(t.TempDir(), "missing.yaml"))
		if err == nil {
			t.Fatal("missing file should be fatal")
		}
		if len(disabled) != 0 {
			t.Fatalf("disabled = %+v", disabled)
		}
		if !os.IsNotExist(err) && !strings.Contains(err.Error(), "no such file") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("invalid yaml", func(t *testing.T) {
		_, disabled, err := LoadServing(writeConfig(t, "pools: [\n"))
		if err == nil {
			t.Fatal("invalid yaml should be fatal")
		}
		if len(disabled) != 0 {
			t.Fatalf("disabled = %+v", disabled)
		}
		if !strings.Contains(strings.ToLower(err.Error()), "yaml") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("missing auth", func(t *testing.T) {
		// A valid ARC pool must not keep the process up when GitHub auth is
		// missing. The credential is process-wide.
		body := `
github:
  auth:
    secretRef: ""
pools:
  - name: full
    backends:
      kubernetes:
        enabled: true
        healthy: true
        maxRunners: 1
        kubernetes:
          namespace: arc-runners
  - name: lite
    backends:
      arc:
        enabled: true
        healthy: true
        maxRunners: 2
`
		cfg, disabled, err := LoadServing(writeConfig(t, body))
		if err == nil {
			t.Fatal("missing github.auth.secretRef should be fatal")
		}
		if len(cfg.Pools) != 0 || len(disabled) != 0 {
			t.Fatalf("cfg pools=%v disabled=%v; missing auth must not isolate", poolNames(cfg), disabled)
		}
		if !strings.Contains(err.Error(), "github.auth.secretRef") || !strings.Contains(err.Error(), "pools[full].backends[kubernetes]") {
			t.Fatalf("error = %q", err.Error())
		}
	})

	t.Run("broken state store", func(t *testing.T) {
		body := `
broker:
  stateStore:
    type: redis
`
		cfg, disabled, err := LoadServing(writeConfig(t, body))
		if err == nil {
			t.Fatal("unsupported state store should be fatal")
		}
		if len(cfg.Pools) != 0 || len(disabled) != 0 {
			t.Fatalf("cfg pools=%v disabled=%v", poolNames(cfg), disabled)
		}
		if !strings.Contains(err.Error(), "broker.stateStore.type") {
			t.Fatalf("error = %q", err.Error())
		}
	})
}

func TestLoadServingEmptyPathUsesDefault(t *testing.T) {
	cfg, disabled, err := LoadServing("")
	if err != nil {
		t.Fatal(err)
	}
	if len(disabled) != 0 || len(cfg.Pools) != len(Default().Pools) {
		t.Fatalf("pools=%v disabled=%v", poolNames(cfg), disabled)
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "broker.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func poolNames(cfg model.BrokerConfig) []model.PoolName {
	names := make([]model.PoolName, 0, len(cfg.Pools))
	for _, pool := range cfg.Pools {
		names = append(names, pool.Name)
	}
	return names
}
