package controller

import (
	"context"
	"io/fs"
	"maps"
	"strings"
	"testing"
	"testing/fstest"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	backflowv1alpha1 "github.com/sserkanml/backflow/api/v1alpha1"
	"github.com/sserkanml/backflow/internal/gitrepo"
)

// repoDiffer is a GitDiffer with a diff per repository and the files of each
// commit (every commit of the tests has its own SHA).
type repoDiffer struct {
	paths map[string][]string
	trees map[string]fstest.MapFS
	calls map[string]int
}

func (d *repoDiffer) ChangedPaths(_ context.Context, repoURL, _, _ string, _ *gitrepo.Auth) ([]string, error) {
	if d.calls == nil {
		d.calls = map[string]int{}
	}
	d.calls[repoURL]++
	return d.paths[repoURL], nil
}

func (d *repoDiffer) HasSymlink(context.Context, string, string, string, *gitrepo.Auth) (bool, error) {
	return false, nil
}

func (d *repoDiffer) HasFileWithSuffix(context.Context, string, string, string, *gitrepo.Auth, ...string) (bool, error) {
	return false, nil
}

func (d *repoDiffer) OpenTree(_ context.Context, _, sha string, _ *gitrepo.Auth) (fs.FS, func(), error) {
	return d.trees[sha], func() {}, nil
}

const (
	valuesRepo = "https://gitlab.com/x/values.git"
	chartRepo  = "https://gitlab.com/x/chart.git"
	vOld       = "1111111111111111111111111111111111111111"
	vNew       = "2222222222222222222222222222222222222222"
	cOld       = "3333333333333333333333333333333333333333"
	cNew       = "4444444444444444444444444444444444444444"
)

func valuesTree(extra ...string) fstest.MapFS {
	m := map[string]string{"env/values.yaml": "a: 1\n", "env/other.yaml": "b: 2\n", "README.md": "docs\n"}
	for _, e := range extra {
		m[e] = "x: 1\n"
	}
	return files(m)
}

func chartTree() fstest.MapFS {
	return files(map[string]string{
		"charts/app/Chart.yaml": chartYAML, "charts/app/values.yaml": "a: 1\n", "charts/app/templates/cm.yaml": "kind: ConfigMap\n",
		"charts/app/README.md": "docs\n", "docs/x.md": "docs\n",
	})
}

func msApp(chart map[string]any, valueFile string, compared, synced []any) *unstructured.Unstructured {
	c := maps.Clone(chart)
	c["helm"] = map[string]any{"valueFiles": []any{valueFile}}
	ref := map[string]any{"repoURL": valuesRepo, "targetRevision": "main", "ref": "values"}
	return multiApp([]any{c, ref}, compared, synced, []any{"Helm", ""})
}

