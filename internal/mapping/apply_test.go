package mapping

import (
	"errors"
	"strings"
	"testing"

	"github.com/sserkanml/backflow/api/v1alpha1"
)

const configMapYAML = `# Demo configuration
apiVersion: v1
kind: ConfigMap
metadata:
  name: demo-config
  namespace: demo
  labels:
    app.kubernetes.io/name: demo
data:
  # Log verbosity: info, debug
  LOG_LEVEL: info
  FEATURE: "on" # quoted on purpose
  TIMEOUT: 30
`

const multiDocYAML = `# Service first
apiVersion: v1
kind: Service
metadata:
  name: demo
spec:
  ports:
    - port: 80
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: demo
spec:
  replicas: 1 # keep small

  template:
    spec:
      containers:
        - name: demo
          image: nginx:1.0
          args: ["a", "b"]
        - name: side
          image: busybox
      # about volumes
      volumes: []
---
apiVersion: v1
kind: Namespace
metadata:
  name: demo
`

const edgeYAML = `apiVersion: v1
kind: ConfigMap
metadata:
  name: edge
  annotations: {}
  labels: {a: b, c: d}
data:
  empty:
  café: x
`

func replace(path, live string) v1alpha1.FieldChange {
	return v1alpha1.FieldChange{Path: path, Op: v1alpha1.OpReplace, Live: live}
}
func add(path, live string) v1alpha1.FieldChange {
	return v1alpha1.FieldChange{Path: path, Op: v1alpha1.OpAdd, Live: live}
}
func remove(path string) v1alpha1.FieldChange {
	return v1alpha1.FieldChange{Path: path, Op: v1alpha1.OpRemove}
}

// swap returns s with the first occurrence of from replaced by to, failing
// the test when from is not there, so a typo cannot make a case pass.
func swap(t *testing.T, s, from, to string) string {
	t.Helper()
	if !strings.Contains(s, from) {
		t.Fatalf("fixture has no %q", from)
	}
	return strings.Replace(s, from, to, 1)
}

