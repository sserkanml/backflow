package chart

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

const (
	ruleTemplate      = "templates/prometheusrule.yaml"
	dashboardTemplate = "templates/grafana-dashboard.yaml"
	dashboardFile     = "../../charts/backflow/dashboards/backflow.json"
)

// alerts the chart must ship, with the severity each has by default.
var alerts = map[string]string{
	"BackflowScmConnectionNotReady": "warning",
	"BackflowPolicyNotReady":        "warning",
	"BackflowArgoCDUnreachable":     "warning",
	"BackflowRenderErrors":          "warning",
	"BackflowDetectionPausedLong":   "warning",
	"BackflowPushRejected":          "warning",
	"BackflowScratchBranchesSwept":  "info",
}

func TestMonitoringIsOffByDefault(t *testing.T) {
	if got := renderTemplates(t, []string{ruleTemplate, dashboardTemplate}); len(got) != 0 {
		t.Errorf("the default chart renders monitoring objects:\n%s", text(t, got))
	}
}

func TestPrometheusRule(t *testing.T) {
	got := renderTemplates(t, []string{ruleTemplate}, "--set", "prometheusRule.enabled=true")
	golden(t, "prometheusrule.yaml", got)
	if len(got) != 1 || got[0]["kind"] != "PrometheusRule" {
		t.Fatalf("want one PrometheusRule:\n%s", text(t, got))
	}
	for _, rule := range rulesOf(t, got[0]) {
		name, _ := rule["alert"].(string)
		severity, ok := alerts[name]
		if !ok {
			t.Errorf("unexpected alert %q", name)
			continue
		}
		labels, _ := rule["labels"].(map[string]any)
		if labels["severity"] != severity {
			t.Errorf("%s: severity = %v, want %s", name, labels["severity"], severity)
		}
		annotations, _ := rule["annotations"].(map[string]any)
		for _, key := range []string{"summary", "description", "runbook"} {
			if v, _ := annotations[key].(string); strings.TrimSpace(v) == "" {
				t.Errorf("%s: no %s", name, key)
			}
		}
		expr, _ := rule["expr"].(string)
		all := expr + fmt.Sprint(annotations)
		for _, old := range []string{"$labels.name", "$labels.namespace", "by (policy,", "by (name", "by (namespace"} {
			if strings.Contains(all, old) {
				t.Errorf("%s uses %q: policies carry policy_namespace and policy, connections scm_namespace and scm", name, old)
			}
		}
		if v, _ := annotations["runbook"].(string); !strings.Contains(v, name) || !strings.Contains(v, "Metrics and alerts") {
			t.Errorf("%s: the runbook hint %q does not point to the README section", name, v)
		}
	}
	found := map[string]bool{}
	for _, rule := range rulesOf(t, got[0]) {
		found[rule["alert"].(string)] = true
	}
	for name := range alerts {
		if !found[name] {
			t.Errorf("alert %s is missing", name)
		}
	}
}

func TestPrometheusRuleLabels(t *testing.T) {
	got := renderTemplates(t, []string{ruleTemplate}, "--set", "prometheusRule.enabled=true",
		"--set", "prometheusRule.labels.release=stack", "--set", "prometheusRule.alertLabels.team=sre")
	meta, _ := got[0]["metadata"].(map[string]any)
	if labels, _ := meta["labels"].(map[string]any); labels["release"] != "stack" {
		t.Errorf("the rule is not labelled for the Prometheus: %v", labels)
	}
	for _, rule := range rulesOf(t, got[0]) {
		if labels, _ := rule["labels"].(map[string]any); labels["team"] != "sre" || labels["severity"] == nil {
			t.Errorf("%v: alert labels = %v", rule["alert"], labels)
		}
	}
}

func rulesOf(t *testing.T, obj map[string]any) []map[string]any {
	t.Helper()
	spec, _ := obj["spec"].(map[string]any)
	groups, _ := spec["groups"].([]any)
	var out []map[string]any
	for _, g := range groups {
		group, _ := g.(map[string]any)
		rules, _ := group["rules"].([]any)
		for _, r := range rules {
			rule, _ := r.(map[string]any)
			out = append(out, rule)
		}
	}
	if len(out) == 0 {
		t.Fatalf("no rules in %v", obj["kind"])
	}
	return out
}

// TestPromtoolChecksTheRules runs promtool on the rendered rules. Without promtool
// the test is skipped, except in CI, where a missing promtool fails it.
func TestPromtoolChecksTheRules(t *testing.T) {
	if _, err := exec.LookPath("promtool"); err != nil {
		if os.Getenv("CI") == "true" {
			t.Fatal("promtool is not installed; CI must install it, or the alert rules are never checked")
		}
		t.Skip("promtool is not installed")
	}
	got := renderTemplates(t, []string{ruleTemplate}, "--set", "prometheusRule.enabled=true")
	spec, _ := got[0]["spec"].(map[string]any)
	out, err := yaml.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "backflow.rules.yaml")
	if err := os.WriteFile(file, out, 0o600); err != nil {
		t.Fatal(err)
	}
	if b, err := exec.Command("promtool", "check", "rules", file).CombinedOutput(); err != nil {
		t.Fatalf("promtool check rules: %v\n%s", err, b)
	}
}

func TestGrafanaDashboard(t *testing.T) {
	got := renderTemplates(t, []string{dashboardTemplate}, "--set", "grafanaDashboard.enabled=true")
	if len(got) != 1 || got[0]["kind"] != "ConfigMap" {
		t.Fatalf("want one ConfigMap:\n%s", text(t, got))
	}
	data, _ := got[0]["data"].(map[string]any)
	rendered, _ := data["backflow.json"].(string)
	want, err := os.ReadFile(dashboardFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(rendered) != strings.TrimSpace(string(want)) {
		t.Error("the ConfigMap does not carry dashboards/backflow.json as it is")
	}
	var dash struct {
		Title  string `json:"title"`
		Panels []struct {
			Type    string `json:"type"`
			Title   string `json:"title"`
			Targets []struct {
				Expr string `json:"expr"`
			} `json:"targets"`
		} `json:"panels"`
	}
	if err := json.Unmarshal([]byte(rendered), &dash); err != nil {
		t.Fatalf("the dashboard is not JSON: %v", err)
	}
	rows := map[string]bool{}
	for _, p := range dash.Panels {
		if p.Type == "row" {
			rows[p.Title] = true
		}
		for _, target := range p.Targets {
			if strings.Contains(target.Expr, "backflow_") {
				continue
			}
			t.Errorf("panel %q has an expression without a Backflow metric: %q", p.Title, target.Expr)
		}
	}
	for _, row := range []string{"Overview", "Latency", "Dependencies", "State"} {
		if !rows[row] {
			t.Errorf("the dashboard has no %s row", row)
		}
	}

	// Golden: the object without the dashboard, which has its own file.
	got[0]["data"] = map[string]any{"backflow.json": "<dashboards/backflow.json>"}
	golden(t, "grafana-dashboard.yaml", got)
}

func TestGrafanaDashboardLabel(t *testing.T) {
	got := renderTemplates(t, []string{dashboardTemplate}, "--set", "grafanaDashboard.enabled=true",
		"--set-string", "grafanaDashboard.labels.grafana_dashboard=team-a", "--set", "grafanaDashboard.namespace=monitoring")
	meta, _ := got[0]["metadata"].(map[string]any)
	if labels, _ := meta["labels"].(map[string]any); labels["grafana_dashboard"] != "team-a" {
		t.Errorf("labels = %v", labels)
	}
	if meta["namespace"] != "monitoring" {
		t.Errorf("namespace = %v", meta["namespace"])
	}
}
