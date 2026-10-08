package mapping

import (
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
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
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
