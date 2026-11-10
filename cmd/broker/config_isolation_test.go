package main

import (
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/store"
)

const contractMixedConfig = `
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

func TestLoadBrokerConfigRejectsProcessWideFailures(t *testing.T) {
	t.Run("unreadable file", func(t *testing.T) {
		_, _, err := loadBrokerConfig(filepath.Join(t.TempDir(), "missing.yaml"))
		if err == nil {
			t.Fatal("missing file should be fatal")
		}
	})
	t.Run("invalid yaml", func(t *testing.T) {
		_, _, err := loadBrokerConfig(writeBrokerYAML(t, "pools: [\n"))
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "yaml") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("missing auth", func(t *testing.T) {
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
        maxRunners: 1
`
		_, disabled, err := loadBrokerConfig(writeBrokerYAML(t, body))
		if err == nil || len(disabled) != 0 || !strings.Contains(err.Error(), "github.auth.secretRef") {
			t.Fatalf("err=%v disabled=%v", err, disabled)
		}
	})
	t.Run("broken state store type", func(t *testing.T) {
		_, _, err := loadBrokerConfig(writeBrokerYAML(t, "broker:\n  stateStore:\n    type: redis\n"))
		if err == nil || !strings.Contains(err.Error(), "broker.stateStore.type") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("all pools invalid", func(t *testing.T) {
		body := `
pools:
  - name: full
    backends:
      kubernetes-homelabai:
        driver: kubernetes
        enabled: true
        healthy: true
        maxRunners: 2
        crossPoolMaxRunners: 2
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
        warmMin: 2
        warmMax: 2
        kubernetes:
          namespace: arc-runners
`
		cfg, disabled, err := loadBrokerConfig(writeBrokerYAML(t, body))
		if err == nil {
			t.Fatal("all pools invalid should be fatal")
		}
		if len(cfg.Pools) != 0 {
			t.Fatalf("serving pools = %d", len(cfg.Pools))
		}
		if len(disabled) != 2 || !strings.Contains(err.Error(), "all pools failed validation") {
			t.Fatalf("err=%v disabled=%v", err, disabled)
		}
		if !strings.Contains(err.Error(), "kubernetes-homelabai") || !strings.Contains(err.Error(), "kubernetes-other") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestOpenStateStoreRejectsBrokenStore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := model.BrokerConfig{Broker: model.BrokerRuntimeConfig{StateStore: model.StateStoreConfig{Type: "file", Path: path}}}
	if _, err := openStateStore(cfg); err == nil {
		t.Fatal("corrupt state file should be fatal")
	}

	cfg.Broker.StateStore = model.StateStoreConfig{Type: "postgres", DSNEnv: "UECB_TEST_MISSING_DSN_219"}
	t.Setenv("UECB_TEST_MISSING_DSN_219", "")
	if _, err := openStateStore(cfg); err == nil || !strings.Contains(err.Error(), "UECB_TEST_MISSING_DSN_219") {
		t.Fatalf("missing postgres dsn error = %v", err)
	}

	mem, err := openStateStore(model.BrokerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if store.IsNil(mem) {
		t.Fatal("default store is nil")
	}
}

// TestContractBrokerBinaryInvalidPoolStaysUp runs the broker binary. One
// invalid pool must not crash the process; the other pool keeps serving and
// the failure shows up on /healthz and /metrics.
func TestContractBrokerBinaryInvalidPoolStaysUp(t *testing.T) {
	addr := freeAddr(t)
	proc := startBroker(t, contractMixedConfig, addr)
	base := "http://" + addr
	health := waitHealthy(t, proc, base)
	for _, needle := range []string{
		`"status":"degraded"`,
		`"pool":"full"`,
		`"backend":"kubernetes-homelabai"`,
		"warm runners are not supported",
		"pools[full].backends[kubernetes-homelabai]",
	} {
		if !strings.Contains(health, needle) {
			t.Fatalf("health missing %q\n%s\nlogs:\n%s", needle, health, proc.logs.String())
		}
	}
	logs := proc.logs.String()
	if !strings.Contains(logs, "CONFIG ERROR: disabled pool full backend kubernetes-homelabai") {
		t.Fatalf("logs missing disabled pool line:\n%s", logs)
	}

	metrics := httpGet(t, base+"/metrics")
	for _, needle := range []string{
		"uecb_pool_config_disabled",
		`pool="full"`,
		`backend="kubernetes-homelabai"`,
		"warm runners are not supported",
	} {
		if !strings.Contains(metrics, needle) {
			t.Fatalf("metrics missing %q\n%s", needle, metrics)
		}
	}

	liteCode, liteBody := httpPost(t, base+"/v1/allocations", `{"pool":"lite","job_timeout":"5m"}`)
	if liteCode != http.StatusCreated || !strings.Contains(liteBody, `"pool":"lite"`) {
		t.Fatalf("lite allocation = %d %s", liteCode, liteBody)
	}
	fullCode, fullBody := httpPost(t, base+"/v1/allocations", `{"pool":"full","job_timeout":"5m"}`)
	if fullCode == http.StatusCreated || (!strings.Contains(fullBody, "unknown_pool") && !strings.Contains(fullBody, "not configured")) {
		t.Fatalf("full allocation = %d %s", fullCode, fullBody)
	}
}

func TestContractBrokerBinaryFatalConfigsExit(t *testing.T) {
	cases := []struct {
		name  string
		yaml  string
		setup func(t *testing.T) string
		want  string
	}{
		{
			name: "invalid yaml",
			yaml: "pools: [\n",
			want: "load config:",
		},
		{
			name: "missing file",
			setup: func(t *testing.T) string {
				t.Helper()
				return filepath.Join(t.TempDir(), "missing.yaml")
			},
			want: "load config:",
		},
		{
			name: "missing auth",
			yaml: `
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
        maxRunners: 1
`,
			want: "github.auth.secretRef",
		},
		{
			name: "all pools invalid",
			yaml: `
pools:
  - name: full
    backends:
      kubernetes-homelabai:
        driver: kubernetes
        enabled: true
        healthy: true
        maxRunners: 2
        crossPoolMaxRunners: 2
        warmMin: 1
        warmMax: 1
        kubernetes: {namespace: arc-runners}
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
        kubernetes: {namespace: arc-runners}
`,
			want: "all pools failed validation",
		},
		{
			name: "broken state store",
			setup: func(t *testing.T) string {
				t.Helper()
				dir := t.TempDir()
				state := filepath.Join(dir, "state.json")
				if err := os.WriteFile(state, []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
				body := "broker:\n  stateStore:\n    type: file\n    path: " + state + "\n"
				return writeBrokerYAML(t, body)
			},
			want: "state store:",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var path string
			if tc.setup != nil {
				path = tc.setup(t)
			} else {
				path = writeBrokerYAML(t, tc.yaml)
			}
			code, logs := runBrokerUntilExit(t, path)
			if code == 0 {
				t.Fatalf("broker exited 0\n%s", logs)
			}
			if !strings.Contains(logs, tc.want) {
				t.Fatalf("logs missing %q\n%s", tc.want, logs)
			}
			if tc.name == "all pools invalid" {
				for _, needle := range []string{"kubernetes-homelabai", "kubernetes-other", "pools[full]", "pools[lite]"} {
					if !strings.Contains(logs, needle) {
						t.Fatalf("logs missing %q\n%s", needle, logs)
					}
				}
			}
		})
	}
}

func writeBrokerYAML(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "broker.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

var (
	brokerOnce sync.Once
	brokerBin  string
	brokerErr  error
)

func brokerBinary(t *testing.T) string {
	t.Helper()
	brokerOnce.Do(func() {
		path := filepath.Join(os.TempDir(), "uecb-broker-config-isolation")
		cmd := exec.Command("go", "build", "-o", path, "./cmd/broker")
		cmd.Dir = moduleRoot()
		out, err := cmd.CombinedOutput()
		if err != nil {
			brokerErr = errors.New(string(out) + "\n" + err.Error())
			return
		}
		brokerBin = path
	})
	if brokerErr != nil {
		t.Fatal(brokerErr)
	}
	return brokerBin
}

func moduleRoot() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "."
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(file)))
}

type brokerProc struct {
	cmd  *exec.Cmd
	logs *bytes.Buffer
	done chan struct{}
	err  error
}

func startBroker(t *testing.T, yaml string, addr string) *brokerProc {
	t.Helper()
	return startBrokerPath(t, writeBrokerYAML(t, yaml), addr)
}

func startBrokerPath(t *testing.T, configPath, addr string) *brokerProc {
	t.Helper()
	cmd := exec.Command(brokerBinary(t))
	cmd.Env = brokerEnv(configPath, addr)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	logs := &bytes.Buffer{}
	cmd.Stdout = logs
	cmd.Stderr = logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	proc := &brokerProc{cmd: cmd, logs: logs, done: make(chan struct{})}
	go func() {
		proc.err = cmd.Wait()
		close(proc.done)
	}()
	t.Cleanup(func() {
		stopBroker(proc)
	})
	return proc
}

func brokerEnv(configPath, addr string) []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"UECB_CONFIG_PATH=" + configPath,
		"UECB_HTTP_ADDR=" + addr,
	}
	if tmp := os.Getenv("TMPDIR"); tmp != "" {
		env = append(env, "TMPDIR="+tmp)
	}
	return env
}

