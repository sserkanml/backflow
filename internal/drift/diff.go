// Package drift computes field-level differences between the desired state
// rendered by Argo CD and the live state of a resource.
package drift

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"

	"github.com/sserkanml/backflow/api/v1alpha1"
)

// builtinIgnores are JSON pointers that are always noise. A pointer also
// ignores everything below it.
var builtinIgnores = []string{
	"/status",
	"/metadata/managedFields",
	"/metadata/resourceVersion",
	"/metadata/uid",
	"/metadata/generation",
	"/metadata/creationTimestamp",
	"/metadata/annotations/" + escape("kubectl.kubernetes.io/last-applied-configuration"),
	"/metadata/annotations/" + escape("argocd.argoproj.io/tracking-id"),
}

// Diff returns the changes needed to turn desired into live, ordered by path.
// Add means the field only exists live, Remove that it only exists in desired.
// extraIgnores are additional JSON pointers to skip, including their subtrees.
func Diff(desired, live map[string]interface{}, extraIgnores ...string) []v1alpha1.FieldChange {
	d := differ{ignores: append(append([]string{}, builtinIgnores...), extraIgnores...)}
	d.diffMaps("", desired, live)
	return d.changes
}

type differ struct {
	ignores []string
	changes []v1alpha1.FieldChange
}

func (d *differ) ignored(path string) bool {
	for _, p := range d.ignores {
		p = strings.TrimRight(p, "/")
		if p != "" && (path == p || strings.HasPrefix(path, p+"/")) {
			return true
		}
	}
	return false
}

func (d *differ) diffMaps(path string, desired, live map[string]interface{}) {
	keys := make([]string, 0, len(desired)+len(live))
	for k := range desired {
		keys = append(keys, k)
	}
	for k := range live {
		if _, ok := desired[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	for _, k := range keys {
		p := path + "/" + escape(k)
		if d.ignored(p) {
			continue
		}
		dv, dOK := desired[k]
		lv, lOK := live[k]
		switch {
		case dOK && !lOK:
			d.changes = append(d.changes, v1alpha1.FieldChange{Path: p, Op: v1alpha1.OpRemove, Desired: encode(dv)})
		case !dOK && lOK:
			d.changes = append(d.changes, v1alpha1.FieldChange{Path: p, Op: v1alpha1.OpAdd, Live: encode(lv)})
		default:
			d.diffValues(p, dv, lv)
		}
	}
}

func (d *differ) diffValues(path string, desired, live interface{}) {
	switch dv := desired.(type) {
	case map[string]interface{}:
		if lv, ok := live.(map[string]interface{}); ok {
			d.diffMaps(path, dv, lv)
			return
		}
	case []interface{}:
		if lv, ok := live.([]interface{}); ok && len(dv) == len(lv) {
			for i := range dv {
				p := path + "/" + itoa(i)
				if d.ignored(p) {
					continue
				}
				d.diffValues(p, dv[i], lv[i])
			}
			return
		}
	}
	if reflect.DeepEqual(desired, live) {
		return
	}
	d.changes = append(d.changes, v1alpha1.FieldChange{
		Path: path, Op: v1alpha1.OpReplace, Desired: encode(desired), Live: encode(live),
	})
}

// escape applies RFC 6901 escaping to a pointer segment.
func escape(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}

func encode(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "null"
	}
	return string(b)
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}
