package mapping

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/sserkanml/backflow/api/v1alpha1"
)

// Apply applies the proposal's changes to one document of a file and returns
// the edited file. The new values are the live values of the changes, because
// the goal is to make Git match the cluster. Everything outside the edited
// entries is returned byte for byte.
//
// Changes are JSON pointers with Add, Replace and Remove:
//   - Replace of a field the file does not have is applied as an Add of the
//     live value: Argo CD's rendered state contains defaulted fields (for
//     example spec.revisionHistoryLimit or an omitted spec.replicas) that
//     the manifest never spelled out.
//   - Add creates the field and any missing intermediate maps; a missing
//     parent that is empty or null becomes a map. Arrays cannot grow.
//   - Remove needs the field to exist and must not be an array item.
//
// Array items are addressed by index. Apply does not check the result; use
// Verify.
func Apply(file string, original []byte, document int, changes []v1alpha1.FieldChange) ([]byte, error) {
	if strings.EqualFold(path.Ext(file), ".json") {
		return nil, fmt.Errorf("%w: editing JSON manifests (%s)", ErrUnsupportedFileFormat, file)
	}
	text := string(original)
	segs := splitDocuments(text)
	if document < 0 || document >= len(segs) {
		return nil, fmt.Errorf("%w: %s has no document %d", ErrCannotApply, file, document)
	}
	seg := segs[document]
	for i, ch := range changes {
		edited, err := applyOne(text, seg, ch)
		if err != nil {
			return nil, fmt.Errorf("change %d (%s %s): %w", i, ch.Op, ch.Path, err)
		}
		seg.end += len(edited) - len(text)
		text = edited
	}
	return []byte(text), nil
}

func cannot(format string, args ...interface{}) error {
	return fmt.Errorf("%w: %s", ErrCannotApply, fmt.Sprintf(format, args...))
}

// step is one level of a walk down a JSON pointer: the child at
// container.Content[idx]. For a mapping idx is the value, idx-1 its key.
type step struct {
	container *yaml.Node
	idx       int
}

func (s step) child() *yaml.Node { return s.container.Content[s.idx] }

// walk follows segs from root as far as the document goes and returns the
// steps taken and the node reached.
func walk(root *yaml.Node, segs []string) (steps []step, reached *yaml.Node) {
	cur := root
	for _, seg := range segs {
		switch cur.Kind {
		case yaml.MappingNode:
			found := false
			for i := 0; i+1 < len(cur.Content); i += 2 {
				if k := cur.Content[i]; k.Kind == yaml.ScalarNode && k.Value == seg {
					steps = append(steps, step{cur, i + 1})
					cur = cur.Content[i+1]
					found = true
					break
				}
			}
			if !found {
				return steps, cur
			}
		case yaml.SequenceNode:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(cur.Content) || strconv.Itoa(i) != seg {
				return steps, cur
			}
			steps = append(steps, step{cur, i})
			cur = cur.Content[i]
		default:
			return steps, cur
		}
	}
	return steps, cur
}

// parsePointer splits an RFC 6901 pointer into unescaped segments.
func parsePointer(p string) ([]string, error) {
	if p == "" || p[0] != '/' {
		return nil, cannot("%q is not a JSON pointer to a field", p)
	}
	segs := strings.Split(p[1:], "/")
	for i, s := range segs {
		segs[i] = strings.ReplaceAll(strings.ReplaceAll(s, "~1", "/"), "~0", "~")
	}
	return segs, nil
}

// edit replaces text[start:end] with repl.
type edit struct {
	start, end int
	repl       string
}

func applyOne(text string, seg segment, ch v1alpha1.FieldChange) (string, error) {
	root, err := parseDocument(text, seg)
	if err != nil {
		return "", cannot("the document cannot be parsed: %v", err)
	}
	if root == nil || root.Kind != yaml.MappingNode {
		return "", cannot("the document is not a mapping")
	}
	segs, err := parsePointer(ch.Path)
	if err != nil {
		return "", err
	}
	li := newLineIndex(parseText(text, seg))

	var e edit
	switch ch.Op {
	case v1alpha1.OpReplace:
		e, err = replaceEdit(li, root, segs, ch.Live)
	case v1alpha1.OpAdd:
		e, err = addEdit(li, root, segs, ch.Live)
	case v1alpha1.OpRemove:
		e, err = removeEdit(li, root, segs)
	default:
		err = cannot("unknown operation %q", ch.Op)
	}
	if err != nil {
		return "", err
	}
	return text[:seg.start+e.start] + e.repl + text[seg.start+e.end:], nil
}

