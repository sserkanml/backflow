package helmmap

import (
	"encoding/json"
	"fmt"
	"strconv"

	"go.yaml.in/yaml/v3"
)

// nullTag is the YAML tag of null.
const nullTag = "!!null"

// defKind is what a definition puts at its key path.
type defKind int

const (
	defScalar defKind = iota // a string, number or bool
	defList                  // a list; lists replace wholesale when merged
	defNull                  // null: Helm removes the key
)

// definition is one place a values document sets something.
type definition struct {
	path  KeyPath
	kind  defKind
	value string // rendered form of a scalar
	// viaMerge is set for entries that come in through a YAML merge key
	// (<<: *anchor). They are defined, but there is no line of their own to
	// edit.
	viaMerge bool
}

// parseValues returns the definitions of a values document in document order.
// A map is not a definition itself; only what it holds is. An empty file has
// none.
func parseValues(data []byte) ([]definition, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if doc.Kind == 0 { // empty input
		return nil, nil
	}
	root := resolve(doc.Content[0])
	if root.Kind == yaml.ScalarNode && root.Tag == nullTag {
		return nil, nil
	}
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("values must be a map at the top level")
	}
	var defs []definition
	collect(&defs, nil, root, false, 0)
	return defs, nil
}

// maxDepth bounds the walk, so a self-referencing alias cannot loop forever.
const maxDepth = 64

func collect(out *[]definition, path KeyPath, n *yaml.Node, viaMerge bool, depth int) {
	if depth > maxDepth {
		return
	}
	n = resolve(n)
	switch n.Kind {
	case yaml.MappingNode:
		for _, e := range mapEntries(n) {
			collect(out, path.child(e.key), e.value, viaMerge || e.merged, depth+1)
		}
	case yaml.SequenceNode:
		*out = append(*out, definition{path: path, kind: defList, viaMerge: viaMerge})
		for i, item := range n.Content {
			collect(out, path.child(strconv.Itoa(i)), item, viaMerge, depth+1)
		}
	case yaml.ScalarNode:
		if n.Tag == nullTag {
			*out = append(*out, definition{path: path, kind: defNull, viaMerge: viaMerge})
			return
		}
		v, _ := renderScalar(n)
		*out = append(*out, definition{path: path, kind: defScalar, value: v, viaMerge: viaMerge})
	}
}

type mapEntry struct {
	key    string
	value  *yaml.Node
	merged bool
}

// mapEntries lists the entries of a mapping node, with the entries of merge
// keys (<<) added unless the mapping sets the key itself.
func mapEntries(m *yaml.Node) []mapEntry {
	var explicit []mapEntry
	var merged []*yaml.Node
	for i := 0; i+1 < len(m.Content); i += 2 {
		k, v := m.Content[i], m.Content[i+1]
		if k.Tag == "!!merge" {
			switch rv := resolve(v); rv.Kind {
			case yaml.MappingNode:
				merged = append(merged, rv)
			case yaml.SequenceNode:
				for _, item := range rv.Content {
					if rm := resolve(item); rm.Kind == yaml.MappingNode {
						merged = append(merged, rm)
					}
				}
			}
			continue
		}
		explicit = append(explicit, mapEntry{key: k.Value, value: v})
	}
	seen := make(map[string]bool, len(explicit))
	for _, e := range explicit {
		seen[e.key] = true
	}
	out := explicit
	for _, mm := range merged {
		for _, e := range mapEntries(mm) {
			if !seen[e.key] {
				seen[e.key] = true
				out = append(out, mapEntry{key: e.key, value: e.value, merged: true})
			}
		}
	}
	return out
}

func resolve(n *yaml.Node) *yaml.Node {
	for i := 0; n.Kind == yaml.AliasNode && n.Alias != nil && i < maxDepth; i++ {
		n = n.Alias
	}
	return n
}

// renderScalar returns the string form a template prints for a scalar, and
// false for null. Numbers are written in plain decimal form, so a value does
// not depend on how the file spelled it (0x10, 1e3).
func renderScalar(n *yaml.Node) (string, bool) {
	switch n.Tag {
	case nullTag:
		return "", false
	case "!!bool":
		var b bool
		if n.Decode(&b) == nil {
			return strconv.FormatBool(b), true
		}
	case "!!int":
		var v any
		if n.Decode(&v) == nil {
			return RenderValue(v), true
		}
	case "!!float":
		var f float64
		if n.Decode(&f) == nil {
			return RenderValue(f), true
		}
	}
	return n.Value, true
}

// RenderValue returns the string form a template prints for a decoded value:
// 3, 3.0 and "3" all render as 3, true as true. Maps, lists and nil have no
// scalar form and render as the empty string.
func RenderValue(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return strconv.FormatInt(i, 10)
		}
		if f, err := x.Float64(); err == nil {
			return strconv.FormatFloat(f, 'f', -1, 64)
		}
		return x.String()
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(x), 'f', -1, 32)
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return fmt.Sprint(x)
	}
	return ""
}

func (p KeyPath) child(seg string) KeyPath {
	c := make(KeyPath, len(p)+1)
	copy(c, p)
	c[len(p)] = seg
	return c
}
