package controller

import (
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/event"

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
	s := make([]string, 0, len(got))
	for _, f := range got {
		s = append(s, f.Ref+":"+f.Path)
	}
	want := []string{":values.yaml", "values:a.yaml", "other:b.yaml", "values:tls.pem"}
	if !reflect.DeepEqual(s, want) {
		t.Errorf("files = %v, want %v", s, want)
	}
}

func TestProposalSources(t *testing.T) {
	other := map[string]any{"repoURL": "https://gitlab.com/x/more.git", "targetRevision": "dev", "ref": "more"}
	app := multiApp([]any{chartSource, valuesRef, other}, []any{"6.7.1", "aaaa", "bbbb"}, nil, []any{"Helm", "", ""})
	got := proposalSources(app)
	roles := make([]backflowv1alpha1.SourceRole, len(got))
	revs := make([]string, len(got))
	for i, s := range got {
		roles[i], revs[i] = s.Role, s.Revision
	}
	if !reflect.DeepEqual(roles, []backflowv1alpha1.SourceRole{backflowv1alpha1.RoleRender, backflowv1alpha1.RoleValues, backflowv1alpha1.RoleReference}) ||
		!reflect.DeepEqual(revs, []string{"6.7.1", "aaaa", "bbbb"}) {
		t.Errorf("sources = %+v", got)
	}

	t.Run("revisions of the last sync when Argo CD compares none", func(t *testing.T) {
		app := multiApp([]any{chartSource, valuesRef}, nil, []any{"6.7.0", "cccc"}, nil)
		if got := proposalSources(app); got[0].Revision != "6.7.0" || got[1].Revision != "cccc" {
			t.Errorf("sources = %+v", got)
		}
	})
	t.Run("two ref sources that hold value files are both Values", func(t *testing.T) {
		c := map[string]any{"repoURL": "ghcr.io/x", "chart": "c", "helm": map[string]any{"valueFiles": []any{"$values/a.yaml", "$more/b.yaml"}}}
		got := proposalSources(multiApp([]any{c, valuesRef, other}, nil, nil, nil))
		if got[1].Role != backflowv1alpha1.RoleValues || got[2].Role != backflowv1alpha1.RoleValues {
			t.Errorf("sources = %+v", got)
		}
	})
	t.Run("outside the supported shape there are none", func(t *testing.T) {
		if got := proposalSources(multiApp([]any{chartSource}, nil, nil, nil)); got != nil {
			t.Errorf("sources = %+v", got)
		}
	})
}

func TestTranslateValueFiles(t *testing.T) {
	registryChart := backflowv1alpha1.ApplicationSource{Position: 1, Role: backflowv1alpha1.RoleRender, RepoURL: "ghcr.io/x/charts", Chart: "podinfo"}
	gitChart := backflowv1alpha1.ApplicationSource{Position: 1, Role: backflowv1alpha1.RoleRender, RepoURL: "https://gitlab.com/x/chart.git", Path: "charts/app"}
	helm := func(files ...any) map[string]any { return map[string]any{"valueFiles": files} }
	tests := []struct {
		name   string
		helm   map[string]any
		render backflowv1alpha1.ApplicationSource
		reason string
		msg    string
		want   []string
	}{
		{name: "all in the ref", helm: helm("$values/a.yaml", "$values/b/c.yaml"), render: registryChart, want: []string{"/a.yaml", "/b/c.yaml"}},
		{name: "a file inside a chart that is not in Git", helm: helm("values-prod.yaml", "$values/a.yaml"), render: registryChart,
			reason: reasonValueFileNotInGit, msg: "values-prod.yaml of source 1 is inside the chart podinfo from ghcr.io/x/charts"},
		{name: "a file inside a chart in Git is a second Git repository", helm: helm("values-prod.yaml", "$values/a.yaml"), render: gitChart,
			reason: reasonMultipleValueRepositories, msg: "inside the chart in https://gitlab.com/x/chart.git"},
		{name: "a file in another ref", helm: helm("$values/a.yaml", "$more/b.yaml"), render: registryChart,
			reason: reasonMultipleValueRepositories, msg: "in the repository of $more"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _, err := translateValueFiles(tt.helm, "values", tt.render)
			if tt.reason == "" {
				if err != nil || !reflect.DeepEqual(got, tt.want) {
					t.Errorf("files = %v, err = %v", got, err)
				}
				return
			}
			v, ok := err.(*verdictError)
			if !ok || v.reason != tt.reason || !strings.Contains(v.err.Error(), tt.msg) {
				t.Errorf("err = %v, want %s containing %q", err, tt.reason, tt.msg)
			}
		})
	}
}

