package helmmap

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestParseValues(t *testing.T) {
	doc := `
replicaCount: 2
image: {tag: "1.27", pullPolicy: null}
labels:
  app.kubernetes.io/name: demo
args: [a, 2]
hex: 0x10
float: 1.50
on: true
base: &base
  size: 1
derived:
  <<: *base
  size: 2
  extra: 3
empty: {}
`
	got, err := parseValues([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		path  string
		kind  defKind
		value string
		merge bool
	}
	rows := make([]row, 0, len(got))
	for _, d := range got {
		rows = append(rows, row{d.path.String(), d.kind, d.value, d.viaMerge})
	}
	want := []row{
		{"replicaCount", defScalar, "2", false},
		{"image.tag", defScalar, "1.27", false},
		{"image.pullPolicy", defNull, "", false},
		{`labels.app\.kubernetes\.io/name`, defScalar, "demo", false},
		{"args", defList, "", false},
		{"args.0", defScalar, "a", false},
		{"args.1", defScalar, "2", false},
		{"hex", defScalar, "16", false},
		{"float", defScalar, "1.5", false},
		{"on", defScalar, "true", false},
		{"base.size", defScalar, "1", false},
		{"derived.size", defScalar, "2", false},
		{"derived.extra", defScalar, "3", false},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("got\n%+v\nwant\n%+v", rows, want)
	}
}

func TestParseValuesMergeKey(t *testing.T) {
	got, err := parseValues([]byte("base: &b\n  a: 1\n  b: 2\nd:\n  <<: *b\n  b: 3\n"))
	if err != nil {
		t.Fatal(err)
	}
	merged := map[string]bool{}
	for _, d := range got {
		merged[d.path.String()] = d.viaMerge
	}
	if merged["d.a"] != true || merged["d.b"] != false {
		t.Fatalf("viaMerge = %v: d.a must come from the merge key, d.b is set explicitly", merged)
	}
}

func TestParseValuesInvalid(t *testing.T) {
	for _, in := range []string{"- a\n", "a: [1\n", "just text\n"} {
		if _, err := parseValues([]byte(in)); err == nil {
			t.Errorf("parseValues(%q) succeeded", in)
		}
	}
	for _, in := range []string{"", "# only a comment\n", "null\n"} {
		defs, err := parseValues([]byte(in))
		if err != nil || len(defs) != 0 {
			t.Errorf("parseValues(%q) = %v, %v", in, defs, err)
		}
	}
}

func TestRenderValue(t *testing.T) {
	tests := []struct {
		in   any
		want string
	}{
		{"3", "3"},
		{3, "3"},
		{int64(3), "3"},
		{float64(3), "3"},
		{1.5, "1.5"},
		{float64(1000000), "1000000"},
		{json.Number("3"), "3"},
		{json.Number("3.0"), "3"},
		{json.Number("2.50"), "2.5"},
		{true, "true"},
		{false, "false"},
		{"", ""},
		{nil, ""},
		{map[string]any{"a": 1}, ""},
	}
	for _, tt := range tests {
		if got := RenderValue(tt.in); got != tt.want {
			t.Errorf("RenderValue(%#v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
