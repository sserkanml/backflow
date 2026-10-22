package helmmap

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/sserkanml/backflow/api/v1alpha1"
	"github.com/sserkanml/backflow/internal/mapping"
)

// ErrCannotSet means a value file cannot be edited with certainty.
var ErrCannotSet = errors.New("cannot set the value")

// Written is the outcome of SetValue.
type Written struct {
	// Result holds the edited file and its diff. Document is 0.
	mapping.Result
	// Line is the 1-based line of the edited key in the new file.
	Line int
	// Text is that line as it reads in the new file.
	Text string
}

// SetValue sets one scalar in a value file and returns the whole new file.
// Everything outside the edited entry stays as it was, including comments, key
// order and the quoting style of the value (the YAML editor of
// internal/mapping does the edit).
//
// The new value keeps the type of the old one when it reads as that type: an
// int stays an int if value is "4", a bool stays a bool for "false". Otherwise
// it is written as a string, so no value is ever converted to a type it did
// not have. The key must exist and hold a scalar.
func SetValue(file string, content []byte, key KeyPath, value string) (*Written, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(content, &doc); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrCannotSet, file, err)
	}
	old, err := lookup(&doc, key)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %s: %v", ErrCannotSet, file, key, err)
	}

	nv := typed(old.Tag, value)
	live, err := json.Marshal(nv)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCannotSet, err)
	}
	pointer := key.pointer()
	edited, err := mapping.Apply(file, content, 0, []v1alpha1.FieldChange{
		{Path: pointer, Op: v1alpha1.OpReplace, Live: string(live)},
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCannotSet, err)
	}
	if err := checkOnlyChanged(content, edited, key, RenderValue(nv)); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrCannotSet, file, err)
	}

	var after yaml.Node
	if err := yaml.Unmarshal(edited, &after); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrCannotSet, file, err)
	}
	node, err := lookup(&after, key)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %s: %v", ErrCannotSet, file, key, err)
	}
	lines := strings.Split(string(edited), "\n")
	text := ""
	if node.Line >= 1 && node.Line <= len(lines) {
		text = strings.TrimRight(lines[node.Line-1], "\r")
	}
	return &Written{
		Result: mapping.Result{
			File:     file,
			Document: 0,
			Edits:    []mapping.Edit{{File: file, Location: pointer, Value: string(live)}},
			Edited:   edited,
			Diff:     mapping.UnifiedDiff(file, content, edited),
		},
		Line: node.Line,
		Text: text,
	}, nil
}

// typed returns value as the Go type of the old YAML tag when writing it as
// that type renders back to exactly the same string, and as a string
// otherwise. The test is the round trip: a templated string has to come out as
// it was meant. 1.20 written as a float renders as 1.2, so a float field gets
// the quoted string "1.20"; 4 written as an int renders as 4 and stays an int.
func typed(tag, value string) any {
	switch tag {
	case "!!int":
		if i, err := strconv.ParseInt(value, 10, 64); err == nil && strconv.FormatInt(i, 10) == value {
			return json.Number(value)
		}
	case "!!float":
		// A JSON number that renders back as written: no leading +, no bare dot,
		// no trailing zero, no exponent.
		if _, err := strconv.ParseFloat(value, 64); err == nil && json.Valid([]byte(value)) && RenderValue(json.Number(value)) == value {
			return json.Number(value)
		}
	case "!!bool":
		if value == "true" || value == "false" {
			return value == "true"
		}
	}
	return value
}

// lookup finds the scalar at key in a parsed document. Map entries are found
// by key and list items by index.
func lookup(doc *yaml.Node, key KeyPath) (*yaml.Node, error) {
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, errors.New("the file is empty")
	}
	cur := resolve(doc.Content[0])
	for i, seg := range key {
		switch cur.Kind {
		case yaml.MappingNode:
			var next *yaml.Node
			for j := 0; j+1 < len(cur.Content); j += 2 {
				if cur.Content[j].Kind == yaml.ScalarNode && cur.Content[j].Value == seg && cur.Content[j].Tag != "!!merge" {
					next = cur.Content[j+1]
				}
			}
			if next == nil {
				return nil, fmt.Errorf("the key %q does not exist (or comes from a merge key)", key[:i+1].String())
			}
			cur = next
		case yaml.SequenceNode:
			n, err := strconv.Atoi(seg)
			if err != nil || n < 0 || n >= len(cur.Content) || strconv.Itoa(n) != seg {
				return nil, fmt.Errorf("%q is not an item of the list at %q", seg, key[:i].String())
			}
			cur = cur.Content[n]
		default:
			return nil, fmt.Errorf("%q is not a map or a list", key[:i].String())
		}
		if i < len(key)-1 {
			cur = resolve(cur)
		}
	}
	// An alias at the key is replaced by the editor; it is a scalar if it
	// points to one.
	target := resolve(cur)
	if target.Kind != yaml.ScalarNode {
		return nil, errors.New("the value is not a scalar")
	}
	return target, nil
}