func replaceEdit(li *lineIndex, root *yaml.Node, segs []string, live string) (edit, error) {
	nw, err := jsonNode(live)
	if err != nil {
		return edit{}, cannot("the new value is not valid JSON: %v", err)
	}
	steps, _ := walk(root, segs)
	if len(steps) != len(segs) {
		// Defaulted by Kubernetes or Argo CD, not written in the file.
		return addEdit(li, root, segs, live)
	}
	unit := nearestEntry(steps, len(steps)-1)
	if unit < 0 {
		return edit{}, cannot("the field is not inside a mapping entry")
	}
	// The range is read from the tree as it was parsed: assigning the new
	// value changes what the entry looks like, but not the lines it occupies.
	start, end := entryRange(li, steps, unit)
	last := steps[len(steps)-1]
	last.container.Content[last.idx] = assign(last.child(), nw)
	return renderEntryEdit(li, steps, unit, start, end)
}

func removeEdit(li *lineIndex, root *yaml.Node, segs []string) (edit, error) {
	steps, _ := walk(root, segs)
	if len(steps) != len(segs) {
		return edit{}, cannot("the field does not exist in the file")
	}
	last := steps[len(steps)-1]
	if last.container.Kind != yaml.MappingNode {
		return edit{}, cannot("array items cannot be removed")
	}
	level := len(steps) - 1

	// Simple case: a block mapping with other keys, the key starts its line.
	key := last.container.Content[last.idx-1]
	if len(last.container.Content) > 2 && last.container.Style&yaml.FlowStyle == 0 && startsLine(li, key) {
		_, end := entryRange(li, steps, level)
		return edit{start: li.starts[key.Line-1], end: end}, nil
	}

	// Otherwise take the key out of the tree and re-render the entry that
	// holds the mapping.
	unit := nearestEntry(steps, level-1)
	if unit < 0 {
		return edit{}, cannot("the last field of a document cannot be removed")
	}
	start, end := entryRange(li, steps, unit)
	last.container.Content = append(last.container.Content[:last.idx-1:last.idx-1], last.container.Content[last.idx+1:]...)
	return renderEntryEdit(li, steps, unit, start, end)
}

func addEdit(li *lineIndex, root *yaml.Node, segs []string, live string) (edit, error) {
	nw, err := jsonNode(live)
	if err != nil {
		return edit{}, cannot("the new value is not valid JSON: %v", err)
	}
	steps, parent := walk(root, segs)
	rest := segs[len(steps):]
	if len(rest) == 0 {
		return edit{}, cannot("the field already exists in the file")
	}
	// Missing intermediate maps: rest[0] is the new key, the others nest.
	value := nw
	for i := len(rest) - 1; i >= 1; i-- {
		value = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{strNode(rest[i]), value}}
	}
	key := strNode(rest[0])

	// Where the entry that holds the new field sits in the text, read before
	// the tree is changed.
	unit := nearestEntry(steps, len(steps)-1)
	var start, end int
	if unit >= 0 {
		start, end = entryRange(li, steps, unit)
	}

	if parent.Kind == yaml.ScalarNode && parent.Tag == "!!null" {
		*parent = yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", LineComment: parent.LineComment, Line: parent.Line, Column: parent.Column}
	}
	if parent.Kind != yaml.MappingNode {
		return edit{}, cannot("the parent of the new field is not a mapping")
	}

	// Simple case: append an entry after the last one of a block mapping.
	if len(parent.Content) > 0 && parent.Style&yaml.FlowStyle == 0 {
		pseudo := append(append([]step{}, steps...), step{parent, len(parent.Content) - 1})
		_, end := entryRange(li, pseudo, len(pseudo)-1)
		indent := parent.Content[0].Column - 1
		text, err := renderEntry(key, value, indent, true)
		if err != nil {
			return edit{}, err
		}
		text = matchLineEndings(li, text)
		if end > 0 && li.text[end-1] != '\n' {
			text = "\n" + strings.TrimSuffix(strings.TrimSuffix(text, "\n"), "\r")
		}
		return edit{start: end, end: end, repl: text}, nil
	}

	// Empty or flow mapping: add the pair and re-render the entry holding it.
	if unit < 0 {
		return edit{}, cannot("the new field would be added to an empty document")
	}
	parent.Style &^= yaml.FlowStyle
	parent.Content = append(parent.Content, key, value)
	return renderEntryEdit(li, steps, unit, start, end)
}

