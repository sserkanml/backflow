package helmmap

import (
	"fmt"
	"slices"
)

// Candidate is a scalar in a value file whose effective value equals the old
// value of a drifted field. It is only a candidate: the same value can feed
// several fields, and a template may derive the field from it. Re-rendering
// decides.
type Candidate struct {
	// File is the repository path of the value file.
	File string
	Path KeyPath
}

// Controlled is a key path an inline setting of the Application decides, so
// editing a value file would not change the rendered value.
type Controlled struct {
	// Layer is one of LayerValues, LayerValuesObject, LayerParameters and
	// LayerFileParameters.
	Layer string
	Path  KeyPath
}

// Result is what Find reports.
type Result struct {
	// Candidates are the editable matches, in the order of the value files
	// and of the keys in each file.
	Candidates []Candidate
	// ChartDefaults are matches found only in the chart's own values.yaml.
	// That file is not edited; the value comes from the chart, not from the
	// Application's repository configuration.
	ChartDefaults []KeyPath
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
	res := &Result{RemoteValueFiles: slices.Clone(s.RemoteValueFiles)}
	if old == nil {
		return res, nil
	}
	want := RenderValue(old)
	if _, ok := old.(string); !ok && want == "" {
		return res, nil // a map or list has no scalar form to compare
	}

	// Layer 0 is the chart's values.yaml; the value files follow in order.
	names := append([]string{s.ChartValues}, s.ValueFiles...)
	layers := make([][]definition, len(names))
	for i, name := range names {
		data, ok := files[name]
		if !ok {
			if i == 0 {
				continue
			}
			return nil, fmt.Errorf("value file %s was not provided", name)
		}
		defs, err := parseValues(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		layers[i] = defs
	}

	seen := map[string]bool{}
	control := func(layer string, p KeyPath) {
		key := layer + "\x00" + p.String()
		if !seen[key] {
			seen[key] = true
			res.Controlled = append(res.Controlled, Controlled{Layer: layer, Path: p})
		}
	}

	for i, defs := range layers {
		for _, d := range defs {
			if d.kind != defScalar || d.value != want || d.viaMerge {
				continue
			}
			if overridden(d, layers[i+1:]) {
				continue
			}
			if v, ok := s.inlineFor(d.path); ok {
				control(v.Layer, v.Path)
				continue
			}
			if i == 0 {
				res.ChartDefaults = append(res.ChartDefaults, d.path)
				continue
			}
			res.Candidates = append(res.Candidates, Candidate{File: names[i], Path: d.path})
		}
	}

	// An inline setting whose own value is the old one.
	for i, v := range s.Inline {
		if !v.HasValue || v.Value != want {
			continue
		}
		shadowed := false
		for _, u := range s.Inline[i+1:] {
			if related(u.Path, v.Path) {
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
			if related(e.path, d.path) {
				return true
			}
		}
	}
	return false
}

// inlineFor returns the last inline setting that decides a value file key
// path: one at the path itself, above it (it replaces a list or map) or below
// it (it turns the key into a map).
func (s *Source) inlineFor(p KeyPath) (InlineValue, bool) {
	for _, v := range slices.Backward(s.Inline) {
		if related(v.Path, p) {
			return v, true
		}
	}
	return InlineValue{}, false
}
