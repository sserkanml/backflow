package helmmap

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/sserkanml/backflow/internal/rendermap"

	"go.yaml.in/yaml/v3"
)

// Structured candidates are the edits of value files that can bring a map or a
// list of a rendered resource to its live state. The value files hold the
// source of those maps and lists (podAnnotations, tolerations, extraEnvVars,
// resources, ...) and a template renders them into the resource, often next to
// entries the chart generates. Which key feeds which field is not written
// anywhere: candidates are found by comparing what the key holds with what the
// resource has in Git (its base render), and a render decides, as for scalars.

// Kinds of structured edits.
const (
	// editSet sets a key to a map, a list or a scalar, creating it when the
	// file does not have it.
	editSet = "set"
	// editRemoveKey removes a key from the file.
	editRemoveKey = "removeKey"
)

// StructuredEdit is the edit of a Candidate that is not the change of one
// scalar.
type StructuredEdit struct {
	Kind string
	// Value is the decoded JSON to set; nil for editRemoveKey.
	Value any
}

// text is the edit as one comparable string.
func (e *StructuredEdit) text() string {
	t, _ := jsonText(e.Value)
	return e.Kind + ":" + t
}

// treeLayer is one layer of values as a decoded tree.
type treeLayer struct {
	name      string
	tree      map[string]any
	isDefault bool
	// prefix is the key the layer's keys have in the parent chart, for a
	// dependency's defaults; empty otherwise.
	prefix string
}

// treeLayers decodes the layers of values, lowest precedence first (the same
// layers as find reads): the defaults of the dependencies in the files, the
// chart's own values.yaml, then the value files in order.
func (s *Source) treeLayers(files map[string][]byte) ([]treeLayer, error) {
	decode := func(name string, data []byte) (map[string]any, error) {
		var m map[string]any
		if err := yaml.Unmarshal(data, &m); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if m == nil {
			m = map[string]any{}
		}
		out, _ := toJSONValue(m).(map[string]any)
		return out, nil
	}
	var layers []treeLayer
	deps, _ := dependencyDefaults(files, s.ChartDir)
	for _, dd := range deps {
		m, err := decode(dd.file, dd.data)
		if err != nil {
			return nil, err
		}
		moved := map[string]any{}
		for k, v := range m {
			if k == "global" {
				moved[k] = v
			} else {
				sub, _ := moved[dd.prefix].(map[string]any)
				if sub == nil {
					sub = map[string]any{}
					moved[dd.prefix] = sub
				}
				sub[k] = v
			}
		}
		layers = append(layers, treeLayer{name: dd.file, tree: moved, isDefault: true, prefix: dd.prefix})
	}
	if data, ok := files[s.ChartValues]; ok {
		m, err := decode(s.ChartValues, data)
		if err != nil {
			return nil, err
		}
		layers = append(layers, treeLayer{name: s.ChartValues, tree: m, isDefault: true})
	}
	for _, name := range s.ValueFiles {
		data, ok := files[name]
		if !ok {
			return nil, fmt.Errorf("value file %s was not provided", name)
		}
		m, err := decode(name, data)
		if err != nil {
			return nil, err
		}
		layers = append(layers, treeLayer{name: name, tree: m})
	}
	return layers, nil
}

// mergeValues merges the layers the way Helm does: maps key by key, anything
// else replaced by the later layer, null removes the key.
func mergeValues(layers []treeLayer) map[string]any {
	out := map[string]any{}
	for _, l := range layers {
		mergeInto(out, l.tree)
	}
	return out
}

func mergeInto(dst, src map[string]any) {
	for k, v := range src {
		switch sv := v.(type) {
		case nil:
			delete(dst, k)
		case map[string]any:
			dm, _ := dst[k].(map[string]any)
			if dm == nil {
				dm = map[string]any{}
			}
			dst[k] = dm
			mergeInto(dm, sv)
		default:
			dst[k] = deepCopy(v)
		}
	}
}

