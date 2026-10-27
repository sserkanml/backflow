// Package jsonedit edits JSON documents in place: the bytes of everything an
// edit does not touch stay as they are (indentation, key order, number text,
// string escapes, line endings, the final newline). It is the JSON counterpart
// of the YAML editor of package mapping, for manifests, patches and value files
// that are JSON.
//
// Edits are JSON pointer operations, Replace, Add and Remove, with the value as
// JSON text. After each edit the result is parsed and compared with the object
// the edit was meant to give; a difference is an error, never a written file.
package jsonedit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"reflect"
	"strconv"
	"strings"
)

// Errors of editing. Each means the document is not changed.
var (
	// ErrInvalid: the document or the value is not JSON this package edits.
	ErrInvalid = errors.New("jsonedit: invalid JSON")
	// ErrAmbiguous: a key on the path of the edit is defined twice in an object.
	ErrAmbiguous = errors.New("jsonedit: a key on the path is defined twice")
	// ErrNotFound: the field to remove or the parent to add to is not there.
	ErrNotFound = errors.New("jsonedit: no such field")
	// ErrCannotApply: the edit does not fit the document (a member of a scalar,
	// an index out of range).
	ErrCannotApply = errors.New("jsonedit: the change cannot be applied")
)

// Kind is the kind of an edit.
type Kind int

// The kinds of edits.
const (
	// Replace sets the value at the pointer; a field the document does not have is
	// added.
	Replace Kind = iota
	// Add sets the value at the pointer, creating the maps on the way; "-" or the
	// length appends to an array.
	Add
	// Remove deletes the member or the element at the pointer.
	Remove
)

// Op is one edit.
type Op struct {
	Kind    Kind
	Pointer string
	// Value is the JSON text of the new value; unused by Remove.
	Value string
}

// Apply applies the edits in order and returns the new document.
func Apply(src []byte, ops ...Op) ([]byte, error) {
	cur := src
	for i, op := range ops {
		next, err := applyOne(cur, op)
		if err != nil {
			return nil, fmt.Errorf("edit %d (%s): %w", i, op.Pointer, err)
		}
		cur = next
	}
	return cur, nil
}

// path is where a pointer leads in a document: the nodes down to the deepest one
// that exists, and the segments that do not exist.
type path struct {
	chain   []*node  // root first
	members []int    // members[i] is the index of chain[i+1] in chain[i]
	missing []string // segments below the deepest existing node
}

func (p *path) deepest() *node { return p.chain[len(p.chain)-1] }

func (p *path) parent() *node {
	if len(p.chain) < 2 {
		return nil
	}
	return p.chain[len(p.chain)-2]
}

// walk follows the segments as far as the document goes.
func walk(root *node, segs []string) (*path, error) {
	p := &path{chain: []*node{root}}
	for i, seg := range segs {
		n := p.deepest()
		switch n.kind {
		case kObject:
			idx := -1
			for j, m := range n.members {
				if m.key == seg {
					if idx >= 0 {
						return nil, fmt.Errorf("%w: %q", ErrAmbiguous, seg)
					}
					idx = j
				}
			}
			if idx < 0 {
				p.missing = segs[i:]
				return p, nil
			}
			p.chain = append(p.chain, n.members[idx].val)
			p.members = append(p.members, idx)
		case kArray:
			idx, err := strconv.Atoi(seg)
			if seg == "-" || (err == nil && idx == len(n.elems)) {
				p.missing = segs[i:]
				return p, nil
			}
			if err != nil || idx < 0 || idx >= len(n.elems) {
				return nil, fmt.Errorf("%w: no element %q", ErrCannotApply, seg)
			}
			p.chain = append(p.chain, n.elems[idx])
			p.members = append(p.members, idx)
		default:
			p.missing = segs[i:]
			return p, nil
		}
	}
	return p, nil
}