// nearestEntry returns the deepest level at or above from whose container is
// a block mapping, or -1. Entries inside a flow collection ({...}, [...]) are
// not lines of their own, so the entry that holds the collection is used.
func nearestEntry(steps []step, from int) int {
	for l := from; l >= 0; l-- {
		if c := steps[l].container; c.Kind == yaml.MappingNode && c.Style&yaml.FlowStyle == 0 {
			return l
		}
	}
	return -1
}

// startsLine reports whether only whitespace precedes the node on its line.
func startsLine(li *lineIndex, n *yaml.Node) bool {
	return strings.TrimSpace(li.text[li.starts[n.Line-1]:li.offset(n.Line, n.Column)]) == ""
}

// renderEntryEdit re-renders the mapping entry at steps[level] from the
// (already modified) tree and returns the edit that swaps it into the text
// range start:end, which was measured before the tree was modified.
func renderEntryEdit(li *lineIndex, steps []step, level, start, end int) (edit, error) {
	s := steps[level]
	key := s.container.Content[s.idx-1]
	text, err := renderEntry(key, s.child(), key.Column-1, false)
	if err != nil {
		return edit{}, err
	}
	text = matchLineEndings(li, text)
	if end > 0 && li.text[end-1] != '\n' {
		text = strings.TrimSuffix(strings.TrimSuffix(text, "\n"), "\r")
	}
	return edit{start: start, end: end, repl: text}, nil
}

// matchLineEndings renders text with CRLF when the document uses it, so an
// edit does not mix line endings.
func matchLineEndings(li *lineIndex, text string) string {
	if strings.Contains(li.text, "\r\n") {
		return strings.ReplaceAll(text, "\n", "\r\n")
	}
	return text
}

// entryRange returns the byte range of the entry at steps[level], from its
// key to the end of its last content line. Comment and blank lines after the
// value belong to what follows and stay out of the range.
func entryRange(li *lineIndex, steps []step, level int) (start, end int) {
	s := steps[level]
	key := s.container.Content[s.idx-1]
	start = li.offset(key.Line, key.Column)

	bound := li.lines() + 1
	for l := level; l >= 0; l-- {
		c := steps[l].container
		if next := steps[l].idx + 1; next < len(c.Content) {
			bound = c.Content[next].Line
			break
		}
	}
	last := bound - 1
	if last > li.lines() {
		last = li.lines()
	}
	// The lines of a literal or folded block scalar at the end of the entry
	// are its value, whatever they look like: a "# ..." line is script text,
	// and with keep chomping (|+) trailing blank lines are part of the value.
	if scalar, indent := trailingBlockScalar(li, key, s.child()); scalar != nil {
		return start, li.lineEnd(blockScalarLastLine(li, scalar, indent, last))
	}
	for last > key.Line {
		t := strings.TrimSpace(li.lineText(last))
		if t != "" && !strings.HasPrefix(t, "#") {
			break
		}
		last--
	}
	return start, li.lineEnd(last)
}

// trailingBlockScalar returns the literal or folded block scalar that ends the
// entry key: value, and the indentation of the line that owns it (its lines
// are indented further), or nil when the entry does not end in one.
func trailingBlockScalar(li *lineIndex, key, value *yaml.Node) (scalar *yaml.Node, indent int) {
	cur, indent := value, key.Column-1
	for len(cur.Content) > 0 && cur.Kind != yaml.AliasNode {
		switch cur.Kind {
		case yaml.MappingNode:
			if cur.Style&yaml.FlowStyle != 0 {
				return nil, 0
			}
			indent = cur.Content[len(cur.Content)-2].Column - 1
			cur = cur.Content[len(cur.Content)-1]
		case yaml.SequenceNode:
			if cur.Style&yaml.FlowStyle != 0 {
				return nil, 0
			}
			cur = cur.Content[len(cur.Content)-1]
			if cur.Line < 1 || cur.Line > li.lines() {
				return nil, 0
			}
			indent = leadingIndent(li.lineText(cur.Line))
		default:
			return nil, 0
		}
	}
	if cur.Kind == yaml.ScalarNode && cur.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0 {
		return cur, indent
	}
	return nil, 0
}

func leadingIndent(line string) int {
	return len(line) - len(strings.TrimLeft(line, " \t"))
}

