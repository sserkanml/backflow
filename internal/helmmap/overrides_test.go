package helmmap

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"maps"
	"reflect"
	"strings"
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

// A default only the chart defines gets an override in the last value file.
func TestFindOverridesInLastValueFile(t *testing.T) {
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
}

// A default that a value file overrides already has its candidate and no override.
func TestFindOverridesShadowedDefault(t *testing.T) {
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
}

// A part of a string default is a fragment override.
func TestFindOverridesFragment(t *testing.T) {
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
}

// A key under a list in the defaults is reported, not overridden.
func TestFindOverridesUnderList(t *testing.T) {
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
}

// An application without value files has nothing to add to.
func TestFindOverridesNoValueFile(t *testing.T) {
	src, files := overrideRepo(t, Spec{Path: "c"}, map[string]string{"c/Chart.yaml": ovChartYAML, "c/values.yaml": "a: 1\n"})
	got, _ := src.Find(files, float64(1))
	if len(got.Overrides) != 0 || len(got.NoOverride) != 1 {
		t.Errorf("overrides = %+v, no override = %+v", got.Overrides, got.NoOverride)
	}
}

// An inline setting that decides the key is reported, not overridden.
func TestFindOverridesInlineControlled(t *testing.T) {
	src, files := overrideRepo(t, Spec{Path: "c", ValueFiles: []string{"prod.yaml"}, Parameters: []Parameter{{Name: "a", Value: "2"}}},
		map[string]string{"c/Chart.yaml": ovChartYAML, "c/values.yaml": "a: 1\n", "c/prod.yaml": "b: 1\n"})
	got, _ := src.Find(files, float64(1))
	if len(got.Overrides) != 0 || len(got.Controlled) != 1 {
		t.Errorf("overrides = %+v, controlled = %+v", got.Overrides, got.Controlled)
	}
}

// A parent default shadows a dependency default.
func TestFindOverridesDependencyShadowed(t *testing.T) {
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
}

// Global keys of a dependency keep their place.
func TestFindOverridesDependencyGlobal(t *testing.T) {
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
}

func TestFindOverridesTarget(t *testing.T) {
	spec := Spec{Path: "c", ValueFiles: []string{"base.yaml", "prod.yaml"}}
	content := map[string]string{
		"c/Chart.yaml": ovChartYAML, "c/values.yaml": "a: 1\n", "c/base.yaml": "b: 1\n", "c/prod.yaml": "b: 2\n",
	}
	for name, tt := range map[string]struct {
		target, want, reason string
	}{
		"unset: the last file":               {"", "c/prod.yaml", ""},
		"relative to the chart":              {"base.yaml", "c/base.yaml", ""},
		"a repository path":                  {"c/base.yaml", "c/base.yaml", ""},
		"a repository path with a slash":     {"/c/base.yaml", "c/base.yaml", ""},
		"not one of the Application's files": {"other.yaml", "", `spec.helm.targetValuesFile "other.yaml" is not one of the Application's value files (c/base.yaml, c/prod.yaml)`},
	} {
		t.Run(name, func(t *testing.T) {
			src, files := overrideRepo(t, spec, content)
			src.OverrideTarget = tt.target
			got, err := src.Find(files, float64(1))
			if err != nil {
				t.Fatal(err)
			}
			if tt.reason != "" {
				if len(got.Overrides) != 0 || len(got.NoOverride) != 1 || got.NoOverride[0].Reason != tt.reason {
					t.Errorf("overrides = %+v, no override = %+v", got.Overrides, got.NoOverride)
				}
				return
			}
			if len(got.Overrides) != 1 || got.Overrides[0].File != tt.want {
				t.Errorf("overrides = %+v, want the file %s", got.Overrides, tt.want)
			}
		})
	}
}

