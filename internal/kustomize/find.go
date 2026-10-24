package kustomize

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/sserkanml/backflow/api/v1alpha1"
	"github.com/sserkanml/backflow/internal/mapping"
	"github.com/sserkanml/backflow/internal/rendermap"
)

// Policy is what a BackflowPolicy allows.
type Policy struct {
	// EditBases allows edits of files outside the Application's path.
	EditBases bool
	// AddPatches allows adding a Backflow patch file to the Application's
	// kustomization as the last resort.
	AddPatches bool
	// Application is the name of the Argo CD Application, for messages.
	Application string
}

// kust is a kustomization file of the closed set.
type kust struct {
	file   string
	dir    string
	root   *yaml.Node
	inPath bool
}

type finder struct {
	src    *Source
	files  map[string][]byte
	origin *Origin
	obj    *unstructured.Unstructured
	group  string
	pol    Policy
	ks     []kust
	live   map[string]any
	// skipped are places that hold the value but were not offered, for the
	// explanation when nothing verifies.
	skipped []string
}

// FieldsFor makes the rendermap fields of a proposal's changes to one
// resource: the candidates in the tiers (see the Tier constants), or the reason
// the change cannot be mapped. origin is where the locate render found the
// resource; it may be nil, which leaves out the resource file.
func FieldsFor(src *Source, files map[string][]byte, origin *Origin, obj *unstructured.Unstructured, pol Policy,
	changes []v1alpha1.FieldChange) ([]rendermap.Field, error) {
	f := &finder{src: src, files: files, origin: origin, obj: obj, group: obj.GroupVersionKind().Group, pol: pol}
	if f.origin == nil {
		f.origin = &Origin{}
	}
	for _, name := range slices.Sorted(mapsKeys(files)) {
		if !isKustomizationFile(name) {
			continue
		}
		docs, err := mapping.Documents(files[name])
		if err != nil || len(docs) == 0 || docs[0].Root == nil {
			continue
		}
		f.ks = append(f.ks, kust{file: name, dir: path.Dir(name), root: docs[0].Root, inPath: src.InPath(name)})
	}
	f.live = liveObject(obj.Object, changes)
	fields := make([]rendermap.Field, len(changes))
	for i, ch := range changes {
		fields[i] = rendermap.Field{Index: i, Change: ch, Found: f.find(ch)}
	}
	return fields, nil
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

func isKustomizationFile(name string) bool {
	return slices.Contains(kustomizationNames, path.Base(name))
}

func (f *finder) find(ch v1alpha1.FieldChange) *rendermap.Found {
	found := &rendermap.Found{}
	if b := f.blocked(ch); b != nil {
		found.Blocked = b
		return found
	}
	tiers := map[int][]rendermap.Candidate{}
	segs := parseSegments(ch.Path)
	desired, scalar := scalarJSON(ch.Desired)
	replace := ch.Op == v1alpha1.OpReplace && scalar
	if replace {
		for _, c := range f.settings(ch, segs, desired) {
			tiers[c.Meta.(*Edit).Tier] = append(tiers[c.Meta.(*Edit).Tier], c)
		}
		for _, c := range f.patches(ch, segs, desired) {
			tiers[c.Meta.(*Edit).Tier] = append(tiers[c.Meta.(*Edit).Tier], c)
		}
	} else if ch.Op == v1alpha1.OpRemove {
		for _, c := range f.patches(ch, segs, "") {
			tiers[c.Meta.(*Edit).Tier] = append(tiers[c.Meta.(*Edit).Tier], c)
		}
	}
	if c := f.resourceFile(ch); c != nil {
		t := c.Meta.(*Edit).Tier
		tiers[t] = append(tiers[t], *c)
	}
	tiers[TierNewPatch] = append(tiers[TierNewPatch], f.newPatch(ch)...)
	var ordered []rendermap.Tier
	for _, t := range []struct {
		n    int
		name string
	}{
		{TierSetting, "a setting of the kustomization"}, {TierPatch, "a patch of the kustomization"},
		{TierResource, "the resource file"}, {TierBase, "a file outside the Application's path"},
		{TierNewPatch, "a new patch"},
	} {
		if t.n == TierBase && !f.pol.EditBases {
			continue
		}
		if cs := f.order(tiers[t.n]); len(cs) > 0 {
			ordered = append(ordered, rendermap.Tier{Name: t.name, Candidates: cs})
		}
	}
	if len(ordered) > 0 {
		found.Candidates = ordered[0].Candidates
		found.Later = ordered[1:]
	}
	if len(f.skipped) > 0 || len(ordered) == 0 {
		found.Diagnostics = diagnostics{skipped: slices.Clone(f.skipped)}
	}
	return found
}

// order puts the candidates that origin and transformer annotations point to
// first. It never drops one: the annotations are hints, not proof.
func (f *finder) order(cs []rendermap.Candidate) []rendermap.Candidate {
	hinted := func(c rendermap.Candidate) bool {
		if c.File == f.origin.File {
			return true
		}
		for _, t := range f.origin.Transformers {
			if t.ConfiguredIn == c.File {
				return true
			}
		}
		return false
	}
	slices.SortStableFunc(cs, func(a, b rendermap.Candidate) int {
		switch {
		case hinted(a) && !hinted(b):
			return -1
		case !hinted(a) && hinted(b):
			return 1
		}
		return 0
	})
	return cs
}

// scalarJSON decodes a JSON scalar and returns it as text.
func scalarJSON(raw string) (string, bool) {
	if raw == "" {
		return "", false
	}
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return "", false
	}
	switch x := v.(type) {
	case string:
		return x, true
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64), true
	case bool:
		return strconv.FormatBool(x), true
	}
	return "", false
}

