// Package managedfields tells, from the metadata.managedFields of a live object, which
// manager (a tool or a controller) last wrote the fields that drifted, with which
// operation and when. It reads nothing but the object: no Kubernetes API, no webhook.
//
// What it cannot tell: a username (managedFields do not hold one), who removed a field
// (a removed field has no owner any more), and anything for an object whose
// managedFields are missing or cannot be parsed.
package managedfields

import (
	"bytes"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"sigs.k8s.io/structured-merge-diff/v6/fieldpath"
	"sigs.k8s.io/structured-merge-diff/v6/value"
)

// Operations, as managedFields spell them.
const (
	OperationUpdate = "Update"
	OperationApply  = "Apply"
)

// Kind classifies a manager.
type Kind string

const (
	// Interactive: a tool a person runs (kubectl, oc, a browser console).
	Interactive Kind = "interactive"
	// Controller: a component that writes to the API by itself, recognised by name.
	Controller Kind = "controller"
	// Unknown: a manager that is neither. The name is whatever the client chose
	// (fieldManager), so a name nobody has verified says nothing about who ran it.
	Unknown Kind = "unknown"
)

// controllerNames are the managers of the components of Kubernetes itself that change
// objects without anyone asking: the controller manager (the HPA scales through it), the
// scheduler, the kubelet and the API server (defaulting, garbage collection).
var controllerNames = []string{
	"kube-controller-manager", "cloud-controller-manager", "kube-scheduler", "kube-apiserver", "kubelet",
	"cluster-autoscaler",
}

// controllerSuffixes are the endings of the names operators and controllers give their
// field manager by convention (cert-manager-controller, kustomize-controller of Flux,
// openshift-controller-manager, cluster-version-operator).
var controllerSuffixes = []string{"-controller", "-operator", "-controller-manager"}

// IsController reports whether the manager is a known controller: one of controllerNames or
// a name that ends the way controllers and operators name themselves.
func IsController(manager string) bool {
	m := strings.ToLower(manager)
	if slices.Contains(controllerNames, m) {
		return true
	}
	for _, suffix := range controllerSuffixes {
		if strings.HasSuffix(m, suffix) {
			return true
		}
	}
	return false
}

// interactivePrefixes are the managers of tools a person runs. A manager name is the
// user agent of the client (or kubectl's name for the command), cut at the first "/".
var interactivePrefixes = []string{
	"kubectl", "oc", "mozilla", "chrome", "firefox", "safari", "edge", "opera",
	"k9s", "lens", "headlamp", "helm", "kustomize", "curl", "okteto",
}

// IsInteractive reports whether the manager is a tool a person runs: its name is one
// of interactivePrefixes, alone or followed by "-", "/" or ".", so that "kubectl-edit",
// "kubectl-client-side-apply", "oc" and "oc-edit" count and "octopus-controller" does not.
func IsInteractive(manager string) bool {
	m := strings.ToLower(manager)
	for _, p := range interactivePrefixes {
		if m == p {
			return true
		}
		if rest, ok := strings.CutPrefix(m, p); ok && rest != "" && strings.ContainsRune("-/. ", rune(rest[0])) {
			return true
		}
	}
	return false
}

// Classify says what kind of writer a manager is. A name is a controller or a tool only
// when it is recognised as one, in that order (kustomize-controller is a controller although
// kustomize is a tool); every other name is Unknown, an empty one too.
func Classify(manager string) Kind {
	switch {
	case manager == "":
		return Unknown
	case IsController(manager):
		return Controller
	case IsInteractive(manager):
		return Interactive
	}
	return Unknown
}

// IsIgnored reports whether a manager is Argo CD's own (it writes what Git says, which
// is not the drift) or Backflow's.
func IsIgnored(manager string) bool {
	m := strings.ToLower(manager)
	return strings.HasPrefix(m, "argocd-") || m == "argocd" || strings.HasPrefix(m, "backflow")
}

// Owner is a manager that wrote some of the drifted fields.
type Owner struct {
	Manager   string
	Operation string
	Time      time.Time
	// Paths are the drifted JSON pointers this manager owns.
	Paths []string
}

// Attribution is the outcome for one proposal: the manager that wrote last, and the
// others when several wrote.
type Attribution struct {
	// Latest is the manager whose entry is the most recent of the owners.
	Latest Owner
	// Others are the other owners, most recent first.
	Others []Owner
}

type entry struct {
	manager, operation string
	time               time.Time
	owned              []string // JSON pointers of every owned field, containers included
	leaves             []string // JSON pointers of the leaves
}

// Attribute finds, among the managedFields of live, the managers that own the drifted
// JSON pointers, leaving out Argo CD's and Backflow's. It returns nil, nil when no one
// owns any of them: a removed field has no owner, and an object without managedFields
// has no one to name. An error means the managedFields could not be read.
func Attribute(live map[string]any, pointers []string) (*Attribution, error) {
	entries, err := parse(live)
	if err != nil {
		return nil, err
	}
	byManager := map[string]*Owner{}
	for _, e := range entries {
		if IsIgnored(e.manager) {
			continue
		}
		var paths []string
		for _, p := range pointers {
			if owns(e, p) {
				paths = append(paths, p)
			}
		}
		if len(paths) == 0 {
			continue
		}
		key := e.manager + "\x00" + e.operation
		if o, ok := byManager[key]; ok {
			// The same manager and operation twice (one entry per subresource): the later time.
			if e.time.After(o.Time) {
				o.Time = e.time
			}
			o.Paths = mergePaths(o.Paths, paths)
			continue
		}
		byManager[key] = &Owner{Manager: e.manager, Operation: e.operation, Time: e.time, Paths: paths}
	}
	if len(byManager) == 0 {
		return nil, nil
	}
	owners := make([]Owner, 0, len(byManager))
	for _, o := range byManager {
		owners = append(owners, *o)
	}
	slices.SortStableFunc(owners, func(a, b Owner) int {
		switch {
		case a.Time.After(b.Time):
			return -1
		case b.Time.After(a.Time):
			return 1
		}
		return strings.Compare(a.Manager, b.Manager)
	})
	return &Attribution{Latest: owners[0], Others: owners[1:]}, nil
}

