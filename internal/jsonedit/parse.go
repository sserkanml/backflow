package jsonedit

import (
	"bytes"
	"fmt"
	"strings"
	"unicode/utf8"
)

// kind is the type of a node.
type kind int

const (
	kObject kind = iota
	kArray
	kString
	kNumber
	kBool
	kNull
)

// node is a JSON value with the bytes it occupies in the source.
type node struct {
	kind       kind
	start, end int // src[start:end] is the value, without surrounding whitespace
	members    []member
	elems      []*node
}

// member is a key and its value in an object.
type member struct {
	key      string
	keyStart int // the opening quote
	keyEnd   int // after the closing quote
	val      *node
}

// parser reads JSON in place and records offsets.
type parser struct {
	src []byte
	pos int
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

func (p *parser) skip() {
	for p.pos < len(p.src) && isSpace(p.src[p.pos]) {
		p.pos++
	}
}

func (p *parser) errorf(format string, args ...any) error {
	line := 1 + bytes.Count(p.src[:min(p.pos, len(p.src))], []byte("\n"))
	return fmt.Errorf("%w: line %d: %s", ErrInvalid, line, fmt.Sprintf(format, args...))
}

// parseDocument parses src as one JSON value, allowing a UTF-8 byte order mark
// and whitespace around it. It returns the root and the offset where the value
// starts.
func parseDocument(src []byte) (*node, error) {
	p := &parser{src: src}
	if bytes.HasPrefix(src, []byte{0xEF, 0xBB, 0xBF}) {
		p.pos = 3
	}
	if bytes.HasPrefix(src, []byte{0xFF, 0xFE}) || bytes.HasPrefix(src, []byte{0xFE, 0xFF}) {
		return nil, fmt.Errorf("%w: UTF-16 is not edited", ErrInvalid)
	}
	p.skip()
	n, err := p.value(0)
	if err != nil {
		return nil, err
	}
	p.skip()
	if p.pos != len(src) {
		return nil, p.errorf("more than one value in the file")
	}
	return n, nil
}

const maxDepth = 512

func (p *parser) value(depth int) (*node, error) {
	if depth > maxDepth {
		return nil, p.errorf("nested too deep")
	}
	if p.pos >= len(p.src) {
		return nil, p.errorf("unexpected end")
	}
	switch c := p.src[p.pos]; {
	case c == '{':
		return p.object(depth)
	case c == '[':
		return p.array(depth)
	case c == '"':
		start := p.pos
		if err := p.str(); err != nil {
			return nil, err
		}
		return &node{kind: kString, start: start, end: p.pos}, nil
	case c == '-' || (c >= '0' && c <= '9'):
		return p.number()
	default:
		for _, lit := range []struct {
			text string
			k    kind
		}{{"true", kBool}, {"false", kBool}, {"null", kNull}} {
			if bytes.HasPrefix(p.src[p.pos:], []byte(lit.text)) {
				n := &node{kind: lit.k, start: p.pos, end: p.pos + len(lit.text)}
				p.pos = n.end
				return n, nil
			}
		}
		return nil, p.errorf("unexpected %q", string(c))
	}
}

func (p *parser) number() (*node, error) {
	start := p.pos
	if p.src[p.pos] == '-' {
		p.pos++
	}
	digits := func() int {
		n := 0
		for p.pos < len(p.src) && p.src[p.pos] >= '0' && p.src[p.pos] <= '9' {
			p.pos++
			n++
		}
		return n
	}
	if p.pos < len(p.src) && p.src[p.pos] == '0' {
		p.pos++
	} else if digits() == 0 {
		return nil, p.errorf("invalid number")
	}
	if p.pos < len(p.src) && p.src[p.pos] == '.' {
		p.pos++
		if digits() == 0 {
			return nil, p.errorf("invalid number")
		}
	}
	if p.pos < len(p.src) && (p.src[p.pos] == 'e' || p.src[p.pos] == 'E') {
		p.pos++
		if p.pos < len(p.src) && (p.src[p.pos] == '+' || p.src[p.pos] == '-') {
			p.pos++
		}
		if digits() == 0 {
			return nil, p.errorf("invalid number")
		}
	}
	return &node{kind: kNumber, start: start, end: p.pos}, nil
}

// str advances over a string literal; p.pos is at the opening quote.
func (p *parser) str() error {
	p.pos++
	for p.pos < len(p.src) {
		switch c := p.src[p.pos]; {
		case c == '"':
			p.pos++
			return nil
		case c == '\\':
			p.pos++
			if p.pos >= len(p.src) {
				return p.errorf("unterminated string")
			}
			if p.src[p.pos] == 'u' {
				if p.pos+4 >= len(p.src) {
					return p.errorf("invalid escape")
				}
				p.pos += 4
			}
			p.pos++
		case c < 0x20:
			return p.errorf("control character in a string")
		default:
			_, size := utf8.DecodeRune(p.src[p.pos:])
			p.pos += size
		}
	}
	return p.errorf("unterminated string")
}

func (p *parser) object(depth int) (*node, error) {
	n := &node{kind: kObject, start: p.pos}
	p.pos++
	p.skip()
	if p.pos < len(p.src) && p.src[p.pos] == '}' {
		p.pos++
		n.end = p.pos
		return n, nil
	}
	for {
		p.skip()
		if p.pos >= len(p.src) || p.src[p.pos] != '"' {
			return nil, p.errorf("expected a key")
		}
		ks := p.pos
		if err := p.str(); err != nil {
			return nil, err
		}
		ke := p.pos
		key, err := unquote(p.src[ks:p.pos])
		if err != nil {
			return nil, p.errorf("invalid key")
		}
		p.skip()
		if p.pos >= len(p.src) || p.src[p.pos] != ':' {
			return nil, p.errorf("expected ':'")
		}
		p.pos++
		p.skip()
		v, err := p.value(depth + 1)
		if err != nil {
			return nil, err
		}
		n.members = append(n.members, member{key: key, keyStart: ks, keyEnd: ke, val: v})
		p.skip()
		if p.pos >= len(p.src) {
			return nil, p.errorf("unterminated object")
		}
		switch p.src[p.pos] {
		case ',':
			p.pos++
		case '}':
			p.pos++
			n.end = p.pos
			return n, nil
		default:
			return nil, p.errorf("expected ',' or '}'")
		}
	}
}

func (p *parser) array(depth int) (*node, error) {
	n := &node{kind: kArray, start: p.pos}
	p.pos++
	p.skip()
	if p.pos < len(p.src) && p.src[p.pos] == ']' {
		p.pos++
		n.end = p.pos
		return n, nil
	}
	for {
		p.skip()
		v, err := p.value(depth + 1)
		if err != nil {
			return nil, err
		}
		n.elems = append(n.elems, v)
		p.skip()
		if p.pos >= len(p.src) {
			return nil, p.errorf("unterminated array")
		}
		switch p.src[p.pos] {
		case ',':
			p.pos++
		case ']':
			p.pos++
			n.end = p.pos
			return n, nil
		default:
			return nil, p.errorf("expected ',' or ']'")
		}
	}
}

func unquote(lit []byte) (string, error) {
	var s string
	if err := unmarshalString(lit, &s); err != nil {
		return "", err
	}
	return s, nil
}

// segments splits a JSON pointer.
func segments(pointer string) ([]string, error) {
	if pointer == "" {
		return nil, nil
	}
	if pointer[0] != '/' {
		return nil, fmt.Errorf("%w: %q is not a JSON pointer", ErrInvalid, pointer)
	}
	parts := strings.Split(pointer[1:], "/")
	for i, p := range parts {
		parts[i] = strings.ReplaceAll(strings.ReplaceAll(p, "~1", "/"), "~0", "~")
	}
	return parts, nil
}
