package helmmap

import (
	"errors"
	"io/fs"
	"strings"
	"testing"

	"github.com/sserkanml/backflow/internal/rendermap"
)

func TestSetValue(t *testing.T) {
	const doc = `# header comment
replicaCount: 2   # trailing comment
image:
  tag: "1.27"
  pullPolicy: 'IfNotPresent'
  digest: abc
config:
  # Log verbosity: debug, info, warn, error, trace
  logLevel: debug
  timeout: 30
  ratio: 1.5
  debug: true
  name: "true"
labels:
  app.kubernetes.io/name: demo
env:
  - name: A
    value: v1
  - name: B
    value: v2
ports: [80, 443]
`
	tests := []struct {
		name  string
		key   string
		value string
		want  string // the edited line
		line  int
	}{
		{name: "string, plain", key: "config.logLevel", value: "info", want: "  logLevel: info", line: 9},
		{name: "int stays int", key: "config.timeout", value: "45", want: "  timeout: 45", line: 10},
		{name: "int with trailing comment", key: "replicaCount", value: "3", want: "replicaCount: 3 # trailing comment", line: 2},
		{name: "int field gets a non-int, so a string", key: "config.timeout", value: "45s", want: "  timeout: 45s", line: 10},
		{name: "int field, non canonical int is a string", key: "config.timeout", value: "045", want: `  timeout: "045"`, line: 10},
		{name: "float stays float", key: "config.ratio", value: "2.25", want: "  ratio: 2.25", line: 11},
		{name: "float field gets an int", key: "config.ratio", value: "2", want: "  ratio: 2", line: 11},
		{name: "bool stays bool", key: "config.debug", value: "false", want: "  debug: false", line: 12},
		{name: "bool field gets a word, so a string", key: "config.debug", value: "maybe", want: "  debug: maybe", line: 12},
		{name: "double quoted string stays quoted", key: "image.tag", value: "1.28", want: `  tag: "1.28"`, line: 4},
		{name: "single quoted string stays quoted", key: "image.pullPolicy", value: "Always", want: "  pullPolicy: 'Always'", line: 5},
		{name: "quoted string that looks like a bool", key: "config.name", value: "false", want: `  name: "false"`, line: 13},
		{name: "plain string that looks like an int is quoted", key: "image.digest", value: "123", want: `  digest: "123"`, line: 6},
		{name: "key with dots", key: `labels.app\.kubernetes\.io/name`, value: "other", want: "  app.kubernetes.io/name: other", line: 15},
		{name: "list item in a map", key: "env.1.value", value: "v3", want: "    value: v3", line: 20},
		{name: "list item", key: "ports.1", value: "8443", want: "ports: [80, 8443]", line: 21},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, err := rendermap.ParseKeyPath(tt.key)
			if err != nil {
				t.Fatal(err)
			}
			w, err := SetValue("values.yaml", []byte(doc), key, tt.value)
			if err != nil {
				t.Fatal(err)
			}
			if w.Text != tt.want || w.Line != tt.line {
				t.Errorf("edited line %d %q, want %d %q", w.Line, w.Text, tt.line, tt.want)
			}
			// Exactly one line differs, and the comments are intact.
			oldLines := strings.Split(doc, "\n")
			newLines := strings.Split(string(w.Edited), "\n")
			if len(oldLines) != len(newLines) {
				t.Fatalf("line count changed: %d -> %d\n%s", len(oldLines), len(newLines), w.Edited)
			}
			changed := 0
			for i := range oldLines {
				if oldLines[i] != newLines[i] {
					changed++
					if i+1 != tt.line {
						t.Errorf("line %d changed: %q -> %q", i+1, oldLines[i], newLines[i])
					}
				}
			}
			if changed != 1 {
				t.Errorf("%d lines changed, want 1\n%s", changed, w.Diff)
			}
			if !strings.Contains(w.Diff, "values.yaml") || w.File != "values.yaml" || len(w.Edits) != 1 {
				t.Errorf("result = %+v", w.Result)
			}
		})
	}
}

