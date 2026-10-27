package kustomize

import (
	"encoding/json"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/sserkanml/backflow/api/v1alpha1"
	"github.com/sserkanml/backflow/internal/jsonedit"
	"github.com/sserkanml/backflow/internal/mapping"
	"github.com/sserkanml/backflow/internal/rendermap"
)

// Writer edits the files of a Kustomize source for the candidates of
// FieldsFor. It keeps comments, key order, indentation and quoting.
type Writer struct{}

var _ rendermap.ContextWriter = Writer{}

// isJSONName reports whether a file is edited as JSON.
func isJSONName(file string) bool { return strings.EqualFold(path.Ext(file), ".json") }

// Write implements rendermap.Writer for an edit that touches no other file.
func (w Writer) Write(c rendermap.Candidate, content []byte, value string) (*rendermap.Written, error) {
	return w.WriteWith(c, map[string][]byte{c.File: content}, value)
}

// WriteWith implements rendermap.ContextWriter.
func (Writer) WriteWith(c rendermap.Candidate, files map[string][]byte, value string) (*rendermap.Written, error) {
	e, ok := c.Meta.(*Edit)
	if !ok {
		return nil, fmt.Errorf("candidate for %s has no edit", c.File)
	}
	live := e.Live
	if c.Fragment {
		b, _ := json.Marshal(value)
		live = string(b)
	}
	content := files[c.File]
	var (
		edited []byte
		err    error
	)
	switch e.Kind {
	case KindSetting, KindPatchFile, KindResource:
		edited, err = applyPointer(c.File, content, e.Document, e.Pointer, e.Op, e.Desired, live)
	case KindJSON6902File:
		if isJSONName(c.File) {
			// A patch in a .json file is a JSON list: edited as JSON, in place.
			edited, err = jsonedit.Apply(content, jsonedit.Op{Kind: jsonedit.Replace, Pointer: e.Pointer, Value: live})
			break
		}
		var s string
		s, err = replaceScalar(string(content), e.Pointer, live)
		edited = []byte(s)
	case KindPatchInline, KindJSON6902Inline:
		edited, err = editInline(c.File, content, e, live)
	case KindEnvFile:
		edited, err = editEnvLine(content, e.Pointer, live)
	case KindNewPatch:
		return writeNewPatch(c, e, files)
	default:
		err = fmt.Errorf("unknown kind of edit %q", e.Kind)
	}
	if err != nil {
		return nil, err
	}
	return result(c.File, edited, c.Path.Pointer(), live), nil
}

func result(file string, edited []byte, location, live string) *rendermap.Written {
	return &rendermap.Written{Result: mapping.Result{File: file, Edited: edited,
		Edits: []mapping.Edit{{File: file, Location: location, Value: live}}}}
}

func applyPointer(file string, content []byte, doc int, pointer string, op v1alpha1.ChangeOperation, desired, live string) ([]byte, error) {
	// A scalar that is replaced is replaced where it stands, which keeps the
	// layout of the line and of the list around it.
	if op == v1alpha1.OpReplace && doc == 0 && live != "" && !isJSONName(file) {
		if docs, err := mapping.Documents(content); err == nil && len(docs) == 1 {
			if n := nodeAt(docs[0].Root, pointer); n != nil && n.Kind == yaml.ScalarNode {
				if s, err := replaceScalar(string(content), pointer, live); err == nil {
					return []byte(s), nil
				}
			}
		}
	}
	return mapping.Apply(file, content, doc, []v1alpha1.FieldChange{{Path: pointer, Op: op, Desired: desired, Live: live}})
}

// editInline edits a patch that is a string in a kustomization: the patches
// entries with a patch, patchesStrategicMerge entries and patchesJson6902
// entries. The string keeps its style (a block scalar stays one).
func editInline(file string, content []byte, e *Edit, live string) ([]byte, error) {
	docs, err := mapping.Documents(content)
	if err != nil || len(docs) == 0 {
		return nil, fmt.Errorf("%s does not parse", file)
	}
	holder := nodeAt(docs[0].Root, e.Holder)
	text, ok := scalarText(holder)
	if !ok {
		return nil, fmt.Errorf("%s of %s is not a string", e.Holder, file)
	}
	var next string
	switch trimmed := strings.TrimSpace(text); {
	case strings.HasPrefix(trimmed, "[") || strings.HasPrefix(trimmed, "{"):
		next, err = editJSONText(text, e, live)
	case e.Kind == KindJSON6902Inline:
		next, err = replaceScalar(text, e.Pointer, live)
	default:
		var b []byte
		b, err = applyPointer(file, []byte(text), 0, e.Pointer, e.Op, e.Desired, live)
		next = string(b)
	}
	if err != nil {
		return nil, fmt.Errorf("%s in %s: %w", e.Holder, file, err)
	}
	quoted, _ := json.Marshal(next)
	return applyPointer(file, content, e.Document, e.Holder, v1alpha1.OpReplace, "", string(quoted))
}