func mergePaths(a, b []string) []string {
	for _, p := range b {
		if !slices.Contains(a, p) {
			a = append(a, p)
		}
	}
	return a
}

// owns reports whether the entry owns the field at the JSON pointer: the field itself,
// something below it (a list that drifted as a whole is owned through its elements), or
// a leaf above it (a value that is owned as a unit).
func owns(e entry, pointer string) bool {
	for _, q := range e.owned {
		if q == pointer || strings.HasPrefix(q, pointer+"/") {
			return true
		}
	}
	for _, q := range e.leaves {
		if strings.HasPrefix(pointer, q+"/") {
			return true
		}
	}
	return false
}

// parse reads metadata.managedFields of the object.
func parse(live map[string]any) ([]entry, error) {
	meta, _ := live["metadata"].(map[string]any)
	raw, ok := meta["managedFields"]
	if !ok || raw == nil {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("metadata.managedFields is a %T, not a list", raw)
	}
	var out []entry
	for i, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("managedFields[%d] is a %T, not an object", i, item)
		}
		fields, ok := m["fieldsV1"]
		if !ok || fields == nil {
			continue // an entry that owns nothing, such as one made by an old API server
		}
		if ft, _ := m["fieldsType"].(string); ft != "" && ft != "FieldsV1" {
			continue
		}
		manager, _ := m["manager"].(string)
		operation, _ := m["operation"].(string)
		e := entry{manager: manager, operation: operation}
		if ts, _ := m["time"].(string); ts != "" {
			t, err := time.Parse(time.RFC3339, ts)
			if err != nil {
				return nil, fmt.Errorf("managedFields[%d].time %q: %w", i, ts, err)
			}
			e.time = t
		}
		data, err := jsonOf(fields)
		if err != nil {
			return nil, fmt.Errorf("managedFields[%d].fieldsV1: %w", i, err)
		}
		set := fieldpath.NewSet()
		if err := set.FromJSON(bytes.NewReader(data)); err != nil {
			return nil, fmt.Errorf("managedFields[%d].fieldsV1 of %q: %w", i, manager, err)
		}
		e.owned = pointersOf(live, set)
		e.leaves = pointersOf(live, set.Leaves())
		out = append(out, e)
	}
	return out, nil
}

// pointersOf turns the paths of a set into JSON pointers into the live object. A path
// whose list element is not in the object any more (it was removed, or the list changed
// since) is left out.
func pointersOf(live map[string]any, set *fieldpath.Set) []string {
	var out []string
	set.Iterate(func(p fieldpath.Path) {
		if ptr, ok := pointerOf(live, p); ok && ptr != "" {
			out = append(out, ptr)
		}
	})
	return out
}

func pointerOf(live any, path fieldpath.Path) (string, bool) {
	cur := live
	var b strings.Builder
	for _, pe := range path {
		switch {
		case pe.FieldName != nil:
			m, ok := cur.(map[string]any)
			if !ok {
				return "", false
			}
			b.WriteString("/" + escape(*pe.FieldName))
			cur = m[*pe.FieldName] // absent: the pointer is still the path of the field
		case pe.Key != nil:
			i, ok := indexByKey(cur, *pe.Key)
			if !ok {
				return "", false
			}
			b.WriteString("/" + strconv.Itoa(i))
			cur = cur.([]any)[i]
		case pe.Value != nil:
			i, ok := indexByValue(cur, *pe.Value)
			if !ok {
				return "", false
			}
			b.WriteString("/" + strconv.Itoa(i))
			cur = cur.([]any)[i]
		case pe.Index != nil:
			list, ok := cur.([]any)
			if !ok || *pe.Index < 0 || *pe.Index >= len(list) {
				return "", false
			}
			b.WriteString("/" + strconv.Itoa(*pe.Index))
			cur = list[*pe.Index]
		default:
			return "", false
		}
	}
	return b.String(), true
}

// escape is the JSON pointer escaping of a token.
func escape(s string) string {
	return strings.NewReplacer("~", "~0", "/", "~1").Replace(s)
}

// indexByKey finds the element of a list whose fields match the key (k:{"name":"web"}).
func indexByKey(cur any, key value.FieldList) (int, bool) {
	list, ok := cur.([]any)
	if !ok {
		return 0, false
	}
	for i, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		match := true
		for _, f := range key {
			if !sameValue(m[f.Name], f.Value.Unstructured()) {
				match = false
				break
			}
		}
		if match {
			return i, true
		}
	}
	return 0, false
}

// indexByValue finds the element of a set list (v:"value").
func indexByValue(cur any, v value.Value) (int, bool) {
	list, ok := cur.([]any)
	if !ok {
		return 0, false
	}
	want := v.Unstructured()
	for i, item := range list {
		if sameValue(item, want) {
			return i, true
		}
	}
	return 0, false
}

// sameValue compares two decoded JSON values; numbers compare by value whatever their Go type.
func sameValue(a, b any) bool {
	if fa, ok := number(a); ok {
		fb, ok := number(b)
		return ok && fa == fb
	}
	return reflect.DeepEqual(a, b)
}

func number(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}