func applyOne(src []byte, op Op) ([]byte, error) {
	root, err := parseDocument(src)
	if err != nil {
		return nil, err
	}
	segs, err := segments(op.Pointer)
	if err != nil {
		return nil, err
	}
	var val *node
	var valSrc []byte
	if op.Kind != Remove {
		valSrc = []byte(op.Value)
		if val, err = parseDocument(valSrc); err != nil {
			return nil, fmt.Errorf("the value: %w", err)
		}
	}
	p, err := walk(root, segs)
	if err != nil {
		return nil, err
	}
	e := &editor{src: src, root: root, valSrc: valSrc}
	var out []byte
	switch op.Kind {
	case Remove:
		out, err = e.remove(p)
	default:
		out, err = e.set(p, val)
	}
	if err != nil {
		return nil, err
	}
	if err := check(src, out, op, segs); err != nil {
		return nil, err
	}
	return out, nil
}

// splice returns src with src[from:to] replaced.
func splice(src []byte, from, to int, text string) []byte {
	out := make([]byte, 0, len(src)-(to-from)+len(text))
	out = append(out, src[:from]...)
	out = append(out, text...)
	return append(out, src[to:]...)
}

// editor holds the document an edit works on.
type editor struct {
	src    []byte
	root   *node
	valSrc []byte
}

// ---- style ------------------------------------------------------------------

func (e *editor) newline() string {
	if bytes.Contains(e.src, []byte("\r\n")) {
		return "\r\n"
	}
	return "\n"
}

// unit is one level of indentation: the leading whitespace of the first indented
// line of the document.
func (e *editor) unit() string {
	for i := 0; i < len(e.src); i++ {
		if e.src[i] != '\n' {
			continue
		}
		j := i + 1
		for j < len(e.src) && (e.src[j] == ' ' || e.src[j] == '\t') {
			j++
		}
		if j > i+1 && j < len(e.src) && e.src[j] != '\n' && e.src[j] != '\r' {
			return string(e.src[i+1 : j])
		}
	}
	return "  "
}

// lineIndent is the leading whitespace of the line that holds offset.
func (e *editor) lineIndent(offset int) string {
	start := bytes.LastIndexByte(e.src[:offset], '\n') + 1
	end := start
	for end < len(e.src) && (e.src[end] == ' ' || e.src[end] == '\t') {
		end++
	}
	return string(e.src[start:end])
}

// kvSep is the text between a key and its value: of the object, or else of the
// first member found in the document.
func (e *editor) kvSep(o *node) string {
	var find func(n *node) (string, bool)
	find = func(n *node) (string, bool) {
		if n.kind == kObject {
			for _, m := range n.members {
				return string(e.src[m.keyEnd:m.val.start]), true
			}
		}
		for _, m := range n.members {
			if s, ok := find(m.val); ok {
				return s, true
			}
		}
		for _, el := range n.elems {
			if s, ok := find(el); ok {
				return s, true
			}
		}
		return "", false
	}
	if s, ok := find(o); ok {
		return s
	}
	if s, ok := find(e.root); ok {
		return s
	}
	// No object to copy from: a one-line document is compact.
	if bytes.IndexByte(e.src, '\n') < 0 {
		return ":"
	}
	return ": "
}

func (e *editor) spans(n *node) bool { return bytes.IndexByte(e.src[n.start:n.end], '\n') >= 0 }

// wsBefore is the whitespace that ends right before offset.
func (e *editor) wsBefore(offset int) string {
	i := offset
	for i > 0 && isSpace(e.src[i-1]) {
		i--
	}
	return string(e.src[i:offset])
}

// render writes a value parsed from valSrc in the style: multi-line with the
// indentation unit at indent, or on one line.
func (e *editor) render(v *node, valSrc []byte, indent string, multi bool, kv string) string {
	var b strings.Builder
	e.write(&b, v, valSrc, indent, multi, kv, e.unit(), e.newline())
	return b.String()
}

func (e *editor) write(b *strings.Builder, v *node, valSrc []byte, indent string, multi bool, kv, unit, nl string) {
	switch v.kind {
	case kObject:
		if len(v.members) == 0 {
			b.WriteString("{}")
			return
		}
		b.WriteByte('{')
		for i, m := range v.members {
			e.sep(b, i, indent, multi, unit, nl, kv)
			b.Write(valSrc[m.keyStart:m.keyEnd])
			b.WriteString(kv)
			e.write(b, m.val, valSrc, indent+unit, multi, kv, unit, nl)
		}
		e.closing(b, indent, multi, nl)
		b.WriteByte('}')
	case kArray:
		if len(v.elems) == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteByte('[')
		for i, el := range v.elems {
			e.sep(b, i, indent, multi, unit, nl, kv)
			e.write(b, el, valSrc, indent+unit, multi, kv, unit, nl)
		}
		e.closing(b, indent, multi, nl)
		b.WriteByte(']')
	default:
		b.Write(valSrc[v.start:v.end])
	}
}

