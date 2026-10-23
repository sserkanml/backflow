package helmmap

import (
	"errors"
	"strings"
	"testing"

	"github.com/sserkanml/backflow/internal/rendermap"
)

func TestAddValue(t *testing.T) {
	const doc = `# Production values.
replicaCount: 2 # replicas

image:
  # Pinned by the platform team.
  repository: nginx

service:
  type: ClusterIP
`
	tests := []struct {
		name    string
		content string
		key     string
		value   string
		tag     string
		want    string // the whole new file
		line    int
	}{
		{
			name: "appended to an existing parent map, comments kept", content: doc, key: "image.tag", value: "1.28", tag: "!!str",
			want: "# Production values.\nreplicaCount: 2 # replicas\n\nimage:\n  # Pinned by the platform team.\n  repository: nginx\n  tag: \"1.28\"\n\nservice:\n  type: ClusterIP\n",
			line: 7,
		},
		{
			name: "intermediate maps are created", content: doc, key: "metrics.serviceMonitor.interval", value: "30s", tag: "!!str",
			want: doc + "metrics:\n  serviceMonitor:\n    interval: 30s\n", line: 12,
		},
		{
			name: "a new top level key", content: doc, key: "nameOverride", value: "web", tag: "!!str",
			want: doc + "nameOverride: web\n", line: 10,
		},
		{
			name: "an int default gives an int", content: doc, key: "service.port", value: "8080", tag: "!!int",
			want: strings.Replace(doc, "  type: ClusterIP\n", "  type: ClusterIP\n  port: 8080\n", 1), line: 10,
		},
		{
			name: "a bool default gives a bool", content: doc, key: "service.enabled", value: "false", tag: "!!bool",
			want: strings.Replace(doc, "  type: ClusterIP\n", "  type: ClusterIP\n  enabled: false\n", 1), line: 10,
		},
		{
			name: "a float default keeps 1.20 a string", content: doc, key: "service.ratio", value: "1.20", tag: "!!float",
			want: strings.Replace(doc, "  type: ClusterIP\n", "  type: ClusterIP\n  ratio: \"1.20\"\n", 1), line: 10,
		},
		{
			name: "a float default gets a float", content: doc, key: "service.ratio", value: "2.5", tag: "!!float",
			want: strings.Replace(doc, "  type: ClusterIP\n", "  type: ClusterIP\n  ratio: 2.5\n", 1), line: 10,
		},
		{
			name: "a string default, a value that looks like a bool is quoted", content: doc, key: "service.flag", value: "true", tag: "!!str",
			want: strings.Replace(doc, "  type: ClusterIP\n", "  type: ClusterIP\n  flag: \"true\"\n", 1), line: 10,
		},
		{
			name: "a value that needs quoting", content: doc, key: "service.note", value: "a: b # c", tag: "!!str",
			want: strings.Replace(doc, "  type: ClusterIP\n", "  type: ClusterIP\n  note: 'a: b # c'\n", 1), line: 10,
		},
		{
			name: "a key with dots", content: doc, key: `service.annotations.example\.com/scrape`, value: "true", tag: "!!str",
			want: strings.Replace(doc, "  type: ClusterIP\n", "  type: ClusterIP\n  annotations:\n    example.com/scrape: \"true\"\n", 1), line: 11,
		},
		{
			name: "a flow style parent is re-rendered", content: "image: {repository: nginx}\n", key: "image.tag", value: "1.28", tag: "!!str",
			want: "image:\n  repository: nginx\n  tag: \"1.28\"\n", line: 3,
		},
		{
			name: "an empty file", content: "", key: "image.tag", value: "1.28", tag: "!!str",
			want: "image:\n  tag: \"1.28\"\n", line: 2,
		},
		{
			name: "a file with comments only", content: "# nothing here yet\n", key: "replicaCount", value: "3", tag: "!!int",
			want: "# nothing here yet\nreplicaCount: 3\n", line: 2,
		},
		{
			name: "a file without a final newline stays without", content: "a: 1", key: "b.c", value: "x", tag: "!!str",
			want: "a: 1\nb:\n  c: x", line: 3,
		},
		{
			name: "CRLF line endings are kept", content: "a: 1\r\nb:\r\n  c: 2\r\n", key: "b.d", value: "x", tag: "!!str",
			want: "a: 1\r\nb:\r\n  c: 2\r\n  d: x\r\n", line: 4,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, err := rendermap.ParseKeyPath(tt.key)
			if err != nil {
				t.Fatal(err)
			}
			w, err := AddValue("v.yaml", []byte(tt.content), key, tt.value, tt.tag)
			if err != nil {
				t.Fatal(err)
			}
			if got := string(w.Edited); got != tt.want {
				t.Errorf("file =\n%q\nwant\n%q", got, tt.want)
			}
			if w.Line != tt.line {
				t.Errorf("line = %d, want %d (%q)", w.Line, tt.line, w.Text)
			}
			if w.File != "v.yaml" || len(w.Edits) != 1 || w.Edits[0].Location != key.Pointer() || w.Diff == "" {
				t.Errorf("result = %+v", w.Result)
			}
			// The new file defines the key and everything the old one did.
			after, err := parseValues(w.Edited)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, d := range after {
				found = found || d.path.Equal(key)
			}
			if !found {
				t.Errorf("the key is not in the new file")
			}
		})
	}
}

func TestAddValueRefuses(t *testing.T) {
	const doc = "image:\n  tag: \"1\"\nenv:\n  - name: A\nflag: null\nlevels: [1, 2]\n"
	for name, key := range map[string]string{
		"the key exists":                "image.tag",
		"a key above exists as a value": "image.tag.minor",
		"a key below exists":            "image",
		"a null is there":               "flag",
		"below a list":                  "levels.0",
		"above a list":                  "env",
	} {
		t.Run(name, func(t *testing.T) {
			k, _ := rendermap.ParseKeyPath(key)
			if _, err := AddValue("v.yaml", []byte(doc), k, "x", "!!str"); !errors.Is(err, ErrCannotSet) {
				t.Errorf("err = %v, want ErrCannotSet", err)
			}
		})
	}
	t.Run("a file that does not parse", func(t *testing.T) {
		if _, err := AddValue("v.yaml", []byte("a: [1\n"), rendermap.KeyPath{"b"}, "x", "!!str"); !errors.Is(err, ErrCannotSet) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("a file that is not a map", func(t *testing.T) {
		if _, err := AddValue("v.yaml", []byte("- a\n- b\n"), rendermap.KeyPath{"b"}, "x", "!!str"); err == nil {
			t.Error("a list at the top level must not be edited")
		}
	})
}
