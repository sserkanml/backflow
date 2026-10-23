package helmmap

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/sserkanml/backflow/internal/rendermap"
)

// NoOverride is a chart default that cannot be overridden from a value file.
type NoOverride struct {
	Path   rendermap.KeyPath
	Reason string
}

// Controlled is a key path an inline setting of the Application decides, so
// editing a value file would not change the rendered value.
type Controlled struct {
	// Layer is one of LayerValues, LayerValuesObject, LayerParameters and
	// LayerFileParameters.
	Layer string
	Path  rendermap.KeyPath
}

// Result is what Find reports.
type Result struct {
	// Candidates are the editable matches, in the order of the value files
	// and of the keys in each file.
	Candidates []rendermap.Candidate
	// ChartDefaults are matches found only in the chart's own values.yaml.
	// That file is not edited; the value comes from the chart, not from the
	// Application's repository configuration.
	ChartDefaults []rendermap.KeyPath
	// Overrides are the ways to set a chart default from the Application: the
	// key added to the last value file, with the value the field has to get.
	// They are only tried when no key a value file already defines works.
	Overrides []rendermap.Candidate
	// NoOverride says, for chart defaults that Overrides does not cover, why
	// they cannot be overridden: a key under a list, or no value file to add to.
	NoOverride []NoOverride
	// UnreadDependencies are the chart's dependencies whose defaults are not in
	// the repository (Argo CD downloads them when it renders), by name or alias.
	// A value may come from one of them.
	UnreadDependencies []string
	// Controlled are inline settings that decide the value: either a setting
	// whose own value equals the old value, or a setting at (or above, or
	// below) a key that a value file matched.
	Controlled []Controlled
	// RemoteValueFiles are value files fetched over http(s). They are not
	// read, so the value may come from one of them.
	RemoteValueFiles []string
}

// Find looks for the keys that hold old, the value a drifted field had in
// Git. old is a decoded JSON value (string, number, bool). Scalars match by
// their rendered string form, so the number 3 matches "3" and true matches
// "true".
//
// files maps repository paths to content and must hold every file in
// src.ValueFiles; src.ChartValues is read when present. Lists and maps are
// never matched themselves, only the scalars in them, at any depth.
//
// Only the effective definition of a key counts. Helm merges maps key by key,
// but a scalar or list from a later file replaces whatever an earlier file
// had at that key, and null deletes it. A match is therefore dropped when a
// later file (or, for a key an inline setting decides, the inline layer)
// overrides it.
func (s *Source) Find(files map[string][]byte, old any) (*Result, error) {
	if old == nil {
		return &Result{RemoteValueFiles: slices.Clone(s.RemoteValueFiles)}, nil
	}
	want := rendermap.RenderValue(old)
	if _, ok := old.(string); !ok && want == "" {
		return &Result{RemoteValueFiles: slices.Clone(s.RemoteValueFiles)}, nil // a map or list has no scalar form to compare
	}
	return s.find(files, func(v string) []occurrence {
		if v != want {
			return nil
		}
		return []occurrence{{}}
	}, false)
}

// FindFragments looks for keys whose value is a part of the drifted string
// field: the field went from old to new, and a key's value occurs in old and
// covers everything that changed. It is what a change of the image tag looks
// like from the cluster ("nginx:1.27-alpine" became "nginx:1.28-alpine"), where
// the key image.tag holds only "1.27-alpine".
//
// The changed region is what is left of old after the longest common prefix and
// suffix of old and new are taken away. A key qualifies when its value, as a
// string, occurs in old and the occurrence includes the whole region; the new
// value of the key is the same stretch cut out of new. Every occurrence is a
// candidate of its own. The rules of Find apply otherwise: effective
// definitions only, the chart's own values.yaml is reported and not offered,
// inline settings are reported.
//
// A key whose value is the whole of old is left out: Find already offers it.
func (s *Source) FindFragments(files map[string][]byte, old, new string) (*Result, error) {
	if old == new {
		return &Result{RemoteValueFiles: slices.Clone(s.RemoteValueFiles)}, nil
	}
	return s.find(files, fragmentMatcher(old, new), true)
}

// occurrence is a way a key's value matches the drifted field. newValue is what
// the key must become; it is empty for a match of the whole value.
type occurrence struct{ newValue string }

// fragmentMatcher returns the matcher of FindFragments.
func fragmentMatcher(old, new string) func(string) []occurrence {
	prefix := commonPrefixLen(old, new)
	suffix := commonSuffixLen(old[prefix:], new[prefix:])
	regionStart, regionEnd := prefix, len(old)-suffix
	return func(v string) []occurrence {
		if v == "" || v == old {
			return nil
		}
		var out []occurrence
		for from := 0; from < len(old); {
			j := strings.Index(old[from:], v)
			if j < 0 {
				break
			}
			at := from + j
			from = at + 1
			end := at + len(v)
			covers := at <= regionStart && end >= regionEnd
			if regionStart == regionEnd { // something was inserted: the key must reach the place
				covers = at <= regionStart && regionStart <= end
			}
			if !covers {
				continue
			}
			cutEnd := len(new) - (len(old) - end)
			if cutEnd < at || cutEnd > len(new) {
				continue
			}
			out = append(out, occurrence{newValue: new[at:cutEnd]})
		}
		return out
	}
}

