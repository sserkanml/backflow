// Package actorwebhook is the observe-only admission webhook that tells Backflow who
// changed a resource. It answers every review "allowed", does no I/O on the request
// path, and keeps the paths of the changed fields, never a value.
package actorwebhook

import (
	"slices"
	"strings"
)

// ignored are the metadata fields whose change is not a change of the resource.
var ignoredMetadata = map[string]bool{
	"managedFields": true, "resourceVersion": true, "generation": true,
}

// lastApplied is the annotation kubectl apply rewrites on every apply.
const lastApplied = "kubectl.kubernetes.io/last-applied-configuration"

// ChangedPaths returns the JSON pointers of the fields that differ between old and new,
// sorted. Maps are compared key by key; lists of the same length element by element; a
// list whose length changed is one path. The values are compared and forgotten: the
// result holds none. metadata.managedFields, resourceVersion and generation, and the
// last-applied annotation, are not changes.
func ChangedPaths(old, cur map[string]any) []string {
	var out []string
	diff(old, cur, "", &out)
	slices.Sort(out)
	return out
}

func diff(a, b any, path string, out *[]string) {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			*out = append(*out, rootIfEmpty(path))
			return
		}
		keys := map[string]struct{}{}
		for k := range av {
			keys[k] = struct{}{}
		}
		for k := range bv {
			keys[k] = struct{}{}
		}
		for k := range keys {
			if path == "/metadata" && ignoredMetadata[k] {
				continue
			}
			child := path + "/" + escape(k)
			if path == "/metadata/annotations" && k == lastApplied {
				continue
			}
			x, inA := av[k]
			y, inB := bv[k]
			if !inA || !inB {
				*out = append(*out, child)
				continue
			}
			diff(x, y, child, out)
		}
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			*out = append(*out, rootIfEmpty(path))
			return
		}
		for i := range av {
			diff(av[i], bv[i], path+"/"+itoa(i), out)
		}
	default:
		if !equal(a, b) {
			*out = append(*out, rootIfEmpty(path))
		}
	}
}

// equal compares two scalars. JSON numbers decode as float64, so 1 and 1.0 agree.
func equal(a, b any) bool { return a == b }

func rootIfEmpty(path string) string {
	if path == "" {
		return "/"
	}
	return path
}

// escape makes a key a JSON pointer token (RFC 6901).
func escape(k string) string {
	k = strings.ReplaceAll(k, "~", "~0")
	return strings.ReplaceAll(k, "/", "~1")
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	n := len(b)
	for i > 0 {
		n--
		b[n] = byte('0' + i%10)
		i /= 10
	}
	return string(b[n:])
}
