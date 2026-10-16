// Package helmmap traces a drifted Helm-rendered value back to the values
// file that feeds it. It is pure logic: it works on file contents and an
// Application's Helm source settings and never talks to Argo CD or a cluster.
package helmmap

import (
	"fmt"
	"strconv"
	"strings"
)

// KeyPath addresses a value inside a values document. A segment is a map key
// or a list index; keys may contain dots (app.kubernetes.io/name), so a path
// is a slice and never a dotted string. A list index and a map key that is
// the same digits are not told apart.
type KeyPath []string

// String is the display form: segments joined by ".", with "." and "\" inside
// a segment escaped by a backslash. It is unambiguous, and ParseKeyPath is its
// inverse.
func (p KeyPath) String() string {
	var b strings.Builder
	for i, s := range p {
		if i > 0 {
			b.WriteByte('.')
		}
		b.WriteString(escapeSegment(s))
	}
	return b.String()
}

func escapeSegment(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, ".", `\.`)
}

// ParseKeyPath parses the display form produced by String.
func ParseKeyPath(s string) (KeyPath, error) {
	if s == "" {
		return nil, fmt.Errorf("empty key path")
	}
	var (
		path KeyPath
		cur  strings.Builder
	)
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\\':
			if i+1 == len(s) {
				return nil, fmt.Errorf("key path %q ends with a lone backslash", s)
			}
			i++
			cur.WriteByte(s[i])
		case '.':
			path = append(path, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	return append(path, cur.String()), nil
}

// Equal reports whether both paths address the same place.
func (p KeyPath) Equal(q KeyPath) bool { return len(p) == len(q) && p.HasPrefix(q) }

// HasPrefix reports whether q is p or an ancestor of p.
func (p KeyPath) HasPrefix(q KeyPath) bool {
	if len(q) > len(p) {
		return false
	}
	for i := range q {
		if p[i] != q[i] {
			return false
		}
	}
	return true
}

// related reports whether one path is the other or an ancestor of it: setting
// one of them decides the value at the other.
func related(p, q KeyPath) bool { return p.HasPrefix(q) || q.HasPrefix(p) }

// pointer is the RFC 6901 JSON pointer of the path, the form the YAML editor
// in internal/mapping addresses fields by.
func (p KeyPath) pointer() string {
	var b strings.Builder
	for _, s := range p {
		b.WriteByte('/')
		b.WriteString(strings.NewReplacer("~", "~0", "/", "~1").Replace(s))
	}
	return b.String()
}

// parseStrvals splits a helm.parameters name the way Helm's --set does: "."
// separates keys, "\." is a literal dot, and "[n]" is a list index.
// "a.b[0].c" is a, b, 0, c.
func parseStrvals(name string) (KeyPath, error) {
	var (
		path KeyPath
		cur  strings.Builder
		have bool // cur holds a key, possibly empty, that has not been appended
	)
	flush := func() {
		if have {
			path = append(path, cur.String())
		}
		cur.Reset()
		have = false
	}
	for i := 0; i < len(name); i++ {
		switch c := name[i]; c {
		case '\\':
			if i+1 < len(name) {
				i++
			}
			cur.WriteByte(name[i])
			have = true
		case '.':
			flush()
		case '[':
			flush()
			end := strings.IndexByte(name[i:], ']')
			if end < 0 {
				return nil, fmt.Errorf("parameter name %q: missing ]", name)
			}
			idx := name[i+1 : i+end]
			if n, err := strconv.Atoi(idx); err != nil || n < 0 {
				return nil, fmt.Errorf("parameter name %q: %q is not a list index", name, idx)
			}
			path = append(path, idx)
			i += end
		default:
			cur.WriteByte(c)
			have = true
		}
	}
	flush()
	if len(path) == 0 {
		return nil, fmt.Errorf("parameter name %q is empty", name)
	}
	return path, nil
}

// KeyPathFromPointer is the inverse of the pointer form the YAML editor uses:
// it splits an RFC 6901 JSON pointer into a key path.
func KeyPathFromPointer(p string) KeyPath {
	if p == "" || p == "/" {
		return nil
	}
	segs := strings.Split(strings.TrimPrefix(p, "/"), "/")
	r := strings.NewReplacer("~1", "/", "~0", "~")
	for i, s := range segs {
		segs[i] = r.Replace(s)
	}
	return segs
}
