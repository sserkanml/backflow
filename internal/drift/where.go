package drift

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"

	"github.com/sserkanml/backflow/api/v1alpha1"
)

// Lookup returns the value at a JSON pointer in a decoded resource. ok is
// false when the path does not exist.
func Lookup(state map[string]interface{}, pointer string) (value interface{}, ok bool) {
	var cur interface{} = state
	if pointer == "" {
		return cur, true
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, false
	}
	for _, seg := range strings.Split(pointer[1:], "/") {
		seg = strings.ReplaceAll(strings.ReplaceAll(seg, "~1", "/"), "~0", "~")
		switch node := cur.(type) {
		case map[string]interface{}:
			next, found := node[seg]
			if !found {
				return nil, false
			}
			cur = next
		case []interface{}:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(node) {
				return nil, false
			}
			cur = node[i]
		default:
			return nil, false
		}
	}
	return cur, true
}

// Where tells which side of a set of recorded changes a resource is on now.
// atDesired is true when every changed field has the value Git had when the
// changes were recorded (or is absent where Git had none): the cluster was
// reset to Git. atLive is true when every field has the live value that was
// recorded: the cluster still holds the change, or Git adopted it. Neither is
// true when the fields hold something else. With no changes both are false.
func Where(state map[string]interface{}, changes []v1alpha1.FieldChange) (atDesired, atLive bool) {
	if len(changes) == 0 {
		return false, false
	}
	atDesired, atLive = true, true
	for _, ch := range changes {
		v, found := Lookup(state, ch.Path)
		if !matches(v, found, ch.Op != v1alpha1.OpAdd, ch.Desired) {
			atDesired = false
		}
		if !matches(v, found, ch.Op != v1alpha1.OpRemove, ch.Live) {
			atLive = false
		}
	}
	return atDesired, atLive
}

// matches compares a found value with a JSON-encoded expectation. When the
// expectation does not exist (present is false), the field must be absent.
func matches(v interface{}, found, present bool, encoded string) bool {
	if !present {
		return !found
	}
	if !found {
		return false
	}
	var want interface{}
	if err := json.Unmarshal([]byte(encoded), &want); err != nil {
		return false
	}
	return reflect.DeepEqual(v, want)
}
