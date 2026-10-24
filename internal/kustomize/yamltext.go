package kustomize

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// parseSegments splits a JSON pointer.
func parseSegments(pointer string) []string {
	if pointer == "" {
		return nil
	}
	parts := strings.Split(pointer[1:], "/")
	for i, p := range parts {
		parts[i] = strings.ReplaceAll(strings.ReplaceAll(p, "~1", "/"), "~0", "~")
	}
	return parts
}

func escapeSegment(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}

func pointerOf(segs ...string) string {
	var b strings.Builder
	for _, s := range segs {
		b.WriteByte('/')
		b.WriteString(escapeSegment(s))
	}
	return b.String()
}

// nodeAt follows a JSON pointer in a parsed document.
func nodeAt(root *yaml.Node, pointer string) *yaml.Node {
	n := root
	for _, seg := range parseSegments(pointer) {
		switch {
		case n == nil:
			return nil
		case n.Kind == yaml.MappingNode:
			n = mapValue(n, seg)
		case n.Kind == yaml.SequenceNode:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(n.Content) {
				return nil
			}
			n = n.Content[i]
		default:
			return nil
		}
	}
	return n
}

func mapValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// parseRoot parses one YAML document (or JSON) without file position
// tricks: the first document of text.
func parseRoot(text string) (*yaml.Node, error) {
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(text), &n); err != nil {
		return nil, err
	}
	if n.Kind != yaml.DocumentNode || len(n.Content) == 0 {
		return nil, fmt.Errorf("empty document")
	}
	return n.Content[0], nil
}

// scalarText is the value of a scalar node as the text a comparison uses.
func scalarText(n *yaml.Node) (string, bool) {
	if n == nil || n.Kind != yaml.ScalarNode {
		return "", false
	}
	return n.Value, true
}

// replaceScalar replaces the single-line scalar at a JSON pointer of a
// document in place and returns the new text. It is for documents whose root
// is not a mapping (a JSON 6902 patch is a list), which the mapping editor
// does not take. Comments, indentation and the quoting style are kept.
func replaceScalar(text, pointer, liveJSON string) (string, error) {
	root, err := parseRoot(text)
	if err != nil {
		return "", err
	}
	n := nodeAt(root, pointer)
	if n == nil || n.Kind != yaml.ScalarNode {
		return "", fmt.Errorf("%s is not a scalar", pointer)
	}
	if n.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0 {
		return "", fmt.Errorf("%s is a block scalar", pointer)
	}
	lines := strings.Split(text, "\n")
	if n.Line < 1 || n.Line > len(lines) {
		return "", fmt.Errorf("position of %s is unknown", pointer)
	}
	line := []rune(lines[n.Line-1])
	start := n.Column - 1
	end, ok := scalarEnd(line, start, n.Style)
	if !ok {
		return "", fmt.Errorf("%s spans several lines", pointer)
	}
	repl, err := scalarSource(liveJSON, n.Style)
	if err != nil {
		return "", err
	}
	lines[n.Line-1] = string(line[:start]) + repl + string(line[end:])
	return strings.Join(lines, "\n"), nil
}

// scalarEnd finds the end (exclusive, in runes) of the scalar starting at start.
func scalarEnd(line []rune, start int, style yaml.Style) (int, bool) {
	if start < 0 || start >= len(line) {
		return 0, false
	}
	switch {
	case style&yaml.DoubleQuotedStyle != 0:
		for i := start + 1; i < len(line); i++ {
			switch line[i] {
			case '\\':
				i++
			case '"':
				return i + 1, true
			}
		}
		return 0, false
	case style&yaml.SingleQuotedStyle != 0:
		for i := start + 1; i < len(line); i++ {
			if line[i] == '\'' {
				if i+1 < len(line) && line[i+1] == '\'' {
					i++
					continue
				}
				return i + 1, true
			}
		}
		return 0, false
	}
	// A plain scalar ends at a comment (" #") or the end of the line, without
	// the spaces before it. A flow collection also ends it at , ] or }.
	end := len(line)
	for i := start; i < len(line); i++ {
		if line[i] == '#' && i > start && (line[i-1] == ' ' || line[i-1] == '\t') {
			end = i
			break
		}
	}
	for end > start && (line[end-1] == ' ' || line[end-1] == '\t' || line[end-1] == '\r') {
		end--
	}
	return end, true
}