func (e *editor) sep(b *strings.Builder, i int, indent string, multi bool, unit, nl, kv string) {
	if i > 0 {
		b.WriteByte(',')
		if !multi && strings.HasSuffix(kv, " ") {
			b.WriteByte(' ')
		}
	}
	if multi {
		b.WriteString(nl + indent + unit)
	}
}

func (e *editor) closing(b *strings.Builder, indent string, multi bool, nl string) {
	if multi {
		b.WriteString(nl + indent)
	}
}

// ---- set --------------------------------------------------------------------

// set replaces the value at the path, or adds it.
func (e *editor) set(p *path, val *node) ([]byte, error) {
	if len(p.missing) == 0 {
		return e.replace(p, val)
	}
	n := p.deepest()
	if n.kind == kNull || (n.kind != kObject && n.kind != kArray) {
		if n.kind != kNull {
			return nil, fmt.Errorf("%w: %s is not a map or a list", ErrCannotApply, describe(n.kind))
		}
		// A null parent becomes a map.
		nest := e.nest(p.missing, val)
		return splice(e.src, n.start, n.end, e.renderNested(nest, p.parent(), n)), nil
	}
	if n.kind == kArray {
		if len(p.missing) == 1 {
			return e.appendElement(n, p.parent(), val)
		}
		return nil, fmt.Errorf("%w: the element %s is not there", ErrCannotApply, p.missing[0])
	}
	return e.addMember(n, p.parent(), p.missing, val)
}

func describe(k kind) string {
	return [...]string{"an object", "an array", "a string", "a number", "a boolean", "null"}[k]
}

// nested is a value under a chain of new keys.
type nested struct {
	keys []string
	val  *node
}

func (e *editor) nest(keys []string, val *node) nested { return nested{keys: keys, val: val} }

// renderNested writes {"k1": {"k2": val}} in place of n, in the style of its parent.
func (e *editor) renderNested(nst nested, parent, at *node) string {
	multi := parent != nil && e.spans(parent) || parent == nil && bytes.IndexByte(e.src, '\n') >= 0
	return e.renderKeys(nst, e.lineIndent(at.start), multi, e.kvSep(e.root))
}

func (e *editor) renderKeys(nst nested, indent string, multi bool, kv string) string {
	unit, nl := e.unit(), e.newline()
	var b strings.Builder
	level := indent
	for _, k := range nst.keys {
		b.WriteByte('{')
		if multi {
			b.WriteString(nl + level + unit)
		}
		b.WriteString(quote(k) + kv)
		level += unit
	}
	e.write(&b, nst.val, e.valSrc, level, multi, kv, unit, nl)
	for range nst.keys {
		level = strings.TrimSuffix(level, unit)
		if multi {
			b.WriteString(nl + level)
		}
		b.WriteByte('}')
	}
	return b.String()
}

// replace puts the value in place of the node at the end of the path.
func (e *editor) replace(p *path, val *node) ([]byte, error) {
	n := p.deepest()
	if len(p.chain) == 1 {
		// The whole document.
		return splice(e.src, n.start, n.end, e.render(val, e.valSrc, "", e.rootMulti(val), e.kvSep(e.root))), nil
	}
	parent := p.parent()
	if n.kind == kString && val.kind == kString {
		var old, nw string
		if unmarshalString(e.src[n.start:n.end], &old) == nil && unmarshalString(e.valSrc[val.start:val.end], &nw) == nil {
			if old == nw {
				return append([]byte(nil), e.src...), nil
			}
			return splice(e.src, n.start, n.end, quote(nw)), nil
		}
	}
	if n.kind == kNumber && val.kind == kNumber && sameNumber(string(e.src[n.start:n.end]), string(e.valSrc[val.start:val.end])) {
		return append([]byte(nil), e.src...), nil
	}
	multi := e.spans(parent)
	if (n.kind == kObject || n.kind == kArray) && (val.kind == kObject || val.kind == kArray) {
		multi = e.spans(n) // keep the style of what is replaced
	}
	text := e.render(val, e.valSrc, e.lineIndent(n.start), multi, e.kvSep(parent))
	return splice(e.src, n.start, n.end, text), nil
}

