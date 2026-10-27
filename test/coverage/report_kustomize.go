//go:build coverage

package coverage

import (
	"fmt"
	"sort"
	"strings"
)

var sources = []string{sourceHelm, sourceKustomize, sourceHelmMulti, sourceDirectory}

// policiesOf are the policy sets the cases of a source type run under.
func policiesOf(source string) []policySet {
	switch source {
	case sourceKustomize:
		return kustomizeModes()
	case sourceDirectory:
		return directoryPolicies()
	}
	return policySets()
}

func bySource(results []caseResult, source string) []caseResult {
	var out []caseResult
	for _, r := range results {
		if r.Source == source {
			out = append(out, r)
		}
	}
	return out
}

// sourceTypeTable is the "By source type" table: every source type under each
// of its policy sets.
func sourceTypeTable(all []caseResult) string {
	var b strings.Builder
	b.WriteString("## By source type\n\n")
	b.WriteString("| Source | Policy | Mapped | Partial | Unmapped | Cases | Mapped % | Mapped or partial % |\n|---|---|---|---|---|---|---|---|\n")
	for _, src := range sources {
		rs := bySource(all, src)
		for _, p := range policiesOf(src) {
			m, pa, total := tally(rs, p.name)
			fmt.Fprintf(&b, "| %s | %s | %d | %d | %d | %d | %.0f%% | %.0f%% |\n", src, p.name, m, pa, total-m-pa, total, pct(m, total), pct(m+pa, total))
		}
	}
	return b.String()
}

// kustomizeReport is the Kustomize part of REPORT.md.
func kustomizeReport(results []caseResult) string {
	var b strings.Builder
	b.WriteString("\n## Kustomize\n\n")
	b.WriteString("Each row is one change made in the cluster to a resource built from an overlay of `testdata/kustomize`, mapped back to its files with the production Kustomize mapping path (the renderer is `kustomize build`, v5.8.1 as in Argo CD; `make kustomize` installs it). The overlays share one base; `kustomize-dev-images` is the `dev` overlay with `spec.source.kustomize.images` set on the Application.\n\n")
	b.WriteString("Modes: **default** is a policy with no options (`spec.kustomize.addPatches` on, `editBases` off); **editBases** also allows edits of files outside the Application's path; **no-addPatches** turns `addPatches` off, so a value that no setting, patch or file holds stays Unmapped. The bracket after an edit is its tier.\n\n")
	b.WriteString("| Mode | Mapped | Partial | Unmapped | Cases | Mapped % |\n|---|---|---|---|---|---|\n")
	for _, p := range kustomizeModes() {
		m, pa, total := tally(results, p.name)
		fmt.Fprintf(&b, "| %s | %d | %d | %d | %d | %.0f%% |\n", p.name, m, pa, total-m-pa, total, pct(m, total))
	}
	group := func(title string, key func(caseResult) string) {
		fmt.Fprintf(&b, "\n### %s\n\n| %s | Cases | Mapped %% (default) | Mapped %% (editBases) | Mapped %% (no-addPatches) |\n|---|---|---|---|---|\n", title, title)
		groups := map[string][]caseResult{}
		var names []string
		for _, r := range results {
			k := key(r)
			if _, ok := groups[k]; !ok {
				names = append(names, k)
			}
			groups[k] = append(groups[k], r)
		}
		sort.Strings(names)
		for _, n := range names {
			d, _, total := tally(groups[n], "default")
			e, _, _ := tally(groups[n], "editBases")
			na, _, _ := tally(groups[n], "no-addPatches")
			fmt.Fprintf(&b, "| %s | %d | %.0f%% | %.0f%% | %.0f%% |\n", n, total, pct(d, total), pct(e, total), pct(na, total))
		}
	}
	group("By Application", func(r caseResult) string { return r.Chart })
	group("By scenario", func(r caseResult) string { return r.Scenario })
	b.WriteString("\n### Cases\n\n| Application | Resource | Scenario | Changed fields | default | editBases | no-addPatches |\n|---|---|---|---|---|---|---|\n")
	for _, r := range results {
		var paths []string
		for _, c := range r.Changes {
			paths = append(paths, "`"+c.Path+"`")
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s |\n", r.Chart, r.Resource, r.Scenario, strings.Join(paths, "<br>"),
			cellOf(r.ByPolicy["default"]), cellOf(r.ByPolicy["editBases"]), cellOf(r.ByPolicy["no-addPatches"]))
	}
	return b.String()
}
