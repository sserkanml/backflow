package helmmap

import (
	"fmt"
	"strconv"

	"github.com/sserkanml/backflow/internal/rendermap"

	"go.yaml.in/yaml/v3"
)

// nullTag is the YAML tag of null, mergeTag that of a merge key (<<).
const (
	nullTag  = "!!null"
	mergeTag = "!!merge"
)

// defKind is what a definition puts at its key path.
type defKind int

const (
	defScalar defKind = iota // a string, number or bool
	defList                  // a list; lists replace wholesale when merged
	defNull                  // null: Helm removes the key
)

// definition is one place a values document sets something.
type definition struct {
	path  rendermap.KeyPath
	kind  defKind
	value string // rendered form of a scalar
	// viaMerge is set for entries that come in through a YAML merge key
	// (<<: *anchor). They are defined, but there is no line of their own to
	// edit.
	viaMerge bool
	// tag is the YAML tag of a scalar (!!str, !!int, ...).
	tag string
	// inList is set when the path goes through a list item. Such a key cannot
	// be set on its own: a list is replaced as a whole.
	inList bool
	// local is the path in the file the definition is read from, when path was
	// moved into the namespace of a parent chart (dependency defaults).
	local rendermap.KeyPath
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
	collect(&defs, nil, root, false, false, 0)
	return defs, nil
}

// maxDepth bounds the walk, so a self-referencing alias cannot loop forever.
const maxDepth = 64

func collect(out *[]definition, path rendermap.KeyPath, n *yaml.Node, viaMerge, inList bool, depth int) {
	if depth > maxDepth {
		return
	}
	n = resolve(n)
	switch n.Kind {
	case yaml.MappingNode:
		for _, e := range mapEntries(n) {
			collect(out, path.Child(e.key), e.value, viaMerge || e.merged, inList, depth+1)
		}
	case yaml.SequenceNode:
		*out = append(*out, definition{path: path, kind: defList, viaMerge: viaMerge, inList: inList})
		for i, item := range n.Content {
			collect(out, path.Child(strconv.Itoa(i)), item, viaMerge, true, depth+1)
		}
	case yaml.ScalarNode:
		if n.Tag == nullTag {
			*out = append(*out, definition{path: path, kind: defNull, viaMerge: viaMerge, inList: inList})
			return
		}
		v, _ := renderScalar(n)
		*out = append(*out, definition{path: path, kind: defScalar, value: v, viaMerge: viaMerge, tag: n.Tag, inList: inList})
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
		if k.Tag == mergeTag {
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
			return rendermap.RenderValue(v), true
		}
	case "!!float":
		var f float64
		if n.Decode(&f) == nil {
			return rendermap.RenderValue(f), true
		}
	}
	return n.Value, true
}
