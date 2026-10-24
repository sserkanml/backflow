package kustomize_test

import (
	"context"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/sserkanml/backflow/internal/kustomize"
	"github.com/sserkanml/backflow/internal/kustomize/kbuild"
	"github.com/sserkanml/backflow/internal/rendermap"
)

// fixture2 has a component with a patch, a label, a name prefix, a patch given
// as a JSON string, and two patches that set the same value.
func fixture2() fstest.MapFS {
	f := func(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }
	return fstest.MapFS{
		"k/base/kustomization.yaml": f("resources:\n- deployment.yaml\n"),
		"k/base/deployment.yaml":    f(deployment),
		"k/comp/kustomization.yaml": f("apiVersion: kustomize.config.k8s.io/v1alpha1\nkind: Component\npatches:\n- path: tolerate.yaml\n"),
		"k/comp/tolerate.yaml": f(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
spec:
  template:
    spec:
      containers:
      - name: web
        imagePullPolicy: Always
`),
		"k/overlays/prod/kustomization.yaml": f(`namePrefix: prod-
resources:
- ../../base
components:
- ../../comp
labels:
- pairs:
    tier: web
patches:
- target:
    kind: Deployment
  patch: '[{"op": "replace", "path": "/spec/replicas", "value": 3}]'
- path: one.yaml
- path: two.yaml
- path: owner.json
  target:
    kind: Deployment
`),
		// Replaces the annotations of the Deployment, and with them the build
		// metadata that says where it comes from.
		"k/overlays/prod/owner.json": f(`[{"op": "add", "path": "/metadata/annotations", "value": {"owner": "sre"}}]`),
		"k/overlays/prod/one.yaml":   f("apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\nspec:\n  template:\n    spec:\n      containers:\n      - name: web\n        env:\n        - name: A\n          value: base-a\n"),
		"k/overlays/prod/two.yaml":   f("apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\nspec:\n  template:\n    spec:\n      containers:\n      - name: web\n        env:\n        - name: A\n          value: base-a\n"),
	}
}

func setup2(t *testing.T) *env {
	t.Helper()
	bin := kustomizeBin(t)
	fsys := fixture2()
	opts := kustomize.Options{}
	src, err := kustomize.NewSource(fsys, "k/overlays/prod", opts)
	if err != nil {
		t.Fatal(err)
	}
	files, err := kustomize.CollectFiles(fsys, src)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, fsys: fsys, src: src, files: files, app: rendermap.App{Name: "prod", DestNamespace: "demo"},
		rend: &kbuild.Renderer{Bin: bin, Dir: "k/overlays/prod", Options: opts}}
	if e.loc, err = kustomize.Locate(context.Background(), e.rend, src, files, e.app); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestComponentsLabelsAndJSONStrings(t *testing.T) {
	e := setup2(t)
	dep := e.obj("Deployment", "prod-web")
	t.Run("a JSON 6902 patch that is a JSON string stays compact JSON", func(t *testing.T) {
		out := e.verify(dep, func(o map[string]any) { o["spec"].(map[string]any)["replicas"] = int64(4) }, defaults)
		wantMapped(t, out, "k/overlays/prod/kustomization.yaml")
		if !strings.Contains(diffs(out), `+  patch: '[{"op":"replace","path":"/spec/replicas","value":4}]'`) {
			t.Errorf("diff:\n%s", diffs(out))
		}
	})
	t.Run("a patch of a component outside the path needs editBases", func(t *testing.T) {
		change := func(o map[string]any) { container(o)["imagePullPolicy"] = "Never" }
		out := e.verify(dep, change, kustomize.Policy{})
		wantUnmapped(t, out, rendermap.ReasonNoVerifiedCandidate)
		if !strings.Contains(out.Unmapped.Message, "k/comp/tolerate.yaml holds the value") {
			t.Errorf("message = %s", out.Unmapped.Message)
		}
		out = e.verify(dep, change, kustomize.Policy{EditBases: true})
		wantMapped(t, out, "k/comp/tolerate.yaml")
		if d := kustomize.Describe(out); len(d.SharedFiles) != 1 {
			t.Errorf("%+v", d)
		}
	})
	t.Run("a label pair of the kustomization", func(t *testing.T) {
		out := e.verify(dep, func(o map[string]any) {
			o["metadata"].(map[string]any)["labels"].(map[string]any)["tier"] = "api"
		}, defaults)
		wantMapped(t, out, "k/overlays/prod/kustomization.yaml")
		if !strings.Contains(diffs(out), "+    tier: api") {
			t.Errorf("diff:\n%s", diffs(out))
		}
	})
}

func TestSameValueInBaseAndPatches(t *testing.T) {
	e := setup2(t)
	dep := e.obj("Deployment", "prod-web")
	change := func(o map[string]any) { container(o)["env"].([]any)[0].(map[string]any)["value"] = "changed" }
	// base/deployment.yaml, one.yaml and two.yaml all say base-a. The patches
	// come before the base, and the later patch wins: only its edit changes
	// the render, even when the base may be edited.
	for name, pol := range map[string]kustomize.Policy{"default": defaults, "editBases": {EditBases: true, AddPatches: true}} {
		t.Run(name, func(t *testing.T) {
			out := e.verify(dep, change, pol)
			wantMapped(t, out, "k/overlays/prod/two.yaml")
		})
	}
}

func TestNewPatchWithoutOrigin(t *testing.T) {
	e := setup2(t)
	dep := e.obj("Deployment", "prod-web")
	if o := e.loc.Origins[rendermap.ResourceKey{Group: "apps", Kind: "Deployment", Namespace: "demo", Name: "prod-web"}]; o == nil || o.File != "" {
		t.Fatalf("the fixture is meant to lose the origin of the Deployment: %+v", o)
	}
	out := e.verify(dep, func(o map[string]any) { container(o)["imagePullPolicy"] = "Never" }, kustomize.Policy{AddPatches: true})
	// The patch targets the name before the prefix, which the render confirms.
	wantMapped(t, out, "k/overlays/prod/backflow-patches/deployment-web.yaml", "k/overlays/prod/kustomization.yaml")
}
