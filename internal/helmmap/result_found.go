package helmmap

import (
	"fmt"
	"strings"

	"github.com/sserkanml/backflow/internal/rendermap"
)

var _ rendermap.Diagnostics = (*Result)(nil)

// Found is the part of the result the engine works with, with this result as
// the source of the explanations.
func (r *Result) Found() *rendermap.Found {
	return &rendermap.Found{Candidates: r.Candidates, Overrides: r.Overrides, Diagnostics: r}
}

func hasOverrideFor(f *rendermap.Found, p rendermap.KeyPath) bool {
	for _, c := range f.Overrides {
		if c.Path.Equal(p) {
			return true
		}
	}
	return false
}

// Explain implements rendermap.Diagnostics: where the value comes from when it
// is not a key a value file defines.
func (r *Result) Explain(found *rendermap.Found, overrides bool) []string {
	var out []string
	for _, p := range r.ChartDefaults {
		switch {
		case !overrides:
			out = append(out, fmt.Sprintf("the value comes from the chart's own defaults (%s); chart defaults are not edited", p))
		case hasOverrideFor(found, p):
			out = append(out, fmt.Sprintf("the value comes from the chart's own defaults (%s)", p))
		default:
			reason := "it cannot be overridden"
			for _, n := range r.NoOverride {
				if n.Path.Equal(p) {
					reason = n.Reason
				}
			}
			out = append(out, fmt.Sprintf("the value comes from the chart's own defaults (%s); %s", p, reason))
		}
	}
	for _, c := range r.Controlled {
		out = append(out, fmt.Sprintf("the value is set by %s at %s in the Application, not by a value file", c.Layer, c.Path))
	}
	if len(r.RemoteValueFiles) > 0 {
		out = append(out, "it may come from a remote value file: "+strings.Join(r.RemoteValueFiles, ", "))
	}
	return out
}

// ExplainEmpty implements rendermap.Diagnostics: dependencies whose defaults
// were not read, said only when nothing else offers a key.
func (r *Result) ExplainEmpty(found *rendermap.Found) []string {
	if len(r.UnreadDependencies) == 0 || len(found.Candidates) != 0 || len(found.Overrides) != 0 {
		return nil
	}
	return []string{fmt.Sprintf("the chart depends on %s, whose defaults are not in the repository (Argo CD downloads it when it renders), so the value may come from them and cannot be overridden",
		strings.Join(r.UnreadDependencies, ", "))}
}
