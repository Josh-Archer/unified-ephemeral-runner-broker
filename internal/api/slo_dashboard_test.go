package api

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestSLODashboardJSON(t *testing.T) {
	root := documentationRepositoryRoot(t)
	packPath := filepath.Join(root, "observability", "grafana-dashboard.json")
	chartPath := filepath.Join(root, "charts", "unified-ephemeral-runner-broker", "dashboards", "uecb-slo.json")

	pack, err := os.ReadFile(packPath)
	if err != nil {
		t.Fatalf("read pack dashboard: %v", err)
	}
	chart, err := os.ReadFile(chartPath)
	if err != nil {
		t.Fatalf("read chart dashboard: %v", err)
	}
	if !bytes.Equal(pack, chart) {
		t.Fatal("charts/unified-ephemeral-runner-broker/dashboards/uecb-slo.json drifted from observability/grafana-dashboard.json")
	}

	var dashboard struct {
		Title         string `json:"title"`
		UID           string `json:"uid"`
		SchemaVersion int    `json:"schemaVersion"`
		Panels        []struct {
			Title   string `json:"title"`
			Type    string `json:"type"`
			Targets []struct {
				Expr string `json:"expr"`
			} `json:"targets"`
		} `json:"panels"`
	}
	if err := json.Unmarshal(pack, &dashboard); err != nil {
		t.Fatalf("dashboard JSON: %v", err)
	}
	if dashboard.Title == "" || dashboard.UID == "" || dashboard.SchemaVersion == 0 {
		t.Fatalf("dashboard missing title, uid, or schemaVersion: %+v", dashboard)
	}
	if len(dashboard.Panels) == 0 {
		t.Fatal("dashboard has no panels")
	}

	exprs := make([]string, 0)
	for _, panel := range dashboard.Panels {
		if panel.Title == "" || panel.Type == "" {
			t.Fatalf("panel missing title or type: %+v", panel)
		}
		for _, target := range panel.Targets {
			if strings.TrimSpace(target.Expr) == "" {
				t.Fatalf("panel %q has an empty expr", panel.Title)
			}
			exprs = append(exprs, target.Expr)
		}
	}
	joined := strings.Join(exprs, "\n")
	for _, want := range []string{
		`histogram_quantile(0.95, sum by (le, pool, backend) (rate(uecb_allocation_latency_seconds_bucket{result="success"}[5m])))`,
		`sum by (pool, backend) (rate(uecb_allocations_total{result="failure"}[5m]))`,
		`uecb_backend_estimated_cost_total`,
		`uecb_backend_cost_class`,
		`uecb_backend_estimated_unit_cost`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("dashboard exprs missing %q", want)
		}
	}
}

func TestSLOPrometheusRules(t *testing.T) {
	root := documentationRepositoryRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "observability", "prometheus-rules.yaml"))
	if err != nil {
		t.Fatalf("read rules: %v", err)
	}
	var doc struct {
		Groups []struct {
			Name  string `yaml:"name"`
			Rules []struct {
				Alert  string `yaml:"alert"`
				Expr   string `yaml:"expr"`
				For    string `yaml:"for"`
				Labels struct {
					SLO string `yaml:"slo"`
				} `yaml:"labels"`
			} `yaml:"rules"`
		} `yaml:"groups"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("rules YAML: %v", err)
	}
	found := map[string]string{}
	for _, group := range doc.Groups {
		if group.Name == "" {
			t.Fatal("rule group missing name")
		}
		for _, rule := range group.Rules {
			if rule.Alert == "" || strings.TrimSpace(rule.Expr) == "" || rule.For == "" {
				t.Fatalf("incomplete rule %+v", rule)
			}
			found[rule.Alert] = rule.Expr
		}
	}
	latency := found["UECBBackendAllocationLatencyHigh"]
	if !strings.Contains(latency, `uecb_allocation_latency_seconds_bucket{result="success"}`) || !strings.Contains(latency, "sum by (le, pool, backend)") {
		t.Fatalf("latency SLO expr = %q", latency)
	}
	failure := found["UECBBackendAllocationFailureRateHigh"]
	if !strings.Contains(failure, `uecb_allocations_total{result="failure"}`) || !strings.Contains(failure, "sum by (pool, backend)") {
		t.Fatalf("failure SLO expr = %q", failure)
	}
}

func TestChartSLOTemplatesRender(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is not installed")
	}
	root := documentationRepositoryRoot(t)
	chart := filepath.Join(root, "charts", "unified-ephemeral-runner-broker")

	defaultRender := helmTemplate(t, helm, chart)
	for _, want := range []string{
		"uecb-slo.json",
		"kind: ConfigMap",
		"grafana_dashboard: \"1\"",
		"UECBBackendAllocationLatencyHigh",
		"UECBBackendAllocationFailureRateHigh",
		`uecb_allocation_latency_seconds_bucket{result="success"}`,
		`uecb_allocations_total{result="failure"}`,
		"uecb_backend_estimated_cost_total",
		"uecb_backend_cost_class",
	} {
		if !strings.Contains(defaultRender, want) {
			t.Errorf("default helm template missing %q", want)
		}
	}
	if strings.Contains(defaultRender, "kind: PrometheusRule") {
		t.Fatal("default render emitted a PrometheusRule")
	}

	operatorRender := helmTemplate(t, helm, chart, "--set", "observability.slo.prometheusOperator=true")
	if !strings.Contains(operatorRender, "kind: PrometheusRule") {
		t.Fatal("prometheusOperator render missing PrometheusRule")
	}
	if strings.Contains(operatorRender, "slo-rules.yaml:") {
		t.Fatal("prometheusOperator render also emitted the rules ConfigMap")
	}

	disabled := helmTemplate(t, helm, chart,
		"--set", "observability.dashboard.enabled=false",
		"--set", "observability.slo.enabled=false",
	)
	if strings.Contains(disabled, "UECBBackendAllocationLatencyHigh") || strings.Contains(disabled, "uecb-slo.json") {
		t.Fatal("disabled observability still rendered the SLO dashboard or rules")
	}
}

func helmTemplate(t *testing.T, helm, chart string, extra ...string) string {
	t.Helper()
	args := append([]string{"template", "uecb", chart}, extra...)
	cmd := exec.Command(helm, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helm template %v: %v\n%s", extra, err, out)
	}
	return string(out)
}
