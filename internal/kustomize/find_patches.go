package kustomize

import (
	"encoding/json"
	"fmt"
	"path"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/sserkanml/backflow/api/v1alpha1"
	"github.com/sserkanml/backflow/internal/mapping"
	"github.com/sserkanml/backflow/internal/rendermap"
)

// KindEnvFile is a line of an env file of a generator.
const KindEnvFile = "env-file"

// patchHolder is a patch of a kustomization: a file or a string in it.
type patchHolder struct {
	pointer string // of the entry in the kustomization
	file    string // repository path, for a file patch
	inline  string // pointer of the string in the kustomization, for an inline patch
	text    string
	kind    string // target kind of the entry, "" for any
	name    string // target name, "" for any
}

func (f *finder) holders(k kust) []patchHolder {
	var out []patchHolder
	fromEntry := func(list string, i int, it *yaml.Node) {
		h := patchHolder{pointer: pointerOf(list, strconv.Itoa(i))}
		if t := mapValue(it, "target"); t != nil {
			h.kind, h.name = scalarOrEmpty(mapValue(t, "kind")), scalarOrEmpty(mapValue(t, keyName))
		}
		if p, ok := scalarText(mapValue(it, "path")); ok && p != "" {
			h.file = path.Join(k.dir, p)
			h.text = string(f.files[h.file])
		} else if s, ok := scalarText(mapValue(it, "patch")); ok {
			h.inline = h.pointer + "/patch"
			h.text = s
		} else {
			return
		}
		out = append(out, h)
	}
	for _, list := range []string{keyPatches, "patchesJson6902"} {
		for i, it := range seqItems(mapValue(k.root, list)) {
			fromEntry(list, i, it)
		}
	}
	for i, it := range seqItems(mapValue(k.root, "patchesStrategicMerge")) {
		s, _ := scalarText(it)
		h := patchHolder{pointer: pointerOf("patchesStrategicMerge", strconv.Itoa(i))}
		if strings.ContainsAny(s, "\n{") {
			h.inline, h.text = h.pointer, s
		} else {
			h.file = path.Join(k.dir, s)
			h.text = string(f.files[h.file])
		}
		out = append(out, h)
	}
	return out
}

// patches offers the values inside the patches the kustomizations already
// have: the field of a strategic merge patch, a JSON 6902 operation.
func (f *finder) patches(ch v1alpha1.FieldChange, segs []string, desired string) []rendermap.Candidate {
	steps, stepsOK := stepsOf(f.group, f.obj.Object, segs)
	var out []rendermap.Candidate
	for _, k := range f.ks {
		for _, h := range f.holders(k) {
			inPath := k.inPath
			if h.file != "" {
				inPath = f.src.InPath(h.file)
			}
			tier := TierPatch
			if !inPath {
				tier = TierBase
			}
			editFile := k.file
			if h.file != "" {
				editFile = h.file
			}
			for _, d := range f.patchDocs(h) {
				var pointer, kind string
				switch {
				case d.root.Kind == yaml.MappingNode && stepsOK && f.docTargets(d):
					p, n, ok := findInPatch(yamlNode{d.root}, steps)
					if !ok {
						continue
					}
					if ch.Op == v1alpha1.OpReplace {
						if v, isScalar := n.scalar(); !isScalar || v != desired {
							continue
						}
					}
					pointer, kind = p, KindPatchFile
					if h.inline != "" {
						kind = KindPatchInline
					}
				case d.root.Kind == yaml.SequenceNode && ch.Op == v1alpha1.OpReplace && f.holderTargets(h):
					idx := jsonOp(d.root, ch.Path, desired)
					if idx < 0 {
						continue
					}
					pointer, kind = pointerOf(strconv.Itoa(idx), "value"), KindJSON6902File
					if h.inline != "" {
						kind = KindJSON6902Inline
					}
				default:
					continue
				}
				if tier == TierBase && !f.pol.EditBases {
					f.skipped = append(f.skipped, fmt.Sprintf("%s holds the value, but it is outside the Application's path and spec.kustomize.editBases is off", editFile))
					continue
				}
				e := &Edit{Tier: tier, Kind: kind, Document: d.index, Holder: h.inline, Pointer: pointer, Op: ch.Op, Desired: ch.Desired, Live: ch.Live,
					Shared: tier == TierBase, Note: fmt.Sprintf("%s in %s", strings.TrimPrefix(ch.Path, "/"), editFile)}
				if h.inline == "" {
					e.Holder = ""
				}
				out = append(out, rendermap.Candidate{File: editFile, Path: rendermap.KeyPathFromPointer(e.Holder + pointer), Variant: kind, Meta: e})
			}
		}
	}
	return out
}

