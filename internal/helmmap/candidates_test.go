package helmmap

import (
	"io/fs"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
)

// load reads the chart values and the value files of src from fsys.
func load(t *testing.T, fsys fs.FS, src *Source) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, name := range append([]string{src.ChartValues}, src.ValueFiles...) {
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			continue // a chart without values.yaml
		}
		out[name] = data
	}
	return out
}

func cand(file, path string) Candidate {
	p, err := ParseKeyPath(path)
	if err != nil {
		panic(err)
	}
	return Candidate{File: file, Path: p}
}

func TestFindInDemoRepository(t *testing.T) {
	const dev = "helm/demo/values-dev.yaml"
	devSpec := Spec{Path: "helm/demo", ValueFiles: []string{"values-dev.yaml"}}
	withParam := devSpec
	withParam.Parameters = []Parameter{{Name: "config.feature", Value: "beta"}}

	tests := []struct {
		name           string
		spec           Spec
		old            any
		want           []Candidate
		wantDefaults   []string
		wantControlled []string // "layer path"
	}{
		{name: "LOG_LEVEL", spec: devSpec, old: "debug", want: []Candidate{cand(dev, "config.logLevel")}},
		{name: "REGION in global", spec: devSpec, old: "eu-central-1", want: []Candidate{cand(dev, "global.region")}},
		{name: "subchart key", spec: devSpec, old: float64(45), want: []Candidate{cand(dev, "demo-sidecar.timeoutSeconds")}},
		{
			name: "two candidates with an equal value", spec: devSpec, old: "green",
			want: []Candidate{cand(dev, "config.colorA"), cand(dev, "config.colorB")},
		},
		{
			name: "two candidates for a derived value", spec: devSpec, old: float64(3),
			want: []Candidate{cand(dev, "config.minA"), cand(dev, "config.minB")},
		},
		{name: "only in the chart defaults", spec: devSpec, old: float64(30), wantDefaults: []string{"config.timeout"}},
		{name: "default overridden by the value file", spec: devSpec, old: "info"},
		{name: "number matches its string form", spec: devSpec, old: "2", want: []Candidate{cand(dev, "replicaCount")}},
		{
			name: "controlled by a parameter", spec: withParam, old: "beta",
			wantControlled: []string{"helm.parameters config.feature"},
		},
		{
			name: "file value overridden by a parameter", spec: withParam, old: "alpha",
			wantControlled: []string{"helm.parameters config.feature"},
		},
		{name: "no match", spec: devSpec, old: "nothing-like-this"},
		{name: "null never matches", spec: devSpec, old: nil},
		{name: "a map has no scalar form", spec: devSpec, old: map[string]any{"a": 1}},
		{
			name: "two value files, the later shadows",
			spec: Spec{Path: "helm/demo", ValueFiles: []string{"values-dev.yaml", "values-prod.yaml"}},
			old:  "debug",
		},
		{
			name: "two value files, shadowing is by key",
			spec: Spec{Path: "helm/demo", ValueFiles: []string{"values-dev.yaml", "values-prod.yaml"}},
			old:  "warn",
			want: []Candidate{cand("helm/demo/values-prod.yaml", "config.logLevel")},
		},
		{
			name: "glob over the services, beta comes after alpha and shadows it",
			spec: Spec{Path: "helm/shared", ValueFiles: []string{"services/*/dev.yaml"}},
			old:  "warn",
			want: []Candidate{cand("helm/shared/services/beta/dev.yaml", "logLevel")},
		},
		{
			name: "glob over the services, the shadowed value is no candidate",
			spec: Spec{Path: "helm/shared", ValueFiles: []string{"services/*/dev.yaml"}},
			old:  "debug",
		},
		{
			name: "slash prefixed value file",
			spec: Spec{Path: "helm/shared", ValueFiles: []string{"/helm/shared/services/alpha/dev.yaml"}},
			old:  "debug",
			want: []Candidate{cand("helm/shared/services/alpha/dev.yaml", "logLevel")},
		},
	}
	fsys := demoRepo(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src, err := NewSource(fsys, tt.spec)
			if err != nil {
				t.Fatal(err)
			}
			res, err := src.Find(load(t, fsys, src), tt.old)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(res.Candidates, tt.want) {
				t.Errorf("Candidates = %v, want %v", res.Candidates, tt.want)
			}
			var defaults []string
			for _, d := range res.ChartDefaults {
				defaults = append(defaults, d.String())
			}
			if !reflect.DeepEqual(defaults, tt.wantDefaults) {
				t.Errorf("ChartDefaults = %v, want %v", defaults, tt.wantDefaults)
			}
			var controlled []string
			for _, c := range res.Controlled {
				controlled = append(controlled, c.Layer+" "+c.Path.String())
			}
			if !reflect.DeepEqual(controlled, tt.wantControlled) {
				t.Errorf("Controlled = %v, want %v", controlled, tt.wantControlled)
			}
		})
	}
}

