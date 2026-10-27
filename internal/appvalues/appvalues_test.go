package appvalues

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestParseTrackingID(t *testing.T) {
	for name, tc := range map[string]struct {
		in   string
		want TrackingID
		ok   bool
	}{
		"same namespace": {"spike-parent:argoproj.io/Application:argocd/child-web",
			TrackingID{AppName: "spike-parent", Group: "argoproj.io", Kind: "Application", Namespace: "argocd", Name: "child-web"}, true},
		"application in another namespace": {"team-a_root:argoproj.io/Application:team-a/web",
			TrackingID{AppNamespace: "team-a", AppName: "root", Group: "argoproj.io", Kind: "Application", Namespace: "team-a", Name: "web"}, true},
		"core group":     {"app:/ConfigMap:ns/cm", TrackingID{AppName: "app", Kind: "ConfigMap", Namespace: "ns", Name: "cm"}, true},
		"too few parts":  {"app:argoproj.io/Application", TrackingID{}, false},
		"no group slash": {"app:Application:argocd/x", TrackingID{}, false},
		"no name":        {"app:argoproj.io/Application:argocd", TrackingID{}, false},
		"empty":          {"", TrackingID{}, false},
	} {
		got, ok := ParseTrackingID(tc.in)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("%s: got %+v, %v; want %+v, %v", name, got, ok, tc.want, tc.ok)
		}
	}
}

func app(resources []any, owners ...metav1.OwnerReference) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": "p"}, "status": map[string]any{"resources": resources}}}
	u.SetOwnerReferences(owners)
	return u
}

func TestManagesAndOwner(t *testing.T) {
	p := app([]any{
		map[string]any{"group": "argoproj.io", "kind": "Application", "namespace": "argocd", "name": "child"},
		map[string]any{"group": "", "kind": "ConfigMap", "namespace": "ns", "name": "cm"},
	})
	if !Manages(p, ResourceRef{"argoproj.io", "Application", "argocd", "child"}) {
		t.Error("the child is listed")
	}
	if Manages(p, ResourceRef{"argoproj.io", "Application", "argocd", "other"}) || Manages(p, ResourceRef{"argoproj.io", "Application", "other", "child"}) {
		t.Error("another name or namespace is not the child")
	}
	if ApplicationSetOwner(p) != "" {
		t.Error("no owner")
	}
	set := app(nil, metav1.OwnerReference{Kind: "ApplicationSet", Name: "set"}, metav1.OwnerReference{Kind: "Other", Name: "x"})
	if ApplicationSetOwner(set) != "set" {
		t.Error("the ApplicationSet owner")
	}
}

func TestHelmOptions(t *testing.T) {
	helm := map[string]any{
		"parameters": []any{
			map[string]any{"name": "a", "value": "1"},
			map[string]any{"name": "image.tag", "value": "1.27"},
			map[string]any{"name": "image.tag", "value": "1.28"}, // the last one wins
		},
		"valuesObject": map[string]any{"replicaCount": float64(2), "ui": map[string]any{"color": "red", "on": true}},
	}
	t.Run("a parameter", func(t *testing.T) {
		o, ok := HelmParameter(helm, "image.tag", "1.29", true)
		if !ok || o.Option != "spec.source.helm.parameters[name=image.tag]" || o.Path != "/spec/source/helm/parameters/2/value" ||
			o.Current != `"1.28"` || o.Suggested != `"1.29"` {
			t.Errorf("%+v %v", o, ok)
		}
		if o, _ := HelmParameter(helm, "image.tag", "", false); o.Suggested != "" {
			t.Errorf("no suggestion was asked for: %+v", o)
		}
		if _, ok := HelmParameter(helm, "nope", "x", true); ok {
			t.Error("no such parameter")
		}
	})
	t.Run("a valuesObject key keeps the type", func(t *testing.T) {
		for path, want := range map[string]struct{ ptr, cur, sug string }{
			"replicaCount": {"/spec/source/helm/valuesObject/replicaCount", "2", "5"},
			"ui.color":     {"/spec/source/helm/valuesObject/ui/color", `"red"`, `"5"`},
			"ui.on":        {"/spec/source/helm/valuesObject/ui/on", "true", `"5"`},
		} {
			segs := splitDots(path)
			o, ok := HelmValuesObject(helm, segs, "5", true)
			if !ok || o.Path != want.ptr || o.Current != want.cur || o.Suggested != want.sug {
				t.Errorf("%s: %+v %v", path, o, ok)
			}
		}
		if o, _ := HelmValuesObject(helm, []string{"ui", "on"}, "false", true); o.Suggested != "false" {
			t.Errorf("a boolean stays a boolean: %+v", o)
		}
		if _, ok := HelmValuesObject(helm, []string{"ui", "missing"}, "x", true); ok {
			t.Error("no such key")
		}
		if _, ok := HelmValuesObject(helm, []string{"replicaCount", "x"}, "x", true); ok {
			t.Error("below a scalar")
		}
	})
	t.Run("helm.values is a location only", func(t *testing.T) {
		o := HelmValues([]string{"ui", "color"})
		if o.Option != "spec.source.helm.values (key ui.color)" || o.Path != "/spec/source/helm/values" || o.Suggested != "" || o.Current != "" {
			t.Errorf("%+v", o)
		}
	})
}

func splitDots(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == '.' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	return append(out, cur)
}