func (e *editor) rootMulti(val *node) bool {
	return bytes.IndexByte(e.src, '\n') >= 0 && (val.kind == kObject || val.kind == kArray)
}

// addMember adds the key (and the maps under it) to the object o.
func (e *editor) addMember(o, parent *node, keys []string, val *node) ([]byte, error) {
	kv := e.kvSep(o)
	unit, nl := e.unit(), e.newline()
	if len(o.members) == 0 {
		multi := parent != nil && e.spans(parent) || (parent == nil && bytes.IndexByte(e.src, '\n') >= 0)
		if e.spans(o) {
			multi = true
		}
		indent := e.lineIndent(o.start)
		var b strings.Builder
		b.WriteByte('{')
		if multi {
			b.WriteString(nl + indent + unit)
		}
		b.WriteString(quote(keys[0]) + kv)
		b.WriteString(e.valueUnder(keys[1:], val, indent+unit, multi, kv))
		if multi {
			b.WriteString(nl + indent)
		}
		b.WriteByte('}')
		return splice(e.src, o.start, o.end, b.String()), nil
	}
	last := o.members[len(o.members)-1]
	lead := e.wsBefore(last.keyStart)
	multi := e.spans(o)
	indent := lead
	if i := strings.LastIndexAny(lead, "\n"); i >= 0 {
		indent = lead[i+1:]
	}
	if !multi {
		lead = ""
		if len(o.members) > 1 {
			lead = e.wsBefore(last.keyStart)
		} else if strings.HasSuffix(kv, " ") {
			lead = " "
		}
	}
	text := "," + lead + quote(keys[0]) + kv + e.valueUnder(keys[1:], val, indent, multi, kv)
	return splice(e.src, last.val.end, last.val.end, text), nil
}

// valueUnder writes val below a chain of new keys.
func (e *editor) valueUnder(keys []string, val *node, indent string, multi bool, kv string) string {
	if len(keys) == 0 {
		return e.render(val, e.valSrc, indent, multi, kv)
	}
	return e.renderKeys(nested{keys: keys, val: val}, indent, multi, kv)
}

// appendElement adds the value at the end of the array a.
func (e *editor) appendElement(a, parent *node, val *node) ([]byte, error) {
	kv := e.kvSep(e.root)
	unit, nl := e.unit(), e.newline()
	if len(a.elems) == 0 {
		multi := parent != nil && e.spans(parent) || (parent == nil && bytes.IndexByte(e.src, '\n') >= 0)
		if e.spans(a) {
			multi = true
		}
		indent := e.lineIndent(a.start)
		var b strings.Builder
		b.WriteByte('[')
		if multi {
			b.WriteString(nl + indent + unit)
		}
		b.WriteString(e.render(val, e.valSrc, indent+unit, multi, kv))
		if multi {
			b.WriteString(nl + indent)
		}
		b.WriteByte(']')
		return splice(e.src, a.start, a.end, b.String()), nil
	}
	last := a.elems[len(a.elems)-1]
	lead := e.wsBefore(last.start)
	multi := e.spans(a)
	indent := lead
	if i := strings.LastIndexAny(lead, "\n"); i >= 0 {
		indent = lead[i+1:]
	}
	if !multi && len(a.elems) == 1 {
		lead = ""
		if strings.HasSuffix(kv, " ") {
			lead = " "
		}
	}
	text := "," + lead + e.render(val, e.valSrc, indent, multi, kv)
	return splice(e.src, last.end, last.end, text), nil
}

// ---- remove -----------------------------------------------------------------