func TestApply(t *testing.T) {
	type tc struct {
		name    string
		file    string
		doc     int
		input   string
		changes []v1alpha1.FieldChange
		want    func(t *testing.T) string
	}
	tests := []tc{
		{
			name: "replace a plain string, comments and other lines untouched",
			file: "cm.yaml", input: configMapYAML,
			changes: []v1alpha1.FieldChange{replace("/data/LOG_LEVEL", `"debug"`)},
			want: func(t *testing.T) string {
				return swap(t, configMapYAML, "  LOG_LEVEL: info\n", "  LOG_LEVEL: debug\n")
			},
		},
		{
			name: "a quoted string stays quoted, its trailing comment stays",
			file: "cm.yaml", input: configMapYAML,
			changes: []v1alpha1.FieldChange{replace("/data/FEATURE", `"off"`)},
			want: func(t *testing.T) string {
				return swap(t, configMapYAML, `  FEATURE: "on" # quoted on purpose`, `  FEATURE: "off" # quoted on purpose`)
			},
		},
		{
			name: "a number stays a number",
			file: "cm.yaml", input: configMapYAML,
			changes: []v1alpha1.FieldChange{replace("/data/TIMEOUT", `45`)},
			want: func(t *testing.T) string {
				return swap(t, configMapYAML, "  TIMEOUT: 30\n", "  TIMEOUT: 45\n")
			},
		},
		{
			name: "a number becoming a string is quoted",
			file: "cm.yaml", input: configMapYAML,
			changes: []v1alpha1.FieldChange{replace("/data/TIMEOUT", `"45"`)},
			want: func(t *testing.T) string {
				return swap(t, configMapYAML, "  TIMEOUT: 30\n", "  TIMEOUT: \"45\"\n")
			},
		},
		{
			name: "a string that looks like a bool is quoted",
			file: "cm.yaml", input: configMapYAML,
			changes: []v1alpha1.FieldChange{replace("/data/LOG_LEVEL", `"true"`)},
			want: func(t *testing.T) string {
				return swap(t, configMapYAML, "  LOG_LEVEL: info\n", "  LOG_LEVEL: \"true\"\n")
			},
		},
		{
			name: "a key with / is addressed by its escaped pointer",
			file: "cm.yaml", input: configMapYAML,
			changes: []v1alpha1.FieldChange{replace("/metadata/labels/app.kubernetes.io~1name", `"other"`)},
			want: func(t *testing.T) string {
				return swap(t, configMapYAML, "    app.kubernetes.io/name: demo\n", "    app.kubernetes.io/name: other\n")
			},
		},
		{
			name: "add a key after the last entry",
			file: "cm.yaml", input: configMapYAML,
			changes: []v1alpha1.FieldChange{add("/data/NEW_KEY", `"x"`)},
			want:    func(t *testing.T) string { return configMapYAML + "  NEW_KEY: x\n" },
		},
		{
			name: "replace of a field the file does not have is applied as an add",
			file: "cm.yaml", input: configMapYAML,
			changes: []v1alpha1.FieldChange{replace("/data/DEFAULTED", `"x"`)},
			want:    func(t *testing.T) string { return configMapYAML + "  DEFAULTED: x\n" },
		},
		{
			name: "replace below a missing parent creates the parents",
			file: "cm.yaml", input: configMapYAML,
			changes: []v1alpha1.FieldChange{replace("/spec/strategy/type", `"Recreate"`)},
			want:    func(t *testing.T) string { return configMapYAML + "spec:\n  strategy:\n    type: Recreate\n" },
		},
		{
			name: "add a key containing ~",
			file: "cm.yaml", input: configMapYAML,
			changes: []v1alpha1.FieldChange{add("/data/a~0b", `"x"`)},
			want:    func(t *testing.T) string { return configMapYAML + "  a~b: x\n" },
		},
		{
			name: "add creates missing intermediate maps",
			file: "cm.yaml", input: configMapYAML,
			changes: []v1alpha1.FieldChange{add("/metadata/annotations/example.com~1owner", `"me"`)},
			want: func(t *testing.T) string {
				return swap(t, configMapYAML, "    app.kubernetes.io/name: demo\n",
					"    app.kubernetes.io/name: demo\n  annotations:\n    example.com/owner: me\n")
			},
		},
		{
			name: "add a nested map several levels deep",
			file: "cm.yaml", input: configMapYAML,
			changes: []v1alpha1.FieldChange{add("/data/a/b/c", `1`)},
			want:    func(t *testing.T) string { return configMapYAML + "  a:\n    b:\n      c: 1\n" },
		},
		{
			name: "remove a key; the comment above its neighbour stays",
			file: "cm.yaml", input: configMapYAML,
			changes: []v1alpha1.FieldChange{remove("/data/FEATURE")},
			want: func(t *testing.T) string {
				return swap(t, configMapYAML, "  FEATURE: \"on\" # quoted on purpose\n", "")
			},
		},
		{
			name: "several changes in one go",
			file: "cm.yaml", input: configMapYAML,
			changes: []v1alpha1.FieldChange{
				add("/data/NEW_KEY", `"x"`), remove("/data/FEATURE"), replace("/data/LOG_LEVEL", `"debug"`),
			},
			want: func(t *testing.T) string {
				s := swap(t, configMapYAML, "  LOG_LEVEL: info\n", "  LOG_LEVEL: debug\n")
				s = swap(t, s, "  FEATURE: \"on\" # quoted on purpose\n", "")
				return s + "  NEW_KEY: x\n"
			},
		},
		{
			name: "replica change in the second document of a multi-document file",
			file: "all.yaml", doc: 1, input: multiDocYAML,
			changes: []v1alpha1.FieldChange{replace("/spec/replicas", `3`)},
			want: func(t *testing.T) string {
				return swap(t, multiDocYAML, "  replicas: 1 # keep small\n", "  replicas: 3 # keep small\n")
			},
		},
		{
			name: "container image inside a list",
			file: "all.yaml", doc: 1, input: multiDocYAML,
			changes: []v1alpha1.FieldChange{
				replace("/spec/template/spec/containers/0/image", `"nginx:2.0"`),
				replace("/spec/template/spec/containers/1/image", `"busybox:1"`),
			},
			want: func(t *testing.T) string {
				s := swap(t, multiDocYAML, "          image: nginx:1.0\n", "          image: nginx:2.0\n")
				return swap(t, s, "          image: busybox\n", "          image: busybox:1\n")
			},
		},
		{
			name: "a whole array replaced with one of a different length",
			file: "all.yaml", doc: 1, input: multiDocYAML,
			changes: []v1alpha1.FieldChange{replace("/spec/template/spec/containers/0/args", `["a","b","c"]`)},
			want: func(t *testing.T) string {
				return swap(t, multiDocYAML, "          args: [\"a\", \"b\"]\n",
					"          args:\n            - a\n            - b\n            - c\n")
			},
		},
		{
			name: "a list item replaced as a whole keeps the comments after the list",
			file: "all.yaml", doc: 1, input: multiDocYAML,
			changes: []v1alpha1.FieldChange{replace("/spec/template/spec/containers/1", `{"name":"side","image":"busybox:2"}`)},
			want: func(t *testing.T) string {
				return swap(t, multiDocYAML, "        - name: side\n          image: busybox\n",
					"        - name: side\n          image: busybox:2\n")
			},
		},
		{
			name: "add into an empty flow map",
			file: "edge.yaml", input: edgeYAML,
			changes: []v1alpha1.FieldChange{add("/metadata/annotations/team", `"x"`)},
			want: func(t *testing.T) string {
				return swap(t, edgeYAML, "  annotations: {}\n", "  annotations:\n    team: x\n")
			},
		},
		{
			name: "replace inside a flow map keeps the flow style",
			file: "edge.yaml", input: edgeYAML,
			changes: []v1alpha1.FieldChange{replace("/metadata/labels/a", `"z"`)},
			want: func(t *testing.T) string {
				return swap(t, edgeYAML, "  labels: {a: b, c: d}\n", "  labels: {a: z, c: d}\n")
			},
		},
		{
			name: "remove from a flow map",
			file: "edge.yaml", input: edgeYAML,
			changes: []v1alpha1.FieldChange{remove("/metadata/labels/a")},
			want: func(t *testing.T) string {
				return swap(t, edgeYAML, "  labels: {a: b, c: d}\n", "  labels: {c: d}\n")
			},
		},
		{
			name: "add below a null value turns it into a map",
			file: "edge.yaml", input: edgeYAML,
			changes: []v1alpha1.FieldChange{add("/data/empty/sub", `"v"`)},
			want: func(t *testing.T) string {
				return swap(t, edgeYAML, "  empty:\n", "  empty:\n    sub: v\n")
			},
		},
		{
			name: "non-ASCII keys",
			file: "edge.yaml", input: edgeYAML,
			changes: []v1alpha1.FieldChange{replace("/data/café", `"y"`)},
			want: func(t *testing.T) string {
				return swap(t, edgeYAML, "  café: x\n", "  café: y\n")
			},
		},
		{
			name: "a file without a final newline stays without one",
			file: "cm.yaml", input: strings.TrimSuffix(configMapYAML, "\n"),
			changes: []v1alpha1.FieldChange{replace("/data/TIMEOUT", `45`)},
			want: func(t *testing.T) string {
				return strings.TrimSuffix(swap(t, configMapYAML, "  TIMEOUT: 30\n", "  TIMEOUT: 45\n"), "\n")
			},
		},
		{
			name: "CRLF line endings are kept",
			file: "cm.yaml", input: strings.ReplaceAll(configMapYAML, "\n", "\r\n"),
			changes: []v1alpha1.FieldChange{replace("/data/LOG_LEVEL", `"debug"`), add("/data/NEW_KEY", `"x"`)},
			want: func(t *testing.T) string {
				s := swap(t, configMapYAML, "  LOG_LEVEL: info\n", "  LOG_LEVEL: debug\n") + "  NEW_KEY: x\n"
				return strings.ReplaceAll(s, "\n", "\r\n")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Apply(tt.file, []byte(tt.input), tt.doc, tt.changes)
			if err != nil {
				t.Fatal(err)
			}
			if want := tt.want(t); string(got) != want {
				t.Errorf("edited file differs\n--- got ---\n%s\n--- want ---\n%s", got, want)
			}
		})
	}
}

