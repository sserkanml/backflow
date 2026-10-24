package kustomize_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/sserkanml/backflow/internal/drift"
	"github.com/sserkanml/backflow/internal/kustomize"
	"github.com/sserkanml/backflow/internal/kustomize/kbuild"
	"github.com/sserkanml/backflow/internal/rendermap"
)

// The tests in this file render with the kustomize binary (v5.8.1, the version
// of Argo CD): bin/kustomize from `make kustomize`, or KUSTOMIZE_BIN. They are
// skipped without it.
func kustomizeBin(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("KUSTOMIZE_BIN")
	if bin == "" {
		bin = filepath.Join("..", "..", "bin", "kustomize")
	}
	if _, err := exec.LookPath(bin); err != nil {
		if _, err := os.Stat(bin); err != nil {
			t.Skip("no kustomize binary; run `make kustomize`")
		}
	}
	return bin
}

const deployment = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
spec:
  replicas: 1
  selector:
    matchLabels:
      app: web
  template:
    metadata:
      labels:
        app: web
    spec:
      tolerations:
      - key: a
        operator: Exists
      - key: b
        operator: Exists
      containers:
      - name: web
        image: nginx:1.0
        imagePullPolicy: IfNotPresent
        env:
        - name: A
          value: base-a
        - name: B
          value: base-b
        ports:
        - containerPort: 80
`

func fixture() fstest.MapFS {
	f := func(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }
	return fstest.MapFS{
		"k/base/kustomization.yaml": f("resources:\n- deployment.yaml\n"),
		"k/base/deployment.yaml":    f(deployment),
		"k/overlays/dev/kustomization.yaml": f(`resources:
- ../../base
- extra.yaml
images:
- name: nginx
  newTag: "1.1"
replicas:
- name: web
  count: 2
patches:
- path: env.yaml
- target:
    kind: Deployment
    name: web
  patch: |-
    - op: replace
      path: /spec/template/spec/containers/0/imagePullPolicy
      value: Always
configMapGenerator:
- name: plain
  literals:
  - mode=dev
  options:
    disableNameSuffixHash: true
- name: hashed
  literals:
  - mode=dev
`),
		"k/overlays/dev/env.yaml": f(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
spec:
  template:
    spec:
      containers:
      - name: web
        env:
        - name: A
          value: dev-a
`),
		"k/overlays/dev/extra.yaml": f("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: extra\ndata:\n  level: one\n"),
	}
}

type env struct {
	t     *testing.T
	fsys  fstest.MapFS
	src   *kustomize.Source
	files map[string][]byte
	rend  *kbuild.Renderer
	loc   *kustomize.Located
	app   rendermap.App
}

func setup(t *testing.T, opts kustomize.Options) *env {
	t.Helper()
	bin := kustomizeBin(t)
	fsys := fixture()
	src, err := kustomize.NewSource(fsys, "k/overlays/dev", opts)
	if err != nil {
		t.Fatal(err)
	}
	files, err := kustomize.CollectFiles(fsys, src)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, fsys: fsys, src: src, files: files, app: rendermap.App{Name: "demo", DestNamespace: "demo"},
		rend: &kbuild.Renderer{Bin: bin, Dir: "k/overlays/dev", Options: opts}}
	e.loc, err = kustomize.Locate(context.Background(), e.rend, src, files, e.app)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) obj(kind, namePrefix string) *unstructured.Unstructured {
	for k, o := range e.loc.Objects {
		if k.Kind == kind && strings.HasPrefix(k.Name, namePrefix) {
			return o
		}
	}
	e.t.Fatalf("no %s %s in %v", kind, namePrefix, e.loc.Objects)
	return nil
}