type diagnostics struct{ skipped []string }

func (d diagnostics) Explain(*rendermap.Found, bool) []string {
	out := make([]string, 0, len(d.skipped))
	out = append(out, d.skipped...)
	return out
}

func (d diagnostics) ExplainEmpty(*rendermap.Found) []string {
	return append([]string{"no setting, patch or file of the kustomization holds the value"}, d.skipped...)
}

// ---- tier 1: settings of the kustomization -------------------------------

var hashSuffix = regexp.MustCompile(`-[a-z0-9]{8,10}$`)

func (f *finder) settings(ch v1alpha1.FieldChange, segs []string, desired string) []rendermap.Candidate {
	var out []rendermap.Candidate
	last := ""
	if len(segs) > 0 {
		last = segs[len(segs)-1]
	}
	isImage := last == "image" && slices.ContainsFunc(segs, isContainerList)
	for _, k := range f.ks {
		tier := TierSetting
		if !k.inPath {
			tier = TierBase
			if !f.pol.EditBases {
				if f.settingHolds(k, segs, desired, isImage) {
					f.skipped = append(f.skipped, fmt.Sprintf("%s holds the value, but it is outside the Application's path and spec.kustomize.editBases is off", k.file))
				}
				continue
			}
		}
		add := func(pointer, live string, op v1alpha1.ChangeOperation, fragment bool, note string) {
			e := &Edit{Tier: tier, Kind: KindSetting, Pointer: pointer, Op: op, Desired: ch.Desired, Live: live, Shared: !k.inPath,
				Note: note + " of " + k.file}
			c := rendermap.Candidate{File: k.file, Path: rendermap.KeyPathFromPointer(pointer), Variant: KindSetting, Meta: e}
			if fragment {
				c.Fragment = true
				var v string
				_ = json.Unmarshal([]byte(live), &v)
				c.NewValue = v
			}
			out = append(out, c)
		}
		switch {
		case isImage:
			f.imageSettings(k, desired, ch.Live, add)
		case slices.Equal(segs, specReplicas):
			for i, r := range seqItems(mapValue(k.root, keyReplicas)) {
				if v, _ := scalarText(mapValue(r, "count")); v == desired && f.nameMatches(mapValue(r, keyName)) {
					add(pointerOf(keyReplicas, strconv.Itoa(i), "count"), ch.Live, v1alpha1.OpReplace, false,
						fmt.Sprintf("replicas[%s].count", scalarOrEmpty(mapValue(r, keyName))))
				}
			}
		}
		if key, kind := labelKey(segs); kind != "" {
			for _, hit := range f.labelSettings(k, kind, key, desired) {
				add(hit, ch.Live, v1alpha1.OpReplace, false, strings.TrimPrefix(strings.ReplaceAll(hit, "/", "."), "."))
			}
		}
		if f.origin.Generated && k.file == f.origin.ConfiguredIn && len(segs) == 2 && segs[0] == "data" {
			out = append(out, f.generatorSettings(k, segs[1], desired, ch, tier)...)
		}
	}
	return out
}