type patchDoc struct {
	root  *yaml.Node
	index int
	kind  string
	name  string
	group string
}

func (f *finder) patchDocs(h patchHolder) []patchDoc {
	if strings.TrimSpace(h.text) == "" {
		return nil
	}
	if h.inline != "" {
		root, err := parseRoot(h.text)
		if err != nil {
			return nil
		}
		return []patchDoc{docOf(root, 0)}
	}
	docs, err := mapping.Documents([]byte(h.text))
	if err != nil {
		return nil
	}
	var out []patchDoc
	for _, d := range docs {
		if d.Root != nil {
			out = append(out, docOf(d.Root, d.Index))
		}
	}
	return out
}

func docOf(root *yaml.Node, index int) patchDoc {
	d := patchDoc{root: root, index: index}
	if root.Kind == yaml.MappingNode {
		d.kind = scalarOrEmpty(mapValue(root, "kind"))
		d.name = scalarOrEmpty(mapValue(mapValue(root, keyMetadata), keyName))
		if g, _, found := strings.Cut(scalarOrEmpty(mapValue(root, "apiVersion")), "/"); found {
			d.group = g
		}
	}
	return d
}

// docTargets reports whether a strategic merge patch is for the resource.
func (f *finder) docTargets(d patchDoc) bool {
	if d.kind != f.obj.GetKind() || d.group != f.group {
		return false
	}
	name := f.obj.GetName()
	return d.name == name || d.name == f.declaredName() || (d.name != "" && strings.Contains(name, d.name))
}

// holderTargets applies the target of a patches entry to the resource.
func (f *finder) holderTargets(h patchHolder) bool {
	if h.kind != "" && h.kind != f.obj.GetKind() {
		return false
	}
	if h.name != "" && h.name != f.obj.GetName() && h.name != f.declaredName() {
		return false
	}
	return true
}

// jsonOp returns the index of a replace or add operation of a JSON 6902 patch
// that sets the pointer to the scalar, or -1.
func jsonOp(list *yaml.Node, pointer, desired string) int {
	for i, op := range list.Content {
		name := scalarOrEmpty(mapValue(op, "op"))
		if (name != "replace" && name != "add") || scalarOrEmpty(mapValue(op, "path")) != pointer {
			continue
		}
		if v, ok := scalarText(mapValue(op, "value")); ok && v == desired {
			return i
		}
	}
	return -1
}

// ---- tier 3 and 4: the resource file --------------------------------------

func (f *finder) resourceFile(ch v1alpha1.FieldChange) *rendermap.Candidate {
	file := f.origin.File
	if file == "" {
		return nil
	}
	content, ok := f.files[file]
	if !ok {
		return nil
	}
	docs, err := mapping.DocumentsOf(file, content)
	if err != nil || f.origin.Index >= len(docs) || docs[f.origin.Index].Kind != f.obj.GetKind() {
		return nil
	}
	tier := TierResource
	if !f.src.InPath(file) {
		tier = TierBase
		if !f.pol.EditBases {
			f.skipped = append(f.skipped, fmt.Sprintf("%s declares the resource, but it is outside the Application's path and spec.kustomize.editBases is off", file))
			return nil
		}
	}
	e := &Edit{Tier: tier, Kind: KindResource, Document: f.origin.Index, Pointer: ch.Path, Op: ch.Op, Desired: ch.Desired, Live: ch.Live,
		Shared: tier == TierBase, Note: strings.TrimPrefix(ch.Path, "/") + " in " + file}
	return &rendermap.Candidate{File: file, Path: rendermap.KeyPathFromPointer(ch.Path), Variant: KindResource, Meta: e}
}

