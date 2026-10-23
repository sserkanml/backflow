package controller

import (
	"context"
	"io/fs"
	"maps"
	"testing"
	"testing/fstest"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	backflowv1alpha1 "github.com/sserkanml/backflow/api/v1alpha1"
	"github.com/sserkanml/backflow/internal/gitrepo"
)

const (
	shaSynced   = "1111111111111111111111111111111111111111"
	shaCompared = "2222222222222222222222222222222222222222"
)

// treeDiffer is a GitDiffer with the files of each commit.
type treeDiffer struct {
	fakeDiffer
	trees    map[string]fstest.MapFS
	openErr  error
	opened   int
	released int
}

func (f *treeDiffer) OpenTree(_ context.Context, _, sha string, _ *gitrepo.Auth) (fs.FS, func(), error) {
	if f.openErr != nil {
		return nil, nil, f.openErr
	}
	f.opened++
	return f.trees[sha], func() { f.released++ }, nil
}

func files(m map[string]string) fstest.MapFS {
	out := fstest.MapFS{}
	for n, c := range m {
		out[n] = &fstest.MapFile{Data: []byte(c)}
	}
	return out
}

func with(base map[string]string, changes map[string]string) map[string]string {
	out := maps.Clone(base)
	for k, v := range changes {
		if v == "\x00" {
			delete(out, k)
			continue
		}
		out[k] = v
	}
	return out
}

const chartYAML = "apiVersion: v2\nname: x\nversion: 0.1.0\n"

func helmApp(path string, helm map[string]any) (*unstructured.Unstructured, backflowv1alpha1.ApplicationSummary) {
	app := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1", "kind": "Application",
		"metadata": map[string]any{"name": "alpha", "namespace": "argocd"},
		"spec": map[string]any{
			"project":     "default",
			"destination": map[string]any{"namespace": "alpha"},
			"source": map[string]any{
				"repoURL": "https://example.com/r.git", "path": path, "targetRevision": "main", "helm": helm,
			},
		},
	}}
	return app, backflowv1alpha1.ApplicationSummary{
		Name: "alpha", RepoURL: "https://example.com/r.git", Path: path, SourceType: string(backflowv1alpha1.SourceHelm),
	}
}