const blockScalarsYAML = `apiVersion: v1
kind: ConfigMap
metadata:
  name: scripts
data:
  start.sh: |
    #!/bin/sh
    echo hello
    # a comment line that is script text
  keep: |+
    kept

  clip: |
    clipped

  folded: >-
    folded
    text
  last.sh: |
    echo last
    # the final line is script text too
`

func TestApplyBlockScalars(t *testing.T) {
	tests := []struct {
		name    string
		changes []v1alpha1.FieldChange
		from    string
		to      string
	}{
		{
			name:    "replace a script that ends with a comment-looking line",
			changes: []v1alpha1.FieldChange{replace("/data/start.sh", `"#!/bin/sh\necho bye\n# a comment line that is script text\n"`)},
			from:    "    echo hello\n    # a comment line that is script text\n",
			to:      "    echo bye\n    # a comment line that is script text\n",
		},
		{
			name:    "replace the last entry, a script that ends with a comment-looking line",
			changes: []v1alpha1.FieldChange{replace("/data/last.sh", `"echo done\n# the final line is script text too\n"`)},
			from:    "    echo last\n",
			to:      "    echo done\n",
		},
		{
			name:    "keep chomping: trailing blank lines are part of the value",
			changes: []v1alpha1.FieldChange{replace("/data/keep", `"changed\n\n"`)},
			from:    "    kept\n\n  clip:",
			to:      "    changed\n\n  clip:",
		},
		{
			name:    "clip chomping: the blank line after the value is not part of it and stays",
			changes: []v1alpha1.FieldChange{replace("/data/clip", `"other\n"`)},
			from:    "    clipped\n\n  folded:",
			to:      "    other\n\n  folded:",
		},
		{
			name:    "a folded scalar is replaced as a whole",
			changes: []v1alpha1.FieldChange{replace("/data/folded", `"new"`)},
			from:    "  folded: >-\n    folded\n    text\n",
			to:      "  folded: new\n",
		},
		{
			name:    "an entry added after a script that ends with a comment-looking line comes after all of it",
			changes: []v1alpha1.FieldChange{add("/data/new", `"x"`)},
			from:    "    # the final line is script text too\n",
			to:      "    # the final line is script text too\n  new: x\n",
		},
		{
			name:    "an entry removed next to a script leaves the script alone",
			changes: []v1alpha1.FieldChange{remove("/data/folded")},
			from:    "  folded: >-\n    folded\n    text\n",
			to:      "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Apply("cm.yaml", []byte(blockScalarsYAML), 0, tt.changes)
			if err != nil {
				t.Fatal(err)
			}
			want := swap(t, blockScalarsYAML, tt.from, tt.to)
			if string(got) != want {
				t.Errorf("edited file differs\n--- got ---\n%s\n--- want ---\n%s", got, want)
			}
		})
	}

	t.Run("editing another entry never touches the scripts", func(t *testing.T) {
		got, err := Apply("cm.yaml", []byte(blockScalarsYAML), 0, []v1alpha1.FieldChange{replace("/metadata/name", `"renamed"`)})
		if err != nil {
			t.Fatal(err)
		}
		if want := swap(t, blockScalarsYAML, "name: scripts", "name: renamed"); string(got) != want {
			t.Errorf("got:\n%s", got)
		}
	})
}

