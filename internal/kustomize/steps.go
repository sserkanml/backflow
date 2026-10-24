package kustomize

import (
	"fmt"
	"strconv"
	"strings"
)

// mergeKey is the key that identifies an item of the list at a field of a
// built-in Kubernetes resource in a strategic merge patch; "" when the list is
// replaced as a whole (tolerations, args, command, ...). parent is the field
// that holds the list, grand the field two levels up (containers for ports).
func mergeKey(group, parent, grand string) string {
	if group != "" && strings.Contains(group, ".") && !strings.HasSuffix(group, ".k8s.io") {
		return "" // a custom resource: kustomize merges its lists as a whole
	}
	switch parent {
	case "containers", "initContainers", "ephemeralContainers", "env", "volumes", "imagePullSecrets":
		return keyName
	case "volumeMounts":
		return "mountPath"
	case "volumeDevices":
		return "devicePath"
	case "ports":
		if grand == "" || isContainerList(grand) {
			return "containerPort"
		}
		return "port"
	}
	return ""
}

func isContainerList(field string) bool {
	return field == "containers" || field == "initContainers" || field == "ephemeralContainers"
}

// step is one step into a strategic merge patch: a key of a mapping, or an
// item of a list found by its merge key.
type step struct {
	key              string
	mergeKey, mergeV string
}

// stepsOf translates the JSON pointer segments of a field of a resource into
// steps of a strategic merge patch. ok is false when a list on the way has no
// merge key, or the item does not exist.
func stepsOf(group string, obj map[string]any, segs []string) ([]step, bool) {
	var (
		steps         []step
		cur           any = obj
		parent, grand string
	)
	for _, seg := range segs {
		switch c := cur.(type) {
		case map[string]any:
			steps = append(steps, step{key: seg})
			cur = c[seg]
			grand, parent = parent, seg
		case []any:
			mk := mergeKey(group, parent, grand)
			i, err := strconv.Atoi(seg)
			if mk == "" || err != nil || i < 0 || i >= len(c) {
				return steps, false
			}
			item, _ := c[i].(map[string]any)
			v, has := item[mk]
			if !has {
				return steps, false
			}
			steps = append(steps, step{mergeKey: mk, mergeV: fmt.Sprint(v)})
			cur = item
		default:
			// The path is below a value the resource does not have: a key to add.
			steps = append(steps, step{key: seg})
			cur = nil
		}
	}
	return steps, true
}

// findInPatch follows steps in a parsed strategic merge patch and returns the
// JSON pointer of the node they reach in the patch; ok is false when the patch
// does not have it.
func findInPatch(root nodeLike, steps []step) (pointer string, node nodeLike, ok bool) {
	cur := root
	var segs []string
	for _, st := range steps {
		if st.key != "" || st.mergeKey == "" {
			next := cur.child(st.key)
			if next == nil {
				return "", nil, false
			}
			segs = append(segs, st.key)
			cur = next
			continue
		}
		item, idx := cur.item(st.mergeKey, st.mergeV)
		if item == nil {
			return "", nil, false
		}
		segs = append(segs, strconv.Itoa(idx))
		cur = item
	}
	return pointerOf(segs...), cur, true
}
