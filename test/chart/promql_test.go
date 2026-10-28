package chart

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/sserkanml/backflow/internal/metrics"
)

// checkExpr parses a PromQL expression with the parser of Prometheus and checks
// it against the metrics of Backflow: every Backflow metric it selects exists, the
// labels it matches on are labels of that metric, and the labels it groups by are
// labels of a metric in the expression. This does what promtool does for syntax, and
// catches what promtool cannot: a metric or a label that does not exist.
func checkExpr(t *testing.T, where, expr string) {
	t.Helper()
	known := metrics.Labels()
	root, err := parser.ParseExpr(expr)
	if err != nil {
		t.Errorf("%s: %v\n%s", where, err, expr)
		return
	}
	union := map[string]bool{"le": true}
	var groupings []string
	selected := 0
	parser.Inspect(root, func(node parser.Node, _ []parser.Node) error {
		switch n := node.(type) {
		case *parser.VectorSelector:
			base := n.Name
			for _, suffix := range []string{"_bucket", "_sum", "_count"} {
				if _, ok := known[strings.TrimSuffix(base, suffix)]; ok && strings.HasSuffix(base, suffix) {
					base = strings.TrimSuffix(base, suffix)
				}
			}
			labels, ok := known[base]
			if !ok {
				if strings.HasPrefix(n.Name, "backflow_") {
					t.Errorf("%s: %s is not a metric of Backflow\n%s", where, n.Name, expr)
				}
				return nil
			}
			selected++
			for _, l := range labels {
				union[l] = true
			}
			for _, m := range n.LabelMatchers {
				if m.Name != "__name__" && !slices.Contains(labels, m.Name) && m.Name != "le" {
					t.Errorf("%s: %s has no label %q (it has %v)\n%s", where, base, m.Name, labels, expr)
				}
			}
		case *parser.AggregateExpr:
			groupings = append(groupings, n.Grouping...)
		}
		return nil
	})
	if selected == 0 {
		t.Errorf("%s: selects no metric of Backflow\n%s", where, expr)
	}
	for _, g := range groupings {
		if !union[g] {
			t.Errorf("%s: groups by %q, which no metric in the expression has\n%s", where, g, expr)
		}
	}
}

func TestAlertExpressionsParse(t *testing.T) {
	got := renderTemplates(t, []string{ruleTemplate}, "--set", "prometheusRule.enabled=true")
	for _, rule := range rulesOf(t, got[0]) {
		expr, _ := rule["expr"].(string)
		checkExpr(t, fmt.Sprint(rule["alert"]), expr)
	}
}

func TestDashboardExpressionsParse(t *testing.T) {
	raw, err := os.ReadFile(dashboardFile)
	if err != nil {
		t.Fatal(err)
	}
	var dash struct {
		Panels []struct {
			Title   string `json:"title"`
			Targets []struct {
				Expr string `json:"expr"`
			} `json:"targets"`
		} `json:"panels"`
	}
	if err := json.Unmarshal(raw, &dash); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, p := range dash.Panels {
		for _, target := range p.Targets {
			// Grafana fills in its own variables before the query goes out.
			expr := strings.NewReplacer("$__rate_interval", "5m", "$__interval", "1m").Replace(target.Expr)
			checkExpr(t, "panel "+p.Title, expr)
			n++
		}
	}
	if n < 20 {
		t.Errorf("only %d queries in the dashboard", n)
	}
}

// TestReadmeExpressionsParse checks the PromQL examples of the README.
func TestReadmeExpressionsParse(t *testing.T) {
	raw, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	_, rest, _ := strings.Cut(string(raw), "```promql\n")
	block, _, _ := strings.Cut(rest, "```")
	n := 0
	for example := range strings.SplitSeq(block, "\n\n") {
		var lines []string
		for l := range strings.SplitSeq(example, "\n") {
			if !strings.HasPrefix(l, "#") {
				lines = append(lines, l)
			}
		}
		if expr := strings.TrimSpace(strings.Join(lines, "\n")); expr != "" {
			checkExpr(t, "README", expr)
			n++
		}
	}
	if n < 8 {
		t.Errorf("only %d examples found in the README", n)
	}
}