// verify changes the live object and maps the drift.
func (e *env) verify(obj *unstructured.Unstructured, mutate func(o map[string]any), pol kustomize.Policy) *rendermap.Outcome {
	e.t.Helper()
	live := obj.DeepCopy()
	mutate(live.Object)
	changes := drift.Diff(obj.Object, live.Object)
	if len(changes) == 0 {
		e.t.Fatal("the mutation changed nothing")
	}
	gv := obj.GroupVersionKind()
	pol.Application = "demo"
	out, err := kustomize.Verify(context.Background(), e.rend, kustomize.Input{
		App: e.app, Resource: rendermap.ResourceKey{Group: gv.Group, Kind: obj.GetKind(), Namespace: obj.GetNamespace(), Name: obj.GetName()},
		Changes: changes, Source: e.src, Files: e.files, Located: e.loc, Policy: pol,
	}, rendermap.Options{})
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

func container(o map[string]any) map[string]any {
	spec := o["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	return spec["containers"].([]any)[0].(map[string]any)
}

func podSpec(o map[string]any) map[string]any {
	return o["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
}

func editedFiles(out *rendermap.Outcome) []string {
	files := make([]string, 0, len(out.Results))
	for _, r := range out.Results {
		files = append(files, r.File)
	}
	return files
}

func wantMapped(t *testing.T, out *rendermap.Outcome, files ...string) {
	t.Helper()
	if !out.Mapped() {
		t.Fatalf("unmapped: %+v", out.Unmapped)
	}
	got := strings.Join(editedFiles(out), ",")
	if got != strings.Join(files, ",") {
		t.Errorf("edited files = %s, want %v\n%s", got, files, diffs(out))
	}
}

func diffs(out *rendermap.Outcome) string {
	var b strings.Builder
	for _, r := range out.Results {
		b.WriteString(r.Diff)
	}
	return b.String()
}

func wantUnmapped(t *testing.T, out *rendermap.Outcome, reason string) {
	t.Helper()
	if out.Mapped() || out.Unmapped.Reason != reason {
		t.Fatalf("outcome = %+v (%+v), want %s", out, out.Unmapped, reason)
	}
}

var defaults = kustomize.Policy{AddPatches: true}

const (
	kfile = "k/overlays/dev/kustomization.yaml"
	envF  = "k/overlays/dev/env.yaml"
	deplF = "k/base/deployment.yaml"
)

func TestMapsToSettings(t *testing.T) {
	e := setup(t, kustomize.Options{})
	dep := e.obj("Deployment", "web")
	t.Run("image tag goes to images newTag", func(t *testing.T) {
		out := e.verify(dep, func(o map[string]any) { container(o)["image"] = "nginx:9.9" }, defaults)
		wantMapped(t, out, kfile)
		if !strings.Contains(diffs(out), `+  newTag: "9.9"`) {
			t.Errorf("diff:\n%s", diffs(out))
		}
	})
	t.Run("replicas go to replicas count", func(t *testing.T) {
		out := e.verify(dep, func(o map[string]any) { o["spec"].(map[string]any)["replicas"] = int64(5) }, defaults)
		wantMapped(t, out, kfile)
		if !strings.Contains(diffs(out), "+  count: 5") {
			t.Errorf("diff:\n%s", diffs(out))
		}
	})
	t.Run("a literal of an unhashed generator", func(t *testing.T) {
		cm := e.obj("ConfigMap", "plain")
		out := e.verify(cm, func(o map[string]any) { o["data"].(map[string]any)["mode"] = "prod" }, defaults)
		wantMapped(t, out, kfile)
		if !strings.Contains(diffs(out), "+  - mode=prod") {
			t.Errorf("diff:\n%s", diffs(out))
		}
	})
	t.Run("a hashed generator is not mapped, and says the name changes", func(t *testing.T) {
		cm := e.obj("ConfigMap", "hashed")
		out := e.verify(cm, func(o map[string]any) { o["data"].(map[string]any)["mode"] = "prod" }, defaults)
		wantUnmapped(t, out, rendermap.ReasonValueAffectsOtherFields)
		if !strings.Contains(out.Unmapped.Message, "name hash") {
			t.Errorf("message = %s", out.Unmapped.Message)
		}
	})
}

func TestMapsToPatches(t *testing.T) {
	e := setup(t, kustomize.Options{})
	dep := e.obj("Deployment", "web")
	t.Run("env value in a strategic merge patch file", func(t *testing.T) {
		out := e.verify(dep, func(o map[string]any) {
			container(o)["env"].([]any)[0].(map[string]any)["value"] = "live-a"
		}, defaults)
		wantMapped(t, out, envF)
		if !strings.Contains(diffs(out), "+          value: live-a") {
			t.Errorf("diff:\n%s", diffs(out))
		}
	})
	t.Run("a JSON 6902 operation in an inline patch", func(t *testing.T) {
		out := e.verify(dep, func(o map[string]any) { container(o)["imagePullPolicy"] = "Never" }, defaults)
		wantMapped(t, out, kfile)
		if !strings.Contains(diffs(out), "+      value: Never") {
			t.Errorf("diff:\n%s", diffs(out))
		}
	})
}

func TestMapsToResourceFiles(t *testing.T) {
	e := setup(t, kustomize.Options{})
	cm := e.obj("ConfigMap", "extra")
	out := e.verify(cm, func(o map[string]any) { o["data"].(map[string]any)["level"] = "two" }, defaults)
	wantMapped(t, out, "k/overlays/dev/extra.yaml")
	if !strings.Contains(diffs(out), "+  level: two") {
		t.Errorf("diff:\n%s", diffs(out))
	}
}

func TestSharedBase(t *testing.T) {
	e := setup(t, kustomize.Options{})
	dep := e.obj("Deployment", "web")
	change := func(o map[string]any) { container(o)["env"].([]any)[1].(map[string]any)["value"] = "live-b" }
	t.Run("without editBases the base is not edited; a new patch is the way", func(t *testing.T) {
		out := e.verify(dep, change, defaults)
		wantMapped(t, out, "k/overlays/dev/backflow-patches/deployment-web.yaml", kfile)
		if !strings.Contains(diffs(out), "- path: backflow-patches/deployment-web.yaml") {
			t.Errorf("diff:\n%s", diffs(out))
		}
	})
	t.Run("with neither editBases nor addPatches it is Unmapped", func(t *testing.T) {
		out := e.verify(dep, change, kustomize.Policy{})
		wantUnmapped(t, out, rendermap.ReasonNoVerifiedCandidate)
		if !strings.Contains(out.Unmapped.Message, "editBases") {
			t.Errorf("message = %s", out.Unmapped.Message)
		}
	})
	t.Run("with editBases the resource file of the base is edited", func(t *testing.T) {
		out := e.verify(dep, change, kustomize.Policy{EditBases: true, AddPatches: true})
		wantMapped(t, out, deplF)
	})
}

func TestNewPatch(t *testing.T) {
	e := setup(t, kustomize.Options{})
	dep := e.obj("Deployment", "web")
	t.Run("a toleration is removed with a patch", func(t *testing.T) {
		out := e.verify(dep, func(o map[string]any) {
			s := podSpec(o)
			s["tolerations"] = s["tolerations"].([]any)[:1]
		}, defaults)
		wantMapped(t, out, "k/overlays/dev/backflow-patches/deployment-web.yaml", kfile)
	})
	t.Run("an env var is removed with $patch: delete", func(t *testing.T) {
		out := e.verify(dep, func(o map[string]any) {
			c := container(o)
			c["env"] = c["env"].([]any)[:1]
		}, defaults)
		wantMapped(t, out, "k/overlays/dev/backflow-patches/deployment-web.yaml", kfile)
		if !strings.Contains(diffs(out), "$patch: delete") {
			t.Errorf("diff:\n%s", diffs(out))
		}
	})
}

func TestSetByApplication(t *testing.T) {
	e := setup(t, kustomize.Options{Images: []string{"nginx=nginx:7.7"}})
	dep := e.obj("Deployment", "web")
	out := e.verify(dep, func(o map[string]any) { container(o)["image"] = "nginx:9.9" }, defaults)
	wantUnmapped(t, out, rendermap.ReasonSetByApplication)
	if !strings.Contains(out.Unmapped.Message, "spec.source.kustomize.images of Application demo; Git cannot change it") {
		t.Errorf("message = %s", out.Unmapped.Message)
	}
}

func TestDescribe(t *testing.T) {
	e := setup(t, kustomize.Options{})
	dep := e.obj("Deployment", "web")
	t.Run("a setting", func(t *testing.T) {
		out := e.verify(dep, func(o map[string]any) { container(o)["image"] = "nginx:9.9" }, defaults)
		d := kustomize.Describe(out)
		if d.Strategy != "KustomizePatch" || len(d.Tiers) != 1 || d.Tiers[0] != kustomize.TierSetting || len(d.SharedFiles) != 0 {
			t.Errorf("%+v", d)
		}
		if text := d.Text(nil); !strings.Contains(text, "images[nginx].newTag") {
			t.Errorf("text = %s", text)
		}
	})
	t.Run("a new patch says a file was added", func(t *testing.T) {
		out := e.verify(dep, func(o map[string]any) {
			s := podSpec(o)
			s["tolerations"] = s["tolerations"].([]any)[:1]
		}, defaults)
		d := kustomize.Describe(out)
		if d.Strategy != "KustomizePatch" || len(d.AddedFiles) != 1 || !strings.Contains(d.Text(nil), "new patch file was added") {
			t.Errorf("%+v\n%s", d, d.Text(nil))
		}
	})
	t.Run("a shared base is Direct and warns about the other Applications", func(t *testing.T) {
		out := e.verify(dep, func(o map[string]any) {
			container(o)["env"].([]any)[1].(map[string]any)["value"] = "live-b"
		}, kustomize.Policy{EditBases: true})
		d := kustomize.Describe(out)
		if d.Strategy != "Direct" || len(d.SharedFiles) != 1 || d.SharedFiles[0] != deplF {
			t.Fatalf("%+v", d)
		}
		text := d.Text(map[string][]string{deplF: {"argocd/demo-prod"}})
		if !strings.Contains(text, "outside the Application's path") || !strings.Contains(text, "argocd/demo-prod") {
			t.Errorf("text = %s", text)
		}
	})
}
