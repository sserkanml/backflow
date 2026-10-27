package kustomize

import (
	"reflect"
	"testing"
	"testing/fstest"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/sserkanml/backflow/api/v1alpha1"
	"github.com/sserkanml/backflow/internal/appvalues"
)

func TestImageEntryWithTag(t *testing.T) {
	for name, tc := range map[string]struct {
		entry, desired, live, want string
		ok                         bool
	}{
		"name=newName:tag":            {"nginx=registry/nginx:1.99", "registry/nginx:1.99", "registry/nginx:2.0", "nginx=registry/nginx:2.0", true},
		"name:tag":                    {"redis:7", "redis:7", "redis:8", "redis:8", true},
		"the tag is not the entry's":  {"redis:7", "redis:6", "redis:8", "", false},
		"another image":               {"redis:7", "redis:7", "valkey:8", "", false},
		"the live image has a digest": {"redis:7", "redis:7", "redis@sha256:abc", "", false},
		"the entry has a digest":      {"redis@sha256:abc", "redis@sha256:abc", "redis:8", "", false},
		"no tag change":               {"redis:7", "redis:7", "redis:7", "", false},
	} {
		got, ok := imageEntryWithTag(tc.entry, tc.desired, tc.live)
		if ok != tc.ok || got != tc.want {
			t.Errorf("%s: got %q, %v; want %q, %v", name, got, ok, tc.want, tc.ok)
		}
	}
}

func TestApplicationOptionsOfTheKustomizeOptions(t *testing.T) {
	fsys := fstest.MapFS{
		"k/base/kustomization.yaml":    {Data: []byte("resources:\n- deployment.yaml\n")},
		"k/base/deployment.yaml":       {Data: []byte("apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\nspec:\n  replicas: 1\n  template:\n    metadata:\n      labels: {app: web}\n    spec:\n      containers:\n      - name: web\n        image: nginx:1.0\n")},
		"k/overlay/kustomization.yaml": {Data: []byte("resources:\n- ../base\n")},
	}
	raw := map[string]any{
		"images":       []any{"redis:7", "nginx=registry/nginx:1.99"},
		"replicas":     []any{map[string]any{"name": "other", "count": int64(1)}, map[string]any{"name": "web", "count": int64(4)}},
		"commonLabels": map[string]any{"env": "prod"},
		"patches":      []any{map[string]any{"patch": "- op: replace\n  path: /spec/replicas\n  value: 9\n", "target": map[string]any{"kind": "Deployment"}}},
	}
	opts := ParseOptions(raw)
	src, err := NewSource(fsys, "k/overlay", opts)
	if err != nil {
		t.Fatal(err)
	}
	files, _ := CollectFiles(fsys, src)
	obj := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "web", "namespace": "ns", "labels": map[string]any{"env": "prod"}},
		"spec":     map[string]any{"replicas": int64(4), "template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"name": "web", "image": "registry/nginx:1.99"}}}}}}}
	changes := []v1alpha1.FieldChange{
		{Path: "/spec/template/spec/containers/0/image", Op: v1alpha1.OpReplace, Desired: `"registry/nginx:1.99"`, Live: `"registry/nginx:2.0"`},
		{Path: "/metadata/labels/env", Op: v1alpha1.OpReplace, Desired: `"prod"`, Live: `"stage"`},
		{Path: "/spec/replicas", Op: v1alpha1.OpReplace, Desired: `4`, Live: `6`},
	}
	fields, err := FieldsFor(src, files, nil, obj, Policy{Application: "demo"}, changes)
	if err != nil {
		t.Fatal(err)
	}
	all := map[string]bool{}
	for _, c := range changes {
		all[c.Path] = true
	}
	got := ApplicationOptions(fields, all)
	want := []appvalues.Option{
		{Option: "spec.source.kustomize.images[nginx]", Path: "/spec/source/kustomize/images/1", Current: `"nginx=registry/nginx:1.99"`,
			Suggested: `"nginx=registry/nginx:2.0"`, Fields: []string{"/spec/template/spec/containers/0/image"}},
		{Option: "spec.source.kustomize.commonLabels[env]", Path: "/spec/source/kustomize/commonLabels/env", Current: `"prod"`,
			Suggested: `"stage"`, Fields: []string{"/metadata/labels/env"}},
		{Option: "spec.source.kustomize.replicas[name=web]", Path: "/spec/source/kustomize/replicas/1/count", Current: "4",
			Suggested: "6", Fields: []string{"/spec/replicas"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got:\n%+v\nwant:\n%+v", got, want)
	}
	if none := ApplicationOptions(fields, map[string]bool{}); len(none) != 0 {
		t.Errorf("no unmapped field: %+v", none)
	}

	t.Run("an inline patch is a location only", func(t *testing.T) {
		ch := []v1alpha1.FieldChange{{Path: "/spec/replicas", Op: v1alpha1.OpReplace, Desired: `9`, Live: `3`}}
		o2 := ParseOptions(map[string]any{"patches": raw["patches"]})
		s2, _ := NewSource(fsys, "k/overlay", o2)
		f2, _ := FieldsFor(s2, files, nil, obj, Policy{Application: "demo"}, ch)
		got := ApplicationOptions(f2, map[string]bool{"/spec/replicas": true})
		if len(got) != 1 || got[0].Option != "spec.source.kustomize.patches[0]" || got[0].Path != "/spec/source/kustomize/patches/0/patch" || got[0].Suggested != "" {
			t.Errorf("%+v", got)
		}
	})
}
