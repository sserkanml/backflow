package kustomize

import (
	"errors"
	"strings"
	"testing"
	"testing/fstest"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/sserkanml/backflow/internal/rendermap"
)

func overlayFS() fstest.MapFS {
	return fstest.MapFS{
		"apps/k/base/kustomization.yaml":         {Data: []byte("resources:\n- deployment.yaml\n")},
		"apps/k/base/deployment.yaml":            {Data: []byte("kind: Deployment\n")},
		"apps/k/overlays/dev/kustomization.yaml": {Data: []byte("# overlay\nresources:\n- ../../base\nimages:\n- name: nginx\n  newTag: \"1.0\"\n")},
	}
}

func TestNewSourceAndCollectFiles(t *testing.T) {
	fsys := overlayFS()
	src, err := NewSource(fsys, "./apps/k/overlays/dev", Options{})
	if err != nil {
		t.Fatal(err)
	}
	files, err := CollectFiles(fsys, src)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"apps/k/base/deployment.yaml", "apps/k/base/kustomization.yaml", "apps/k/overlays/dev/kustomization.yaml"} {
		if _, ok := files[want]; !ok {
			t.Errorf("%s is not in the upload set %v", want, keys(files))
		}
	}
	for f := range files {
		if strings.HasSuffix(f, "kustomization.yml") || strings.HasSuffix(f, "Kustomization") {
			t.Errorf("%s does not exist and must not be uploaded", f)
		}
	}
	if !src.InPath("apps/k/overlays/dev/x.yaml") || src.InPath("apps/k/base/deployment.yaml") || src.InPath("apps/k/overlays/devops/x.yaml") {
		t.Error("InPath")
	}
	if got := src.Rel("apps/k/base/deployment.yaml"); got != "../../base/deployment.yaml" {
		t.Errorf("Rel = %q", got)
	}
}

func TestNewSourceNotClosed(t *testing.T) {
	fsys := overlayFS()
	fsys["apps/k/overlays/dev/kustomization.yaml"] = &fstest.MapFile{Data: []byte("resources:\n- https://github.com/org/repo//base?ref=v1\n- ../../missing\n")}
	_, err := NewSource(fsys, "apps/k/overlays/dev", Options{})
	if !errors.Is(err, ErrNotClosed) || ReasonOf(err) != ReasonSourceNotClosed {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "remote") || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("the message does not carry the RelevantFiles reasons: %v", err)
	}
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestParseOptions(t *testing.T) {
	o := ParseOptions(map[string]any{
		"namePrefix": "p-", "images": []any{"nginx=registry/nginx:9.9", "redis:7"},
		"replicas":     []any{map[string]any{"name": "web", "count": int64(5)}},
		"commonLabels": map[string]any{"env": "x"}, "components": []any{"../c"},
		"patches":           []any{map[string]any{"path": "p.yaml", "target": map[string]any{"kind": "Deployment", "name": "web"}}},
		"forceCommonLabels": true, "version": "v5",
	})
	if o.NamePrefix != "p-" || o.Replicas["web"] != "5" || o.CommonLabels["env"] != "x" || len(o.Components) != 1 {
		t.Errorf("%+v", o)
	}
	if got := o.PatchFiles(); len(got) != 1 || got[0] != "p.yaml" {
		t.Errorf("patch files = %v", got)
	}
	if len(o.Other) != 1 || o.Other[0] != "forceCommonLabels" {
		t.Errorf("other = %v", o.Other)
	}
	for image, want := range map[string]string{
		"nginx:1.0": "nginx=registry/nginx:9.9", "registry/nginx:2": "nginx=registry/nginx:9.9",
		"redis:6": "redis:7", "busybox": "",
	} {
		if got := o.SetsImage(image); got != want {
			t.Errorf("SetsImage(%q) = %q, want %q", image, got, want)
		}
	}
	if len(ParseOptions(nil).Images) != 0 {
		t.Error("nil options")
	}
}

func TestBuildMetadataAndOrigins(t *testing.T) {
	fsys := overlayFS()
	src, _ := NewSource(fsys, "apps/k/overlays/dev", Options{})
	files, _ := CollectFiles(fsys, src)
	loc, err := src.WithBuildMetadata(files)
	if err != nil {
		t.Fatal(err)
	}
	got := string(loc["apps/k/overlays/dev/kustomization.yaml"])
	if !strings.Contains(got, "buildMetadata") || !strings.Contains(got, "# overlay") {
		t.Errorf("kustomization = %q", got)
	}
	if string(files["apps/k/overlays/dev/kustomization.yaml"]) == got {
		t.Error("the unmodified files were changed")
	}
	if string(loc["apps/k/base/deployment.yaml"]) != string(files["apps/k/base/deployment.yaml"]) {
		t.Error("only the kustomization of the Application gets the metadata")
	}

	dep := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "web", "namespace": "ns", "annotations": map[string]any{
			"config.kubernetes.io/origin":                "path: ../../base/deployment.yaml\n",
			"alpha.config.kubernetes.io/transformations": "- configuredIn: kustomization.yaml\n  configuredBy:\n    kind: ImageTagTransformer\n",
			"keep": "me"}}}}
	cm := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": "cfg-abc", "annotations": map[string]any{
			"config.kubernetes.io/origin": "configuredIn: kustomization.yaml\nconfiguredBy:\n  kind: ConfigMapGenerator\n"}}}}
	clean, origins, err := src.ParseOrigins([]*unstructured.Unstructured{dep, cm})
	if err != nil {
		t.Fatal(err)
	}
	if a := clean[0].GetAnnotations(); len(a) != 1 || a["keep"] != "me" {
		t.Errorf("annotations = %v", a)
	}
	if clean[1].GetAnnotations() != nil {
		t.Errorf("annotations = %v", clean[1].GetAnnotations())
	}
	if _, still := dep.GetAnnotations()["config.kubernetes.io/origin"]; !still {
		t.Error("ParseOrigins changed its input")
	}
	d := origins[rendermap.ResourceKey{Group: "apps", Kind: "Deployment", Namespace: "ns", Name: "web"}]
	if d == nil || d.File != "apps/k/base/deployment.yaml" || d.Generated ||
		len(d.Transformers) != 1 || d.Transformers[0].Kind != "ImageTagTransformer" || d.Transformers[0].ConfiguredIn != "apps/k/overlays/dev/kustomization.yaml" {
		t.Errorf("deployment origin = %+v", d)
	}
	g := origins[rendermap.ResourceKey{Kind: "ConfigMap", Name: "cfg-abc"}]
	if g == nil || !g.Generated || g.ConfiguredIn != "apps/k/overlays/dev/kustomization.yaml" {
		t.Errorf("generated origin = %+v", g)
	}
}