// commonPrefixLen is the length in bytes of the longest common prefix of a and
// b that ends between two characters.
func commonPrefixLen(a, b string) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	for n > 0 && n < len(a) && !utf8.RuneStart(a[n]) {
		n--
	}
	return n
}

// commonSuffixLen is the length in bytes of the longest common suffix of a and
// b that starts between two characters.
func commonSuffixLen(a, b string) int {
	n := 0
	for n < len(a) && n < len(b) && a[len(a)-1-n] == b[len(b)-1-n] {
		n++
	}
	for n > 0 && !utf8.RuneStart(a[len(a)-n]) {
		n--
	}
	return n
}

// find is the search of Find and FindFragments. match says how a scalar's
// string form relates to the drifted field; a scalar that does not match gives
// no occurrence.
func (s *Source) find(files map[string][]byte, match func(string) []occurrence, fragments bool) (*Result, error) {
	res := &Result{RemoteValueFiles: slices.Clone(s.RemoteValueFiles)}

	// The layers, lowest precedence first: the defaults of the dependencies, the
	// chart's own values.yaml, then the value files in order.
	type layer struct {
		name      string
		defs      []definition
		isDefault bool
	}
	var layers []layer
	deps, missing := dependencyDefaults(files, s.ChartDir)
	res.UnreadDependencies = missing
	for _, dd := range deps {
		defs, err := parseValues(dd.data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", dd.file, err)
		}
		layers = append(layers, layer{name: dd.file, defs: moveInto(defs, dd.prefix), isDefault: true})
	}
	if data, ok := files[s.ChartValues]; ok {
		defs, err := parseValues(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", s.ChartValues, err)
		}
		layers = append(layers, layer{name: s.ChartValues, defs: defs, isDefault: true})
	}
	for _, name := range s.ValueFiles {
		data, ok := files[name]
		if !ok {
			return nil, fmt.Errorf("value file %s was not provided", name)
		}
		defs, err := parseValues(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		layers = append(layers, layer{name: name, defs: defs})
	}
	later := func(i int) [][]definition {
		out := make([][]definition, 0, len(layers)-i-1)
		for _, l := range layers[i+1:] {
			out = append(out, l.defs)
		}
		return out
	}

	target, targetWhy := s.overrideFile()
	seen := map[string]bool{}
	control := func(layer string, p rendermap.KeyPath) {
		key := layer + "\x00" + p.String()
		if !seen[key] {
			seen[key] = true
			res.Controlled = append(res.Controlled, Controlled{Layer: layer, Path: p})
		}
	}

	for i, l := range layers {
		for _, d := range l.defs {
			if d.kind != defScalar || d.viaMerge {
				continue
			}
			occurrences := match(d.value)
			if len(occurrences) == 0 || overridden(d, later(i)) {
				continue
			}
			if v, ok := s.inlineFor(d.path); ok {
				control(v.Layer, v.Path)
				continue
			}
			if l.isDefault {
				res.ChartDefaults = append(res.ChartDefaults, d.path)
				switch {
				case d.inList:
					res.NoOverride = append(res.NoOverride, NoOverride{Path: d.path, Reason: "the key is in a list in the chart's defaults; a list item cannot be overridden on its own"})
				case target == "":
					res.NoOverride = append(res.NoOverride, NoOverride{Path: d.path, Reason: targetWhy})
				default:
					key := d.local
					if key == nil {
						key = d.path
					}
					def := &rendermap.Default{File: l.name, Key: key, Value: d.value, Tag: d.tag}
					for _, o := range occurrences {
						res.Overrides = append(res.Overrides, rendermap.Candidate{
							File: target, Path: d.path, Fragment: fragments, NewValue: o.newValue,
							Override: true, Default: def,
						})
					}
				}
				continue
			}
			for _, o := range occurrences {
				res.Candidates = append(res.Candidates, rendermap.Candidate{File: l.name, Path: d.path, Fragment: fragments, NewValue: o.newValue})
			}
		}
	}

	// An inline setting whose own value matches.
	for i, v := range s.Inline {
		if !v.HasValue || len(match(v.Value)) == 0 {
			continue
		}
		shadowed := false
		for _, u := range s.Inline[i+1:] {
			if rendermap.Related(u.Path, v.Path) {
				shadowed = true
				break
			}
		}
		if !shadowed {
			control(v.Layer, v.Path)
		}
	}
	return res, nil
}

// overridden reports whether a definition in a later layer replaces d: a
// scalar, list or null at d's path or above it, or anything below it, which
// turns d's key into a map. Maps themselves are not definitions, so every
// related definition decides the key.
func overridden(d definition, later [][]definition) bool {
	for _, defs := range later {
		for _, e := range defs {
			if rendermap.Related(e.path, d.path) {
				return true
			}
		}
	}
	return false
}

// inlineFor returns the last inline setting that decides a value file key
// path: one at the path itself, above it (it replaces a list or map) or below
// it (it turns the key into a map).
func (s *Source) inlineFor(p rendermap.KeyPath) (InlineValue, bool) {
	for _, v := range slices.Backward(s.Inline) {
		if rendermap.Related(v.Path, p) {
			return v, true
		}
	}
	return InlineValue{}, false
}
