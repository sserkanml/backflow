package helmmap

import (
	"errors"
	"slices"
	"testing"
	"testing/fstest"
)

const externalDefaults = `# the chart's own values
replicaCount: 1
ui:
  color: "#34577c"
  message: ""
image:
  repository: ghcr.io/stefanprodan/podinfo
  tag: 6.7.1
podAnnotations: {}
redis:
  enabled: false
`

// valuesRepo is a repository that holds only value files, and has a Chart.yaml
// and a values.yaml at its root that are not the chart's.
func valuesRepo() fstest.MapFS {
	return fstest.MapFS{
		"Chart.yaml":      &fstest.MapFile{Data: []byte("apiVersion: v2\nname: decoy\nversion: 0.0.1\ndependencies:\n  - name: decoy-dep\n    version: 1.0.0\n    repository: https://example.com\n")},
		"values.yaml":     &fstest.MapFile{Data: []byte("replicaCount: 99\nui:\n  color: red\n")},
		"env/values.yaml": &fstest.MapFile{Data: []byte("# environment values\nreplicaCount: 3\nimage:\n  tag: 6.7.1\n")},
		"env/extra.yaml":  &fstest.MapFile{Data: []byte("podAnnotations:\n  team: a\n")},
	}
}

func externalSpec(valueFiles ...string) Spec {
	return Spec{
		ValueFiles: valueFiles,
		External: &ExternalChart{
			Name: "podinfo", Version: "6.7.1", Values: []byte(externalDefaults), UnreadDependencies: []string{"redis"},
		},
	}
}

func TestExternalChartSource(t *testing.T) {
	fsys := valuesRepo()
	src, err := NewSource(fsys, externalSpec("/env/values.yaml", "/env/extra.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if src.ChartDir != "" || src.ChartValues != "podinfo@6.7.1/values.yaml" {
		t.Errorf("ChartDir = %q, ChartValues = %q", src.ChartDir, src.ChartValues)
	}
	if !slices.Equal(src.ValueFiles, []string{"env/values.yaml", "env/extra.yaml"}) {
		t.Errorf("value files = %v", src.ValueFiles)
	}

	files, err := CollectFiles(fsys, src)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for n := range files {
		names = append(names, n)
	}
	slices.Sort(names)
	want := []string{"env/extra.yaml", "env/values.yaml", "podinfo@6.7.1/values.yaml"}
	if !slices.Equal(names, want) {
		t.Errorf("files = %v, want %v: the repository's own Chart.yaml and values.yaml are not the chart's", names, want)
	}

	t.Run("a key a value file defines is a candidate, a default is an override", func(t *testing.T) {
		got, err := src.Find(files, float64(3))
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Candidates) != 1 || got.Candidates[0].File != "env/values.yaml" || got.Candidates[0].Path.String() != "replicaCount" {
			t.Errorf("candidates = %+v", got.Candidates)
		}
		got, err = src.Find(files, "#34577c")
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Candidates) != 0 || len(got.ChartDefaults) != 1 || got.ChartDefaults[0].String() != "ui.color" {
			t.Fatalf("result = %+v", got)
		}
		if len(got.Overrides) != 1 || got.Overrides[0].File != "env/extra.yaml" || !got.Overrides[0].Override ||
			got.Overrides[0].Default.File != "podinfo@6.7.1/values.yaml" {
			t.Errorf("overrides = %+v: the last value file receives it", got.Overrides)
		}
	})

	t.Run("the dependencies whose defaults are not available are reported", func(t *testing.T) {
		got, err := src.Find(files, "no such value")
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got.UnreadDependencies, []string{"redis"}) {
			t.Errorf("unread = %v, not the decoy dependency of the repository's Chart.yaml", got.UnreadDependencies)
		}
	})

	t.Run("the layers have the chart's defaults, not the repository's values.yaml", func(t *testing.T) {
		layers, err := src.Layers(files)
		if err != nil {
			t.Fatal(err)
		}
		if len(layers) != 3 || !layers[0].IsDefault || layers[0].Name != "podinfo@6.7.1/values.yaml" || layers[1].Name != "env/values.yaml" {
			t.Fatalf("layers = %+v", layers)
		}
		if ui, _ := layers[0].Tree["ui"].(map[string]any); ui["color"] != "#34577c" {
			t.Errorf("defaults = %v", layers[0].Tree)
		}
		if file, why := src.OverrideFile(); file != "env/extra.yaml" || why != "" {
			t.Errorf("override file = %q (%s)", file, why)
		}
	})
}

func TestExternalChartMissingValueFile(t *testing.T) {
	if _, err := NewSource(valuesRepo(), externalSpec("/env/nope.yaml")); !errors.Is(err, ErrMissingValueFile) {
		t.Errorf("err = %v", err)
	}
	src, err := NewSource(valuesRepo(), Spec{ValueFiles: []string{"/env/nope.yaml"}, IgnoreMissingValueFiles: true,
		External: &ExternalChart{Name: "c", Version: "1", Values: []byte("a: 1\n")}})
	if err != nil || len(src.ValueFiles) != 0 {
		t.Errorf("src = %+v, err = %v", src, err)
	}
}