func TestSetValueKeepsCommentsAndOrder(t *testing.T) {
	data, err := fs.ReadFile(demoRepo(t), "helm/demo/values-dev.yaml")
	if err != nil {
		t.Fatal(err)
	}
	w, err := SetValue("helm/demo/values-dev.yaml", data, rendermap.KeyPath{"config", "logLevel"}, "info")
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(string(data), "logLevel: debug", "logLevel: info", 1)
	if string(w.Edited) != want {
		t.Fatalf("edited file differs from a one-line replacement:\n%s", w.Diff)
	}
	if !strings.Contains(string(w.Edited), "# Log verbosity: debug, info, warn, error, trace") {
		t.Error("the comment above the key is gone")
	}
	if w.Line != 12 || w.Text != "  logLevel: info" {
		t.Errorf("line %d %q", w.Line, w.Text)
	}
}

func TestSetValueMapValue(t *testing.T) {
	data, err := fs.ReadFile(demoRepo(t), "helm/demo/values-dev.yaml")
	if err != nil {
		t.Fatal(err)
	}
	w, err := SetValue("v.yaml", data, rendermap.KeyPath{"demo-sidecar", "timeoutSeconds"}, "60")
	if err != nil {
		t.Fatal(err)
	}
	if w.Text != "  timeoutSeconds: 60" {
		t.Errorf("text = %q", w.Text)
	}
}

func TestSetValueErrors(t *testing.T) {
	const doc = "a:\n  b: 1\nl: [1, 2]\nbase: &b\n  s: 1\nd:\n  <<: *b\n"
	tests := []struct {
		name string
		file string
		key  rendermap.KeyPath
	}{
		{"missing key", "v.yaml", rendermap.KeyPath{"a", "c"}},
		{"missing parent", "v.yaml", rendermap.KeyPath{"x", "y"}},
		{"a map is not a scalar", "v.yaml", rendermap.KeyPath{"a"}},
		{"a list is not a scalar", "v.yaml", rendermap.KeyPath{"l"}},
		{"list index out of range", "v.yaml", rendermap.KeyPath{"l", "2"}},
		{"key below a scalar", "v.yaml", rendermap.KeyPath{"a", "b", "c"}},
		{"entry that only exists through a merge key", "v.yaml", rendermap.KeyPath{"d", "s"}},
		{"json files are not edited", "values.json", rendermap.KeyPath{"a", "b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := SetValue(tt.file, []byte(doc), tt.key, "9")
			if !errors.Is(err, ErrCannotSet) && tt.file != "values.json" {
				t.Fatalf("err = %v, want ErrCannotSet", err)
			}
			if err == nil {
				t.Fatal("succeeded")
			}
		})
	}
	if _, err := SetValue("v.yaml", []byte("a: [1\n"), rendermap.KeyPath{"a"}, "1"); !errors.Is(err, ErrCannotSet) {
		t.Errorf("invalid YAML: err = %v", err)
	}
}

func TestSetValueFloatRoundTrip(t *testing.T) {
	const doc = "image:\n  tag: 1.10\n  digits: 127\n  name: app\n"
	tests := []struct {
		key, value, want string
	}{
		// 1.10 is a float that renders as 1.1; 1.20 would render as 1.2, which is
		// not what was meant, so it is written as a string.
		{"image.tag", "1.20", `  tag: "1.20"`},
		{"image.tag", "1.2", "  tag: 1.2"},
		{"image.tag", "2", "  tag: 2"},
		{"image.tag", "1e3", `  tag: "1e3"`},
		{"image.tag", "+1.5", `  tag: "+1.5"`},
		{"image.tag", "true", `  tag: "true"`},
		{"image.digits", "128", "  digits: 128"},
		{"image.digits", "0128", `  digits: "0128"`},
		{"image.name", "app2", "  name: app2"},
	}
	for _, tt := range tests {
		t.Run(tt.key+"="+tt.value, func(t *testing.T) {
			key, _ := rendermap.ParseKeyPath(tt.key)
			w, err := SetValue("v.yaml", []byte(doc), key, tt.value)
			if err != nil {
				t.Fatal(err)
			}
			if w.Text != tt.want {
				t.Errorf("line = %q, want %q", w.Text, tt.want)
			}
		})
	}
}