func deepCopy(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, c := range x {
			out[k] = deepCopy(c)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, c := range x {
			out[i] = deepCopy(c)
		}
		return out
	}
	return v
}

// getAt returns the value at a key path through maps.
func getAt(tree any, path rendermap.KeyPath) (any, bool) {
	cur := tree
	for _, seg := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[seg]; !ok {
			return nil, false
		}
	}
	return cur, true
}

// valueNode is a map or list of the merged values.
type valueNode struct {
	path  rendermap.KeyPath
	value any
}

// valueNodes lists the maps and lists of the merged values, outside lists.
func valueNodes(merged map[string]any) []valueNode {
	var out []valueNode
	var walk func(path rendermap.KeyPath, m map[string]any)
	walk = func(path rendermap.KeyPath, m map[string]any) {
		for _, k := range slices.Sorted(maps.Keys(m)) {
			p := path.Child(k)
			switch v := m[k].(type) {
			case map[string]any:
				out = append(out, valueNode{p, v})
				walk(p, v)
			case []any:
				out = append(out, valueNode{p, v})
			}
		}
	}
	walk(nil, merged)
	return out
}

func isEmptyContainer(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		return len(x) == 0
	case []any:
		return len(x) == 0
	}
	return false
}

func emptyLike(v any) any {
	if _, ok := v.([]any); ok {
		return []any{}
	}
	return map[string]any{}
}

// treeFromLeaves rebuilds the value at a pointer from the leaves of a render.
func treeFromLeaves(l leaves, pointer string) (any, bool) {
	if v, ok := l[pointer]; ok {
		switch v {
		case "{}":
			return map[string]any{}, true
		case "[]":
			return []any{}, true
		}
		var x any
		if json.Unmarshal([]byte(v), &x) != nil {
			return nil, false
		}
		return x, true
	}
	type entry struct {
		rel []string
		val string
	}
	var entries []entry
	for k, v := range l {
		if rest, ok := strings.CutPrefix(k, pointer+"/"); ok {
			parts := strings.Split(rest, "/")
			for i, p := range parts {
				parts[i] = strings.NewReplacer("~1", "/", "~0", "~").Replace(p)
			}
			entries = append(entries, entry{parts, v})
		}
	}
	if len(entries) == 0 {
		return nil, false
	}
	var build func(es []entry) any
	build = func(es []entry) any {
		if len(es) == 1 && len(es[0].rel) == 0 {
			switch es[0].val {
			case "{}":
				return map[string]any{}
			case "[]":
				return []any{}
			}
			var x any
			_ = json.Unmarshal([]byte(es[0].val), &x)
			return x
		}
		groups := map[string][]entry{}
		for _, e := range es {
			groups[e.rel[0]] = append(groups[e.rel[0]], entry{e.rel[1:], e.val})
		}
		isList := true
		for i := range len(groups) {
			if _, ok := groups[strconv.Itoa(i)]; !ok {
				isList = false
				break
			}
		}
		if isList {
			out := make([]any, len(groups))
			for i := range out {
				out[i] = build(groups[strconv.Itoa(i)])
			}
			return out
		}
		out := make(map[string]any, len(groups))
		for k, g := range groups {
			out[k] = build(g)
		}
		return out
	}
	return build(entries), true
}

// pointerSegments splits a JSON pointer into unescaped segments.
func pointerSegments(p string) []string {
	if p == "" {
		return nil
	}
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	for i, s := range parts {
		parts[i] = strings.NewReplacer("~1", "/", "~0", "~").Replace(s)
	}
	return parts
}

func isIndex(seg string) bool {
	n, err := strconv.Atoi(seg)
	return err == nil && n >= 0 && strconv.Itoa(n) == seg
}