func TestChangedUnderPathMultiSource(t *testing.T) {
	ociChart := map[string]any{"repoURL": "ghcr.io/stefanprodan/charts", "chart": "podinfo", "targetRevision": "6.7.1"}
	gitChart := map[string]any{"repoURL": chartRepo, "path": "charts/app", "targetRevision": "main"}
	summary := backflowv1alpha1.ApplicationSummary{Name: "multi", RepoURL: "x", SourceType: "Helm"}

	tests := []struct {
		name      string
		chart     map[string]any
		valueFile string
		compared  []any
		synced    []any
		paths     map[string][]string
		trees     map[string]fstest.MapFS
		want      int
		unchecked bool
	}{
		{
			name:  "a chart from a registry: a commit to another file of the values repository does not pause",
			chart: ociChart, valueFile: "$values/env/values.yaml",
			compared: []any{"6.7.1", vNew}, synced: []any{"6.7.1", vOld},
			paths: map[string][]string{valuesRepo: {"README.md", "env/other.yaml"}},
			trees: map[string]fstest.MapFS{vOld: valuesTree(), vNew: valuesTree()}, want: 0,
		},
		{
			name:  "a commit to the value file does",
			chart: ociChart, valueFile: "$values/env/values.yaml",
			compared: []any{"6.7.1", vNew}, synced: []any{"6.7.1", vOld},
			paths: map[string][]string{valuesRepo: {"env/values.yaml", "README.md"}},
			trees: map[string]fstest.MapFS{vOld: valuesTree(), vNew: valuesTree()}, want: 1,
		},
		{
			name:  "a new file that a glob of the value files matches counts",
			chart: ociChart, valueFile: "$values/env/*.yaml",
			compared: []any{"6.7.1", vNew}, synced: []any{"6.7.1", vOld},
			paths: map[string][]string{valuesRepo: {"env/extra.yaml", "README.md"}},
			trees: map[string]fstest.MapFS{vOld: valuesTree(), vNew: valuesTree("env/extra.yaml")}, want: 1,
		},
		{
			name:  "a value file that is not there yet counts when it appears",
			chart: ociChart, valueFile: "$values/env/later.yaml",
			compared: []any{"6.7.1", vNew}, synced: []any{"6.7.1", vOld},
			paths: map[string][]string{valuesRepo: {"env/later.yaml"}},
			trees: map[string]fstest.MapFS{vOld: valuesTree(), vNew: valuesTree("env/later.yaml")}, want: 1,
		},
		{
			name:  "a chart in Git at the same revision is not looked at",
			chart: gitChart, valueFile: "$values/env/values.yaml",
			compared: []any{cOld, vNew}, synced: []any{cOld, vOld},
			paths: map[string][]string{valuesRepo: {"env/values.yaml"}, chartRepo: {"charts/app/templates/cm.yaml"}},
			trees: map[string]fstest.MapFS{vOld: valuesTree(), vNew: valuesTree(), cOld: chartTree()}, want: 1,
		},
		{
			name:  "a chart in Git: its templates and the value file both count, in their own repositories",
			chart: gitChart, valueFile: "$values/env/values.yaml",
			compared: []any{cNew, vNew}, synced: []any{cOld, vOld},
			paths: map[string][]string{
				valuesRepo: {"env/values.yaml", "README.md"},
				chartRepo:  {"charts/app/templates/cm.yaml", "charts/app/README.md", "docs/x.md"},
			},
			trees: map[string]fstest.MapFS{vOld: valuesTree(), vNew: valuesTree(), cOld: chartTree(), cNew: chartTree()}, want: 2,
		},
		{
			name:  "a chart in Git: a file the chart does not read does not count",
			chart: gitChart, valueFile: "$values/env/values.yaml",
			compared: []any{cNew, vNew}, synced: []any{cOld, vOld},
			paths: map[string][]string{valuesRepo: {"README.md"}, chartRepo: {"charts/app/README.md", "docs/x.md"}},
			trees: map[string]fstest.MapFS{vOld: valuesTree(), vNew: valuesTree(), cOld: chartTree(), cNew: chartTree()}, want: 0,
		},
		{
			name:  "a source that is not at a commit gives no answer",
			chart: ociChart, valueFile: "$values/env/values.yaml",
			compared: []any{"6.7.1", "main"}, synced: []any{"6.7.1", vOld},
			paths: map[string][]string{valuesRepo: {"README.md"}}, unchecked: true,
		},
		{
			name:  "no revision for every source gives no answer",
			chart: ociChart, valueFile: "$values/env/values.yaml",
			compared: []any{"6.7.1"}, synced: []any{"6.7.1", vOld}, unchecked: true,
		},
		{
			name:  "a symbolic link as the value file gives no answer",
			chart: ociChart, valueFile: "$values/env/values.yaml",
			compared: []any{"6.7.1", vNew}, synced: []any{"6.7.1", vOld},
			paths: map[string][]string{valuesRepo: {"README.md"}},
			trees: map[string]fstest.MapFS{vOld: valuesTree(), vNew: {
				"env/values.yaml": &fstest.MapFile{Data: []byte("../x"), Mode: fs.ModeSymlink},
			}}, unchecked: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			git := &repoDiffer{paths: tt.paths, trees: tt.trees}
			r := &DriftReconciler{Git: git}
			app := msApp(tt.chart, tt.valueFile, tt.compared, tt.synced)
			st := gitAhead(app)
			if !st.paused {
				t.Fatalf("the Application is not paused: %+v", st)
			}
			changed, checked := r.changedUnderPath(context.Background(), &backflowv1alpha1.BackflowPolicy{}, summary, app, st)
			if checked == tt.unchecked {
				t.Fatalf("checked = %v, want %v", checked, !tt.unchecked)
			}
			if checked && changed != tt.want {
				t.Errorf("changed = %d, want %d", changed, tt.want)
			}
		})
	}
}

