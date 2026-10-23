package controller

import (
	"context"
	"io/fs"
	"testing"
	"testing/fstest"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	backflowv1alpha1 "github.com/sserkanml/backflow/api/v1alpha1"
)

func kustomizeApp(path string, kustomize map[string]any) (*unstructured.Unstructured, backflowv1alpha1.ApplicationSummary) {
	src := map[string]any{"repoURL": "https://example.com/r.git", "path": path, "targetRevision": "main"}
	if kustomize != nil {
		src["kustomize"] = kustomize
	}
	app := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1", "kind": "Application",
		"metadata": map[string]any{"name": "web", "namespace": "argocd"},
		"spec":     map[string]any{"project": "default", "destination": map[string]any{"namespace": "web"}, "source": src},
	}}
	return app, backflowv1alpha1.ApplicationSummary{
		Name: "web", RepoURL: "https://example.com/r.git", Path: path, SourceType: string(backflowv1alpha1.SourceKustomize),
	}
}

func TestChangedUnderPathKustomize(t *testing.T) {
	overlay := map[string]string{
		"apps/web/base/kustomization.yaml":          "resources: [deploy.yaml]\n",
		"apps/web/base/deploy.yaml":                 "kind: Deployment\n",
		"apps/web/overlays/dev/kustomization.yaml":  "resources: [../../base]\npatches:\n  - path: replicas.yaml\n",
		"apps/web/overlays/dev/replicas.yaml":       "kind: Deployment\n",
		"apps/web/overlays/dev/notes.txt":           "notes\n",
		"apps/web/overlays/prod/kustomization.yaml": "resources: [../../base]\n",
		"apps/other/kustomization.yaml":             "resources: []\n",
		"README.md":                                 "docs\n",
	}
	remote := map[string]string{
		"apps/web/overlays/dev/kustomization.yaml": "resources:\n  - github.com/org/repo//base?ref=v1\n",
		"README.md": "docs\n",
	}
	const dev = "apps/web/overlays/dev"
	tests := []struct {
		name             string
		kustomize        map[string]any
		synced, compared map[string]string
		changed          []string
		want             int
		unchecked        bool
	}{
		{
			name:   "a commit elsewhere in the repository does not pause",
			synced: overlay, compared: overlay,
			changed: []string{"README.md", "apps/other/kustomization.yaml", "apps/web/overlays/prod/kustomization.yaml", dev + "/notes.txt"}, want: 0,
		},
		{
			name:   "a change in the base outside the Application's directory counts",
			synced: overlay, compared: overlay,
			changed: []string{"apps/web/base/deploy.yaml", "README.md"}, want: 1,
		},
		{
			name:   "a patch file and the kustomization of the overlay count",
			synced: overlay, compared: overlay,
			changed: []string{dev + "/replicas.yaml", dev + "/kustomization.yaml", dev + "/notes.txt"}, want: 2,
		},
		{
			name:      "a component of the Application's options counts",
			kustomize: map[string]any{"components": []any{"../../components/tls"}},
			synced:    with(overlay, map[string]string{"apps/web/components/tls/kustomization.yaml": "kind: Component\n"}),
			compared:  with(overlay, map[string]string{"apps/web/components/tls/kustomization.yaml": "kind: Component\n"}),
			changed:   []string{"apps/web/components/tls/kustomization.yaml"}, want: 1,
		},
		{
			name:   "a file the overlay stopped referencing still counts",
			synced: overlay, compared: with(overlay, map[string]string{dev + "/kustomization.yaml": "resources: [../../base]\n"}),
			changed: []string{dev + "/replicas.yaml", dev + "/kustomization.yaml"}, want: 2,
		},
		{
			name:   "a remote base is not closed: the pause applies",
			synced: remote, compared: remote, changed: []string{"README.md"}, unchecked: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			git := &treeDiffer{
				fakeDiffer: fakeDiffer{paths: tt.changed},
				trees:      map[string]fstest.MapFS{shaSynced: files(tt.synced), shaCompared: files(tt.compared)},
			}
			r := &DriftReconciler{Git: git}
			app, summary := kustomizeApp(dev, tt.kustomize)
			st := aheadState{synced: shaSynced, compared: shaCompared, paused: true}

			changed, checked := r.changedUnderPath(context.Background(), &backflowv1alpha1.BackflowPolicy{}, summary, app, st)
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

func TestChangedUnderPathKustomizeCannotReadTrees(t *testing.T) {
	app, summary := kustomizeApp("apps/web", nil)
	st := aheadState{synced: shaSynced, compared: shaCompared, paused: true}
	policy := &backflowv1alpha1.BackflowPolicy{}

	r := &DriftReconciler{Git: &fakeDiffer{paths: []string{"README.md"}}} // cannot open trees
	if _, checked := r.changedUnderPath(context.Background(), policy, summary, app, st); checked {
		t.Error("a GitDiffer that cannot read files answered for a Kustomize source")
	}

	tree := map[string]string{"apps/web/kustomization.yaml": "resources: []\n"}
	r = &DriftReconciler{Git: &treeDiffer{
		fakeDiffer: fakeDiffer{paths: []string{"README.md"}},
		trees:      map[string]fstest.MapFS{shaSynced: files(tree), shaCompared: files(tree)},
		openErr:    fs.ErrPermission,
	}}
	if _, checked := r.changedUnderPath(context.Background(), policy, summary, app, st); checked {
		t.Error("a tree that cannot be read gave an answer")
	}
}

func TestChangedUnderPathKustomizeIsRemembered(t *testing.T) {
	tree := map[string]string{"apps/web/kustomization.yaml": "resources: [deploy.yaml]\n", "apps/web/deploy.yaml": "x\n"}
	git := &treeDiffer{
		fakeDiffer: fakeDiffer{paths: []string{"apps/web/deploy.yaml"}},
		trees:      map[string]fstest.MapFS{shaSynced: files(tree), shaCompared: files(tree)},
	}
	r := &DriftReconciler{Git: git}
	app, summary := kustomizeApp("apps/web", nil)
	st := aheadState{synced: shaSynced, compared: shaCompared, paused: true}
	for range 3 {
		if n, checked := r.changedUnderPath(context.Background(), &backflowv1alpha1.BackflowPolicy{}, summary, app, st); !checked || n != 1 {
			t.Fatalf("n = %d, checked = %v", n, checked)
		}
	}
	if git.calls != 1 {
		t.Errorf("the repository was looked at %d times", git.calls)
	}
	// A changed Application (a component) is looked at again.
	app2, summary2 := kustomizeApp("apps/web", map[string]any{"components": []any{"c"}})
	git.trees[shaSynced]["apps/web/c/kustomization.yaml"] = &fstest.MapFile{Data: []byte("kind: Component\n")}
	git.trees[shaCompared]["apps/web/c/kustomization.yaml"] = &fstest.MapFile{Data: []byte("kind: Component\n")}
	r.changedUnderPath(context.Background(), &backflowv1alpha1.BackflowPolicy{}, summary2, app2, st)
	if git.calls != 2 {
		t.Errorf("a new component list did not cause a new look (%d)", git.calls)
	}
}