// settingHolds reports whether a kustomization outside the path has a setting
// that could hold the value, for the explanation.
func (f *finder) settingHolds(k kust, segs []string, desired string, isImage bool) bool {
	switch {
	case isImage:
		for _, it := range seqItems(mapValue(k.root, "images")) {
			if f.imageEntryMatches(it, desired) {
				return true
			}
		}
	case slices.Equal(segs, specReplicas):
		for _, r := range seqItems(mapValue(k.root, keyReplicas)) {
			if v, _ := scalarText(mapValue(r, "count")); v == desired && f.nameMatches(mapValue(r, keyName)) {
				return true
			}
		}
	}
	return false
}

func seqItems(n *yaml.Node) []*yaml.Node {
	if n == nil || n.Kind != yaml.SequenceNode {
		return nil
	}
	return n.Content
}

func scalarOrEmpty(n *yaml.Node) string {
	s, _ := scalarText(n)
	return s
}

// nameMatches reports whether a name in a kustomization can be the resource's.
func (f *finder) nameMatches(n *yaml.Node) bool {
	name, ok := scalarText(n)
	if !ok {
		return false
	}
	return name == f.obj.GetName() || strings.Contains(f.obj.GetName(), name) || f.declaredName() == name
}

// declaredName is the name of the resource in the file that declares it.
func (f *finder) declaredName() string { return f.declaredNames()[0] }

// declaredNames are the names the resource can have before the transformers
// that rename it. With the origin it is the one name in the declaring file.
// Without it (a patch replaced the annotations that carry it) the rendered
// name comes first, then the name without the prefix and suffix of the
// kustomizations and of the Application; which one is right is for the render
// to say.
func (f *finder) declaredNames() []string {
	rendered := f.obj.GetName()
	if f.origin.File != "" {
		docs, err := mapping.Documents(f.files[f.origin.File])
		if err == nil && f.origin.Index < len(docs) && docs[f.origin.Index].Name != "" {
			return []string{docs[f.origin.Index].Name}
		}
		return []string{rendered}
	}
	names := []string{rendered}
	add := func(n string) {
		if n != "" && !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	o := f.src.Options
	prefixes, suffixes := []string{o.NamePrefix}, []string{o.NameSuffix}
	for _, k := range f.ks {
		prefixes = append(prefixes, scalarOrEmpty(mapValue(k.root, "namePrefix")))
		suffixes = append(suffixes, scalarOrEmpty(mapValue(k.root, "nameSuffix")))
	}
	for _, p := range prefixes {
		if p != "" && strings.HasPrefix(rendered, p) {
			add(strings.TrimPrefix(rendered, p))
		}
	}
	for _, s := range suffixes {
		if s != "" && strings.HasSuffix(rendered, s) {
			add(strings.TrimSuffix(rendered, s))
		}
	}
	return names
}

// splitImage returns the name, the tag and the digest of an image reference.
func splitImage(ref string) (name, tag, digest string) {
	if i := strings.Index(ref, "@"); i >= 0 {
		ref, digest = ref[:i], ref[i+1:]
	}
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		ref, tag = ref[:i], ref[i+1:]
	}
	return ref, tag, digest
}

func (f *finder) imageEntryMatches(it *yaml.Node, desiredImage string) bool {
	name, _, _ := splitImage(desiredImage)
	eff := scalarOrEmpty(mapValue(it, "newName"))
	if eff == "" {
		eff = scalarOrEmpty(mapValue(it, keyName))
	}
	return eff == name
}

func (f *finder) imageSettings(k kust, desired, liveJSON string, add func(pointer, live string, op v1alpha1.ChangeOperation, fragment bool, note string)) {
	var live string
	if json.Unmarshal([]byte(liveJSON), &live) != nil {
		return
	}
	dn, dt, dd := splitImage(desired)
	ln, lt, ld := splitImage(live)
	for i, it := range seqItems(mapValue(k.root, "images")) {
		if !f.imageEntryMatches(it, desired) {
			continue
		}
		entry := "images[" + scalarOrEmpty(mapValue(it, keyName)) + "]"
		set := func(key, value string) {
			op := v1alpha1.OpReplace
			if mapValue(it, key) == nil {
				op = v1alpha1.OpAdd
			}
			b, _ := json.Marshal(value)
			add(pointerOf("images", strconv.Itoa(i), key), string(b), op, true, entry+"."+key)
		}
		if ln != dn {
			set("newName", ln)
		}
		switch {
		case ld != "" && ld != dd:
			set("digest", ld)
		case ld == "" && lt != dt && lt != "":
			set("newTag", lt)
		}
	}
}