func TestChangedUnderPathMultiSourceOutsideTheShape(t *testing.T) {
	app := multiApp([]any{
		map[string]any{"repoURL": chartRepo, "path": "a", "kustomize": map[string]any{}},
		map[string]any{"repoURL": valuesRepo, "ref": "values"},
	}, []any{cNew, vNew}, []any{cOld, vOld}, []any{"Kustomize", ""})
	r := &DriftReconciler{Git: &repoDiffer{}}
	st := gitAhead(app)
	if _, checked := r.changedUnderPath(context.Background(), &backflowv1alpha1.BackflowPolicy{}, backflowv1alpha1.ApplicationSummary{Name: "multi"}, app, st); checked {
		t.Error("a Kustomize source with a ref was looked at")
	}
}

func TestChangedUnderPathMultiSourceIsRemembered(t *testing.T) {
	app := msApp(map[string]any{"repoURL": "ghcr.io/stefanprodan/charts", "chart": "podinfo", "targetRevision": "6.7.1"},
		"$values/env/values.yaml", []any{"6.7.1", vNew}, []any{"6.7.1", vOld})
	git := &repoDiffer{paths: map[string][]string{valuesRepo: {"env/values.yaml"}},
		trees: map[string]fstest.MapFS{vOld: valuesTree(), vNew: valuesTree()}}
	r := &DriftReconciler{Git: git}
	st := gitAhead(app)
	for range 3 {
		if n, checked := r.changedUnderPath(context.Background(), &backflowv1alpha1.BackflowPolicy{}, backflowv1alpha1.ApplicationSummary{Name: "multi"}, app, st); !checked || n != 1 {
			t.Fatalf("n = %d, checked = %v", n, checked)
		}
	}
	if git.calls[valuesRepo] != 1 {
		t.Errorf("the repository was looked at %d times", git.calls[valuesRepo])
	}
}

func TestChartChange(t *testing.T) {
	chart := func(rev string) map[string]any {
		return map[string]any{"repoURL": "ghcr.io/stefanprodan/charts", "chart": "podinfo", "targetRevision": rev}
	}
	ref := map[string]any{"repoURL": valuesRepo, "ref": "values"}
	app := func(spec string, compared, synced []any, last string) *unstructured.Unstructured {
		a := multiApp([]any{chart(spec), ref}, compared, synced, []any{"Helm", ""})
		_ = unstructured.SetNestedSlice(a.Object, []any{chart(last), ref}, "status", "operationState", "syncResult", "sources")
		return a
	}
	tests := []struct {
		name string
		app  *unstructured.Unstructured
		want string
	}{
		{"nothing changed", app("6.7.*", []any{"6.7.1", vNew}, []any{"6.7.1", vOld}, "6.7.*"), ""},
		{"a range resolved to a new version", app("6.7.*", []any{"6.7.2", vOld}, []any{"6.7.1", vOld}, "6.7.*"), "moved from 6.7.1 to 6.7.2"},
		{"the version in the Application changed", app("6.8.0", []any{"6.8.0", vOld}, []any{"6.7.1", vOld}, "6.7.1"), "moved from 6.7.1 to 6.8.0"},
		{"changed in the spec before Argo CD compared", app("6.8.0", []any{"6.7.1", vOld}, []any{"6.7.1", vOld}, "6.7.1"), "changed in the Application since the last sync (6.8.0, was 6.7.1)"},
		{"never synced", app("6.7.1", []any{"6.7.1", vOld}, nil, "6.7.1"), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := chartChange(tt.app)
			if (tt.want == "") != (got == "") || tt.want != "" && !strings.Contains(got, tt.want) {
				t.Errorf("chartChange = %q, want %q", got, tt.want)
			}
		})
	}

	t.Run("it is an Application change, not Git ahead", func(t *testing.T) {
		st := gitAhead(app("6.7.*", []any{"6.7.2", vOld}, []any{"6.7.1", vOld}, "6.7.*"))
		if st.hold() == "" || st.holdKind != holdApplicationChanged {
			t.Errorf("hold = %q (%s)", st.hold(), st.holdKind)
		}
		st = gitAhead(app("6.7.*", []any{"6.7.1", vNew}, []any{"6.7.1", vOld}, "6.7.*"))
		if st.hold() != "" || !st.paused {
			t.Errorf("a ref source that moved is Git ahead: hold = %q, paused = %v", st.hold(), st.paused)
		}
	})
}