func (e *editor) remove(p *path) ([]byte, error) {
	if len(p.missing) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, strings.Join(p.missing, "/"))
	}
	parent := p.parent()
	if parent == nil {
		return nil, fmt.Errorf("%w: the whole document cannot be removed", ErrCannotApply)
	}
	idx := p.members[len(p.members)-1]
	var count int
	var start, end func(i int) int
	switch parent.kind {
	case kObject:
		count = len(parent.members)
		start = func(i int) int { return parent.members[i].keyStart }
		end = func(i int) int { return parent.members[i].val.end }
	case kArray:
		count = len(parent.elems)
		start = func(i int) int { return parent.elems[i].start }
		end = func(i int) int { return parent.elems[i].end }
	default:
		return nil, fmt.Errorf("%w: the parent is not a map or a list", ErrCannotApply)
	}
	switch {
	case count == 1:
		open, close := "{", "}"
		if parent.kind == kArray {
			open, close = "[", "]"
		}
		return splice(e.src, parent.start, parent.end, open+close), nil
	case idx == 0:
		return splice(e.src, start(0), start(1), ""), nil
	default:
		return splice(e.src, end(idx-1), end(idx), ""), nil
	}
}

// ---- helpers ----------------------------------------------------------------

// quote encodes a string as a JSON literal, keeping non-ASCII characters and
// the characters HTML would escape as they are.
func quote(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return strings.TrimSuffix(buf.String(), "\n")
}

func unmarshalString(lit []byte, s *string) error { return json.Unmarshal(lit, s) }

// sameNumber reports whether two number literals are the same number.
func sameNumber(a, b string) bool {
	x, ok1 := new(big.Rat).SetString(a)
	y, ok2 := new(big.Rat).SetString(b)
	return ok1 && ok2 && x.Cmp(y) == 0
}

// ---- the check --------------------------------------------------------------

// check parses the result and compares it with the object the edit was meant to
// give: the original with the same operation applied to its decoded form.
func check(before, after []byte, op Op, segs []string) error {
	got, err := decode(after)
	if err != nil {
		return fmt.Errorf("%w: the result does not parse: %v", ErrInvalid, err)
	}
	want, err := decode(before)
	if err != nil {
		return err
	}
	var val any
	if op.Kind != Remove {
		if val, err = decode([]byte(op.Value)); err != nil {
			return err
		}
	}
	want, err = applyGeneric(want, segs, op.Kind, val)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(canon(got), canon(want)) {
		return fmt.Errorf("%w: the edited document is not the intended one", ErrCannotApply)
	}
	return nil
}

func decode(b []byte) (any, error) {
	b = bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF})
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// canon makes numbers comparable by value.
func canon(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, c := range x {
			out[k] = canon(c)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, c := range x {
			out[i] = canon(c)
		}
		return out
	case json.Number:
		if r, ok := new(big.Rat).SetString(string(x)); ok {
			return "n:" + r.String()
		}
	}
	return v
}

func applyGeneric(root any, segs []string, kind Kind, val any) (any, error) {
	if len(segs) == 0 {
		if kind == Remove {
			return nil, fmt.Errorf("%w: the whole document cannot be removed", ErrCannotApply)
		}
		return val, nil
	}
	seg, rest := segs[0], segs[1:]
	switch x := root.(type) {
	case map[string]any:
		child, has := x[seg]
		if !has && kind == Remove {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, seg)
		}
		if len(rest) == 0 && kind == Remove {
			delete(x, seg)
			return x, nil
		}
		next, err := applyGeneric(child, rest, kind, val)
		if err != nil {
			return nil, err
		}
		x[seg] = next
		return x, nil
	case []any:
		idx, err := strconv.Atoi(seg)
		if seg == "-" {
			idx, err = len(x), nil
		}
		if err != nil || idx < 0 || idx > len(x) || (idx == len(x) && kind == Remove) {
			return nil, fmt.Errorf("%w: no element %q", ErrCannotApply, seg)
		}
		if len(rest) == 0 && kind == Remove {
			return append(x[:idx:idx], x[idx+1:]...), nil
		}
		var child any
		if idx < len(x) {
			child = x[idx]
		}
		next, err := applyGeneric(child, rest, kind, val)
		if err != nil {
			return nil, err
		}
		if idx == len(x) {
			return append(x, next), nil
		}
		x[idx] = next
		return x, nil
	case nil:
		if kind == Remove {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, seg)
		}
		next, err := applyGeneric(nil, rest, kind, val)
		if err != nil {
			return nil, err
		}
		return map[string]any{seg: next}, nil
	}
	return nil, fmt.Errorf("%w: %s is not a map or a list", ErrCannotApply, seg)
}
