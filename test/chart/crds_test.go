package chart

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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

type versionSchema struct {
	Name                     string         `json:"name"`
	Served                   bool           `json:"served"`
	Storage                  bool           `json:"storage"`
	Deprecated               bool           `json:"deprecated"`
	DeprecationWarning       string         `json:"deprecationWarning"`
	Schema                   map[string]any `json:"schema"`
	Subresources             map[string]any `json:"subresources"`
	AdditionalPrinterColumns []any          `json:"additionalPrinterColumns"`
}

type crdWithConversion struct {
	Spec struct {
		Group      string          `json:"group"`
		Conversion map[string]any  `json:"conversion"`
		Versions   []versionSchema `json:"versions"`
	} `json:"spec"`
}

// backflow.io/v1beta1 stays served for all of 1.x with the schema of v1 and conversion None (a field the two
// disagree on would be silently dropped or kept by the API server when an object is read as the other version). The
// schemas are generated from the Go types, which are made from each other (hack/sync-v1beta1.sh); this compares what
// comes
// out, in the CRDs of config/, of the chart and of the bundle when there is one.
func TestV1AndV1beta1HaveTheSameSchema(t *testing.T) {
	dirs := []string{"../../config/crd/bases", "../../charts/backflow/crds"}
	if _, err := os.Stat("../../bundle/manifests"); err == nil {
		dirs = append(dirs, "../../bundle/manifests")
	}
	checked := 0
	for _, dir := range dirs {
		files, _ := filepath.Glob(filepath.Join(dir, "backflow.io_*.yaml"))
		for _, f := range files {
			data, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			var crd crdWithConversion
			if err := yaml.Unmarshal(data, &crd); err != nil {
				t.Fatalf("%s: %v", f, err)
			}
			if crd.Spec.Group != "backflow.io" {
				continue
			}
			byName := map[string]versionSchema{}
			for _, v := range crd.Spec.Versions {
				byName[v.Name] = v
			}
			v1, beta := byName["v1"], byName["v1beta1"]
			switch {
			case len(crd.Spec.Versions) != 2 || v1.Name == "" || beta.Name == "":
				t.Errorf("%s: the versions are %v, want v1 and v1beta1", f, byName)
				continue
			case !v1.Served || !v1.Storage || v1.Deprecated:
				t.Errorf("%s: v1 must be served, the storage version, and not deprecated: %+v", f, v1.Name)
			case !beta.Served || beta.Storage || !beta.Deprecated || beta.DeprecationWarning == "":
				t.Errorf("%s: v1beta1 must be served, not the storage version, and deprecated with a warning", f)
			}
			if s := crd.Spec.Conversion["strategy"]; s != nil && s != "None" {
				t.Errorf("%s: conversion strategy %v, want None (the schemas are the same)", f, s)
			}
			if !reflect.DeepEqual(v1.Schema, beta.Schema) {
				t.Errorf("%s: the schema of v1beta1 differs from v1:\n%s", f, schemaDiff(v1.Schema, beta.Schema, ""))
			}
			if !reflect.DeepEqual(v1.Subresources, beta.Subresources) ||
				!reflect.DeepEqual(v1.AdditionalPrinterColumns, beta.AdditionalPrinterColumns) {
				t.Errorf("%s: the subresources or the printer columns of v1beta1 differ from v1", f)
			}
			checked++
		}
	}
	if checked < 6 {
		t.Fatalf("compared %d CRDs, want the three of config/ and the three of the chart", checked)
	}
}

// schemaDiff names the first places where two decoded schemas differ.
func schemaDiff(a, b any, path string) string {
	switch x := a.(type) {
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok {
			return path + ": a map against something else\n"
		}
		var out string
		for k, v := range x {
			if w, has := y[k]; !has {
				out += path + "/" + k + ": only in v1\n"
			} else if !reflect.DeepEqual(v, w) {
				out += schemaDiff(v, w, path+"/"+k)
			}
		}
		for k := range y {
			if _, has := x[k]; !has {
				out += path + "/" + k + ": only in v1beta1\n"
			}
		}
		return out
	default:
		if !reflect.DeepEqual(a, b) {
			return path + ": " + fmt.Sprint(a) + " against " + fmt.Sprint(b) + "\n"
		}
		return ""
	}
}

// api/v1beta1 is written by hack/sync-v1beta1.sh from api/v1, not kept by hand.
func TestV1beta1GoTypesAreMadeFromV1(t *testing.T) {
	out := t.TempDir()
	cmd := exec.Command("../../hack/sync-v1beta1.sh")
	cmd.Env = append(os.Environ(), "OUT_DIR="+out)
	if msg, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("hack/sync-v1beta1.sh: %v\n%s", err, msg)
	}
	made, _ := filepath.Glob(filepath.Join(out, "*.go"))
	if len(made) == 0 {
		t.Fatal("the script wrote nothing")
	}
	for _, f := range made {
		want, _ := os.ReadFile(f)
		got, err := os.ReadFile(filepath.Join("../../api/v1beta1", filepath.Base(f)))
		if err != nil || string(got) != string(want) {
			t.Errorf("api/v1beta1/%s is not what hack/sync-v1beta1.sh makes from api/v1: run make sync-v1beta1",
				filepath.Base(f))
		}
	}
}