// packageChart builds a chart package with the given files under the top
// directory name.
func packageChart(t *testing.T, name string, files map[string]string) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for n, c := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name + "/" + n, Mode: 0o644, Size: int64(len(c)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(c)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestFindOverridesPackagedDependency(t *testing.T) {
	redis := packageChart(t, "redis", map[string]string{
		"Chart.yaml":  "apiVersion: v2\nname: redis\nversion: 1.0.0\n",
		"values.yaml": "auth:\n  port: 6379\nglobal:\n  imageRegistry: docker.io\n",
	})
	other := packageChart(t, "redis-ha", map[string]string{"values.yaml": "auth:\n  port: 1111\n"})
	deps := ovChartYAML + "dependencies:\n  - name: redis\n    alias: cache\n    version: 1.0.0\n    repository: https://example.invalid\n"
	spec := Spec{Path: "c", ValueFiles: []string{"prod.yaml"}}
	content := map[string]string{
		"c/Chart.yaml": deps, "c/values.yaml": "b: 1\n", "c/prod.yaml": "a: 1\n",
		"c/charts/redis-1.0.0.tgz":    redis,
		"c/charts/redis-ha-2.0.0.tgz": other, // another chart with the same prefix
	}

	t.Run("the defaults of a packaged subchart, under its alias", func(t *testing.T) {
		src, files := overrideRepo(t, spec, content)
		got, err := src.Find(files, float64(6379))
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Overrides) != 1 {
			t.Fatalf("overrides = %+v", got.Overrides)
		}
		o := got.Overrides[0]
		if o.Path.String() != "cache.auth.port" || o.File != "c/prod.yaml" ||
			o.Default.File != "c/charts/redis-1.0.0.tgz!redis/values.yaml" || o.Default.Key.String() != "auth.port" || o.Default.Tag != "!!int" {
			t.Errorf("override = %+v default = %+v", o, o.Default)
		}
		if len(got.UnreadDependencies) != 0 {
			t.Errorf("unread = %v", got.UnreadDependencies)
		}
	})
	t.Run("global keys of a packaged subchart keep their place", func(t *testing.T) {
		src, files := overrideRepo(t, spec, content)
		got, _ := src.Find(files, "docker.io")
		if len(got.Overrides) != 1 || got.Overrides[0].Path.String() != "global.imageRegistry" {
			t.Errorf("overrides = %+v", got.Overrides)
		}
	})
	t.Run("a package of another chart with the same prefix is not read", func(t *testing.T) {
		src, files := overrideRepo(t, spec, content)
		if got, _ := src.Find(files, float64(1111)); len(got.Overrides) != 0 {
			t.Errorf("overrides = %+v", got.Overrides)
		}
	})
	t.Run("a package that is not a chart archive is skipped", func(t *testing.T) {
		c := maps.Clone(content)
		c["c/charts/redis-1.0.0.tgz"] = "not gzip"
		src, files := overrideRepo(t, spec, c)
		got, err := src.Find(files, float64(6379))
		if err != nil || len(got.Overrides) != 0 {
			t.Errorf("overrides = %+v, err = %v", got.Overrides, err)
		}
		if len(got.UnreadDependencies) != 1 || got.UnreadDependencies[0] != "cache" {
			t.Errorf("unread = %v", got.UnreadDependencies)
		}
	})
}

func TestFindUnreadDependencies(t *testing.T) {
	deps := ovChartYAML + "dependencies:\n" +
		"  - name: redis\n    alias: cache\n    version: 1.0.0\n    repository: https://example.invalid\n" +
		"  - name: unpacked\n    version: 1.0.0\n    repository: https://example.invalid\n" +
		"  - name: postgresql\n    version: 1.0.0\n    repository: oci://registry.invalid/charts\n"
	src, files := overrideRepo(t, Spec{Path: "c", ValueFiles: []string{"prod.yaml"}}, map[string]string{
		"c/Chart.yaml": deps, "c/values.yaml": "b: 1\n", "c/prod.yaml": "a: 1\n",
		"c/charts/unpacked/Chart.yaml": "apiVersion: v2\nname: unpacked\nversion: 1.0.0\n", // no values.yaml: no defaults, but in Git
	})
	got, err := src.Find(files, "anything")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"cache", "postgresql"}; !reflect.DeepEqual(got.UnreadDependencies, want) {
		t.Errorf("unread = %v, want %v (the alias is named, an unpacked chart is in Git)", got.UnreadDependencies, want)
	}

	t.Run("the message says the defaults are not in the repository", func(t *testing.T) {
		f := parsedField{Field: Field{Found: got}, desired: `"anything"`}
		msg := noCandidateDetail(f, nil, true)
		for _, w := range []string{"cache, postgresql", "not in the repository", "downloads it when it renders", "cannot be overridden"} {
			if !strings.Contains(msg, w) {
				t.Errorf("message %q lacks %q", msg, w)
			}
		}
	})
	t.Run("not when a key was found", func(t *testing.T) {
		f := parsedField{Field: Field{Found: &Result{UnreadDependencies: []string{"cache"}, Candidates: []Candidate{{File: "x"}}}}, desired: `"x"`}
		if msg := noCandidateDetail(f, []string{"x y: it does not reproduce the drift"}, true); strings.Contains(msg, "not in the repository") {
			t.Errorf("message = %q", msg)
		}
	})
}