// argoMultiApp is a multi-source Application the way Argo CD reports it after a
// sync: status.sync.revision is empty (the revisions are a list), and the result
// of the sync has both source (the first source) and sources (all of them).
func argoMultiApp(compared, synced []any) *unstructured.Unstructured {
	app := multiApp([]any{chartSource, valuesRef}, compared, synced, []any{"Helm", ""})
	status := app.Object["status"].(map[string]any)
	result := status["operationState"].(map[string]any)["syncResult"].(map[string]any)
	result["source"] = chartSource
	result["sources"] = []any{chartSource, valuesRef}
	return app
}

func TestSourcesOfPrefersTheListOfSources(t *testing.T) {
	app := argoMultiApp([]any{"6.7.1", "aaaa"}, []any{"6.7.1", "aaaa"})
	got, ok := sourcesOf(app, []string{"status", "operationState", "syncResult", "source"}, []string{"status", "operationState", "syncResult", "sources"})
	if !ok || len(got) != 2 {
		t.Fatalf("sources of the last sync = %d (ok %v), want both: the result of a sync has source, the first one, too", len(got), ok)
	}
	single := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"source": map[string]any{"repoURL": "x"}}}}
	if got, ok := sourcesOf(single, []string{"spec", "source"}, []string{"spec", "sources"}); !ok || len(got) != 1 {
		t.Errorf("single source: %d, %v", len(got), ok)
	}
}

func TestSourceChangeOfAMultiSourceApplication(t *testing.T) {
	t.Run("nothing changed since the last sync", func(t *testing.T) {
		if got := sourceChange(argoMultiApp([]any{"6.7.1", "bbbb"}, []any{"6.7.1", "aaaa"})); got != "" {
			t.Errorf("sourceChange = %q: the number of sources must not look changed (2 now, 1 then)", got)
		}
		if st := gitAhead(argoMultiApp([]any{"6.7.1", "bbbb"}, []any{"6.7.1", "aaaa"})); st.hold() != "" || !st.paused {
			t.Errorf("a ref source that moved is Git ahead, not a change of the Application: hold = %q, paused = %v", st.hold(), st.paused)
		}
	})
	t.Run("a source changed in the Application", func(t *testing.T) {
		app := argoMultiApp([]any{"6.7.1", "aaaa"}, []any{"6.7.1", "aaaa"})
		sources, _ := app.Object["spec"].(map[string]any)["sources"].([]any)
		other := map[string]any{"repoURL": valuesRepo, "targetRevision": "dev", "ref": "values"}
		app.Object["spec"].(map[string]any)["sources"] = []any{sources[0], other}
		if got := sourceChange(app); !strings.Contains(got, "targetRevision") {
			t.Errorf("sourceChange = %q", got)
		}
	})
}

func TestSyncedRevisionOfAMultiSourceApplication(t *testing.T) {
	if got := syncedRevision(argoMultiApp([]any{"6.7.1", "bbbb"}, []any{"6.7.1", "aaaa"})); got != "6.7.1,bbbb" {
		t.Errorf("syncedRevision = %q, want the revisions of all sources, the form a proposal records", got)
	}
	single := &unstructured.Unstructured{Object: map[string]any{"status": map[string]any{"sync": map[string]any{"revision": "abc"}}}}
	if got := syncedRevision(single); got != "abc" {
		t.Errorf("single source: %q", got)
	}
}

func TestApplicationChangedSeesTheRevisionsOfSeveralSources(t *testing.T) {
	update := applicationChanged().Update
	old := argoMultiApp([]any{"6.7.1", "aaaa"}, []any{"6.7.1", "aaaa"})
	if update(event.UpdateEvent{ObjectOld: old, ObjectNew: old.DeepCopy()}) {
		t.Error("an Application that did not change triggers a reconcile")
	}
	if !update(event.UpdateEvent{ObjectOld: old, ObjectNew: argoMultiApp([]any{"6.7.1", "bbbb"}, []any{"6.7.1", "aaaa"})}) {
		t.Error("a new commit of a ref source does not trigger a reconcile of the policy")
	}
	if !update(event.UpdateEvent{ObjectOld: old, ObjectNew: argoMultiApp([]any{"6.7.2", "aaaa"}, []any{"6.7.1", "aaaa"})}) {
		t.Error("a new chart version does not trigger a reconcile of the policy")
	}
}