// ---- tier 5: a new patch --------------------------------------------------

func (f *finder) newPatch(ch v1alpha1.FieldChange) []rendermap.Candidate {
	if !f.pol.AddPatches || f.origin.Generated {
		return nil
	}
	var overlay *kust
	for i := range f.ks {
		if f.ks[i].dir == f.src.Dir {
			overlay = &f.ks[i]
			break
		}
	}
	if overlay == nil {
		return nil
	}
	var liveValue any
	if ch.Live != "" {
		if err := json.Unmarshal([]byte(ch.Live), &liveValue); err != nil {
			return nil
		}
	}
	body, ok := buildPatch(f.group, f.obj.Object, f.live, parseSegments(ch.Path), ch.Op, liveValue)
	if !ok {
		return nil
	}
	var out []rendermap.Candidate
	for _, name := range f.declaredNames() {
		patch := map[string]any{
			"apiVersion": f.obj.GetAPIVersion(), "kind": f.obj.GetKind(),
			"metadata": map[string]any{"name": name},
		}
		mergePatch(patch, body)
		file := path.Join(f.src.Dir, patchesDirectory, strings.ToLower(f.obj.GetKind())+"-"+name+".yaml")
		_, exists := f.files[file]
		e := &Edit{Tier: TierNewPatch, Kind: KindNewPatch, Pointer: ch.Path, Op: ch.Op, Desired: ch.Desired, Live: ch.Live,
			Kustomization: overlay.file, Patch: patch, Note: strings.TrimPrefix(ch.Path, "/") + " in " + file}
		out = append(out, rendermap.Candidate{File: file, Path: rendermap.KeyPathFromPointer(ch.Path), Variant: KindNewPatch, Meta: e, NewFile: !exists})
	}
	return out
}

// buildPatch makes the strategic merge patch of one change: the nested keys
// down to the field, the list items found by their merge key on the way, and
// the whole live list where a list has no merge key.
func buildPatch(group string, desired, live any, segs []string, op v1alpha1.ChangeOperation, value any) (map[string]any, bool) {
	out, ok := patchAt(group, desired, live, segs, "", "", op, value)
	m, isMap := out.(map[string]any)
	return m, ok && isMap
}

func patchAt(group string, des, live any, segs []string, parent, grand string, op v1alpha1.ChangeOperation, value any) (any, bool) {
	if len(segs) == 0 {
		if op == v1alpha1.OpRemove {
			return nil, true
		}
		dl, desIsList := des.([]any)
		ll, liveIsList := value.([]any)
		if mk := mergeKey(group, parent, grand); mk != "" && desIsList && liveIsList {
			return listPatch(mk, dl, ll)
		}
		return value, true
	}
	switch c := des.(type) {
	case map[string]any:
		lm, _ := live.(map[string]any)
		sub, ok := patchAt(group, c[segs[0]], lm[segs[0]], segs[1:], segs[0], parent, op, value)
		return map[string]any{segs[0]: sub}, ok
	case []any:
		ll, _ := live.([]any)
		mk := mergeKey(group, parent, grand)
		i, err := strconv.Atoi(segs[0])
		if mk == "" || err != nil || i < 0 || i > len(c) {
			// Strategic merge replaces this list as a whole.
			if ll == nil {
				return nil, false
			}
			return ll, true
		}
		if i == len(c) { // an item is added
			item, isMap := value.(map[string]any)
			if op != v1alpha1.OpAdd || len(segs) != 1 || !isMap {
				return nil, false
			}
			return append(keyOnly(mk, c), item), true
		}
		item, _ := c[i].(map[string]any)
		key := item[mk]
		// The items before this one are listed by key, so the merge keeps their
		// order: kustomize puts the items of a patch first.
		before := keyOnly(mk, c[:i])
		if len(segs) == 1 {
			switch op {
			case v1alpha1.OpRemove:
				return append(before, map[string]any{mk: key, "$patch": "delete"}), true
			case v1alpha1.OpReplace, v1alpha1.OpAdd:
				if m, isMap := value.(map[string]any); isMap {
					return append(before, m), true
				}
			}
			return nil, false
		}
		var liveItem any
		for _, l := range ll {
			if lm, isMap := l.(map[string]any); isMap && fmt.Sprint(lm[mk]) == fmt.Sprint(key) {
				liveItem = lm
			}
		}
		sub, ok := patchAt(group, item, liveItem, segs[1:], parent, grand, op, value)
		sm, isMap := sub.(map[string]any)
		if !isMap {
			return nil, false
		}
		sm[mk] = key
		return append(before, sm), ok
	}
	// Below a value the resource does not have: keys to add.
	sub, ok := patchAt(group, nil, nil, segs[1:], segs[0], parent, op, value)
	if _, isMap := des.(map[string]any); !isMap && des != nil {
		return nil, false
	}
	return map[string]any{segs[0]: sub}, ok
}

