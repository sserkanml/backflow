package mapping

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/sserkanml/backflow/api/v1alpha1"
	"github.com/sserkanml/backflow/internal/jsonedit"
)

// JSON files are read the way Argo CD reads them for a Directory source
// (reposerver/repository/repository.go, getObjsFromYAMLOrJSON, v3.5.3):
//
//   - a file whose name ends in ".json" (case-sensitive) holds exactly one JSON
//     object; a second value in the file is an error, and so is an array;
//   - an object that has an "items" array is a list (kind List, or any kind: the
//     test is the array), and its items are the resources; "items": null is an
//     empty list;
//   - other files (.yaml, .yml) are read as YAML or JSON documents, several per
//     file, and are edited as YAML.
//
// The documents of a JSON file are numbered like the documents of a YAML file:
// one for an object, one per item for a list.

// isJSONFile reports whether Argo CD reads the file as a single JSON object.
func isJSONFile(name string) bool { return strings.HasSuffix(name, ".json") }

// jsonDoc is a resource of a JSON file.
type jsonDoc struct {
	// Prefix is the JSON pointer of the resource in the file: "" for a file that
	// is one object, "/items/N" for an item of a list.
	Prefix string
	// Obj is the resource.
	Obj map[string]any
}

// jsonDocuments reads the resources of a JSON file.
func jsonDocuments(data []byte) ([]jsonDoc, error) {
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
	dec := json.NewDecoder(bytes.NewReader(data))
	var root map[string]json.RawMessage
	if err := dec.Decode(&root); err != nil {
		return nil, fmt.Errorf("not a single JSON object: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("more than one object in the file; Argo CD allows a single object in a JSON file")
	}
	items, isList := root["items"]
	if isList {
		var list []json.RawMessage
		if bytes.Equal(bytes.TrimSpace(items), []byte("null")) {
			return nil, nil
		}
		if err := json.Unmarshal(items, &list); err == nil {
			docs := make([]jsonDoc, len(list))
			for i, raw := range list {
				obj, err := jsonMap(raw)
				if err != nil {
					return nil, fmt.Errorf("item %d: %w", i, err)
				}
				docs[i] = jsonDoc{Prefix: "/items/" + strconv.Itoa(i), Obj: obj}
			}
			return docs, nil
		}
	}
	obj, err := jsonMap(data)
	if err != nil {
		return nil, err
	}
	return []jsonDoc{{Obj: obj}}, nil
}

// jsonMap decodes a JSON object the way Argo CD does (the last of two equal keys
// wins), with numbers as float64 like the values of a proposal.
func jsonMap(raw []byte) (map[string]any, error) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, fmt.Errorf("not an object")
	}
	return m, nil
}

// applyJSON applies changes to one resource of a JSON file, in place.
func applyJSON(file string, original []byte, document int, changes []v1alpha1.FieldChange) ([]byte, error) {
	docs, err := jsonDocuments(original)
	if err != nil {
		return nil, cannot("%s: %v", file, err)
	}
	if document < 0 || document >= len(docs) {
		return nil, cannot("%s has no document %d", file, document)
	}
	prefix := docs[document].Prefix
	ops := make([]jsonedit.Op, len(changes))
	for i, ch := range changes {
		if ch.Path == "" || !strings.HasPrefix(ch.Path, "/") {
			return nil, cannot("change %d: %q is not a JSON pointer to a field", i, ch.Path)
		}
		ops[i] = jsonedit.Op{Pointer: prefix + ch.Path, Value: ch.Live}
		switch ch.Op {
		case v1alpha1.OpAdd:
			ops[i].Kind = jsonedit.Add
		case v1alpha1.OpRemove:
			ops[i].Kind = jsonedit.Remove
		case v1alpha1.OpReplace:
			ops[i].Kind = jsonedit.Replace
		default:
			return nil, cannot("change %d: unknown operation %q", i, ch.Op)
		}
	}
	edited, err := jsonedit.Apply(original, ops...)
	switch {
	case err == nil:
		return edited, nil
	case errors.Is(err, jsonedit.ErrAmbiguous):
		return nil, fmt.Errorf("%w: %s: %v", ErrAmbiguous, file, err)
	default:
		return nil, cannot("%s: %v", file, err)
	}
}

// verifyJSON is Verify for a JSON file: the other resources of the file are
// unchanged and the edited resource differs from the original by the changes.
func verifyJSON(original, edited []byte, document int, changes []v1alpha1.FieldChange) error {
	fail := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrVerificationFailed, fmt.Sprintf(format, args...))
	}
	od, err := jsonDocuments(original)
	if err != nil {
		return fail("the original file: %v", err)
	}
	ed, err := jsonDocuments(edited)
	if err != nil {
		return fail("the edited file: %v", err)
	}
	if len(od) != len(ed) || document < 0 || document >= len(od) {
		return fail("the number of documents changed")
	}
	for i := range od {
		if i != document && !deepEqual(od[i].Obj, ed[i].Obj) {
			return fail("document %d was modified", i)
		}
	}
	return verifyMaps(od[document].Obj, ed[document].Obj, changes)
}

// objectDefines is documentDefines for a decoded JSON object.
func objectDefines(obj map[string]any, destNamespace string, res ResourceID) bool {
	apiVersion, _ := obj["apiVersion"].(string)
	group := ""
	if g, _, found := strings.Cut(apiVersion, "/"); found {
		group = g
	}
	meta, _ := obj["metadata"].(map[string]any)
	kind, _ := obj["kind"].(string)
	name, _ := meta["name"].(string)
	if kind != res.Kind || group != res.Group || name != res.Name {
		return false
	}
	ns, _ := meta["namespace"].(string)
	if ns == "" && res.Namespace != "" {
		ns = destNamespace
	}
	return ns == res.Namespace
}
