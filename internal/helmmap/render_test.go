package helmmap

import (
	"context"
	"errors"
	"io/fs"
	"maps"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/sserkanml/backflow/internal/argocd"
)

type fakeManifests struct {
	got  argocd.ManifestsQuery
	resp *argocd.ManifestsResponse
	err  error
}

func (f *fakeManifests) GetManifestsWithFiles(_ context.Context, q argocd.ManifestsQuery) (*argocd.ManifestsResponse, error) {
	f.got = q
	return f.resp, f.err
}

func TestArgoRendererRender(t *testing.T) {
	f := &fakeManifests{resp: &argocd.ManifestsResponse{Manifests: []string{
		`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"a"},"data":{"k":"v"}}`,
		`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"b","namespace":"other"}}`,
		`{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"n"}}`,
		`{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"ClusterRole","metadata":{"name":"r"}}`,
		`{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"Role","metadata":{"name":"r"}}`,
		`{"apiVersion":"example.com/v1","kind":"Widget","metadata":{"name":"w"}}`,
		`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"new"}}`,
		`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"new-in-ns","namespace":"x"}}`,
		`{}`,
	}}}
	r := &ArgoRenderer{Client: f}
	app := App{
		Name: "demo-helm", Namespace: "argocd", Project: "default", DestNamespace: "demo",
		Resources: []ListedResource{
			{Kind: "ConfigMap", Namespace: "demo", Name: "a"},
			{Kind: "ConfigMap", Namespace: "other", Name: "b"},
			{Kind: "Namespace", Name: "n"},
			{Group: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "r"},
			{Group: "rbac.authorization.k8s.io", Kind: "Role", Namespace: "demo", Name: "r"},
			// A custom resource that is cluster-scoped: the list says so, a
			// built-in table of kinds could not.
			{Group: "example.com", Kind: "Widget", Name: "w"},
		},
	}
	objs, err := r.Render(context.Background(), app, map[string][]byte{"x": []byte("y")})
	if err != nil {
		t.Fatal(err)
	}
	if f.got.AppName != "demo-helm" || f.got.AppNamespace != "argocd" || f.got.Project != "default" || string(f.got.Files["x"]) != "y" {
		t.Errorf("query = %+v", f.got)
	}
	got := make([]string, 0, len(objs))
	for _, o := range objs {
		got = append(got, o.GetKind()+"/"+o.GetName()+" ns="+o.GetNamespace())
	}
	want := []string{
		"ConfigMap/a ns=demo",      // listed as namespaced: the chart left the namespace out
		"ConfigMap/b ns=other",     // its own namespace
		"Namespace/n ns=",          // listed without a namespace
		"ClusterRole/r ns=",        // listed without a namespace
		"Role/r ns=demo",           // same name as the ClusterRole, a different kind
		"Widget/w ns=",             // a cluster-scoped custom resource, known from the list
		"ConfigMap/new ns=demo",    // not listed, no namespace: the destination namespace
		"ConfigMap/new-in-ns ns=x", // not listed, has a namespace: that one
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("rendered\n%v\nwant\n%v", got, want)
	}
}

func TestNamespaceForSeveralListed(t *testing.T) {
	app := App{DestNamespace: "d", Resources: []ListedResource{
		{Kind: "ConfigMap", Namespace: "one", Name: "a"}, {Kind: "ConfigMap", Namespace: "two", Name: "a"},
	}}
	mk := func(ns string) *unstructured.Unstructured {
		u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "a"}}}
		if ns != "" {
			u.SetNamespace(ns)
		}
		return u
	}
	if got := namespaceFor(app, mk("two")); got != "two" {
		t.Errorf("own namespace among the listed = %q", got)
	}
	if got := namespaceFor(app, mk("")); got != "d" {
		t.Errorf("listed in several namespaces, none in the manifest = %q, want the destination namespace", got)
	}
}

