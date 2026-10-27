// Package appvalues describes values that an Argo CD Application sets in its own
// manifest (helm.parameters, helm.valuesObject, kustomize.images, ...) and finds
// the Application that defines it. It is pure: nothing here reads a cluster or
// a repository.
package appvalues

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Option is one option of an Application that sets a drifted value.
type Option struct {
	// Option names it for people: spec.source.helm.parameters[name=image.tag].
	Option string
	// Path is the JSON pointer of the option's value in the Application.
	Path string
	// Current is the JSON of the value the option has; empty when the option is
	// a string that holds YAML or JSON (helm.values, kustomize patches).
	Current string
	// Suggested is the JSON that would reproduce the live state, when it can be
	// derived exactly; otherwise empty.
	Suggested string
	// Fields are the JSON pointers of the drifted fields the option sets.
	Fields []string
}

// TrackingID is the parsed argocd.argoproj.io/tracking-id of a resource: the
// Application that manages it and the resource itself.
type TrackingID struct {
	AppNamespace, AppName string
	Group, Kind           string
	Namespace, Name       string
}

// TrackingAnnotation is the annotation Argo CD puts on what it manages.
const TrackingAnnotation = "argocd.argoproj.io/tracking-id"

// ParseTrackingID parses "<app>:<group>/<kind>:<namespace>/<name>", where
// <app> is "<namespace>_<name>" for an Application outside the default
// namespace of Argo CD.
func ParseTrackingID(id string) (TrackingID, bool) {
	parts := strings.Split(id, ":")
	if len(parts) != 3 {
		return TrackingID{}, false
	}
	var t TrackingID
	if ns, name, ok := strings.Cut(parts[0], "_"); ok {
		t.AppNamespace, t.AppName = ns, name
	} else {
		t.AppName = parts[0]
	}
	gk := strings.SplitN(parts[1], "/", 2)
	if len(gk) != 2 {
		return TrackingID{}, false
	}
	t.Group, t.Kind = gk[0], gk[1]
	nn := strings.SplitN(parts[2], "/", 2)
	if len(nn) != 2 {
		return TrackingID{}, false
	}
	t.Namespace, t.Name = nn[0], nn[1]
	return t, t.AppName != "" && t.Kind != "" && t.Name != ""
}

// ResourceRef is a managed resource as an Application lists it in
// status.resources.
type ResourceRef struct{ Group, Kind, Namespace, Name string }

// Manages reports whether an Application lists the resource in
// status.resources.
func Manages(app *unstructured.Unstructured, r ResourceRef) bool {
	items, _, _ := unstructured.NestedSlice(app.Object, "status", "resources")
	for _, it := range items {
		m, _ := it.(map[string]any)
		if str(m, "group") == r.Group && str(m, "kind") == r.Kind && str(m, "namespace") == r.Namespace && str(m, "name") == r.Name {
			return true
		}
	}
	return false
}

func str(m map[string]any, k string) string { s, _ := m[k].(string); return s }

// ApplicationSetOwner returns the name of the ApplicationSet that owns an
// Application, or "".
func ApplicationSetOwner(app *unstructured.Unstructured) string {
	for _, o := range app.GetOwnerReferences() {
		if o.Kind == "ApplicationSet" {
			return o.Name
		}
	}
	return ""
}

func jsonOf(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// Where is the helm block of a source in the Application: its JSON pointer and
// its name for people.
type Where struct{ Pointer, Name string }

// SingleSource is spec.source.helm.
var SingleSource = Where{Pointer: "/spec/source/helm", Name: "spec.source.helm"}

// SourceAt is spec.sources[i].helm of a multi-source Application.
func SourceAt(i int) Where {
	return Where{Pointer: fmt.Sprintf("/spec/sources/%d/helm", i), Name: fmt.Sprintf("spec.sources[%d].helm", i)}
}

// HelmParameter describes the helm.parameters entry named name. suggested is the
// new value as text, when it is known exactly. The last entry of that name wins,
// as in Helm.
func HelmParameter(at Where, helm map[string]any, name, suggested string, hasSuggested bool) (Option, bool) {
	items, _ := helm["parameters"].([]any)
	idx := -1
	for i, it := range items {
		if m, _ := it.(map[string]any); str(m, "name") == name {
			idx = i
		}
	}
	if idx < 0 {
		return Option{}, false
	}
	m, _ := items[idx].(map[string]any)
	o := Option{
		Option:  fmt.Sprintf("%s.parameters[name=%s]", at.Name, name),
		Path:    fmt.Sprintf("%s/parameters/%d/value", at.Pointer, idx),
		Current: jsonOf(m["value"]),
	}
	if hasSuggested {
		o.Suggested = jsonOf(suggested)
	}
	return o, true
}

// HelmValuesObject describes the key at path of helm.valuesObject. The suggested
// value keeps the type of the current one: a number stays a number, a boolean a
// boolean.
func HelmValuesObject(at Where, helm map[string]any, path []string, suggested string, hasSuggested bool) (Option, bool) {
	cur := helm["valuesObject"]
	var escaped []string
	for _, seg := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return Option{}, false
		}
		if cur, ok = m[seg]; !ok {
			return Option{}, false
		}
		escaped = append(escaped, strings.NewReplacer("~", "~0", "/", "~1").Replace(seg))
	}
	pointer := at.Pointer + "/valuesObject"
	if len(escaped) > 0 {
		pointer += "/" + strings.Join(escaped, "/")
	}
	o := Option{Option: at.Name + ".valuesObject." + strings.Join(path, "."), Path: pointer, Current: jsonOf(cur)}
	if hasSuggested {
		o.Suggested = typedLike(cur, suggested)
	}
	return o, true
}

// HelmValues describes a key of helm.values, a string that holds YAML: where it
// is, without a value.
func HelmValues(at Where, path []string) Option {
	return Option{Option: at.Name + ".values (key " + strings.Join(path, ".") + ")", Path: at.Pointer + "/values"}
}

// typedLike renders text as JSON of the same type as like.
func typedLike(like any, text string) string {
	switch like.(type) {
	case float64:
		if _, err := strconv.ParseFloat(text, 64); err == nil {
			return text
		}
	case bool:
		if text == "true" || text == "false" {
			return text
		}
	}
	return jsonOf(text)
}