// checkOnlyChanged verifies that the edit changed the one key and nothing
// else: the same definitions in the same order, with only the value at key
// different, and rendering as want.
func checkOnlyChanged(before, after []byte, key KeyPath, want string) error {
	b, err := parseValues(before)
	if err != nil {
		return err
	}
	a, err := parseValues(after)
	if err != nil {
		return fmt.Errorf("the edited file does not parse: %w", err)
	}
	if len(a) != len(b) {
		return fmt.Errorf("the edit changed the structure of the file")
	}
	seen := false
	for i := range a {
		if !a[i].path.Equal(b[i].path) || a[i].kind != b[i].kind {
			return fmt.Errorf("the edit changed the structure of the file")
		}
		if a[i].path.Equal(key) {
			seen = true
			if a[i].kind != defScalar || a[i].value != want {
				return fmt.Errorf("the edited value is %q, want %q", a[i].value, want)
			}
			continue
		}
		if a[i].value != b[i].value {
			return fmt.Errorf("the edit changed %s", a[i].path)
		}
	}
	if !seen {
		return fmt.Errorf("the key is gone from the edited file")
	}
	return nil
}

// AddValue adds a scalar to a value file at a key path the file does not
// define, to override a default of the chart. Missing intermediate maps are
// created; an existing parent map gets the entry appended after its last one,
// in its own indentation. Everything else stays as it was, comments and
// quoting included.
//
// defaultTag is the YAML tag of the default being overridden (!!int, !!bool,
// !!float, or !!str); the value keeps that type when it reads as that type, as
// for SetValue. The file must not define the key, nor anything above or below
// it: an override is only ever added where the file has nothing to say.
func AddValue(file string, content []byte, key KeyPath, value, defaultTag string) (*Written, error) {
	if len(key) == 0 {
		return nil, fmt.Errorf("%w: %s: an empty key path", ErrCannotSet, file)
	}
	before, err := parseValues(content)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrCannotSet, file, err)
	}
	for _, d := range before {
		if related(d.path, key) {
			return nil, fmt.Errorf("%w: %s already defines %s", ErrCannotSet, file, d.path)
		}
	}

	nv := typed(defaultTag, value)
	live, err := json.Marshal(nv)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCannotSet, err)
	}
	pointer := key.pointer()
	edited, err := mapping.Apply(file, content, 0, []v1alpha1.FieldChange{
		{Path: pointer, Op: v1alpha1.OpAdd, Live: string(live)},
	})
	if errors.Is(err, mapping.ErrCannotApply) && !hasMapRoot(content) {
		edited, err = appendKey(content, key, nv)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCannotSet, err)
	}
	if err := checkOnlyAdded(before, edited, key, RenderValue(nv)); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrCannotSet, file, err)
	}

	var after yaml.Node
	if err := yaml.Unmarshal(edited, &after); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrCannotSet, file, err)
	}
	node, err := lookup(&after, key)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %s: %v", ErrCannotSet, file, key, err)
	}
	lines := strings.Split(string(edited), "\n")
	text := ""
	if node.Line >= 1 && node.Line <= len(lines) {
		text = strings.TrimRight(lines[node.Line-1], "\r")
	}
	return &Written{
		Result: mapping.Result{
			File:     file,
			Document: 0,
			Edits:    []mapping.Edit{{File: file, Location: pointer, Value: string(live)}},
			Edited:   edited,
			Diff:     mapping.UnifiedDiff(file, content, edited),
		},
		Line: node.Line,
		Text: text,
	}, nil
}

// hasMapRoot reports whether the file has a map at its top level (of any
// style). A file with only comments, or with an explicit null, has none.
func hasMapRoot(content []byte) bool {
	var doc yaml.Node
	if err := yaml.Unmarshal(content, &doc); err != nil || doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return false
	}
	return resolve(doc.Content[0]).Kind == yaml.MappingNode
}

// appendKey writes the key at the end of a file that has no map yet (empty, or
// comments only).
func appendKey(content []byte, key KeyPath, value any) ([]byte, error) {
	var node any = value
	if n, ok := value.(json.Number); ok {
		if i, err := n.Int64(); err == nil {
			node = i
		} else if f, err := n.Float64(); err == nil {
			node = f
		}
	}
	for i := len(key) - 1; i >= 0; i-- {
		node = map[string]any{key[i]: node}
	}
	var buf strings.Builder
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(node); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	out := string(content)
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return []byte(out + buf.String()), nil
}

// checkOnlyAdded verifies that the edit added the one key and nothing else:
// every definition of the file is still there with the same value, and the key
// is a new scalar rendering as want.
func checkOnlyAdded(before []definition, edited []byte, key KeyPath, want string) error {
	after, err := parseValues(edited)
	if err != nil {
		return fmt.Errorf("the edited file does not parse: %w", err)
	}
	type id struct {
		path string
		kind defKind
	}
	old := make(map[id]string, len(before))
	for _, d := range before {
		old[id{d.path.String(), d.kind}] = d.value
	}
	added := 0
	for _, d := range after {
		if d.path.Equal(key) {
			if d.kind != defScalar || d.value != want {
				return fmt.Errorf("the added value is %q, want %q", d.value, want)
			}
			added++
			continue
		}
		v, ok := old[id{d.path.String(), d.kind}]
		if !ok {
			return fmt.Errorf("the edit added %s", d.path)
		}
		if v != d.value {
			return fmt.Errorf("the edit changed %s", d.path)
		}
		delete(old, id{d.path.String(), d.kind})
	}
	if added != 1 {
		return fmt.Errorf("the key was not added")
	}
	if len(old) > 0 {
		return fmt.Errorf("the edit removed definitions")
	}
	return nil
}