// applyFields returns what the subtree at target looks like with the changes of
// the fields below it made. ok is false when the subtree disappears.
func applyFields(d any, target string, fields []parsedField) (any, bool) {
	cur := deepCopy(d)
	for _, f := range fields {
		rel := pointerSegments(f.Change.Path)[len(pointerSegments(target)):]
		if len(rel) == 0 {
			if !f.hasLive {
				return nil, false
			}
			cur = deepCopy(f.liveV)
			continue
		}
		if f.hasLive {
			setAtRel(cur, rel, f.liveV)
		} else {
			removeAtRel(cur, rel)
		}
	}
	return cur, true
}

func setAtRel(root any, rel []string, v any) {
	for i, seg := range rel {
		last := i == len(rel)-1
		switch c := root.(type) {
		case map[string]any:
			if last {
				c[seg] = deepCopy(v)
				return
			}
			next, ok := c[seg]
			if !ok {
				next = map[string]any{}
				c[seg] = next
			}
			root = next
		case []any:
			n, err := strconv.Atoi(seg)
			if err != nil || n < 0 || n >= len(c) {
				return
			}
			if last {
				c[n] = deepCopy(v)
				return
			}
			root = c[n]
		default:
			return
		}
	}
}

func removeAtRel(root any, rel []string) {
	for i, seg := range rel {
		last := i == len(rel)-1
		switch c := root.(type) {
		case map[string]any:
			if last {
				delete(c, seg)
				return
			}
			root = c[seg]
		case []any:
			n, err := strconv.Atoi(seg)
			if err != nil || n < 0 || n >= len(c) {
				return
			}
			root = c[n]
		default:
			return
		}
	}
}

func sameValue(a, b any) bool { return reflect.DeepEqual(toJSONValue(a), toJSONValue(b)) }

// structCand is a structured edit and the fields it is proposed for.
type structCand struct {
	cand   Candidate
	value  string
	origin []int
	score  int
}

// structuredSet are the structured candidates of a Verify. Level 0 are the
// edits for a structured field itself, level 1 the edits of an ancestor of a
// field: the resources of a container for a drifted limit.
type structuredSet struct {
	edits     [2][]structCand
	overrides [2][]structCand
	// failures are the fields that have too many candidates.
	failures []FieldFailure
	// untried are the candidates left out by the bound, per field.
	untried map[int][]untriedKey
}

func (ss *structuredSet) any(level int, overrides bool) bool {
	if ss == nil {
		return false
	}
	if overrides {
		return len(ss.overrides[level]) > 0
	}
	return len(ss.edits[level]) > 0
}

// structuredCandidates finds the edits that could bring the fields to their
// live state, from what the value files hold and what the resource has in Git.
func (v *run) structuredCandidates(base snapshot, fields []parsedField, opts Options) *structuredSet {
	ss := &structuredSet{}
	src := v.in.Source
	if src == nil {
		return ss
	}
	layers, err := src.treeLayers(v.in.Files)
	if err != nil {
		return ss
	}
	merged := mergeValues(layers)
	nodes := valueNodes(merged)
	res := base[v.in.Resource]
	g := &structGen{run: v, src: src, layers: layers, nodes: nodes, base: res, fields: fields, seen: map[string]*structCand{},
		resTokens: tokens(v.in.Resource.Name)}

	type target struct {
		pointer string
		level   int
		origin  []int
	}
	var targets []target
	for i, f := range fields {
		if f.structured {
			targets = append(targets, target{f.Change.Path, 0, []int{i}})
		}
	}
	ancestors := map[string][]int{}
	var order []string
	for i, f := range fields {
		segs := pointerSegments(f.Change.Path)
		for n := len(segs) - 1; n >= 2; n-- {
			if isIndex(segs[n-1]) {
				continue
			}
			a := "/" + strings.Join(escapeSegments(segs[:n]), "/")
			if _, ok := ancestors[a]; !ok {
				order = append(order, a)
			}
			ancestors[a] = append(ancestors[a], i)
		}
	}
	for _, a := range order {
		targets = append(targets, target{a, 1, ancestors[a]})
	}
	for _, t := range targets {
		g.generate(t.pointer, t.level, t.origin)
	}
	ss.failures = g.finish(ss, opts)
	return ss
}