func TestChangedUnderPathHelm(t *testing.T) {
	sharedRoot := map[string]string{
		"Chart.yaml": chartYAML, "values.yaml": "a: 1\n", "templates/cm.yaml": "kind: ConfigMap\n",
		"services/alpha/dev.yaml": "a: 2\n", "services/beta/dev.yaml": "a: 3\n", "README.md": "docs\n",
	}
	perEnv := map[string]string{
		"helm/demo/Chart.yaml": chartYAML, "helm/demo/values.yaml": "a: 1\n", "helm/demo/templates/cm.yaml": "x\n",
		"helm/demo/values-dev.yaml": "a: 2\n", "helm/demo/values-prod.yaml": "a: 3\n", "helm/demo/README.md": "docs\n",
	}
	usesFiles := map[string]string{
		"c/Chart.yaml": chartYAML, "c/templates/cm.yaml": "{{ .Files.Get \"app.ini\" }}\n", "c/app.ini": "x\n", "c/README.md": "docs\n",
	}
	plain := map[string]string{
		"c/Chart.yaml": chartYAML, "c/templates/cm.yaml": "x\n", "c/app.ini": "x\n", "c/README.md": "docs\n",
	}
	withDep := map[string]string{
		"app/Chart.yaml":       chartYAML + "dependencies:\n  - name: lib\n    version: 1.0.0\n    repository: file://../lib\n",
		"app/templates/x.yaml": "x\n", "lib/Chart.yaml": chartYAML, "lib/templates/y.yaml": "y\n", "lib/README.md": "docs\n",
	}
	glob := map[string]string{"c/Chart.yaml": chartYAML, "c/env/a.yaml": "a: 1\n"}
	link := &fstest.MapFile{Data: []byte("x"), Mode: fs.ModeSymlink}

	tests := []struct {
		name string
		path string
		helm map[string]any
		// synced is the tree at the last sync, compared the tree at the
		// revision Argo CD compares against; changed are the paths of the diff.
		synced, compared map[string]string
		changed          []string
		want             int
		unchecked        bool
		mutate           func(m fstest.MapFS) // applied to the compared tree
		openErr          error
	}{
		{
			name: "shared chart: a commit to beta's values does not pause alpha",
			path: ".", helm: map[string]any{"valueFiles": []any{"services/alpha/dev.yaml"}},
			synced: sharedRoot, compared: with(sharedRoot, map[string]string{"services/beta/dev.yaml": "a: 9\n"}),
			changed: []string{"services/beta/dev.yaml"}, want: 0,
		},
		{
			name: "shared chart: a commit to alpha's values does",
			path: ".", helm: map[string]any{"valueFiles": []any{"services/alpha/dev.yaml"}},
			synced: sharedRoot, compared: with(sharedRoot, map[string]string{"services/alpha/dev.yaml": "a: 9\n"}),
			changed: []string{"services/alpha/dev.yaml"}, want: 1,
		},
		{
			name: "shared chart: a commit to templates does",
			path: ".", helm: map[string]any{"valueFiles": []any{"services/alpha/dev.yaml"}},
			synced: sharedRoot, compared: with(sharedRoot, map[string]string{"templates/cm.yaml": "changed\n"}),
			changed: []string{"templates/cm.yaml"}, want: 1,
		},
		{
			name: "shared chart: README and the other service together count one file",
			path: ".", helm: map[string]any{"valueFiles": []any{"services/alpha/dev.yaml"}},
			synced: sharedRoot, compared: with(sharedRoot, map[string]string{"README.md": "new\n", "services/beta/dev.yaml": "a: 9\n", "values.yaml": "a: 5\n"}),
			changed: []string{"README.md", "services/beta/dev.yaml", "values.yaml"}, want: 1,
		},
		{
			name: "per-environment values: values-prod.yaml does not pause the dev Application",
			path: "helm/demo", helm: map[string]any{"valueFiles": []any{"values-dev.yaml"}},
			synced: perEnv, compared: with(perEnv, map[string]string{"helm/demo/values-prod.yaml": "a: 9\n"}),
			changed: []string{"helm/demo/values-prod.yaml"}, want: 0,
		},
		{
			name: "per-environment values: a README next to the chart does not either",
			path: "helm/demo", helm: map[string]any{"valueFiles": []any{"values-dev.yaml"}},
			synced: perEnv, compared: with(perEnv, map[string]string{"helm/demo/README.md": "x\n"}),
			changed: []string{"helm/demo/README.md"}, want: 0,
		},
		{
			name: "per-environment values: values-dev.yaml does",
			path: "helm/demo", helm: map[string]any{"valueFiles": []any{"values-dev.yaml"}},
			synced: perEnv, compared: with(perEnv, map[string]string{"helm/demo/values-dev.yaml": "a: 9\n"}),
			changed: []string{"helm/demo/values-dev.yaml"}, want: 1,
		},
		{
			name: "a chart using .Files: a README change pauses",
			path: "c", helm: map[string]any{},
			synced: usesFiles, compared: with(usesFiles, map[string]string{"c/README.md": "x\n"}),
			changed: []string{"c/README.md"}, want: 1,
		},
		{
			name: "a chart that does not use .Files: a README change does not",
			path: "c", helm: map[string]any{},
			synced: plain, compared: with(plain, map[string]string{"c/README.md": "x\n"}),
			changed: []string{"c/README.md", "c/app.ini"}, want: 0,
		},
		{
			name: "a chart using .Files: a file its .helmignore excludes does not pause",
			path: "c", helm: map[string]any{},
			synced:   with(usesFiles, map[string]string{"c/.helmignore": "README.md\n"}),
			compared: with(usesFiles, map[string]string{"c/.helmignore": "README.md\n", "c/README.md": "x\n"}),
			changed:  []string{"c/README.md"}, want: 0,
		},
		{
			name: "a file:// dependency change pauses",
			path: "app", helm: map[string]any{},
			synced: withDep, compared: with(withDep, map[string]string{"lib/templates/y.yaml": "z\n"}),
			changed: []string{"lib/templates/y.yaml"}, want: 1,
		},
		{
			name: "a file:// dependency's README does not",
			path: "app", helm: map[string]any{},
			synced: withDep, compared: with(withDep, map[string]string{"lib/README.md": "z\n"}),
			changed: []string{"lib/README.md"}, want: 0,
		},
		{
			name: "a value files glob that starts matching a new file pauses",
			path: "c", helm: map[string]any{"valueFiles": []any{"env/*.yaml"}},
			synced: glob, compared: with(glob, map[string]string{"c/env/b.yaml": "a: 2\n"}),
			changed: []string{"c/env/b.yaml"}, want: 1,
		},
		{
			name: "a glob: a file it matched at the old revision and no longer matches still pauses",
			path: "c", helm: map[string]any{"valueFiles": []any{"env/*.yaml"}},
			synced: with(glob, map[string]string{"c/env/b.yaml": "a: 2\n"}), compared: glob,
			changed: []string{"c/env/b.yaml"}, want: 1,
		},
		{
			name: "a value file outside the chart directory",
			path: "c", helm: map[string]any{"valueFiles": []any{"/shared/common.yaml"}},
			synced:   with(plain, map[string]string{"shared/common.yaml": "a: 1\n"}),
			compared: with(plain, map[string]string{"shared/common.yaml": "a: 2\n", "shared/other.yaml": "a: 2\n"}),
			changed:  []string{"shared/common.yaml", "shared/other.yaml"}, want: 1,
		},
		{
			name: "a file parameter's file",
			path: "c", helm: map[string]any{"fileParameters": []any{map[string]any{"name": "tls", "path": "/secrets/tls.pem"}}},
			synced:   with(plain, map[string]string{"secrets/tls.pem": "a"}),
			compared: with(plain, map[string]string{"secrets/tls.pem": "b"}),
			changed:  []string{"secrets/tls.pem"}, want: 1,
		},
		{
			name: "value file paths with the revision variable follow each revision",
			path: "c", helm: map[string]any{"valueFiles": []any{"v-$ARGOCD_APP_REVISION.yaml"}},
			synced:   with(plain, map[string]string{"c/v-" + shaSynced + ".yaml": "a: 1\n"}),
			compared: with(plain, map[string]string{"c/v-" + shaCompared + ".yaml": "a: 1\n"}),
			changed:  []string{"c/v-" + shaCompared + ".yaml"}, want: 1,
		},
		{
			name: "a symlink among the templates pauses",
			path: "c", helm: map[string]any{},
			synced: plain, compared: plain, changed: []string{"c/README.md"}, unchecked: true,
			mutate: func(m fstest.MapFS) { m["c/templates/link.yaml"] = link },
		},
		{
			name: "a symlinked value file pauses",
			path: "c", helm: map[string]any{"valueFiles": []any{"values-dev.yaml"}},
			synced: with(plain, map[string]string{"c/values-dev.yaml": "a: 1\n"}), compared: with(plain, map[string]string{"c/values-dev.yaml": "a: 1\n"}),
			changed: []string{"c/README.md"}, unchecked: true,
			mutate: func(m fstest.MapFS) { m["c/values-dev.yaml"] = link },
		},
		{
			name: "a missing value file pauses",
			path: "c", helm: map[string]any{"valueFiles": []any{"nope.yaml"}},
			synced: plain, compared: plain, changed: []string{"c/README.md"}, unchecked: true,
		},
		{
			name: "a tree that cannot be read pauses",
			path: "c", helm: map[string]any{},
			synced: plain, compared: plain, changed: []string{"c/README.md"}, unchecked: true, openErr: fs.ErrPermission,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			compared := files(tt.compared)
			if tt.mutate != nil {
				tt.mutate(compared)
			}
			git := &treeDiffer{
				fakeDiffer: fakeDiffer{paths: tt.changed},
				trees:      map[string]fstest.MapFS{shaSynced: files(tt.synced), shaCompared: compared},
				openErr:    tt.openErr,
			}
			r := &DriftReconciler{Git: git}
			app, summary := helmApp(tt.path, tt.helm)
			st := aheadState{synced: shaSynced, compared: shaCompared, paused: true}
			policy := &backflowv1alpha1.BackflowPolicy{}

			changed, checked := r.changedUnderPath(context.Background(), policy, summary, app, st)
			if checked == tt.unchecked {
				t.Fatalf("checked = %v, want %v", checked, !tt.unchecked)
			}
			if checked && changed != tt.want {
				t.Errorf("changed = %d, want %d", changed, tt.want)
			}
			if git.opened != git.released {
				t.Errorf("%d trees opened, %d released", git.opened, git.released)
			}
		})
	}
}

