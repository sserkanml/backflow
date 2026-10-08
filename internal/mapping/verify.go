package mapping

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/sserkanml/backflow/api/v1alpha1"
	"github.com/sserkanml/backflow/internal/drift"
)

// Verify checks an edit: the other documents of the file are unchanged, and
// the difference between the original and the edited document is exactly the
// proposal's changes, no more and no less. This is what makes a mapping
// trustworthy; only a verified edit may go to Git.
//
// One relaxation: a proposal Replace at a path the original does not have is
// a defaulted field (Argo CD's rendered state contains defaults the manifest
// never spelled out). Apply adds it, so the edit shows up as an Add at that
// path or at the first missing parent. That is accepted when the edited
// document holds exactly the live value at the proposal's path and the Add
// contains nothing else.
func Verify(original, edited []byte, document int, changes []v1alpha1.FieldChange) error {
	fail := func(format string, args ...interface{}) error {
		return fmt.Errorf("%w: %s", ErrVerificationFailed, fmt.Sprintf(format, args...))
	}
	oText, eText := string(original), string(edited)
	oSegs, eSegs := splitDocuments(oText), splitDocuments(eText)
	if len(oSegs) != len(eSegs) || document < 0 || document >= len(oSegs) {
		return fail("the number of documents changed")
	}
	for i := range oSegs {
		if i == document {
			continue
		}
		if oText[oSegs[i].start:oSegs[i].end] != eText[eSegs[i].start:eSegs[i].end] {
			return fail("document %d was modified", i)
		}
	}

	o, err := documentMap(oText, oSegs[document])
	if err != nil {
		return fail("the original document: %v", err)
	}
	e, err := documentMap(eText, eSegs[document])
	if err != nil {
		return fail("the edited document: %v", err)
	}
	got := drift.Diff(o, e)
	want := changes
	want, got, err = acceptDefaulted(o, e, got, changes)
	if err != nil {
		return fail("%v", err)
	}
	if reflect.DeepEqual(normalize(got), normalize(want)) {
		return nil
	}
	return fail("the edit changes %s, the proposal expects %s", summarize(got), summarize(want))
}

// acceptDefaulted pairs each proposal Replace on a path that is absent in the
// original with the Add in got that created it, checks the value, and returns
// the proposal changes and the edit's changes that are left to compare.
func acceptDefaulted(orig, edited map[string]interface{}, got, want []v1alpha1.FieldChange) (restWant, restGot []v1alpha1.FieldChange, err error) {
	claimed := map[int][]string{} // index in got -> proposal paths it covers
	for _, c := range want {
		if c.Op != v1alpha1.OpReplace {
			restWant = append(restWant, c)
			continue
		}
		segs, perr := parsePointer(c.Path)
		if perr != nil {
			restWant = append(restWant, c)
			continue
		}
		if _, present := lookup(orig, segs); present {
			restWant = append(restWant, c)
			continue
		}
		idx := -1
		for i, g := range got {
			if g.Op == v1alpha1.OpAdd && (g.Path == c.Path || strings.HasPrefix(c.Path, g.Path+"/")) {
				idx = i
				break
			}
		}
		if idx < 0 {
			return nil, nil, fmt.Errorf("%s is not in the file and the edit does not add it", c.Path)
		}
		var live interface{}
		if jerr := json.Unmarshal([]byte(c.Live), &live); jerr != nil {
			return nil, nil, fmt.Errorf("the live value of %s is not valid JSON: %v", c.Path, jerr)
		}
		if have, ok := lookup(edited, segs); !ok || !reflect.DeepEqual(have, live) {
			return nil, nil, fmt.Errorf("the edited file does not hold the live value at %s", c.Path)
		}
		claimed[idx] = append(claimed[idx], c.Path)
	}

	for i, g := range got {
		paths, ok := claimed[i]
		if !ok {
			restGot = append(restGot, g)
			continue
		}
		var added interface{}
		if jerr := json.Unmarshal([]byte(g.Live), &added); jerr != nil {
			return nil, nil, fmt.Errorf("the edit's value at %s is not valid JSON: %v", g.Path, jerr)
		}
		for _, leaf := range leafPaths(g.Path, added) {
			if !coveredBy(leaf, paths) {
				return nil, nil, fmt.Errorf("the edit adds %s, which the proposal does not ask for", leaf)
			}
		}
	}
	return restWant, restGot, nil
}

func coveredBy(path string, claimed []string) bool {
	for _, c := range claimed {
		if path == c || strings.HasPrefix(path, c+"/") {
			return true
		}
	}
	return false
}

// leafPaths lists the JSON pointers of everything under value that is not a
// non-empty object.
func leafPaths(base string, value interface{}) []string {
	m, ok := value.(map[string]interface{})
	if !ok || len(m) == 0 {
		return []string{base}
	}
	var out []string
	for k, v := range m {
		out = append(out, leafPaths(base+"/"+escapeSegment(k), v)...)
	}
	return out
}

func escapeSegment(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}

// lookup follows a JSON pointer in decoded JSON.
func lookup(doc interface{}, segs []string) (interface{}, bool) {
	cur := doc
	for _, s := range segs {
		switch t := cur.(type) {
		case map[string]interface{}:
			v, ok := t[s]
			if !ok {
				return nil, false
			}
			cur = v
		case []interface{}:
			i, err := strconv.Atoi(s)
			if err != nil || i < 0 || i >= len(t) {
				return nil, false
			}
			cur = t[i]
		default:
			return nil, false
		}
	}
	return cur, true
}

// documentMap parses a document into the same shape drift.Diff gets from
// Argo CD: JSON types only.
func documentMap(text string, seg segment) (map[string]interface{}, error) {
	var v interface{}
	if err := yaml.Unmarshal([]byte(parseText(text, seg)), &v); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func normalize(c []v1alpha1.FieldChange) []v1alpha1.FieldChange {
	if len(c) == 0 {
		return nil
	}
	return c
}

func summarize(c []v1alpha1.FieldChange) string {
	if len(c) == 0 {
		return "nothing"
	}
	parts := make([]string, len(c))
	for i, ch := range c {
		parts[i] = fmt.Sprintf("%s %s", ch.Op, ch.Path)
	}
	return strings.Join(parts, ", ")
}