// editJSONText replaces a scalar in a patch written as JSON; the result is
// compact JSON.
func editJSONText(text string, e *Edit, live string) (string, error) {
	root, err := parseRoot(text)
	if err != nil {
		return "", err
	}
	n := nodeAt(root, e.Pointer)
	if n == nil || n.Kind != yaml.ScalarNode {
		return "", fmt.Errorf("%s is not a scalar", e.Pointer)
	}
	if err := setScalar(n, live); err != nil {
		return "", err
	}
	return compactJSON(root)
}

// writeNewPatch adds the drifted fields to the Backflow patch file of a
// resource, creating the file, and lists it in the kustomization.
func writeNewPatch(c rendermap.Candidate, e *Edit, files map[string][]byte) (*rendermap.Written, error) {
	var out []byte
	var err error
	if existing := files[c.File]; len(existing) > 0 {
		out, err = mergeIntoFile(existing, e.Patch)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", c.File, err)
		}
	} else if out, err = marshalPatch(e.Patch); err != nil {
		return nil, err
	}
	kfile := e.Kustomization
	registered, err := registerPatch(kfile, files[kfile], c.File)
	if err != nil {
		return nil, err
	}
	w := result(c.File, out, c.Path.Pointer(), e.Live)
	w.Also = []mapping.Result{{File: kfile, Edited: registered,
		Edits: []mapping.Edit{{File: kfile, Location: "/patches/-", Value: fmt.Sprintf("%q", relativeTo(kfile, c.File))}}}}
	return w, nil
}

// mergeIntoFile merges a patch into the content of an existing patch file,
// keeping the comments of the file.
func mergeIntoFile(content []byte, add map[string]any) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(content, &doc); err != nil {
		return nil, fmt.Errorf("does not parse: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("is not a mapping")
	}
	mergeNode(doc.Content[0], add)
	return encodeNode(&doc)
}

func encodeNode(n *yaml.Node) ([]byte, error) {
	var b strings.Builder
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(n); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}

func valueNode(v any) *yaml.Node {
	var n yaml.Node
	_ = n.Encode(v)
	return &n
}

// mergeNode merges the strategic merge patch add into a mapping node: maps are
// merged, lists of mappings by their merge key, anything else is replaced.
func mergeNode(dst *yaml.Node, add map[string]any) {
	for _, k := range slices.Sorted(maps.Keys(add)) {
		v := add[k]
		cur := mapValue(dst, k)
		if cur == nil {
			dst.Content = append(dst.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k}, valueNode(v))
			continue
		}
		switch av := v.(type) {
		case map[string]any:
			if cur.Kind == yaml.MappingNode {
				mergeNode(cur, av)
				continue
			}
		case []any:
			if cur.Kind == yaml.SequenceNode {
				mergeSeq(cur, av)
				continue
			}
		}
		repl := valueNode(v)
		repl.HeadComment, repl.LineComment, repl.FootComment = cur.HeadComment, cur.LineComment, cur.FootComment
		*cur = *repl
	}
}

func mergeSeq(dst *yaml.Node, add []any) {
	for _, a := range add {
		am, ok := a.(map[string]any)
		if !ok {
			dst.Content = valueNode(add).Content // a list without a merge key is replaced
			return
		}
		key := listKey(am)
		merged := false
		for _, item := range dst.Content {
			if v, _ := scalarText(mapValue(item, key)); key != "" && item.Kind == yaml.MappingNode && v == fmt.Sprint(am[key]) {
				mergeNode(item, am)
				merged = true
				break
			}
		}
		if !merged {
			dst.Content = append(dst.Content, valueNode(a))
		}
	}
}

func relativeTo(kustomization, file string) string {
	return strings.TrimPrefix(file, path.Dir(kustomization)+"/")
}

func marshalPatch(patch map[string]any) ([]byte, error) {
	var b strings.Builder
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(patch); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}