func TestFindSynthetic(t *testing.T) {
	tests := []struct {
		name           string
		files          map[string]string // chart/values.yaml is the chart default
		spec           Spec
		old            any
		want           []Candidate
		wantDefaults   []string
		wantControlled []string
	}{
		{
			name:  "later file shadows an earlier one",
			files: map[string]string{"c/a.yaml": "x: 1\ny: 1\n", "c/b.yaml": "x: 2\n"},
			spec:  Spec{Path: "c", ValueFiles: []string{"a.yaml", "b.yaml"}},
			old:   float64(1),
			want:  []Candidate{cand("c/a.yaml", "y")},
		},
		{
			name:  "same value in both files, only the later is effective",
			files: map[string]string{"c/a.yaml": "x: 1\n", "c/b.yaml": "x: 1\n"},
			spec:  Spec{Path: "c", ValueFiles: []string{"a.yaml", "b.yaml"}},
			old:   float64(1),
			want:  []Candidate{cand("c/b.yaml", "x")},
		},
		{
			name:  "order of the value files decides, not the file names",
			files: map[string]string{"c/a.yaml": "x: 1\n", "c/b.yaml": "x: 2\n"},
			spec:  Spec{Path: "c", ValueFiles: []string{"b.yaml", "a.yaml"}},
			old:   float64(1),
			want:  []Candidate{cand("c/a.yaml", "x")},
		},
		{
			name:  "a later list replaces the earlier list as a whole",
			files: map[string]string{"c/a.yaml": "l: [a, b, c]\n", "c/b.yaml": "l: [x]\n"},
			spec:  Spec{Path: "c", ValueFiles: []string{"a.yaml", "b.yaml"}},
			old:   "c",
		},
		{
			name:  "maps merge by key",
			files: map[string]string{"c/a.yaml": "m: {p: 1, q: 2}\n", "c/b.yaml": "m: {q: 3}\n"},
			spec:  Spec{Path: "c", ValueFiles: []string{"a.yaml", "b.yaml"}},
			old:   float64(1),
			want:  []Candidate{cand("c/a.yaml", "m.p")},
		},
		{
			name:  "a later scalar replaces an earlier map",
			files: map[string]string{"c/a.yaml": "m: {p: 1}\n", "c/b.yaml": "m: off\n"},
			spec:  Spec{Path: "c", ValueFiles: []string{"a.yaml", "b.yaml"}},
			old:   float64(1),
		},
		{
			name:  "a later map replaces an earlier scalar",
			files: map[string]string{"c/a.yaml": "m: 1\n", "c/b.yaml": "m: {p: 2}\n"},
			spec:  Spec{Path: "c", ValueFiles: []string{"a.yaml", "b.yaml"}},
			old:   float64(1),
		},
		{
			name:  "null in a later file deletes the key",
			files: map[string]string{"c/a.yaml": "x: 1\n", "c/b.yaml": "x: null\n"},
			spec:  Spec{Path: "c", ValueFiles: []string{"a.yaml", "b.yaml"}},
			old:   float64(1),
		},
		{
			name:  "keys with dots",
			files: map[string]string{"c/a.yaml": "podLabels:\n  app.kubernetes.io/name: demo\n  a.b: demo\n"},
			spec:  Spec{Path: "c", ValueFiles: []string{"a.yaml"}},
			old:   "demo",
			want:  []Candidate{cand("c/a.yaml", `podLabels.app\.kubernetes\.io/name`), cand("c/a.yaml", `podLabels.a\.b`)},
		},
		{
			name:  "list items, at any depth",
			files: map[string]string{"c/a.yaml": "env:\n  - name: A\n    value: v1\n  - name: B\n    value: v2\nports: [80, 443]\n"},
			spec:  Spec{Path: "c", ValueFiles: []string{"a.yaml"}},
			old:   "v2",
			want:  []Candidate{cand("c/a.yaml", "env.1.value")},
		},
		{
			name:  "number in a list",
			files: map[string]string{"c/a.yaml": "ports: [80, 443]\n"},
			spec:  Spec{Path: "c", ValueFiles: []string{"a.yaml"}},
			old:   float64(443),
			want:  []Candidate{cand("c/a.yaml", "ports.1")},
		},
		{
			name:  "bool matches its string form",
			files: map[string]string{"c/a.yaml": "debug: true\nname: \"true\"\n"},
			spec:  Spec{Path: "c", ValueFiles: []string{"a.yaml"}},
			old:   true,
			want:  []Candidate{cand("c/a.yaml", "debug"), cand("c/a.yaml", "name")},
		},
		{
			name:  "global and subchart keys are plain keys",
			files: map[string]string{"c/a.yaml": "global:\n  region: x\nsub:\n  inner:\n    region: x\n"},
			spec:  Spec{Path: "c", ValueFiles: []string{"a.yaml"}},
			old:   "x",
			want:  []Candidate{cand("c/a.yaml", "global.region"), cand("c/a.yaml", "sub.inner.region")},
		},
		{
			name:         "chart defaults are reported, never candidates",
			files:        map[string]string{"c/values.yaml": "x: 1\n", "c/a.yaml": "y: 2\n"},
			spec:         Spec{Path: "c", ValueFiles: []string{"a.yaml"}},
			old:          float64(1),
			wantDefaults: []string{"x"},
		},
		{
			name:  "the chart values file is never a candidate, a value file with the same name is",
			files: map[string]string{"c/values.yaml": "x: 1\n"},
			spec:  Spec{Path: "c", ValueFiles: []string{"values.yaml"}},
			old:   float64(1),
			want:  []Candidate{cand("c/values.yaml", "x")},
		},
		{
			name:           "valuesObject controls the key",
			files:          map[string]string{"c/a.yaml": "x: 1\n"},
			spec:           Spec{Path: "c", ValueFiles: []string{"a.yaml"}, ValuesObject: []byte(`{"x": 5}`)},
			old:            float64(1),
			wantControlled: []string{"helm.valuesObject x"},
		},
		{
			name:           "an inline setting below a matched key controls it",
			files:          map[string]string{"c/a.yaml": "m: 1\n"},
			spec:           Spec{Path: "c", ValueFiles: []string{"a.yaml"}, Parameters: []Parameter{{Name: "m.p", Value: "2"}}},
			old:            float64(1),
			wantControlled: []string{"helm.parameters m.p"},
		},
		{
			name:           "an inline list above a matched item controls it",
			files:          map[string]string{"c/a.yaml": "l: [a, b]\n"},
			spec:           Spec{Path: "c", ValueFiles: []string{"a.yaml"}, Values: "l: [z]\n"},
			old:            "b",
			wantControlled: []string{"helm.values l"},
		},
		{
			name:           "a file parameter controls the key",
			files:          map[string]string{"c/a.yaml": "tls: old\n", "c/tls.pem": "pem"},
			spec:           Spec{Path: "c", ValueFiles: []string{"a.yaml"}, FileParameters: []FileParameter{{Name: "tls", Path: "tls.pem"}}},
			old:            "old",
			wantControlled: []string{"helm.fileParameters tls"},
		},
		{
			name:           "the later inline setting wins",
			files:          map[string]string{"c/a.yaml": "x: 0\n"},
			spec:           Spec{Path: "c", ValueFiles: []string{"a.yaml"}, Values: "x: 1\n", Parameters: []Parameter{{Name: "x", Value: "2"}}},
			old:            "1",
			wantControlled: nil, // x=1 is shadowed by the parameter x=2
		},
		{
			name:           "inline value equal to the old value, key not in any file",
			files:          map[string]string{"c/a.yaml": "x: 0\n"},
			spec:           Spec{Path: "c", ValueFiles: []string{"a.yaml"}, Values: "y: 7\n"},
			old:            float64(7),
			wantControlled: []string{"helm.values y"},
		},
		{
			name:  "a key spelled through a merge key is not a candidate",
			files: map[string]string{"c/a.yaml": "base: &b\n  size: 9\nd:\n  <<: *b\n"},
			spec:  Spec{Path: "c", ValueFiles: []string{"a.yaml"}},
			old:   float64(9),
			want:  []Candidate{cand("c/a.yaml", "base.size")},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := fstest.MapFS{}
			raw := map[string][]byte{}
			for n, c := range tt.files {
				m[n] = &fstest.MapFile{Data: []byte(c)}
				raw[n] = []byte(c)
			}
			src, err := NewSource(m, tt.spec)
			if err != nil {
				t.Fatal(err)
			}
			res, err := src.Find(raw, tt.old)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(res.Candidates, tt.want) {
				t.Errorf("Candidates = %v, want %v", res.Candidates, tt.want)
			}
			var defaults []string
			for _, d := range res.ChartDefaults {
				defaults = append(defaults, d.String())
			}
			if !reflect.DeepEqual(defaults, tt.wantDefaults) {
				t.Errorf("ChartDefaults = %v, want %v", defaults, tt.wantDefaults)
			}
			var controlled []string
			for _, c := range res.Controlled {
				controlled = append(controlled, c.Layer+" "+c.Path.String())
			}
			if !reflect.DeepEqual(controlled, tt.wantControlled) {
				t.Errorf("Controlled = %v, want %v", controlled, tt.wantControlled)
			}
		})
	}
}

