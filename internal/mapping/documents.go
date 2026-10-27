package mapping

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

// segment is one YAML document of a file: text[start:end]. A segment that
// begins with a "---" line includes it.
type segment struct{ start, end int }

func isSeparator(line string) bool {
	if !strings.HasPrefix(line, "---") {
		return false
	}
	return len(line) == 3 || line[3] == ' ' || line[3] == '\t' || line[3] == '\r'
}

// splitDocuments splits text at "---" lines. The segments cover the whole text.
func splitDocuments(text string) []segment {
	var segs []segment
	start := 0
	for pos := 0; pos < len(text); {
		end := len(text)
		next := len(text)
		if nl := strings.IndexByte(text[pos:], '\n'); nl >= 0 {
			end = pos + nl
			next = pos + nl + 1
		}
		if isSeparator(text[pos:end]) && pos > start {
			segs = append(segs, segment{start, pos})
			start = pos
		}
		pos = next
	}
	if len(text) > start {
		segs = append(segs, segment{start, len(text)})
	}
	return segs
}

// parseText returns the text of a segment as it is parsed: a leading "---" is
// blanked out with spaces, so line and column numbers match the file.
func parseText(text string, seg segment) string {
	s := text[seg.start:seg.end]
	if isSeparator(firstLine(s)) {
		return "   " + s[3:]
	}
	return s
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// parseDocument parses a segment. root is nil for an empty document.
func parseDocument(text string, seg segment) (root *yaml.Node, err error) {
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(parseText(text, seg)), &n); err != nil {
		return nil, err
	}
	if n.Kind != yaml.DocumentNode || len(n.Content) == 0 {
		return nil, nil
	}
	return n.Content[0], nil
}

// mapGet returns the value for key in a mapping node.
func mapGet(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Kind == yaml.ScalarNode && m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func scalarOf(n *yaml.Node) string {
	if n == nil || n.Kind != yaml.ScalarNode {
		return ""
	}
	return n.Value
}

// lineIndex maps line/column positions of a parsed text to byte offsets.
type lineIndex struct {
	text   string
	starts []int // byte offset of each line
}

func newLineIndex(text string) *lineIndex {
	li := &lineIndex{text: text, starts: []int{0}}
	for i := 0; i < len(text); i++ {
		if text[i] == '\n' && i+1 < len(text) {
			li.starts = append(li.starts, i+1)
		}
	}
	return li
}

// lines is the number of lines; a final newline does not start a new line.
func (li *lineIndex) lines() int { return len(li.starts) }

// lineText returns the 1-based line without its line ending.
func (li *lineIndex) lineText(line int) string {
	s := li.text[li.starts[line-1]:li.lineEnd(line)]
	return strings.TrimRight(s, "\r\n")
}

// lineEnd is the offset just after the line's newline, or the end of the text.
func (li *lineIndex) lineEnd(line int) int {
	if line < len(li.starts) {
		return li.starts[line]
	}
	return len(li.text)
}

// offset converts a 1-based line and rune column to a byte offset.
func (li *lineIndex) offset(line, col int) int {
	off := li.starts[line-1]
	for i := 1; i < col && off < len(li.text); i++ {
		_, size := utf8.DecodeRuneInString(li.text[off:])
		off += size
	}
	return off
}

// DocumentInfo describes one YAML document of a file, in the order Apply
// numbers them.
type DocumentInfo struct {
	Index            int
	APIVersion, Kind string
	Namespace, Name  string
	// Root is the parsed document; nil for an empty one. Its positions are
	// those of the file.
	Root *yaml.Node
}

// Documents parses the documents of a file. A document that does not parse is
// an error.
func Documents(content []byte) ([]DocumentInfo, error) {
	text := string(content)
	var out []DocumentInfo
	for i, seg := range splitDocuments(text) {
		root, err := parseDocument(text, seg)
		if err != nil {
			return nil, fmt.Errorf("document %d: %w", i, err)
		}
		d := DocumentInfo{Index: i, Root: root}
		if root != nil && root.Kind == yaml.MappingNode {
			meta := mapGet(root, "metadata")
			d.APIVersion, d.Kind = scalarOf(mapGet(root, "apiVersion")), scalarOf(mapGet(root, "kind"))
			d.Name, d.Namespace = scalarOf(mapGet(meta, "name")), scalarOf(mapGet(meta, "namespace"))
		}
		out = append(out, d)
	}
	return out, nil
}

// DocumentsOf is Documents for the file with the given name: a file Argo CD reads
// as JSON has one document per item when it is a list (an object with an "items"
// array), and one otherwise (see jsondocs.go). The nodes are read with the YAML
// parser, which JSON is a subset of, so their positions are not those of the file.
func DocumentsOf(file string, content []byte) ([]DocumentInfo, error) {
	if !isJSONFile(file) {
		return Documents(content)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(content, &doc); err != nil {
		return nil, err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, nil
	}
	root := doc.Content[0]
	nodes := []*yaml.Node{root}
	if items := mapGet(root, "items"); items != nil {
		switch {
		case items.Kind == yaml.SequenceNode:
			nodes = items.Content
		case items.Kind == yaml.ScalarNode && items.Tag == nullTag:
			nodes = nil
		}
	}
	out := make([]DocumentInfo, len(nodes))
	for i, n := range nodes {
		d := DocumentInfo{Index: i, Root: n}
		if n.Kind == yaml.MappingNode {
			meta := mapGet(n, "metadata")
			d.APIVersion, d.Kind = scalarOf(mapGet(n, "apiVersion")), scalarOf(mapGet(n, "kind"))
			d.Name, d.Namespace = scalarOf(mapGet(meta, "name")), scalarOf(mapGet(meta, "namespace"))
		}
		out[i] = d
	}
	return out, nil
}