// labelKey tells whether the pointer is a label or an annotation of the
// resource or its pod template, and its key.
func labelKey(segs []string) (key, kind string) {
	if len(segs) < 3 {
		return "", ""
	}
	switch segs[len(segs)-2] {
	case keyLabels, "matchLabels":
		return segs[len(segs)-1], keyLabels
	case keyAnnotations:
		return segs[len(segs)-1], keyAnnotations
	}
	return "", ""
}

func (f *finder) labelSettings(k kust, kind, key, desired string) []string {
	var out []string
	check := func(m *yaml.Node, prefix ...string) {
		if m != nil && m.Kind == yaml.MappingNode {
			if v, _ := scalarText(mapValue(m, key)); v == desired && mapValue(m, key) != nil {
				out = append(out, pointerOf(append(prefix, key)...))
			}
		}
	}
	if kind == keyLabels {
		check(mapValue(k.root, "commonLabels"), "commonLabels")
		for i, l := range seqItems(mapValue(k.root, keyLabels)) {
			check(mapValue(l, "pairs"), keyLabels, strconv.Itoa(i), "pairs")
		}
	} else {
		check(mapValue(k.root, "commonAnnotations"), "commonAnnotations")
	}
	return out
}

// generatorSettings offers the literals and the env file lines of an unhashed
// generator that give a data key.
func (f *finder) generatorSettings(k kust, key, desired string, ch v1alpha1.FieldChange, tier int) []rendermap.Candidate {
	var out []rendermap.Candidate
	for _, gk := range []string{"configMapGenerator", "secretGenerator"} {
		for gi, g := range seqItems(mapValue(k.root, gk)) {
			if !f.generatorIs(g) {
				continue
			}
			var live string
			_ = json.Unmarshal([]byte(ch.Live), &live)
			for li, lit := range seqItems(mapValue(g, "literals")) {
				if lit.Value == key+"="+desired {
					b, _ := json.Marshal(key + "=" + live)
					pointer := pointerOf(gk, strconv.Itoa(gi), "literals", strconv.Itoa(li))
					e := &Edit{Tier: tier, Kind: KindSetting, Pointer: pointer, Op: v1alpha1.OpReplace, Desired: ch.Desired, Live: string(b),
						Shared: !k.inPath, Note: fmt.Sprintf("literal %s of %s %q of %s", key, gk, scalarOrEmpty(mapValue(g, keyName)), k.file)}
					out = append(out, rendermap.Candidate{File: k.file, Path: rendermap.KeyPathFromPointer(pointer), Variant: KindSetting, Meta: e})
				}
			}
			for _, env := range append(seqItems(mapValue(g, "envs")), mapValue(g, "env")) {
				file, ok := scalarText(env)
				if !ok || file == "" {
					continue
				}
				target := path.Join(k.dir, file)
				for line := range strings.SplitSeq(string(f.files[target]), "\n") {
					if strings.TrimRight(line, "\r") == key+"="+desired {
						e := &Edit{Tier: tier, Kind: KindEnvFile, Pointer: key, Op: v1alpha1.OpReplace, Desired: ch.Desired, Live: ch.Live,
							Shared: !k.inPath, Note: fmt.Sprintf("line %s of %s", key, target)}
						out = append(out, rendermap.Candidate{File: target, Path: rendermap.KeyPath{key}, Variant: KindEnvFile, Meta: e})
					}
				}
			}
		}
	}
	return out
}

// generatorIs reports whether a generator can be the one the resource comes
// from: its name is the resource's name without the hash and the prefix.
func (f *finder) generatorIs(g *yaml.Node) bool {
	name := scalarOrEmpty(mapValue(g, keyName))
	rendered := hashSuffix.ReplaceAllString(f.obj.GetName(), "")
	return name != "" && (rendered == name || strings.Contains(f.obj.GetName(), name))
}

// hashedGenerator returns the generator of the resource when it adds a hash to
// the name.
func (f *finder) hashedGenerator() (kust, string, bool) {
	for _, k := range f.ks {
		if k.file != f.origin.ConfiguredIn {
			continue
		}
		global := scalarOrEmpty(mapValue(mapValue(k.root, "generatorOptions"), "disableNameSuffixHash")) == textTrue
		for _, gk := range []string{"configMapGenerator", "secretGenerator"} {
			for _, g := range seqItems(mapValue(k.root, gk)) {
				if !f.generatorIs(g) {
					continue
				}
				disabled := global
				if o := mapValue(mapValue(g, "options"), "disableNameSuffixHash"); o != nil {
					disabled = o.Value == textTrue
				}
				return k, scalarOrEmpty(mapValue(g, keyName)), !disabled
			}
		}
	}
	return kust{}, "", false
}