// blockScalarLastLine returns the last line, up to limit, that belongs to the
// block scalar: blank lines and lines indented more than indent. Trailing
// blank lines count only with keep chomping, where they are part of the value.
func blockScalarLastLine(li *lineIndex, scalar *yaml.Node, indent, limit int) int {
	lastContent, lastAny := scalar.Line, scalar.Line
	for l := scalar.Line + 1; l <= limit; l++ {
		text := li.lineText(l)
		if strings.TrimSpace(text) == "" {
			lastAny = l
			continue
		}
		if leadingIndent(text) <= indent {
			break
		}
		lastContent, lastAny = l, l
	}
	header := li.lineText(scalar.Line)
	if c := scalar.Column - 1; c >= 0 && c < len(header) {
		header = header[c:]
	}
	if i := strings.IndexAny(header, " #"); i >= 0 {
		header = header[:i]
	}
	if strings.Contains(header, "+") {
		return lastAny
	}
	return lastContent
}

// renderEntry renders "key: value" as block YAML. Lines after the first are
// indented by indent spaces; so is the first when indentFirst is set.
// Comments above and below the entry are not part of it.
func renderEntry(key, value *yaml.Node, indent int, indentFirst bool) (string, error) {
	k := *key
	k.HeadComment, k.FootComment = "", ""
	m := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{&k, cloneStripped(value)}}

	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(m); err != nil {
		return "", cannot("rendering the entry: %v", err)
	}
	if err := enc.Close(); err != nil {
		return "", cannot("rendering the entry: %v", err)
	}
	pad := strings.Repeat(" ", indent)
	lines := strings.SplitAfter(b.String(), "\n")
	var out strings.Builder
	for i, l := range lines {
		if l == "" {
			continue
		}
		if (i > 0 || indentFirst) && strings.TrimRight(l, "\r\n") != "" {
			out.WriteString(pad) // blank lines, such as those a |+ value ends with, stay empty
		}
		out.WriteString(l)
	}
	return out.String(), nil
}

// cloneStripped copies a node tree without foot comments; those sit after
// the entry in the file and are not part of the range being replaced.
func cloneStripped(n *yaml.Node) *yaml.Node {
	c := *n
	c.FootComment = ""
	c.Content = make([]*yaml.Node, len(n.Content))
	for i, ch := range n.Content {
		c.Content[i] = cloneStripped(ch)
	}
	return &c
}

// assign puts a new value in the place of old. A scalar replaced by a scalar
// keeps its quoting style and trailing comment, so a quoted string stays
// quoted and the line only changes where it must.
func assign(old, nw *yaml.Node) *yaml.Node {
	if old.Kind == yaml.ScalarNode && nw.Kind == yaml.ScalarNode {
		const quoted = yaml.SingleQuotedStyle | yaml.DoubleQuotedStyle
		const block = yaml.LiteralStyle | yaml.FoldedStyle
		keep := old.Style
		switch {
		case nw.Tag != "!!str":
			keep = 0
		case keep&block != 0 && !strings.Contains(nw.Value, "\n"):
			keep &^= block
		case keep&quoted != 0 && keep&block != 0:
			keep &^= block
		}
		old.Value, old.Tag, old.Style = nw.Value, nw.Tag, keep&^yaml.TaggedStyle
		return old
	}
	nw.LineComment = old.LineComment
	return nw
}

func strNode(s string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s}
}

// jsonNode converts JSON text to a YAML node tree, keeping object key order.
func jsonNode(s string) (*yaml.Node, error) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	n, err := jsonValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("unexpected data after the value")
	}
	return n, nil
}

func jsonValue(dec *json.Decoder) (*yaml.Node, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		if t == '{' {
			m := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				v, err := jsonValue(dec)
				if err != nil {
					return nil, err
				}
				m.Content = append(m.Content, strNode(kt.(string)), v)
			}
			_, err := dec.Token()
			return m, err
		}
		seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		for dec.More() {
			v, err := jsonValue(dec)
			if err != nil {
				return nil, err
			}
			seq.Content = append(seq.Content, v)
		}
		_, err := dec.Token()
		return seq, err
	case string:
		return strNode(t), nil
	case json.Number:
		tag := "!!int"
		if strings.ContainsAny(t.String(), ".eE") {
			tag = "!!float"
		}
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: t.String()}, nil
	case bool:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: strconv.FormatBool(t)}, nil
	case nil:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}, nil
	}
	return nil, fmt.Errorf("unexpected JSON token %v", tok)
}
