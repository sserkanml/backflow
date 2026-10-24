package controller

import (
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	backflowv1alpha1 "github.com/sserkanml/backflow/api/v1alpha1"
)

// multiApp builds an Application with these sources and, when given, status
// revisions and source types.
func multiApp(sources []any, compared, synced, types []any) *unstructured.Unstructured {
	app := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1", "kind": "Application",
		"metadata": map[string]any{"name": "multi", "namespace": "argocd"},
		"spec":     map[string]any{"sources": sources},
	}}
	status := map[string]any{}
	if compared != nil {
		status["sync"] = map[string]any{"revisions": compared}
	}
	if synced != nil {
		status["operationState"] = map[string]any{"phase": "Succeeded", "syncResult": map[string]any{"revisions": synced}}
	}
	if types != nil {
		status["sourceTypes"] = types
	}
	if len(status) > 0 {
		app.Object["status"] = status
	}
	return app
}

var (
	chartSource = map[string]any{
		"repoURL": "ghcr.io/stefanprodan/charts", "chart": "podinfo", "targetRevision": "6.7.*",
		"helm": map[string]any{"valueFiles": []any{"$values/env/values.yaml"}},
	}
	valuesRef = map[string]any{"repoURL": "https://gitlab.com/x/values.git", "targetRevision": "main", "ref": "values"}
)

func TestSummarizeSources(t *testing.T) {
	app := multiApp([]any{chartSource, valuesRef},
		[]any{"6.7.1", "aaaa"}, []any{"6.7.0", "bbbb"}, []any{"Helm", ""})
	got := summarizeSources(app)
	want := []backflowv1alpha1.SourceSummary{
		{Position: 1, RepoURL: "ghcr.io/stefanprodan/charts", Chart: "podinfo", TargetRevision: "6.7.*",
			Revision: "6.7.1", SyncedRevision: "6.7.0", SourceType: "Helm"},
		{Position: 2, RepoURL: "https://gitlab.com/x/values.git", TargetRevision: "main", Ref: "values",
			Revision: "aaaa", SyncedRevision: "bbbb"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sources = %+v\nwant     %+v", got, want)
	}

	t.Run("not reconciled yet: the spec says what each source is", func(t *testing.T) {
		got := summarizeSources(multiApp([]any{chartSource, valuesRef}, nil, nil, nil))
		if got[0].SourceType != "Helm" || got[1].SourceType != "" || got[0].Revision != "" {
			t.Errorf("sources = %+v", got)
		}
	})
	t.Run("a single-source Application has none", func(t *testing.T) {
		app := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"source": map[string]any{"repoURL": "x"}}}}
		if got := summarizeSources(app); got != nil {
			t.Errorf("sources = %+v", got)
		}
	})
}

func TestHelmMultiSource(t *testing.T) {
	gitChart := map[string]any{"repoURL": "https://gitlab.com/x/chart.git", "path": "charts/app", "helm": map[string]any{}}
	tests := []struct {
		name    string
		sources []any
		types   []any
		render  int
		refs    []string
		why     string
	}{
		{name: "a chart from a Helm repository and a ref", sources: []any{chartSource, valuesRef}, render: 0, refs: []string{"values"}},
		{name: "a chart in Git and a ref in another repository", sources: []any{gitChart, valuesRef}, render: 0, refs: []string{"values"}},
		{name: "the ref first", sources: []any{valuesRef, chartSource}, render: 1, refs: []string{"values"}},
		{name: "two refs", sources: []any{chartSource, valuesRef, map[string]any{"repoURL": "https://gitlab.com/x/more.git", "ref": "more"}},
			render: 0, refs: []string{"more", "values"}},
		{name: "no ref source", sources: []any{chartSource, gitChart}, why: "both render"},
		{name: "only a chart and a plain directory", sources: []any{chartSource, map[string]any{"repoURL": "https://gitlab.com/x/r.git", "path": "a"}},
			why: "both render"},
		{name: "a Kustomize source with a ref", sources: []any{map[string]any{"repoURL": "https://gitlab.com/x/r.git", "path": "a", "kustomize": map[string]any{}}, valuesRef},
			why: "not a Helm source"},
		{name: "Argo CD says the source is Kustomize", sources: []any{gitChart, valuesRef}, types: []any{"Kustomize", ""}, why: "Kustomize source"},
		{name: "a plugin", sources: []any{map[string]any{"repoURL": "https://gitlab.com/x/r.git", "path": "a", "helm": map[string]any{}, "plugin": map[string]any{}}, valuesRef},
			why: "plugin"},
		{name: "a ref that also renders", sources: []any{chartSource, map[string]any{"repoURL": "https://gitlab.com/x/r.git", "path": "a", "ref": "v"}},
			why: "ref source that also renders"},
		{name: "only refs", sources: []any{valuesRef, map[string]any{"repoURL": "https://gitlab.com/x/r.git", "ref": "o"}}, why: "no source renders"},
		{name: "a chart and nothing else", sources: []any{chartSource}, why: "no ref source"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ms, why := helmMultiSource(multiApp(tt.sources, nil, nil, tt.types))
			if tt.why != "" {
				if ms != nil || !strings.Contains(why, tt.why) {
					t.Fatalf("ms = %+v, why = %q, want %q", ms, why, tt.why)
				}
				return
			}
			if ms == nil {
				t.Fatalf("not matched: %s", why)
			}
			if ms.Render != tt.render || len(ms.Refs) != len(tt.refs) {
				t.Errorf("render = %d, refs = %v", ms.Render, ms.Refs)
			}
			for _, r := range tt.refs {
				if _, ok := ms.Refs[r]; !ok {
					t.Errorf("ref %s missing in %v", r, ms.Refs)
				}
			}
		})
	}
}

func TestSplitRefPath(t *testing.T) {
	tests := []struct{ raw, ref, path string }{
		{"$values/env/values.yaml", "values", "env/values.yaml"},
		{"${values}/env/values.yaml", "values", "env/values.yaml"},
		{"$values//env/values.yaml", "values", "env/values.yaml"},
		{"values-prod.yaml", "", "values-prod.yaml"},
		{"/shared/common.yaml", "", "/shared/common.yaml"},
		{"$values", "", "$values"},
		{"$/x", "", "$/x"},
	}
	for _, tt := range tests {
		got := splitRefPath(tt.raw)
		if got.Ref != tt.ref || got.Path != tt.path || got.Raw != tt.raw {
			t.Errorf("splitRefPath(%q) = %+v", tt.raw, got)
		}
	}
}

func TestValueFileRefs(t *testing.T) {
	helm := map[string]any{
		"valueFiles":     []any{"values.yaml", "$values/a.yaml", "$other/b.yaml"},
		"fileParameters": []any{map[string]any{"name": "tls", "path": "$values/tls.pem"}},
	}
	got := valueFileRefs(helm)
	var s []string
	for _, f := range got {
		s = append(s, f.Ref+":"+f.Path)
	}
	want := []string{":values.yaml", "values:a.yaml", "other:b.yaml", "values:tls.pem"}
	if !reflect.DeepEqual(s, want) {
		t.Errorf("files = %v, want %v", s, want)
	}
}