func TestFindRemoteValueFilesAreReported(t *testing.T) {
	fsys := fstest.MapFS{"c/a.yaml": &fstest.MapFile{Data: []byte("x: 1\n")}}
	src, err := NewSource(fsys, Spec{Path: "c", ValueFiles: []string{"https://example.com/v.yaml", "a.yaml"}})
	if err != nil {
		t.Fatal(err)
	}
	res, err := src.Find(map[string][]byte{"c/a.yaml": []byte("x: 1\n")}, float64(2))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.RemoteValueFiles, []string{"https://example.com/v.yaml"}) {
		t.Fatalf("RemoteValueFiles = %v", res.RemoteValueFiles)
	}
}

func TestFindErrors(t *testing.T) {
	fsys := fstest.MapFS{"c/a.yaml": &fstest.MapFile{Data: []byte("x: 1\n")}}
	src, err := NewSource(fsys, Spec{Path: "c", ValueFiles: []string{"a.yaml"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.Find(map[string][]byte{}, "x"); err == nil || !strings.Contains(err.Error(), "c/a.yaml") {
		t.Errorf("a value file that was not provided: err = %v", err)
	}
	if _, err := src.Find(map[string][]byte{"c/a.yaml": []byte("- not a map\n")}, "x"); err == nil {
		t.Error("a value file that is not a map was accepted")
	}
}