func escapeSegments(segs []string) []string {
	out := make([]string, len(segs))
	for i, s := range segs {
		out[i] = strings.NewReplacer("~", "~0", "/", "~1").Replace(s)
	}
	return out
}

// structGen collects the candidates of one Verify.
type structGen struct {
	run    *run
	src    *Source
	layers []treeLayer
	nodes  []valueNode
	base   leaves
	fields []parsedField
	// resTokens are the words of the resource's name: redis-prod-master prefers
	// master.podAnnotations over replica.podAnnotations.
	resTokens map[string]bool
	// seen merges the same edit proposed twice.
	seen  map[string]*structCand
	items []*structCand
	// levels of the candidates, parallel to items.
	levels []int
}

// definedIn returns the last value file (not a default) that has the key.
func (g *structGen) definedIn(path rendermap.KeyPath) (string, bool) {
	for _, l := range slices.Backward(g.layers) {
		if l.isDefault {
			continue
		}
		if _, ok := getAt(l.tree, path); ok {
			return l.name, true
		}
	}
	return "", false
}

// defaultOf describes the chart default that holds the key.
func (g *structGen) defaultOf(path rendermap.KeyPath) *ChartDefault {
	for _, l := range slices.Backward(g.layers) {
		if !l.isDefault {
			continue
		}
		if val, ok := getAt(l.tree, path); ok {
			key := path
			if l.prefix != "" && len(path) > 0 && path[0] == l.prefix {
				key = path[1:]
			}
			text, _ := jsonText(val)
			if len(text) > 80 {
				text = text[:77] + "..."
			}
			return &ChartDefault{File: l.name, Key: key, Value: text}
		}
	}
	return nil
}

// emit records an edit: an edit of the file that defines the anchor (the key
// itself, or the map a key is added to), or when no value file does, an override
// in the file that receives overrides.
func (g *structGen) emit(kind string, path, anchor rendermap.KeyPath, value any, level, score int, origin []int, mustBeDefined bool) {
	var file string
	var def *ChartDefault
	override := false
	if f, ok := g.definedIn(anchor); ok {
		file = f
	} else {
		if mustBeDefined {
			return
		}
		target, why := g.src.overrideFile()
		if why != "" {
			return
		}
		if !g.run.overrides {
			return // overrides are off: this is not a candidate, and not one that was cut
		}
		file, override = target, true
		def = g.defaultOf(anchor)
		if def == nil {
			return
		}
	}
	edit := &StructuredEdit{Kind: kind, Value: value}
	k := fmt.Sprintf("%s\x00%s\x00%s\x00%v", file, path, edit.text(), override)
	if prev, ok := g.seen[k]; ok {
		for _, o := range origin {
			if !slices.Contains(prev.origin, o) {
				prev.origin = append(prev.origin, o)
			}
		}
		prev.score = max(prev.score, score)
		return
	}
	c := &structCand{
		cand:   Candidate{File: file, Path: path, Override: override, Default: def, Structured: edit},
		value:  edit.text(),
		origin: slices.Clone(origin),
		score:  score,
	}
	g.seen[k] = c
	g.items = append(g.items, c)
	g.levels = append(g.levels, level)
}

