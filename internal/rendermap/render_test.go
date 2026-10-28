package rendermap

import (
	"context"
	"errors"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/sserkanml/backflow/internal/argocd"
	"github.com/sserkanml/backflow/internal/metrics/metricstest"
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

func TestArgoRendererCountsRenders(t *testing.T) {
	count := func(result string) float64 {
		return metricstest.Value("backflow_renders_total", map[string]string{"renderer": "upload", "result": result})
	}
	ok, failed, bad := count("ok"), count("failed"), count("error")
	for _, f := range []*fakeManifests{
		{resp: &argocd.ManifestsResponse{}},
		{err: argocd.ErrRenderFailed},
		{err: argocd.ErrArchiveTooLarge},
		{err: argocd.ErrUnreachable},
	} {
		_, _ = (&ArgoRenderer{Client: f}).Render(context.Background(), App{Name: "a"}, nil)
	}
	if got := count("ok") - ok; got != 1 {
		t.Errorf("ok = %v, want 1", got)
	}
	if got := count("failed") - failed; got != 2 {
		t.Errorf("failed = %v, want 2 (a source that does not render, files over the limits)", got)
	}
	if got := count("error") - bad; got != 1 {
		t.Errorf("error = %v, want 1 (Argo CD unreachable is not a verdict about the source)", got)
	}
}
