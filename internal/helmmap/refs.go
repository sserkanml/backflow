package helmmap

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"regexp"
	"slices"
	"strings"

	"github.com/sserkanml/backflow/internal/rendermap"

	"go.yaml.in/yaml/v3"
)

// A value key that no template of the chart reads cannot change what the chart
// renders. When candidates are left untried (too many keys fit the field), that
// is the cheap, static proof that a key cannot be the one: the proposal is only
// mapped when every key that was left out is provably not read.

// valueRefs are the places of .Values the files of a chart read.
type valueRefs struct {
	paths []rendermap.KeyPath
	// all is set when some template uses .Values as a whole (passes it on, assigns
	// it to a variable, indexes it with a computed key): any key may be read. It
	// is also set when no template was found at all: nothing can be said then.
	all bool
	// templates counts the template files that were read.
	templates int
	// prefixes are the names and aliases of the dependencies the charts list: the
	// first segment of a key that is under one of them is not part of the key
	// the dependency's own templates read.
	prefixes map[string]bool
}

// maxArchiveText bounds what is read from a packaged chart.
const maxArchiveText = 8 << 20

var (
	templateComment = regexp.MustCompile(`(?s)\{\{-?\s*/\*.*?\*/\s*-?\}\}`)
	valuesWord      = regexp.MustCompile(`\.Values\b`)
	dotPath         = regexp.MustCompile(`^(?:\.[A-Za-z_][A-Za-z0-9_]*)+`)
	quotedKeys      = regexp.MustCompile(`^\s+((?:"[^"]*"\s*)+)`)
	oneQuoted       = regexp.MustCompile(`"([^"]*)"`)
)

// scanValueRefs reads every text file of the render (templates, helpers, value
// files, which can hold tpl strings) and the files inside packaged charts.
func scanValueRefs(files map[string][]byte) *valueRefs {
	refs := &valueRefs{}
	for _, name := range slices.Sorted(mapsKeys(files)) {
		data := files[name]
		switch {
		case strings.HasSuffix(name, ".tgz") || strings.HasSuffix(name, ".tar.gz"):
			refs.scanArchive(data)
		case !bytes.Contains(data, []byte{0}):
			refs.countTemplate(name)
			refs.scanDependencies(name, data)
			refs.scanText(string(data))
		}
	}
	if refs.templates == 0 {
		refs.all = true
	}
	return refs
}

func mapsKeys(m map[string][]byte) func(yield func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// scanDependencies reads the names and aliases of the dependencies out of a
// Chart.yaml or requirements.yaml.
func (r *valueRefs) scanDependencies(name string, data []byte) {
	base := name[strings.LastIndex(name, "/")+1:]
	if base != chartYAMLName && base != requirementsName {
		return
	}
	var doc struct {
		Dependencies []struct {
			Name  string `yaml:"name"`
			Alias string `yaml:"alias"`
		} `yaml:"dependencies"`
	}
	if yaml.Unmarshal(data, &doc) != nil {
		return
	}
	if r.prefixes == nil {
		r.prefixes = map[string]bool{}
	}
	for _, d := range doc.Dependencies {
		r.prefixes[d.Name] = true
		if d.Alias != "" {
			r.prefixes[d.Alias] = true
		}
	}
}

func (r *valueRefs) countTemplate(name string) {
	if strings.HasPrefix(name, "templates/") || strings.Contains(name, "/templates/") {
		r.templates++
	}
}

func (r *valueRefs) scanArchive(data []byte) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		r.all = true // cannot read what it holds: anything may be read
		return
	}
	tr := tar.NewReader(gz)
	total := 0
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return
		}
		if err != nil {
			r.all = true
			return
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(tr, maxArchiveText))
		total += len(b)
		if err != nil || total > maxArchiveText {
			r.all = true
			return
		}
		if !bytes.Contains(b, []byte{0}) {
			r.countTemplate(h.Name)
			r.scanDependencies(h.Name, b)
			r.scanText(string(b))
		}
	}
}

func (r *valueRefs) scanText(text string) {
	text = templateComment.ReplaceAllString(text, "")
	for _, loc := range valuesWord.FindAllStringIndex(text, -1) {
		rest := text[loc[1]:]
		if m := dotPath.FindString(rest); m != "" {
			r.paths = append(r.paths, rendermap.KeyPath(strings.Split(strings.TrimPrefix(m, "."), ".")))
			continue
		}
		// index .Values "a" "b" or get .Values "a"
		before := text[max(0, loc[0]-12):loc[0]]
		if strings.HasSuffix(strings.TrimRight(before, " ("), "index") || strings.HasSuffix(strings.TrimRight(before, " ("), "get") {
			if m := quotedKeys.FindStringSubmatch(rest); m != nil {
				quoted := oneQuoted.FindAllStringSubmatch(m[1], -1)
				p := make(rendermap.KeyPath, 0, len(quoted))
				for _, q := range quoted {
					p = append(p, q[1])
				}
				r.paths = append(r.paths, p)
				continue
			}
		}
		r.all = true
	}
}

// Mentions reports whether a key may be read: a template reads the key, a key
// above it (the map it is in) or a key below it. When the first segment is the
// name or alias of a dependency, the dependency's templates read the key
// without it.
func (r *valueRefs) Mentions(key rendermap.KeyPath) bool {
	if r.all {
		return true
	}
	for _, p := range r.paths {
		if rendermap.Related(p, key) || (len(key) > 1 && r.prefixes[key[0]] && rendermap.Related(p, key[1:])) {
			return true
		}
	}
	return false
}