func stopBroker(proc *brokerProc) {
	if proc == nil || proc.cmd == nil || proc.cmd.Process == nil {
		return
	}
	select {
	case <-proc.done:
		return
	default:
	}
	_ = syscall.Kill(-proc.cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-proc.done:
	case <-time.After(5 * time.Second):
		_ = syscall.Kill(-proc.cmd.Process.Pid, syscall.SIGKILL)
		<-proc.done
	}
}

func (p *brokerProc) wait(timeout time.Duration) error {
	select {
	case <-p.done:
		return p.err
	case <-time.After(timeout):
		stopBroker(p)
		return errors.New("timeout waiting for broker to exit")
	}
}

func runBrokerUntilExit(t *testing.T, configPath string) (int, string) {
	t.Helper()
	proc := startBrokerPath(t, configPath, freeAddr(t))
	err := proc.wait(15 * time.Second)
	logs := proc.logs.String()
	if err == nil {
		return 0, logs
	}
	if errors.Is(err, errors.New("timeout waiting for broker to exit")) || err.Error() == "timeout waiting for broker to exit" {
		t.Fatalf("%v\n%s", err, logs)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), logs
	}
	t.Fatalf("broker wait: %v\n%s", err, logs)
	return -1, logs
}

func waitHealthy(t *testing.T, proc *brokerProc, base string) string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var last string
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		select {
		case <-proc.done:
			t.Fatalf("broker exited: %v\n%s", proc.err, proc.logs.String())
		default:
		}
		resp, err := client.Get(base + "/healthz")
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			last = string(body)
			if resp.StatusCode == http.StatusOK {
				return last
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("broker did not become healthy: %s\nlogs:\n%s", last, proc.logs.String())
	return ""
}

func httpGet(t *testing.T, url string) string {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d %s", url, resp.StatusCode, body)
	}
	return string(body)
}

func httpPost(t *testing.T, url, payload string) (int, string) {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(url, "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}