func TestArgoRendererErrors(t *testing.T) {
	tests := []struct {
		name string
		resp *argocd.ManifestsResponse
		err  error
		want error // nil: any error
		not  error
	}{
		{name: "template error", err: errors.Join(argocd.ErrRenderFailed, errors.New("boom")), want: ErrRenderFailed},
		{name: "too large", err: argocd.ErrArchiveTooLarge, want: ErrFilesTooLarge},
		{name: "unreachable stays as it is", err: argocd.ErrUnreachable, want: argocd.ErrUnreachable, not: ErrRenderFailed},
		{name: "unauthorized stays as it is", err: argocd.ErrUnauthorized, want: argocd.ErrUnauthorized, not: ErrRenderFailed},
		{name: "not JSON", resp: &argocd.ManifestsResponse{Manifests: []string{"{"}}},
		{name: "no name", resp: &argocd.ManifestsResponse{Manifests: []string{`{"kind":"ConfigMap"}`}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &ArgoRenderer{Client: &fakeManifests{resp: tt.resp, err: tt.err}}
			_, err := r.Render(context.Background(), App{Name: "a"}, nil)
			if err == nil {
				t.Fatal("no error")
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
			if tt.not != nil && errors.Is(err, tt.not) {
				t.Errorf("err = %v must not be %v", err, tt.not)
			}
		})
	}
}

func sortedKeys(m map[string][]byte) []string { return slices.Sorted(maps.Keys(m)) }

func TestCollectFilesDemo(t *testing.T) {
	fsys := demoRepo(t)
	src, err := NewSource(fsys, Spec{Path: "helm/demo", ValueFiles: []string{"values-dev.yaml", "/helm/shared/services/alpha/dev.yaml"}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := CollectFiles(fsys, src)
	if err != nil {
		t.Fatal(err)
	}
	names := strings.Join(sortedKeys(got), "\n")
	for _, want := range []string{
		"helm/demo/Chart.yaml", "helm/demo/values.yaml", "helm/demo/values-dev.yaml", "helm/demo/values-prod.yaml", // the whole chart directory
		"helm/demo-sidecar/Chart.yaml", "helm/demo-sidecar/values.yaml", // the file:// dependency
		"helm/shared/services/alpha/dev.yaml", // a value file outside the chart
	} {
		if !strings.Contains(names, want+"\n") && !strings.HasSuffix(names, want) {
			t.Errorf("%s is not collected:\n%s", want, names)
		}
	}
	if _, ok := got["helm/shared/Chart.yaml"]; ok {
		t.Error("a directory that is only used for a value file was collected as a whole")
	}
}

func TestCollectFilesDependencies(t *testing.T) {
	file := func(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }
	dep := func(repo string) string {
		return "apiVersion: v2\nname: x\ndependencies:\n  - name: d\n    version: 1.0.0\n    repository: " + repo + "\n"
	}
	tests := []struct {
		name    string
		fsys    fstest.MapFS
		wantErr error
		want    []string
	}{
		{
			name: "dependencies of dependencies",
			fsys: fstest.MapFS{
				"a/Chart.yaml": file(dep("file://../b")), "b/Chart.yaml": file(dep("file://../c")), "c/Chart.yaml": file("name: c\n"),
			},
			want: []string{"a/Chart.yaml", "b/Chart.yaml", "c/Chart.yaml", "a/values.yaml"},
		},
		{
			name: "a dependency cycle ends",
			fsys: fstest.MapFS{"a/Chart.yaml": file(dep("file://../b")), "b/Chart.yaml": file(dep("file://../a"))},
			want: []string{"a/Chart.yaml", "b/Chart.yaml", "a/values.yaml"},
		},
		{
			name: "requirements.yaml of an apiVersion v1 chart",
			fsys: fstest.MapFS{
				"a/Chart.yaml": file("name: a\n"), "a/requirements.yaml": file("dependencies:\n  - name: d\n    repository: file://../b\n"),
				"b/Chart.yaml": file("name: b\n"),
			},
			want: []string{"a/Chart.yaml", "a/requirements.yaml", "b/Chart.yaml", "a/values.yaml"},
		},
		{
			name: "remote dependencies are left to Argo CD",
			fsys: fstest.MapFS{"a/Chart.yaml": file(dep("https://charts.example.com"))},
			want: []string{"a/Chart.yaml", "a/values.yaml"},
		},
		{
			name:    "dependency leaving the repository",
			fsys:    fstest.MapFS{"a/Chart.yaml": file(dep("file://../../x"))},
			wantErr: ErrDependencyOutsideRepository,
		},
		{
			name:    "missing dependency directory",
			fsys:    fstest.MapFS{"a/Chart.yaml": file(dep("file://../nope"))},
			wantErr: ErrDependencyOutsideRepository,
		},
		{
			name: "symlink in the chart",
			fsys: fstest.MapFS{
				"a/Chart.yaml": file("name: a\n"),
				"a/link":       &fstest.MapFile{Data: []byte("Chart.yaml"), Mode: fs.ModeSymlink},
			},
			wantErr: ErrSymlinkInChart,
		},
		{
			name:    "argocd source overrides",
			fsys:    fstest.MapFS{"a/Chart.yaml": file("name: a\n"), "a/.argocd-source-demo.yaml": file("helm:\n  parameters: []\n")},
			wantErr: ErrArgoSourceOverrides,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.fsys["a/values.yaml"] = file("x: 1\n")
			src, err := NewSource(tt.fsys, Spec{Path: "a", ValueFiles: []string{"values.yaml"}})
			if err != nil {
				t.Fatal(err)
			}
			got, err := CollectFiles(tt.fsys, src)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := slices.Sorted(slices.Values(tt.want))
			if g := strings.Join(sortedKeys(got), ","); g != strings.Join(want, ",") {
				t.Errorf("files = %s, want %s", g, strings.Join(want, ","))
			}
		})
	}
}