func TestChangedUnderPathHelmWithoutTreeAccess(t *testing.T) {
	r := &DriftReconciler{Git: &fakeDiffer{paths: []string{"README.md"}}} // cannot open trees
	app, summary := helmApp("c", map[string]any{})
	st := aheadState{synced: shaSynced, compared: shaCompared, paused: true}
	if _, checked := r.changedUnderPath(context.Background(), &backflowv1alpha1.BackflowPolicy{}, summary, app, st); checked {
		t.Error("a GitDiffer that cannot read files answered for a Helm source")
	}
}

func TestChangedUnderPathHelmIsRemembered(t *testing.T) {
	plain := map[string]string{"c/Chart.yaml": chartYAML, "c/templates/x.yaml": "x\n"}
	git := &treeDiffer{
		fakeDiffer: fakeDiffer{paths: []string{"c/templates/x.yaml"}},
		trees:      map[string]fstest.MapFS{shaSynced: files(plain), shaCompared: files(plain)},
	}
	r := &DriftReconciler{Git: git}
	app, summary := helmApp("c", map[string]any{})
	st := aheadState{synced: shaSynced, compared: shaCompared, paused: true}
	for range 3 {
		if n, checked := r.changedUnderPath(context.Background(), &backflowv1alpha1.BackflowPolicy{}, summary, app, st); !checked || n != 1 {
			t.Fatalf("n = %d, checked = %v", n, checked)
		}
	}
	if git.calls != 1 {
		t.Errorf("the repository was looked at %d times", git.calls)
	}
	// A changed Application is looked at again.
	app2, summary2 := helmApp("c", map[string]any{"valueFiles": []any{"values.yaml"}})
	git.trees[shaSynced]["c/values.yaml"] = &fstest.MapFile{Data: []byte("a: 1\n")}
	git.trees[shaCompared]["c/values.yaml"] = &fstest.MapFile{Data: []byte("a: 1\n")}
	r.changedUnderPath(context.Background(), &backflowv1alpha1.BackflowPolicy{}, summary2, app2, st)
	if git.calls != 2 {
		t.Errorf("a new value file list did not cause a new look (%d)", git.calls)
	}
}

