//go:build coverage

package coverage

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	"github.com/sserkanml/backflow/api/v1alpha1"
	"github.com/sserkanml/backflow/internal/drift"
	"github.com/sserkanml/backflow/internal/mapping"
	"github.com/sserkanml/backflow/internal/rendermap"
)

// sourceDirectory is an Application whose source is a directory of manifests:
// there is nothing to render, the manifests are what Argo CD applies.
const sourceDirectory = "Directory"

// directoryApps are the same manifests as YAML files and as JSON files (one
// object per file, and a List whose items are the resources).
var directoryApps = []struct{ name, path string }{
	{"directory-yaml", "yaml"},
	{"directory-json", "json"},
}

// directoryPolicies has the one policy of a Directory source: there are no
// options that change what is mapped.
func directoryPolicies() []policySet { return []policySet{{name: "default"}} }

// runDirectory maps every scenario on every resource of the directories.
func runDirectory() ([]caseResult, error) {
	fsys := os.DirFS(filepath.Join("testdata", "directory"))
	var out []caseResult
	for _, app := range directoryApps {
		objs, err := loadManifests(fsys, app.path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", app.name, err)
		}
		for _, obj := range objs {
			for _, id := range kustomizeScenarios {
				sc := scenarioByID(id)
				kind, _ := obj["kind"].(string)
				if !contains(sc.kinds, kind) {
					continue
				}
				live, err := deepCopy(obj)
				if err != nil {
					return nil, err
				}
				if !sc.apply(live) {
					continue
				}
				changes := drift.Diff(obj, live)
				if len(changes) == 0 {
					continue
				}
				meta, _ := obj["metadata"].(map[string]any)
				name, _ := meta["name"].(string)
				ns, _ := meta["namespace"].(string)
				res := mapping.ResourceID{Kind: kind, Namespace: ns, Name: name}
				if g, _, ok := strings.Cut(fmt.Sprint(obj["apiVersion"]), "/"); ok {
					res.Group = g
				}
				out = append(out, caseResult{
					Source: sourceDirectory, Chart: app.name, Resource: kind + " " + name, Scenario: sc.id, Changes: changes,
					ByPolicy: map[string]outcome{"default": mapDirectory(fsys, app.path, res, changes)},
				})
			}
		}
	}
	return out, nil
}

func mapDirectory(fsys fs.FS, dir string, res mapping.ResourceID, changes []v1alpha1.FieldChange) outcome {
	r, err := mapping.Map(fsys, dir, mapping.DirectoryOptions{}, "demo", res, changes)
	if err != nil {
		reason := err.Error()
		for _, e := range []struct {
			err  error
			name string
		}{
			{mapping.ErrNotFound, "NotFound"}, {mapping.ErrAmbiguous, "Ambiguous"}, {mapping.ErrCannotApply, "CannotApply"},
			{mapping.ErrVerificationFailed, "VerificationFailed"}, {mapping.ErrUnsupportedFileFormat, "UnsupportedFileFormat"},
		} {
			if errors.Is(err, e.err) {
				reason = e.name
			}
		}
		return outcome{Result: Unmapped, Reason: reason}
	}
	var keys []string
	for _, e := range r.Edits {
		keys = append(keys, e.File+": "+rendermap.KeyPathFromPointer(e.Location).String())
	}
	sort.Strings(keys)
	return outcome{Result: Mapped, Keys: strings.Join(keys, ", ")}
}

// loadManifests reads the manifests of a directory the way Argo CD does: a .json
// file is one object, or a list whose items are the resources; other files are
// YAML documents.
func loadManifests(fsys fs.FS, dir string) ([]map[string]any, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := fs.ReadFile(fsys, path.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
		for {
			var m map[string]any
			if err := dec.Decode(&m); err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				return nil, fmt.Errorf("%s: %w", e.Name(), err)
			}
			if items, ok := m["items"].([]any); ok {
				for _, it := range items {
					if im, ok := it.(map[string]any); ok {
						out = append(out, im)
					}
				}
				continue
			}
			if len(m) > 0 {
				out = append(out, m)
			}
		}
	}
	return out, nil
}

func deepCopy(m map[string]any) (map[string]any, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	return out, json.Unmarshal(b, &out)
}
