//go:build coverage

package coverage

import (
	"fmt"
	"sort"
	"strings"
)

// multiSourceReport is the part of REPORT.md for Applications with a chart from
// a Helm repository and their value files in a Git repository.
func multiSourceReport(results []caseResult) string {
	var b strings.Builder
	b.WriteString("\n## Helm, several sources\n\n")
	b.WriteString("The charts `nginx` and `webapp` of the Helm section again, deployed as an Application with a chart from a Helm repository and its value files in a Git repository (`$values/...`). The chart is not in Git: its own `values.yaml` and the names of its subcharts stand in for what Argo CD returns for it (`appdetails`), and the renderer is `helm template` over the chart files and the edited value file; in production Argo CD renders the commit of the edit by SHA. The policies are those of the Helm section.\n\n")
	b.WriteString("| Policy | Mapped | Partial | Unmapped | Cases | Mapped % | Mapped or partial % |\n|---|---|---|---|---|---|---|\n")
	for _, p := range policySets() {
		m, pa, total := tally(results, p.name)
		fmt.Fprintf(&b, "| %s | %d | %d | %d | %d | %.0f%% | %.0f%% |\n", p.name, m, pa, total-m-pa, total, pct(m, total), pct(m+pa, total))
	}
	b.WriteString("\n### By chart\n\n| Chart | Cases | Mapped % (default) | Mapped or partial % (permissive) |\n|---|---|---|---|\n")
	groups := map[string][]caseResult{}
	var names []string
	for _, r := range results {
		if _, ok := groups[r.Chart]; !ok {
			names = append(names, r.Chart)
		}
		groups[r.Chart] = append(groups[r.Chart], r)
	}
	sort.Strings(names)
	for _, n := range names {
		m, _, total := tally(groups[n], "default")
		m2, pa2, _ := tally(groups[n], "permissive")
		fmt.Fprintf(&b, "| %s | %d | %.0f%% | %.0f%% |\n", n, total, pct(m, total), pct(m2+pa2, total))
	}
	b.WriteString("\n### Cases\n\n| Chart | Resource | Scenario | Changed fields | default | permissive | no-overrides |\n|---|---|---|---|---|---|---|\n")
	for _, r := range results {
		var paths []string
		for _, c := range r.Changes {
			paths = append(paths, "`"+c.Path+"`")
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s |\n", r.Chart, r.Resource, scenarioLabel(r.Scenario), strings.Join(paths, "<br>"),
			cellOf(r.ByPolicy["default"]), cellOf(r.ByPolicy["permissive"]), cellOf(r.ByPolicy["no-overrides"]))
	}
	return b.String()
}