// generate proposes edits for the target pointer: a structured field of its
// own (level 0) or an ancestor of some fields (level 1).
func (g *structGen) generate(target string, level int, origin []int) {
	var below []parsedField
	for _, f := range g.fields {
		if under(target, f.Change.Path) {
			below = append(below, f)
		}
	}
	var d, l any
	var hasD, hasL bool
	if level == 0 {
		f := g.fields[origin[0]]
		d, l, hasD, hasL = f.desiredV, f.liveV, f.hasDesired, f.hasLive
	} else {
		d, hasD = treeFromLeaves(g.base, target)
		if !hasD {
			return
		}
		if isScalar(d) {
			return
		}
		l, hasL = applyFields(d, target, below)
	}
	if hasD && hasL && sameValue(d, l) {
		return
	}
	tokensOf := targetTokens(target)
	f0 := g.fields[origin[0]]

	for _, n := range g.nodes {
		nt := tokens(n.path.String())
		gate := affinity(tokensOf, tokens(lastNames(n.path.Pointer(), 2)...))
		g.rules(ruleInput{n: n, aff: gate, score: gate*10 + affinity(g.resTokens, nt), level: level, origin: origin,
			d: d, l: l, hasD: hasD, hasL: hasL, f0: f0})
	}
}

// lastNames are the last n segments of a pointer that are names, not indexes.
func lastNames(pointer string, n int) []string {
	segs := pointerSegments(pointer)
	var out []string
	for i := len(segs) - 1; i >= 0 && len(out) < n; i-- {
		if !isIndex(segs[i]) {
			out = append(out, segs[i])
		}
	}
	return out
}

// ruleInput is what the rules know about one target of the drift and one node
// of the merged values.
type ruleInput struct {
	n      valueNode
	aff    int // words the key shares with the target
	score  int // how well the key fits: affinity, then the resource's name
	level  int
	origin []int
	d, l   any
	hasD   bool
	hasL   bool
	f0     parsedField
}

// rules proposes the edits of one values node for a target.
func (g *structGen) rules(in ruleInput) {
	if _, controlled := g.src.inlineFor(in.n.path); controlled {
		return
	}
	g.ruleSameValue(in)
	g.ruleEmpty(in)
	g.ruleListStretch(in)
	g.ruleMapPart(in)
	if in.level == 0 {
		g.ruleKey(in)
	}
}

func (g *structGen) set(in ruleInput, value any, score int) {
	g.emit(editSet, in.n.path, in.n.path, value, in.level, score, in.origin, false)
}

// ruleSameValue: the key holds what the resource has in Git, so it is set to
// what is live (or emptied, when the field is gone).
func (g *structGen) ruleSameValue(in ruleInput) {
	empty := isEmptyContainer(in.n.value)
	if !in.hasD || !sameValue(in.n.value, in.d) || (empty && in.aff == 0) {
		return
	}
	score := in.score
	if !empty {
		score += 100
	}
	if in.hasL {
		g.set(in, in.l, score)
	} else {
		g.set(in, emptyLike(in.n.value), score)
	}
}

// ruleEmpty: an empty key whose name fits feeds a field that does not exist in
// Git (it appears), or takes the place of what the chart generates (resources
// instead of resourcesPreset) and then holds the whole live state of a map.
func (g *structGen) ruleEmpty(in ruleInput) {
	if !isEmptyContainer(in.n.value) || in.aff == 0 || !in.hasL {
		return
	}
	switch {
	case !in.hasD && sameKind(in.n.value, in.l):
		g.set(in, in.l, in.score)
	case in.hasD:
		if _, isMap := in.n.value.(map[string]any); isMap && sameKind(in.n.value, in.l) && sameKind(in.n.value, in.d) {
			g.set(in, in.l, in.score)
		}
	}
}

// ruleListStretch: a values list that is a stretch of the longer rendered list.
func (g *structGen) ruleListStretch(in ruleInput) {
	dl, ok1 := in.d.([]any)
	ll, ok2 := in.l.([]any)
	nl, ok3 := in.n.value.([]any)
	if !in.hasD || !in.hasL || !ok1 || !ok2 || !ok3 || (len(nl) == 0 && in.aff == 0) {
		return
	}
	for _, region := range listRegions(dl, ll, nl) {
		g.set(in, region, in.score+boolInt(len(nl) > 0)*50)
	}
}

