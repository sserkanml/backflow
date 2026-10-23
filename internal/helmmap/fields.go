package helmmap

import (
	"encoding/json"

	v1alpha1 "github.com/sserkanml/backflow/api/v1alpha1"
)

// ScalarOf decodes a change's JSON value when it is a string, a number or a
// boolean: the kinds of value a key of a value file can hold.
func ScalarOf(raw string) (any, bool) {
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return nil, false
	}
	switch v.(type) {
	case string, float64, bool:
		return v, true
	}
	return nil, false
}

// FieldsFor turns the changes of a proposal into the Fields Verify takes: for
// each Replace of a scalar, the keys that hold the old value (Find) and, when
// the old and new values are strings, the keys that hold a part of it
// (FindFragments), the image tag of "repository:tag" for example. Fragment
// keys are tried only when no key holds the whole value. Other changes have no
// candidates. files are the unmodified files of the render (CollectFiles).
func FieldsFor(src *Source, files map[string][]byte, changes []v1alpha1.FieldChange) ([]Field, error) {
	fields := make([]Field, len(changes))
	for i, ch := range changes {
		found := &Result{}
		old, scalar := ScalarOf(ch.Desired)
		replace := scalar && ch.Op == v1alpha1.OpReplace
		if replace {
			var err error
			if found, err = src.Find(files, old); err != nil {
				return nil, err
			}
		}
		field := Field{Index: i, Change: ch, Found: found.Found()}
		if replace {
			newV, newOK := ScalarOf(ch.Live)
			o, oIsString := old.(string)
			n, nIsString := newV.(string)
			if newOK && oIsString && nIsString {
				frags, err := src.FindFragments(files, o, n)
				if err != nil {
					return nil, err
				}
				field.Fragments = frags.Found()
			}
		}
		fields[i] = field
	}
	return fields, nil
}
