package chart

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

type crdFile struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Group string `json:"group"`
		Names struct {
			Plural string `json:"plural"`
		} `json:"names"`
		Versions []struct {
			Name string `json:"name"`
		} `json:"versions"`
	} `json:"spec"`
}

func crdFiles(t *testing.T, dir string) map[string]crdFile {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no CRDs in %s: %v", dir, err)
	}
	out := map[string]crdFile{}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var crd crdFile
		if err := yaml.Unmarshal(data, &crd); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		out[filepath.Base(f)] = crd
	}
	return out
}

// Helm applies charts/backflow/crds on install and the API server rejects a CRD that has no group, so a chart with one
// cannot be installed from scratch (the released 0.18.0 had three, made by controller-gen from a package that is not an
// API).
func TestEveryCRDHasAGroupAndAMatchingName(t *testing.T) {
	for _, dir := range []string{"../../config/crd/bases", "../../charts/backflow/crds"} {
		for name, crd := range crdFiles(t, dir) {
			switch {
			case crd.Spec.Group == "":
				t.Errorf("%s/%s: the CRD has no spec.group (metadata.name %q)", dir, name, crd.Metadata.Name)
			case crd.Spec.Names.Plural == "" || crd.Metadata.Name != crd.Spec.Names.Plural+"."+crd.Spec.Group:
				t.Errorf("%s/%s: metadata.name is %q, want %s.%s", dir, name, crd.Metadata.Name,
					crd.Spec.Names.Plural, crd.Spec.Group)
			case len(crd.Spec.Versions) == 0:
				t.Errorf("%s/%s: no versions", dir, name)
			case !strings.HasPrefix(name, crd.Spec.Group+"_"):
				t.Errorf("%s/%s: the file is not named <group>_<plural>.yaml", dir, name)
			}
		}
	}
}

// The chart has the CRDs that config/crd lists, and no others; and bases/ has no file that kustomize does not list.
func TestTheChartHasTheCRDsOfTheKustomization(t *testing.T) {
	data, err := os.ReadFile("../../config/crd/kustomization.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var listed []string
	for line := range strings.SplitSeq(string(data), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "- bases/"); ok {
			listed = append(listed, rest)
		}
	}
	slices.Sort(listed)
	for _, dir := range []string{"../../config/crd/bases", "../../charts/backflow/crds"} {
		var have []string
		for name := range crdFiles(t, dir) {
			have = append(have, name)
		}
		slices.Sort(have)
		if !slices.Equal(have, listed) {
			t.Errorf("%s has %v, config/crd/kustomization.yaml lists %v", dir, have, listed)
		}
	}
}