// shared-beta of the demo repository: one chart in helm/shared, and a values
// file per service under it, named by the Application with a path relative to
// the chart (services/beta/dev.yaml).
func TestChangedUnderPathHelmSharedChartPerServiceValues(t *testing.T) {
	tree := map[string]string{
		"helm/shared/Chart.yaml": chartYAML, "helm/shared/values.yaml": "logLevel: info\n",
		"helm/shared/templates/cm.yaml":       "kind: ConfigMap\n",
		"helm/shared/services/alpha/dev.yaml": "logLevel: debug\n", "helm/shared/services/beta/dev.yaml": "logLevel: warn\n",
		"helm/demo/values-dev.yaml": "a: 1\n", "README.md": "docs\n",
	}
	tests := []struct {
		name    string
		changed []string
		want    int
	}{
		{"a commit to the Application's own values file", []string{"helm/shared/services/beta/dev.yaml"}, 1},
		{"a commit to another service's values file", []string{"helm/shared/services/alpha/dev.yaml"}, 0},
		{"a template of the shared chart", []string{"helm/shared/templates/cm.yaml"}, 1},
		{"a file elsewhere in the repository", []string{"helm/demo/values-dev.yaml", "README.md"}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synced, compared := files(tree), files(with(tree, map[string]string{"helm/shared/services/beta/dev.yaml": "logLevel: error\n"}))
			git := &treeDiffer{
				fakeDiffer: fakeDiffer{paths: tt.changed},
				trees:      map[string]fstest.MapFS{shaSynced: synced, shaCompared: compared},
			}
			r := &DriftReconciler{Git: git}
			app, summary := helmApp("helm/shared", map[string]any{"valueFiles": []any{"services/beta/dev.yaml"}})
			st := aheadState{synced: shaSynced, compared: shaCompared, paused: true}
			changed, checked := r.changedUnderPath(context.Background(), &backflowv1alpha1.BackflowPolicy{}, summary, app, st)
			if !checked || changed != tt.want {
				t.Errorf("changed = %d, checked = %v, want %d, true", changed, checked, tt.want)
			}
		})
	}
}
