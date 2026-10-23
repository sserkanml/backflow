package helmmap

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/sserkanml/backflow/api/v1alpha1"
	"github.com/sserkanml/backflow/internal/mapping"
)

// SetNode sets a key of a value file to a map, a list or a scalar, and returns
// the whole new file. The key may or may not exist in the file: an existing
// node (of any kind, a null included) is replaced, a missing one is added with
// the maps above it. Maps and lists are written in block style and the rest of
// the file is untouched: comments, key order, siblings and quoting stay.
//
// The key must not go through a list. The result is checked: the file must
// parse, hold exactly the value at the key, and define everything else the
// file defined before, unchanged.
func SetNode(file string, content []byte, key KeyPath, value any) (*Written, error) {
	if len(key) == 0 {
		return nil, fmt.Errorf("%w: %s: an empty key path", ErrCannotSet, file)
	}
	before, err := parseValues(content)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrCannotSet, file, err)
	}
	for _, d := range before {
		if d.kind == defList && len(d.path) < len(key) && key.HasPrefix(d.path) {
			return nil, fmt.Errorf("%w: %s: %s goes through the list at %s", ErrCannotSet, file, key, d.path)
		}
	}
	text, err := jsonText(value)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCannotSet, err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(content, &doc); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrCannotSet, file, err)
	}
	op := v1alpha1.OpAdd
	if _, found := lookupNode(&doc, key); found {
		op = v1alpha1.OpReplace
	}
	pointer := key.pointer()
	edited, err := mapping.Apply(file, content, 0, []v1alpha1.FieldChange{{Path: pointer, Op: op, Live: text}})
	if errors.Is(err, mapping.ErrCannotApply) && !hasMapRoot(content) {
		edited, err = appendKey(content, key, value)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCannotSet, err)
	}
	if err := checkNodeSet(before, edited, key, value); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrCannotSet, file, err)
	}
	return written(file, content, edited, key, pointer, text), nil
}

// RemoveKey removes a key of a value file, with everything below it. The key
// must exist in the file and not be a list item. The result is checked like
// the one of SetNode.
func RemoveKey(file string, content []byte, key KeyPath) (*Written, error) {
	if len(key) == 0 {
		return nil, fmt.Errorf("%w: %s: an empty key path", ErrCannotSet, file)
	}
	before, err := parseValues(content)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrCannotSet, file, err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(content, &doc); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrCannotSet, file, err)
	}
	if _, found := lookupNode(&doc, key); !found {
		return nil, fmt.Errorf("%w: %s: the key %s does not exist", ErrCannotSet, file, key)
	}
	pointer := key.pointer()
	edited, err := mapping.Apply(file, content, 0, []v1alpha1.FieldChange{{Path: pointer, Op: v1alpha1.OpRemove}})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCannotSet, err)
	}
	if err := checkNodeSet(before, edited, key, nil); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrCannotSet, file, err)
	}
	return written(file, content, edited, key, pointer, ""), nil
}

func written(file string, content, edited []byte, key KeyPath, pointer, text string) *Written {
	w := &Written{Result: mapping.Result{
		File: file, Document: 0, Edited: edited, Diff: mapping.UnifiedDiff(file, content, edited),
		Edits: []mapping.Edit{{File: file, Location: pointer, Value: text}},
	}}
	var after yaml.Node
	if yaml.Unmarshal(edited, &after) == nil {
		if node, ok := lookupNode(&after, key); ok && node.Line >= 1 {
			lines := strings.Split(string(edited), "\n")
			if node.Line <= len(lines) {
				w.Line, w.Text = node.Line, strings.TrimRight(lines[node.Line-1], "\r")
			}
		}
	}
	return w
}

// lookupNode finds the node at a key path of a parsed document, of any kind.
func lookupNode(doc *yaml.Node, key KeyPath) (*yaml.Node, bool) {
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, false
	}
	cur := resolve(doc.Content[0])
	for _, seg := range key {
		if cur.Kind != yaml.MappingNode {
			return nil, false
		}
		var next *yaml.Node
		for j := 0; j+1 < len(cur.Content); j += 2 {
			if cur.Content[j].Kind == yaml.ScalarNode && cur.Content[j].Value == seg && cur.Content[j].Tag != "!!merge" {
				next = cur.Content[j+1]
			}
		}
		if next == nil {
			return nil, false
		}
		cur = resolve(next)
	}
	return cur, true
}

// jsonText is the JSON of a value for the YAML editor: whole numbers without an
// exponent, no HTML escaping.
func jsonText(v any) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(normalizeNumbers(v)); err != nil {
		return "", err
	}
	return strings.TrimSpace(buf.String()), nil
}

func normalizeNumbers(v any) any {
	switch x := v.(type) {
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1<<53 {
			return json.Number(strconv.FormatInt(int64(x), 10))
		}
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, c := range x {
			out[k] = normalizeNumbers(c)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, c := range x {
			out[i] = normalizeNumbers(c)
		}
		return out
	}
	return v
}

// checkNodeSet verifies an edit of one key: the file parses, the key holds
// exactly want (or does not exist, for nil), and no definition outside the key
// changed.
func checkNodeSet(before []definition, edited []byte, key KeyPath, want any) error {
	after, err := parseValues(edited)
	if err != nil {
		return fmt.Errorf("the edited file does not parse: %w", err)
	}
	outside := func(defs []definition) map[string]string {
		m := map[string]string{}
		for _, d := range defs {
			if d.path.HasPrefix(key) {
				continue
			}
			m[fmt.Sprintf("%s\x00%d", d.path.String(), d.kind)] = d.value
		}
		return m
	}
	b, a := outside(before), outside(after)
	if len(a) != len(b) {
		return errors.New("the edit changed the structure of the file")
	}
	for k, v := range b {
		if got, ok := a[k]; !ok || got != v {
			return fmt.Errorf("the edit changed %s", strings.SplitN(k, "\x00", 2)[0])
		}
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(edited, &doc); err != nil {
		return err
	}
	node, found := lookupNode(&doc, key)
	if want == nil {
		if found {
			return errors.New("the key is still in the edited file")
		}
		return nil
	}
	if !found {
		return errors.New("the key is not in the edited file")
	}
	var got any
	if err := node.Decode(&got); err != nil {
		return err
	}
	if !sameJSON(got, want) {
		return fmt.Errorf("the file holds %s at the key, want %s", jsonOf(got), jsonOf(want))
	}
	return nil
}

func jsonOf(v any) string {
	t, err := jsonText(toJSONValue(v))
	if err != nil {
		return fmt.Sprint(v)
	}
	return t
}

// sameJSON compares two decoded values the way JSON does: YAML decodes whole
// numbers as ints and maps with any key type; JSON as float64 and string keys.
func sameJSON(a, b any) bool {
	return reflect.DeepEqual(toJSONValue(a), toJSONValue(b))
}

func toJSONValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, c := range x {
			out[k] = toJSONValue(c)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(x))
		for k, c := range x {
			out[fmt.Sprint(k)] = toJSONValue(c)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, c := range x {
			out[i] = toJSONValue(c)
		}
		return out
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case uint64:
		return float64(x)
	case json.Number:
		f, _ := x.Float64()
		return f
	}
	return v
}
