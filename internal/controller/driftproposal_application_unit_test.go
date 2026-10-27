package controller

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/sserkanml/backflow/internal/appvalues"
	"github.com/sserkanml/backflow/internal/rendermap"
)

func TestJSONAt(t *testing.T) {
	obj := map[string]any{"spec": map[string]any{"a/b": []any{"x", map[string]any{"n": float64(2)}}}}
	for pointer, want := range map[string]string{
		"/spec/a~1b/0": `"x"`, "/spec/a~1b/1/n": "2", "/spec/a~1b/1": `{"n":2}`,
	} {
		if got, ok := jsonAt(obj, pointer); !ok || got != want {
			t.Errorf("%s: %q, %v; want %q", pointer, got, ok, want)
		}
	}
	for _, pointer := range []string{"", "/nope", "/spec/a~1b/2", "/spec/a~1b/x", "/spec/a~1b/0/y"} {
		if got, ok := jsonAt(obj, pointer); ok {
			t.Errorf("%q: found %q", pointer, got)
		}
	}
}

func TestApplicationVerdict(t *testing.T) {
	out := func(reason string, pointers ...string) *rendermap.Outcome {
		u := &rendermap.Unmapped{Reason: reason, Message: "engine"}
		for _, p := range pointers {
			u.Fields = append(u.Fields, rendermap.FieldFailure{Pointer: p, Reason: reason})
		}
		return &rendermap.Outcome{Unmapped: u}
	}
	opt := func(fields ...string) []appvalues.Option { return []appvalues.Option{{Option: "o", Fields: fields}} }
	for name, tc := range map[string]struct {
		out    *rendermap.Outcome
		opts   []appvalues.Option
		reason string
	}{
		"options set every unmapped field":     {out("NoVerifiedCandidate", "/a", "/b"), opt("/a", "/b"), rendermap.ReasonSetByApplication},
		"one field is not set by an option":    {out("NoVerifiedCandidate", "/a", "/b"), opt("/a"), "NoVerifiedCandidate"},
		"no options":                           {out("NoVerifiedCandidate", "/a"), nil, "NoVerifiedCandidate"},
		"no field is named, so nothing to say": {out("NoVerifiedCandidate"), opt("/a"), "NoVerifiedCandidate"},
	} {
		v := applicationVerdict(tc.out, tc.opts)
		if v.reason != tc.reason || len(v.options) != len(tc.opts) || v.err.Error() != "engine" {
			t.Errorf("%s: %+v", name, v)
		}
	}
}

func TestParentSourceProblem(t *testing.T) {
	app := func(source map[string]any, sourceType string, sources ...any) *unstructured.Unstructured {
		spec := map[string]any{"source": source}
		if len(sources) > 0 {
			spec["sources"] = sources
		}
		return &unstructured.Unstructured{Object: map[string]any{"spec": spec, "status": map[string]any{"sourceType": sourceType}}}
	}
	git := map[string]any{"repoURL": "https://git.test/x.git", "path": "p"}
	for name, tc := range map[string]struct {
		app  *unstructured.Unstructured
		want string
	}{
		"directory":       {app(git, "Directory"), ""},
		"helm":            {app(git, "Helm"), ""},
		"kustomize":       {app(git, "Kustomize"), ""},
		"several sources": {app(git, "Directory", map[string]any{}, map[string]any{}), "several sources"},
		"a chart":         {app(map[string]any{"repoURL": "https://charts.test", "chart": "podinfo"}, "Helm"), "does not have a Git source"},
		"no repository":   {app(map[string]any{}, "Directory"), "does not have a Git source"},
		"credentials":     {app(map[string]any{"repoURL": "https://u:p@git.test/x.git"}, "Directory"), "contains credentials"},
		"a plugin":        {app(git, "Plugin"), `"Plugin" source`},
	} {
		got := parentSourceProblem(tc.app)
		if (tc.want == "") != (got == "") || (tc.want != "" && !strings.Contains(got, tc.want)) {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
}