// liveObject applies the changes to a copy of the rendered object: the state
// the cluster has.
func liveObject(obj map[string]any, changes []v1alpha1.FieldChange) map[string]any {
	b, _ := json.Marshal(obj)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	for _, ch := range changes {
		var v any
		if ch.Live != "" {
			_ = json.Unmarshal([]byte(ch.Live), &v)
		}
		setAt(out, parseSegments(ch.Path), ch.Op, v)
	}
	return out
}

func setAt(root map[string]any, segs []string, op v1alpha1.ChangeOperation, v any) {
	var cur any = root
	var parents []any
	for i, seg := range segs {
		last := i == len(segs)-1
		switch c := cur.(type) {
		case map[string]any:
			switch {
			case last && op == v1alpha1.OpRemove:
				delete(c, seg)
			case last:
				c[seg] = v
			default:
				if c[seg] == nil {
					c[seg] = map[string]any{}
				}
				parents = append(parents, c)
				cur = c[seg]
			}
		case []any:
			idx, err := strconv.Atoi(seg)
			if err != nil || idx < 0 || idx > len(c) {
				return
			}
			switch {
			case last && op == v1alpha1.OpRemove && idx < len(c):
				replaceInParent(parents, segs[i-1], append(c[:idx:idx], c[idx+1:]...))
			case last && idx == len(c):
				replaceInParent(parents, segs[i-1], append(c, v))
			case last:
				c[idx] = v
			default:
				if idx == len(c) {
					return
				}
				parents = append(parents, c)
				cur = c[idx]
			}
		default:
			return
		}
	}
}

func replaceInParent(parents []any, key string, list []any) {
	if len(parents) == 0 {
		return
	}
	if m, ok := parents[len(parents)-1].(map[string]any); ok {
		m[key] = list
	} else if l, ok := parents[len(parents)-1].([]any); ok {
		if idx, err := strconv.Atoi(key); err == nil && idx < len(l) {
			l[idx] = list
		}
	}
}

// keyOnly lists items by their merge key alone.
func keyOnly(mk string, items []any) []any {
	out := make([]any, 0, len(items)+1)
	for _, it := range items {
		m, _ := it.(map[string]any)
		out = append(out, map[string]any{mk: m[mk]})
	}
	return out
}

// listPatch is the patch that turns the list desired into live when both are
// lists of items with a merge key: the items of live in their order, and a
// delete directive for each item that is gone.
func listPatch(mk string, desired, live []any) (any, bool) {
	var out []any
	liveKeys := map[string]bool{}
	for _, it := range live {
		m, isMap := it.(map[string]any)
		if !isMap {
			return nil, false
		}
		if _, has := m[mk]; !has {
			return nil, false
		}
		liveKeys[fmt.Sprint(m[mk])] = true
		out = append(out, m)
	}
	for _, it := range desired {
		m, _ := it.(map[string]any)
		if !liveKeys[fmt.Sprint(m[mk])] {
			out = append(out, map[string]any{mk: m[mk], "$patch": "delete"})
		}
	}
	return out, true
}