// scalarSource renders a JSON scalar as YAML source in the style of the
// scalar it replaces.
func scalarSource(liveJSON string, style yaml.Style) (string, error) {
	var v any
	if err := json.Unmarshal([]byte(liveJSON), &v); err != nil {
		return "", err
	}
	switch x := v.(type) {
	case string:
		switch {
		case style&yaml.SingleQuotedStyle != 0 && !strings.ContainsAny(x, "\n\\"):
			return "'" + strings.ReplaceAll(x, "'", "''") + "'", nil
		case style&yaml.DoubleQuotedStyle != 0 || !plainSafe(x):
			b, _ := json.Marshal(x)
			return string(b), nil
		}
		return x, nil
	case map[string]any, []any:
		return "", fmt.Errorf("a scalar is replaced by a collection")
	case nil:
		return textNull, nil
	}
	return liveJSON, nil
}

// plainSafe reports whether a string reads back unchanged as a plain scalar.
func plainSafe(s string) bool {
	if s == "" || strings.TrimSpace(s) != s || strings.ContainsAny(s, ":#\n\t\"'`{}[],&*!|>%@\\") && !simpleColon(s) {
		return false
	}
	if strings.HasPrefix(s, "-") || strings.HasPrefix(s, "?") {
		return false
	}
	var back any
	if err := yaml.Unmarshal([]byte(s), &back); err != nil {
		return false
	}
	got, ok := back.(string)
	if !ok || got != s {
		return false
	}
	switch strings.ToLower(s) {
	case "y", "n", "yes", "no", "on", "off", textTrue, "false", textNull, "~":
		return false
	}
	return !looksNumeric(s)
}

// simpleColon allows a colon inside a word (a registry port, an image tag).
func simpleColon(s string) bool {
	if strings.ContainsAny(s, "#\n\t\"'`{}[],&*!|>%@\\") {
		return false
	}
	return !strings.Contains(s, ": ") && !strings.HasSuffix(s, ":")
}

func looksNumeric(s string) bool {
	t := strings.NewReplacer("_", "", ":", "").Replace(s)
	if _, err := strconv.ParseFloat(t, 64); err == nil {
		return true
	}
	if _, err := strconv.ParseInt(t, 0, 64); err == nil {
		return true
	}
	return false
}

// compactJSON writes a parsed JSON document as compact JSON, keeping the order
// of keys.
func compactJSON(n *yaml.Node) (string, error) {
	var b strings.Builder
	if err := writeJSON(&b, n); err != nil {
		return "", err
	}
	return b.String(), nil
}

func writeJSON(b *strings.Builder, n *yaml.Node) error {
	switch n.Kind {
	case yaml.MappingNode:
		b.WriteByte('{')
		for i := 0; i+1 < len(n.Content); i += 2 {
			if i > 0 {
				b.WriteByte(',')
			}
			k, _ := json.Marshal(n.Content[i].Value)
			b.Write(k)
			b.WriteByte(':')
			if err := writeJSON(b, n.Content[i+1]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	case yaml.SequenceNode:
		b.WriteByte('[')
		for i, c := range n.Content {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writeJSON(b, c); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case yaml.ScalarNode:
		switch n.Tag {
		case "!!int", "!!float", "!!bool":
			b.WriteString(n.Value)
		case "!!null":
			b.WriteString(textNull)
		default:
			s, _ := json.Marshal(n.Value)
			b.Write(s)
		}
	default:
		return fmt.Errorf("unsupported YAML node in JSON")
	}
	return nil
}

// setScalar sets a scalar node to a JSON scalar.
func setScalar(n *yaml.Node, liveJSON string) error {
	var v any
	if err := json.Unmarshal([]byte(liveJSON), &v); err != nil {
		return err
	}
	n.Style = 0
	switch x := v.(type) {
	case string:
		n.Tag, n.Value = "!!str", x
	case bool:
		n.Tag, n.Value = "!!bool", strconv.FormatBool(x)
	case float64:
		n.Value = liveJSON
		n.Tag = "!!float"
		if !strings.ContainsAny(liveJSON, ".eE") {
			n.Tag = "!!int"
		}
	case nil:
		n.Tag, n.Value = "!!null", textNull
	default:
		return fmt.Errorf("a scalar is replaced by a collection")
	}
	return nil
}
