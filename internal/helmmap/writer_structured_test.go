package helmmap

import (
	"errors"
	"strings"
	"testing"

	"github.com/sserkanml/backflow/internal/rendermap"
)

func TestSetNode(t *testing.T) {
	const doc = `# Production values.
replicaCount: 2 # replicas

podAnnotations: {}
tolerations: []
nodeSelector:
  # Only the big nodes.
  size: large
extraEnvVars:
  - name: A
    value: "1"
image:
  repository: nginx
`
	tests := []struct {
		name  string
		src   string
		key   string
		value any
		want  string
	}{
		{
			name: "an empty map is filled", src: doc, key: "podAnnotations",
			value: map[string]any{"backflow.io/test": "1"},
			want:  strings.Replace(doc, "podAnnotations: {}\n", "podAnnotations:\n  backflow.io/test: \"1\"\n", 1),
		},
		{
			name: "an empty list gets maps", src: doc, key: "tolerations",
			value: []any{map[string]any{"key": "dedicated", "operator": "Equal", "value": "bf", "effect": "NoSchedule"}},
			want: strings.Replace(doc, "tolerations: []\n",
				"tolerations:\n  - effect: NoSchedule\n    key: dedicated\n    operator: Equal\n    value: bf\n", 1),
		},
		{
			name: "a list is replaced as a whole, siblings and comments stay", src: doc, key: "extraEnvVars",
			value: []any{map[string]any{"name": "A", "value": "1"}, map[string]any{"name": "B", "value": "true"}},
			want: strings.Replace(doc, "extraEnvVars:\n  - name: A\n    value: \"1\"\n",
				"extraEnvVars:\n  - name: A\n    value: \"1\"\n  - name: B\n    value: \"true\"\n", 1),
		},
		{
			name: "a list becomes empty", src: doc, key: "extraEnvVars", value: []any{},
			want: strings.Replace(doc, "extraEnvVars:\n  - name: A\n    value: \"1\"\n", "extraEnvVars: []\n", 1),
		},
		{
			name: "a key is added to a map, the comment above its entries stays", src: doc, key: "nodeSelector.disk",
			value: "ssd",
			want:  strings.Replace(doc, "  size: large\n", "  size: large\n  disk: ssd\n", 1),
		},
		{
			name: "a key that needs quotes", src: doc, key: `nodeSelector.kubernetes\.io/arch`, value: "arm64",
			want: strings.Replace(doc, "  size: large\n", "  size: large\n  kubernetes.io/arch: arm64\n", 1),
		},
		{
			name: "numbers and booleans that are strings stay strings", src: doc, key: "podAnnotations",
			value: map[string]any{"a": "1", "b": "true", "c": "no", "d": "1.20", "e": "null", "f": ""},
			want: strings.Replace(doc, "podAnnotations: {}\n",
				"podAnnotations:\n  a: \"1\"\n  b: \"true\"\n  c: \"no\"\n  d: \"1.20\"\n  e: \"null\"\n  f: \"\"\n", 1),
		},
		{
			name: "a real number stays a number", src: doc, key: "limits", value: map[string]any{"cpu": float64(2), "ratio": 1.5, "on": true},
			want: doc + "limits:\n  cpu: 2\n  \"on\": true\n  ratio: 1.5\n",
		},
		{
			name: "missing maps are created", src: doc, key: "metrics.serviceMonitor.labels", value: map[string]any{"release": "prom"},
			want: doc + "metrics:\n  serviceMonitor:\n    labels:\n      release: prom\n",
		},
		{
			name: "a flow style map is re-rendered in block style", src: "podLabels: {a: b}\nother: 1\n", key: "podLabels",
			value: map[string]any{"a": "b", "c": "d"},
			want:  "podLabels:\n  a: b\n  c: d\nother: 1\n",
		},
		{
			name: "an empty file", src: "", key: "podAnnotations", value: map[string]any{"a": "b"},
			want: "podAnnotations:\n  a: b\n",
		},
		{
			name: "a null becomes a map", src: "podAnnotations:\nother: 1\n", key: "podAnnotations", value: map[string]any{"a": "b"},
			want: "podAnnotations:\n  a: b\nother: 1\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, err := rendermap.ParseKeyPath(tt.key)
			if err != nil {
				t.Fatal(err)
			}
			w, err := SetNode("v.yaml", []byte(tt.src), key, tt.value)
			if err != nil {
				t.Fatal(err)
			}
			if got := string(w.Edited); got != tt.want {
				t.Errorf("file =\n%s\nwant\n%s", got, tt.want)
			}
			if w.File != "v.yaml" || len(w.Edits) != 1 || w.Edits[0].Location != key.Pointer() || w.Diff == "" || w.Line == 0 {
				t.Errorf("result = %+v line %d", w.Result, w.Line)
			}
		})
	}
}

func TestSetNodeRefuses(t *testing.T) {
	const doc = "env:\n  - name: A\nflag: 1\n"
	for name, tt := range map[string]struct {
		key   string
		value any
	}{
		"through a list": {"env.0.value", "x"},
		"below a scalar": {"flag.x", "y"},
		"an empty key":   {"", "x"},
	} {
		t.Run(name, func(t *testing.T) {
			key, _ := rendermap.ParseKeyPath(tt.key)
			if tt.key == "" {
				key = nil
			}
			if _, err := SetNode("v.yaml", []byte(doc), key, tt.value); !errors.Is(err, ErrCannotSet) {
				t.Errorf("err = %v, want ErrCannotSet", err)
			}
		})
	}
	t.Run("a file that does not parse", func(t *testing.T) {
		if _, err := SetNode("v.yaml", []byte("a: [1\n"), rendermap.KeyPath{"b"}, "x"); !errors.Is(err, ErrCannotSet) {
			t.Errorf("err = %v", err)
		}
	})
}

func TestRemoveKey(t *testing.T) {
	const doc = "# Values.\npodAnnotations:\n  # team\n  team: platform\n  extra: \"1\"\nreplicaCount: 2\n"
	t.Run("a key of a map", func(t *testing.T) {
		w, err := RemoveKey("v.yaml", []byte(doc), rendermap.KeyPath{"podAnnotations", "extra"})
		if err != nil {
			t.Fatal(err)
		}
		if want := "# Values.\npodAnnotations:\n  # team\n  team: platform\nreplicaCount: 2\n"; string(w.Edited) != want {
			t.Errorf("file =\n%s", w.Edited)
		}
	})
	t.Run("a whole map", func(t *testing.T) {
		w, err := RemoveKey("v.yaml", []byte(doc), rendermap.KeyPath{"podAnnotations"})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(w.Edited), "replicaCount: 2") || strings.Contains(string(w.Edited), "team") {
			t.Errorf("file =\n%s", w.Edited)
		}
	})
	t.Run("a key the file does not have", func(t *testing.T) {
		if _, err := RemoveKey("v.yaml", []byte(doc), rendermap.KeyPath{"nope"}); !errors.Is(err, ErrCannotSet) {
			t.Errorf("err = %v", err)
		}
	})
}