// registerPatch lists a patch file under patches: in a kustomization, once.
func registerPatch(kfile string, content []byte, patchFile string) ([]byte, error) {
	rel := relativeTo(kfile, patchFile)
	text := string(content)
	root, err := parseRoot(text)
	if err != nil {
		return nil, fmt.Errorf("%s does not parse: %w", kfile, err)
	}
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s is not a mapping", kfile)
	}
	list := mapValue(root, keyPatches)
	if list != nil && list.Kind == yaml.SequenceNode {
		for _, item := range list.Content {
			if p, _ := scalarText(mapValue(item, "path")); p == rel {
				return content, nil
			}
		}
	}
	if list == nil || list.Kind != yaml.SequenceNode || list.Style&yaml.FlowStyle != 0 || len(list.Content) == 0 {
		items := make([]any, 0, 1)
		if list != nil && list.Kind == yaml.SequenceNode {
			if err := list.Decode(&items); err != nil {
				return nil, err
			}
		}
		items = append(items, map[string]any{"path": rel})
		b, _ := json.Marshal(items)
		op := v1alpha1.OpAdd
		if list != nil {
			op = v1alpha1.OpReplace
		}
		return applyPointer(kfile, content, 0, "/"+keyPatches, op, "", string(b))
	}
	return appendItem(text, root, list, "path: "+yamlString(rel)), nil
}

func yamlString(s string) string {
	if plainSafe(s) {
		return s
	}
	b, _ := json.Marshal(s)
	return string(b)
}

// appendItem adds a one-line mapping item after the last item of a block
// sequence, with the indentation of its items.
func appendItem(text string, root, list *yaml.Node, entry string) []byte {
	lines := strings.Split(text, "\n")
	indent := max(list.Content[0].Column-3, 0) // "- " is before the item
	// The sequence ends before the next key of the root mapping, or at the end.
	limit := len(lines)
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i+1] == list && i+2 < len(root.Content) {
			limit = root.Content[i+2].Line - 1 // the line of the next key
		}
	}
	last := list.Content[0].Line
	for l := len(lines[:limit]); l >= list.Content[0].Line; l-- {
		t := strings.TrimSpace(lines[l-1])
		if t != "" && !strings.HasPrefix(t, "#") {
			last = l
			break
		}
	}
	item := strings.Repeat(" ", indent) + "- " + entry
	out := append(append(append([]string{}, lines[:last]...), item), lines[last:]...)
	return []byte(strings.Join(out, "\n"))
}

// mergePatch merges the strategic merge patch add into dst: maps are merged,
// lists of mappings are merged by the merge key they share, anything else is
// replaced.
func mergePatch(dst, add map[string]any) {
	for k, v := range add {
		switch av := v.(type) {
		case map[string]any:
			if dm, ok := dst[k].(map[string]any); ok {
				mergePatch(dm, av)
				continue
			}
		case []any:
			if dl, ok := dst[k].([]any); ok {
				dst[k] = mergeList(dl, av)
				continue
			}
		}
		dst[k] = v
	}
}

func mergeList(dst, add []any) []any {
	for _, a := range add {
		am, ok := a.(map[string]any)
		if !ok {
			return add // a list without a merge key is replaced as a whole
		}
		key := listKey(am)
		merged := false
		for i, d := range dst {
			dm, ok := d.(map[string]any)
			if ok && key != "" && fmt.Sprint(dm[key]) == fmt.Sprint(am[key]) {
				mergePatch(dm, am)
				dst[i] = dm
				merged = true
				break
			}
		}
		if !merged {
			dst = append(dst, a)
		}
	}
	return dst
}

// listKey is the merge key an item of a patch list carries.
func listKey(item map[string]any) string {
	for _, k := range []string{keyName, "containerPort", "mountPath", "devicePath", "port", "ip", "topologyKey"} {
		if _, ok := item[k]; ok {
			return k
		}
	}
	return ""
}

// editEnvLine sets the value of a key in an env file ("key=value" lines).
func editEnvLine(content []byte, key, liveJSON string) ([]byte, error) {
	var live string
	if err := json.Unmarshal([]byte(liveJSON), &live); err != nil {
		return nil, fmt.Errorf("an env file line holds a string: %w", err)
	}
	lines := strings.Split(string(content), "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, key+"=") {
			lines[i] = key + "=" + live
			return []byte(strings.Join(lines, "\n")), nil
		}
	}
	return nil, fmt.Errorf("the env file has no line for %s", key)
}