func TestApplyKeepChompingAtTheEndOfTheFile(t *testing.T) {
	in := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: k\ndata:\n  keep: |+\n    kept\n\n\n"
	got, err := Apply("cm.yaml", []byte(in), 0, []v1alpha1.FieldChange{add("/data/new", `"x"`)})
	if err != nil {
		t.Fatal(err)
	}
	want := in + "  new: x\n"
	if string(got) != want {
		t.Errorf("got:\n%q\nwant:\n%q", got, want)
	}
}

func TestApplyKeepsOtherDocumentsAndLinesByteForByte(t *testing.T) {
	got, err := Apply("all.yaml", []byte(multiDocYAML), 1, []v1alpha1.FieldChange{
		replace("/spec/replicas", `3`),
		replace("/spec/template/spec/containers", `[{"name":"demo","image":"nginx:2.0"},{"name":"side","image":"busybox"},{"name":"new","image":"x"}]`),
	})
	if err != nil {
		t.Fatal(err)
	}
	segs := splitDocuments(multiDocYAML)
	gotSegs := splitDocuments(string(got))
	if len(segs) != 3 || len(gotSegs) != 3 {
		t.Fatalf("documents: %d -> %d", len(segs), len(gotSegs))
	}
	for _, i := range []int{0, 2} {
		if multiDocYAML[segs[i].start:segs[i].end] != string(got)[gotSegs[i].start:gotSegs[i].end] {
			t.Errorf("document %d changed", i)
		}
	}
	for _, line := range []string{"# Service first", "  replicas: 3 # keep small", "      # about volumes", "      volumes: []", "---"} {
		if !strings.Contains(string(got), line+"\n") {
			t.Errorf("line %q is gone from:\n%s", line, got)
		}
	}
	// The blank line between replicas and template sits after the entry and is kept.
	if !strings.Contains(string(got), "# keep small\n\n  template:") {
		t.Errorf("blank line lost:\n%s", got)
	}
	if !strings.Contains(string(got), "name: new") {
		t.Errorf("new container missing:\n%s", got)
	}
}

