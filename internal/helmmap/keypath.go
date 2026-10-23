package helmmap

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/sserkanml/backflow/internal/rendermap"
)

// parseStrvals splits a helm.parameters name the way Helm's --set does: "."
// separates keys, "\." is a literal dot, and "[n]" is a list index.
// "a.b[0].c" is a, b, 0, c.
func parseStrvals(name string) (rendermap.KeyPath, error) {
	var (
		path rendermap.KeyPath
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