// ruleMapPart: a values map that is a part of the rendered map.
func (g *structGen) ruleMapPart(in ruleInput) {
	dm, ok1 := in.d.(map[string]any)
	lm, ok2 := in.l.(map[string]any)
	nm, ok3 := in.n.value.(map[string]any)
	if !in.hasD || !in.hasL || !ok1 || !ok2 || !ok3 || (len(nm) == 0 && in.aff == 0) || sameValue(nm, dm) {
		return
	}
	if next, ok := mapRegion(dm, lm, nm); ok {
		g.set(in, next, in.score+boolInt(len(nm) > 0)*50)
	}
}

// ruleKey: a key of the field's own map appears or disappears. The key is added
// to (or removed from) the values map that is a part of the rendered map.
func (g *structGen) ruleKey(in ruleInput) {
	segs := pointerSegments(in.f0.Change.Path)
	if len(segs) < 2 {
		return
	}
	key := segs[len(segs)-1]
	parent := "/" + strings.Join(escapeSegments(segs[:len(segs)-1]), "/")
	pm, ok := treeFromLeaves(g.base, parent)
	pmap, isMap := pm.(map[string]any)
	nm, nIsMap := in.n.value.(map[string]any)
	if !ok || !isMap || !nIsMap || !subsetOf(nm, pmap) {
		return
	}
	pAff := affinity(targetTokens(parent), tokens(lastNames(in.n.path.Pointer(), 2)...))
	if pAff == 0 && (len(nm) == 0 || !sameValue(nm, pmap)) {
		return
	}
	score := pAff*10 + affinity(g.resTokens, tokens(in.n.path.String())) + boolInt(len(nm) > 0)*50
	_, has := nm[key]
	switch {
	case in.f0.hasLive && !in.f0.hasDesired && !has: // a key appears
		g.emit(editSet, in.n.path.Child(key), in.n.path, in.f0.liveV, in.level, score, in.origin, false)
	case in.f0.hasDesired && !in.f0.hasLive && has: // a key disappears
		g.emit(editRemoveKey, in.n.path.Child(key), in.n.path.Child(key), nil, in.level, score, in.origin, true)
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func sameKind(a, b any) bool {
	switch a.(type) {
	case map[string]any:
		_, ok := b.(map[string]any)
		return ok
	case []any:
		_, ok := b.([]any)
		return ok
	}
	return false
}

// subsetOf reports whether every entry of a is in b with the same value.
func subsetOf(a, b map[string]any) bool {
	for k, v := range a {
		if bv, ok := b[k]; !ok || !sameValue(v, bv) {
			return false
		}
	}
	return true
}

// listRegions returns what a values list n must become so that the rendered
// list d becomes l, when n is a stretch of d (an empty n sits at its end or at
// its start) and the rest of l is d's. These are the lists a chart builds from
// entries of its own and the user's: extraEnvVars next to generated variables.
func listRegions(d, l, n []any) [][]any {
	var starts []int
	if len(n) == 0 {
		starts = []int{len(d), 0}
	} else {
		for a := 0; a+len(n) <= len(d); a++ {
			if sameValue(d[a:a+len(n)], n) {
				starts = append(starts, a)
			}
		}
	}
	var out [][]any
	for _, a := range starts {
		b := a + len(n)
		tail := len(d) - b
		if len(l) < a+tail || !sameValue(l[:a], d[:a]) || !sameValue(l[len(l)-tail:], d[b:]) {
			continue
		}
		region := slices.Clone(l[a : len(l)-tail])
		if region == nil {
			region = []any{}
		}
		if sameValue(region, n) {
			continue
		}
		if !slices.ContainsFunc(out, func(o []any) bool { return sameValue(o, region) }) {
			out = append(out, region)
		}
	}
	return out
}

// mapRegion returns what a values map n must become so that the rendered map d
// becomes l, when n is a part of d. The keys that l lacks cannot be left out by
// a map that is merged with the defaults and the chart's own entries, so a
// removal is only expressed for the keys of n itself.
func mapRegion(d, l, n map[string]any) (map[string]any, bool) {
	if !subsetOf(n, d) {
		return nil, false
	}
	next := map[string]any{}
	for k, v := range n {
		if lv, ok := l[k]; ok {
			next[k] = lv
		} else if len(n) == 0 {
			return nil, false
		} // else: dropped from n
		_ = v
	}
	for k, lv := range l {
		if dv, ok := d[k]; !ok || !sameValue(dv, lv) {
			next[k] = lv
		}
	}
	for k := range d {
		if _, ok := l[k]; !ok {
			if _, inN := n[k]; !inN {
				return nil, false // removed from the render but not from this key
			}
		}
	}
	if sameValue(next, n) {
		return nil, false
	}
	return next, true
}

// finish sorts the candidates into the set, bounds their number per field by
// path affinity, and reports the fields that have too many.
func (g *structGen) finish(ss *structuredSet, opts Options) []FieldFailure {
	// The candidates of a field are bounded per level: the edits for the field
	// itself and the edits of its ancestors are tried in different stages.
	type slot struct{ field, level int }
	perField := map[slot][]int{}
	for i, c := range g.items {
		for _, o := range c.origin {
			perField[slot{o, g.levels[i]}] = append(perField[slot{o, g.levels[i]}], i)
		}
	}
	slots := slices.SortedFunc(maps.Keys(perField), func(a, b slot) int {
		if a.field != b.field {
			return a.field - b.field
		}
		return a.level - b.level
	})
	dropped := map[int]bool{}
	var failures []FieldFailure
	for _, sl := range slots {
		f, idx := sl.field, perField[sl]
		if len(idx) <= opts.MaxCandidates {
			continue
		}
		top := 0
		for _, i := range idx {
			top = max(top, g.items[i].score)
		}
		var tier []int
		for _, i := range idx {
			if g.items[i].score == top {
				tier = append(tier, i)
			}
		}
		if top <= 0 || len(tier) > opts.MaxCandidates {
			if g.fields[f].structured && sl.level == 0 {
				failures = append(failures, FieldFailure{Pointer: g.fields[f].Change.Path, Reason: ReasonTooManyCandidates,
					Detail: fmt.Sprintf("%d value keys could hold %s, at most %d are tried (for example %s)",
						len(idx), textOf(g.fields[f].live), opts.MaxCandidates, g.examples(idx, 3))})
			}
			for _, i := range idx {
				dropped[i] = true
			}
			continue
		}
		for _, i := range idx {
			if g.items[i].score != top {
				dropped[i] = true
			}
		}
	}
	for i, c := range g.items {
		if dropped[i] {
			for _, o := range c.origin {
				if ss.untried == nil {
					ss.untried = map[int][]untriedKey{}
				}
				ss.untried[o] = append(ss.untried[o], untriedKey{cand: c.cand, score: c.score,
					rank: stageRank(false, c.cand.Override, g.levels[i] == 1)})
			}
			continue
		}
		lvl := g.levels[i]
		if c.cand.Override {
			ss.overrides[lvl] = append(ss.overrides[lvl], *c)
		} else {
			ss.edits[lvl] = append(ss.edits[lvl], *c)
		}
	}
	return failures
}

// textOf bounds a value for a message.
func textOf(s string) string {
	if len(s) > 120 {
		return s[:117] + "..."
	}
	return s
}

// examples names some of the candidates for a message.
func (g *structGen) examples(idx []int, n int) string {
	var names []string
	for _, i := range idx {
		names = append(names, g.items[i].cand.Path.String())
		if len(names) == n {
			break
		}
	}
	return strings.Join(names, ", ")
}

// targetTokens are the words that name what a pointer is: its last name, and
// "pod" below the metadata of a pod template, where charts call the keys
// podAnnotations and podLabels.
func targetTokens(pointer string) map[string]bool {
	t := tokens(lastNames(pointer, 1)...)
	if strings.HasPrefix(pointer, "/spec/template/metadata/") {
		t["pod"] = true
	}
	return t
}