func TestApplyErrors(t *testing.T) {
	tests := []struct {
		name    string
		file    string
		doc     int
		change  v1alpha1.FieldChange
		wantErr error
	}{
		{"replace below a scalar", "cm.yaml", 0, replace("/data/LOG_LEVEL/x", `"x"`), ErrCannotApply},
		{"replace of a missing array item", "all.yaml", 1, replace("/spec/template/spec/containers/5/image", `"x"`), ErrCannotApply},
		{"add a field that already exists", "cm.yaml", 0, add("/data/LOG_LEVEL", `"x"`), ErrCannotApply},
		{"remove a field that is not in the file", "cm.yaml", 0, remove("/data/MISSING"), ErrCannotApply},
		{"remove an array item", "all.yaml", 1, remove("/spec/template/spec/containers/1"), ErrCannotApply},
		{"add into an array", "all.yaml", 1, add("/spec/template/spec/containers/5", `{}`), ErrCannotApply},
		{"index out of range", "all.yaml", 1, replace("/spec/template/spec/containers/9/image", `"x"`), ErrCannotApply},
		{"index with leading zero", "all.yaml", 1, replace("/spec/template/spec/containers/00/image", `"x"`), ErrCannotApply},
		{"not a pointer", "cm.yaml", 0, replace("data/LOG_LEVEL", `"x"`), ErrCannotApply},
		{"empty pointer", "cm.yaml", 0, replace("", `"x"`), ErrCannotApply},
		{"invalid JSON value", "cm.yaml", 0, replace("/data/LOG_LEVEL", `{oops`), ErrCannotApply},
		{"trailing data after the value", "cm.yaml", 0, replace("/data/LOG_LEVEL", `"a" "b"`), ErrCannotApply},
		{"unknown operation", "cm.yaml", 0, v1alpha1.FieldChange{Path: "/data/LOG_LEVEL", Op: "Move"}, ErrCannotApply},
		{"document out of range", "cm.yaml", 5, replace("/data/LOG_LEVEL", `"x"`), ErrCannotApply},
		{"JSON files cannot be edited yet", "cm.json", 0, replace("/data/LOG_LEVEL", `"x"`), ErrUnsupportedFileFormat},
	}
	inputs := map[string]string{"cm.yaml": configMapYAML, "cm.json": `{"data": {"LOG_LEVEL": "info"}}`, "all.yaml": multiDocYAML}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Apply(tt.file, []byte(inputs[tt.file]), tt.doc, []v1alpha1.FieldChange{tt.change})
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("err = %v, want %v", err, tt.wantErr)
			}
		})
	}
}
