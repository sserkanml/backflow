package helmmap

import (
	"reflect"
	"testing"
	"testing/fstest"
)

// chartFiles builds a repository with a chart at c and the given files.
func overrideRepo(t *testing.T, spec Spec, content map[string]string) (*Source, map[string][]byte) {
	t.Helper()
	m := fstest.MapFS{}
	files := map[string][]byte{}
	for n, c := range content {
		m[n] = &fstest.MapFile{Data: []byte(c)}
		files[n] = []byte(c)
	}
	src, err := NewSource(m, spec)
	if err != nil {
		t.Fatal(err)
	}
	return src, files
}

const ovChartYAML = "apiVersion: v2\nname: web\nversion: 1.0.0\n"

func TestFindOverrides(t *testing.T) {
	t.Run("a default only the chart defines gets an override in the last value file", func(t *testing.T) {
		src, files := overrideRepo(t, Spec{Path: "c", ValueFiles: []string{"base.yaml", "prod.yaml"}}, map[string]string{
			"c/Chart.yaml":  ovChartYAML,
			"c/values.yaml": "config:\n  timeout: 30\n  mode: fast\n",
			"c/base.yaml":   "config:\n  mode: slow\n",
			"c/prod.yaml":   "replicaCount: 2\n",
		})
		got, err := src.Find(files, float64(30))
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Candidates) != 0 {
			t.Errorf("candidates = %v, want none: no value file defines the key", got.Candidates)
		}
		want := []Candidate{{
			File: "c/prod.yaml", Path: KeyPath{"config", "timeout"}, Override: true,
			Default: &ChartDefault{File: "c/values.yaml", Key: KeyPath{"config", "timeout"}, Value: "30", Tag: "!!int"},
		}}
		if !reflect.DeepEqual(got.Overrides, want) {
			t.Errorf("overrides = %+v, want %+v", got.Overrides[0], want[0])
		}
		if len(got.ChartDefaults) != 1 || got.ChartDefaults[0].String() != "config.timeout" {
			t.Errorf("chart defaults = %v", got.ChartDefaults)
		}
	})

	t.Run("a default that a value file overrides already has its candidate and no override", func(t *testing.T) {
		src, files := overrideRepo(t, Spec{Path: "c", ValueFiles: []string{"prod.yaml"}}, map[string]string{
			"c/Chart.yaml": ovChartYAML, "c/values.yaml": "level: info\n", "c/prod.yaml": "level: info\n",
		})
		got, _ := src.Find(files, "info")
		if len(got.Candidates) != 1 || got.Candidates[0].File != "c/prod.yaml" || got.Candidates[0].Override {
			t.Errorf("candidates = %+v", got.Candidates)
		}
		if len(got.Overrides) != 0 || len(got.ChartDefaults) != 0 {
			t.Errorf("overrides = %v, defaults = %v: the default is shadowed", got.Overrides, got.ChartDefaults)
		}
	})

	t.Run("a part of a string default is a fragment override", func(t *testing.T) {
		src, files := overrideRepo(t, Spec{Path: "c", ValueFiles: []string{"prod.yaml"}}, map[string]string{
			"c/Chart.yaml": ovChartYAML, "c/values.yaml": "image:\n  tag: 1.27-alpine\n", "c/prod.yaml": "replicaCount: 2\n",
		})
		got, err := src.FindFragments(files, "nginx:1.27-alpine", "nginx:1.28-alpine")
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Overrides) != 1 {
			t.Fatalf("overrides = %+v", got.Overrides)
		}
		o := got.Overrides[0]
		if !o.Override || !o.Fragment || o.NewValue != "1.28-alpine" || o.File != "c/prod.yaml" || o.Path.String() != "image.tag" {
			t.Errorf("override = %+v", o)
		}
		if o.Default == nil || o.Default.Value != "1.27-alpine" || o.Default.Tag != "!!str" {
			t.Errorf("default = %+v", o.Default)
		}
	})

	t.Run("a key under a list in the defaults is reported, not overridden", func(t *testing.T) {
		src, files := overrideRepo(t, Spec{Path: "c", ValueFiles: []string{"prod.yaml"}}, map[string]string{
			"c/Chart.yaml": ovChartYAML, "c/values.yaml": "env:\n  - name: LOG\n    value: json\n", "c/prod.yaml": "a: 1\n",
		})
		got, _ := src.Find(files, "json")
		if len(got.Overrides) != 0 {
			t.Errorf("overrides = %+v", got.Overrides)
		}
		if len(got.NoOverride) != 1 || got.NoOverride[0].Path.String() != "env.0.value" {
			t.Errorf("no override = %+v", got.NoOverride)
		}
	})

	t.Run("an Application without value files has nothing to add to", func(t *testing.T) {
		src, files := overrideRepo(t, Spec{Path: "c"}, map[string]string{"c/Chart.yaml": ovChartYAML, "c/values.yaml": "a: 1\n"})
		got, _ := src.Find(files, float64(1))
		if len(got.Overrides) != 0 || len(got.NoOverride) != 1 {
			t.Errorf("overrides = %+v, no override = %+v", got.Overrides, got.NoOverride)
		}
	})

	t.Run("an inline setting that decides the key is reported, not overridden", func(t *testing.T) {
		src, files := overrideRepo(t, Spec{Path: "c", ValueFiles: []string{"prod.yaml"}, Parameters: []Parameter{{Name: "a", Value: "2"}}},
			map[string]string{"c/Chart.yaml": ovChartYAML, "c/values.yaml": "a: 1\n", "c/prod.yaml": "b: 1\n"})
		got, _ := src.Find(files, float64(1))
		if len(got.Overrides) != 0 || len(got.Controlled) != 1 {
			t.Errorf("overrides = %+v, controlled = %+v", got.Overrides, got.Controlled)
		}
	})

	t.Run("a parent default shadows a dependency default", func(t *testing.T) {
		src, files := overrideRepo(t, Spec{Path: "c", ValueFiles: []string{"prod.yaml"}}, map[string]string{
			"c/Chart.yaml":               ovChartYAML + "dependencies:\n  - name: redis\n    alias: cache\n    version: 1.0.0\n    repository: https://example.invalid\n",
			"c/charts/redis/Chart.yaml":  "apiVersion: v2\nname: redis\nversion: 1.0.0\n",
			"c/charts/redis/values.yaml": "auth:\n  user: default\n  port: 6379\n",
			"c/values.yaml":              "cache:\n  auth:\n    user: admin\n",
			"c/prod.yaml":                "a: 1\n",
		})
		if got, _ := src.Find(files, "default"); len(got.Overrides) != 0 || len(got.ChartDefaults) != 0 {
			t.Errorf("the parent's values.yaml sets cache.auth.user: overrides = %+v", got.Overrides)
		}
		got, _ := src.Find(files, float64(6379))
		if len(got.Overrides) != 1 {
			t.Fatalf("overrides = %+v", got.Overrides)
		}
		o := got.Overrides[0]
		if o.Path.String() != "cache.auth.port" || o.File != "c/prod.yaml" {
			t.Errorf("override = %+v", o)
		}
		if o.Default.File != "c/charts/redis/values.yaml" || o.Default.Key.String() != "auth.port" || o.Default.Value != "6379" {
			t.Errorf("default = %+v", o.Default)
		}
	})

	t.Run("global keys of a dependency keep their place", func(t *testing.T) {
		src, files := overrideRepo(t, Spec{Path: "c", ValueFiles: []string{"prod.yaml"}}, map[string]string{
			"c/Chart.yaml":       ovChartYAML + "dependencies:\n  - name: common\n    version: 1.0.0\n    repository: file://../common\n",
			"common/values.yaml": "global:\n  imageRegistry: docker.io\nlocal: x\n",
			"c/values.yaml":      "b: 1\n",
			"c/prod.yaml":        "a: 1\n",
		})
		got, _ := src.Find(files, "docker.io")
		if len(got.Overrides) != 1 || got.Overrides[0].Path.String() != "global.imageRegistry" {
			t.Fatalf("overrides = %+v", got.Overrides)
		}
		got, _ = src.Find(files, "x")
		if len(got.Overrides) != 1 || got.Overrides[0].Path.String() != "common.local" || got.Overrides[0].Default.Key.String() != "local" {
			t.Errorf("overrides = %+v", got.Overrides)
		}
	})
}
