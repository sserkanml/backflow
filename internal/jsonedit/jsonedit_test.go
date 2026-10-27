package jsonedit

import (
	"errors"
	"strings"
	"testing"
)

const (
	two = `{
  "apiVersion": "apps/v1",
  "kind": "Deployment",
  "metadata": {
    "name": "web"
  },
  "spec": {
    "replicas": 2,
    "template": {
      "spec": {
        "containers": [
          {
            "name": "web",
            "image": "nginx:1.0",
            "env": [
              {
                "name": "A",
                "value": "1"
              },
              {
                "name": "B",
                "value": "2"
              }
            ]
          }
        ]
      }
    }
  }
}
`
	four = "{\n    \"a\": 1,\n    \"b\": {\n        \"c\": [\n            1,\n            2\n        ]\n    }\n}\n"
	tabs = "{\n\t\"a\": 1,\n\t\"b\": {\n\t\t\"c\": \"x\"\n\t}\n}\n"
)

func TestApply(t *testing.T) {
	tests := []struct {
		name string
		src  string
		ops  []Op
		want string
	}{
		{
			name: "replace a string in a nested array of objects",
			src:  two,
			ops:  []Op{{Replace, "/spec/template/spec/containers/0/env/1/value", `"3"`}},
			want: strings.Replace(two, `"value": "2"`, `"value": "3"`, 1),
		},
		{
			name: "replace a number keeps its type and the rest byte for byte",
			src:  two,
			ops:  []Op{{Replace, "/spec/replicas", `5`}},
			want: strings.Replace(two, `"replicas": 2`, `"replicas": 5`, 1),
		},
		{
			name: "an unchanged number keeps its original text",
			src:  `{"v": 1.10, "w": 1e3}`,
			ops:  []Op{{Replace, "/v", `1.1`}, {Replace, "/w", `1000`}},
			want: `{"v": 1.10, "w": 1e3}`,
		},
		{
			name: "a changed number is written as given",
			src:  `{"v": 1.10, "w": 1e3}`,
			ops:  []Op{{Replace, "/v", `1.25`}, {Replace, "/w", `2E3`}},
			want: `{"v": 1.25, "w": 2E3}`,
		},
		{
			name: "types change: string to number, bool, null",
			src:  `{"a": "1", "b": 2, "c": true, "d": 1}`,
			ops:  []Op{{Replace, "/a", `1`}, {Replace, "/b", `"x"`}, {Replace, "/c", `false`}, {Replace, "/d", `null`}},
			want: `{"a": 1, "b": "x", "c": false, "d": null}`,
		},
		{
			name: "strings are escaped, non-ASCII and HTML characters are kept",
			src:  `{"a": "x", "b": "y"}`,
			ops:  []Op{{Replace, "/a", `"line\nbreak \"q\" \\ tab\t"`}, {Replace, "/b", `"<é & ü>é ☃"`}},
			want: `{"a": "line\nbreak \"q\" \\ tab\t", "b": "<é & ü>é ☃"}`,
		},
		{
			name: "an unchanged string keeps its escapes",
			src:  `{"a": "café"}`,
			ops:  []Op{{Replace, "/a", `"café"`}},
			want: `{"a": "café"}`,
		},
		{
			name: "a key with a slash and a tilde",
			src:  `{"a/b": {"c~d": 1}}`,
			ops:  []Op{{Replace, "/a~1b/c~0d", `2`}},
			want: `{"a/b": {"c~d": 2}}`,
		},
		{
			name: "four-space indentation: add a member after the last one",
			src:  four,
			ops:  []Op{{Add, "/z", `"new"`}},
			want: strings.Replace(four, "    }\n}\n", "    },\n    \"z\": \"new\"\n}\n", 1),
		},
		{
			name: "tabs: add inside a nested object",
			src:  tabs,
			ops:  []Op{{Add, "/b/d", `true`}},
			want: "{\n\t\"a\": 1,\n\t\"b\": {\n\t\t\"c\": \"x\",\n\t\t\"d\": true\n\t}\n}\n",
		},
		{
			name: "tabs: add a map",
			src:  tabs,
			ops:  []Op{{Add, "/m", `{"k":1,"l":[1,2]}`}},
			want: "{\n\t\"a\": 1,\n\t\"b\": {\n\t\t\"c\": \"x\"\n\t},\n\t\"m\": {\n\t\t\"k\": 1,\n\t\t\"l\": [\n\t\t\t1,\n\t\t\t2\n\t\t]\n\t}\n}\n",
		},
		{
			name: "compact one line: add",
			src:  `{"a":1,"b":{"c":2}}`,
			ops:  []Op{{Add, "/b/d", `3`}, {Add, "/e", `[1,2]`}},
			want: `{"a":1,"b":{"c":2,"d":3},"e":[1,2]}`,
		},
		{
			name: "spaced one line: add",
			src:  `{"a": 1, "b": 2}`,
			ops:  []Op{{Add, "/c", `3`}},
			want: `{"a": 1, "b": 2, "c": 3}`,
		},
		{
			name: "add into an empty object formats like the parent",
			src:  "{\n  \"a\": {},\n  \"b\": 1\n}\n",
			ops:  []Op{{Add, "/a/k", `"v"`}},
			want: "{\n  \"a\": {\n    \"k\": \"v\"\n  },\n  \"b\": 1\n}\n",
		},
		{
			name: "add into an empty object of a compact parent",
			src:  `{"a":{},"b":1}`,
			ops:  []Op{{Add, "/a/k", `"v"`}},
			want: `{"a":{"k":"v"},"b":1}`,
		},
		{
			name: "add into an empty array formats like the parent",
			src:  "{\n  \"a\": [],\n  \"b\": 1\n}\n",
			ops:  []Op{{Add, "/a/-", `{"n":1}`}},
			want: "{\n  \"a\": [\n    {\n      \"n\": 1\n    }\n  ],\n  \"b\": 1\n}\n",
		},
		{
			name: "append to an array of objects",
			src:  two,
			ops:  []Op{{Add, "/spec/template/spec/containers/0/env/2", `{"name":"C","value":"3"}`}},
			want: strings.Replace(two, "              }\n            ]\n          }", "              },\n              {\n                \"name\": \"C\",\n                \"value\": \"3\"\n              }\n            ]\n          }", 1),
		},
		{
			name: "add creates the maps on the way",
			src:  "{\n  \"a\": 1\n}\n",
			ops:  []Op{{Add, "/x/y/z", `1`}},
			want: "{\n  \"a\": 1,\n  \"x\": {\n    \"y\": {\n      \"z\": 1\n    }\n  }\n}\n",
		},
		{
			name: "add under a null makes it a map",
			src:  `{"a": null}`,
			ops:  []Op{{Add, "/a/k", `1`}},
			want: `{"a": {"k": 1}}`,
		},
		{
			name: "replace of a field the document lacks adds it",
			src:  "{\n  \"a\": 1\n}\n",
			ops:  []Op{{Replace, "/b", `2`}},
			want: "{\n  \"a\": 1,\n  \"b\": 2\n}\n",
		},
		{
			name: "replace a whole list in the surrounding style",
			src:  two,
			ops:  []Op{{Replace, "/spec/template/spec/containers/0/env", `[{"name":"A","value":"1"}]`}},
			want: strings.Replace(two, "              },\n              {\n                \"name\": \"B\",\n                \"value\": \"2\"\n              }\n", "              }\n", 1),
		},
		{
			name: "replace a scalar by a map in the surrounding style",
			src:  tabs,
			ops:  []Op{{Replace, "/a", `{"k":1}`}},
			want: "{\n\t\"a\": {\n\t\t\"k\": 1\n\t},\n\t\"b\": {\n\t\t\"c\": \"x\"\n\t}\n}\n",
		},
		{
			name: "replace a compact map stays compact",
			src:  `{"a": {"k": 1, "l": 2}, "b": 1}`,
			ops:  []Op{{Replace, "/a", `{"m": 3}`}},
			want: `{"a": {"m": 3}, "b": 1}`,
		},
		{
			name: "remove the last member and its comma",
			src:  two,
			ops:  []Op{{Remove, "/spec/replicas", ``}},
			want: strings.Replace(two, "    \"replicas\": 2,\n", "", 1),
		},
		{
			name: "remove the first member",
			src:  four,
			ops:  []Op{{Remove, "/a", ``}},
			want: "{\n    \"b\": {\n        \"c\": [\n            1,\n            2\n        ]\n    }\n}\n",
		},
		{
			name: "remove the last member: no trailing comma, no blank line",
			src:  four,
			ops:  []Op{{Remove, "/b", ``}},
			want: "{\n    \"a\": 1\n}\n",
		},
		{
			name: "remove the only member leaves an empty object",
			src:  tabs,
			ops:  []Op{{Remove, "/b/c", ``}},
			want: "{\n\t\"a\": 1,\n\t\"b\": {}\n}\n",
		},
		{
			name: "remove an array element, the last one and a middle one",
			src:  "{\n  \"l\": [\n    1,\n    2,\n    3\n  ]\n}\n",
			ops:  []Op{{Remove, "/l/2", ``}, {Remove, "/l/0", ``}},
			want: "{\n  \"l\": [\n    2\n  ]\n}\n",
		},
		{
			name: "remove from compact JSON",
			src:  `{"a":1,"b":2,"c":3}`,
			ops:  []Op{{Remove, "/b", ``}, {Remove, "/c", ``}},
			want: `{"a":1}`,
		},
		{
			name: "no trailing newline stays absent",
			src:  "{\n  \"a\": 1\n}",
			ops:  []Op{{Replace, "/a", `2`}},
			want: "{\n  \"a\": 2\n}",
		},
		{
			name: "CRLF is kept and used for what is added",
			src:  "{\r\n  \"a\": 1,\r\n  \"b\": 2\r\n}\r\n",
			ops:  []Op{{Add, "/c", `{"d":1}`}, {Remove, "/a", ``}},
			want: "{\r\n  \"b\": 2,\r\n  \"c\": {\r\n    \"d\": 1\r\n  }\r\n}\r\n",
		},
		{
			name: "a byte order mark is kept",
			src:  "\xef\xbb\xbf{\"a\": 1}\n",
			ops:  []Op{{Replace, "/a", `2`}},
			want: "\xef\xbb\xbf{\"a\": 2}\n",
		},
		{
			name: "a root list",
			src:  "[\n  {\n    \"op\": \"replace\",\n    \"value\": 1\n  }\n]\n",
			ops:  []Op{{Replace, "/0/value", `7`}},
			want: "[\n  {\n    \"op\": \"replace\",\n    \"value\": 7\n  }\n]\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Apply([]byte(tc.src), tc.ops...)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Errorf("got:\n%s\nwant:\n%s", got, tc.want)
			}
		})
	}
}

func TestApplyErrors(t *testing.T) {
	tests := []struct {
		name string
		src  string
		op   Op
		want error
	}{
		{"a duplicate key on the path", `{"a": {"b": 1}, "a": {"b": 2}}`, Op{Replace, "/a/b", `3`}, ErrAmbiguous},
		{"a duplicate key at the end of the path", `{"a": 1, "a": 2}`, Op{Replace, "/a", `3`}, ErrAmbiguous},
		{"remove of a field that is not there", `{"a": 1}`, Op{Remove, "/b", ``}, ErrNotFound},
		{"an element out of range", `{"l": [1]}`, Op{Replace, "/l/5", `1`}, ErrCannotApply},
		{"a member of a scalar", `{"a": 1}`, Op{Add, "/a/b", `1`}, ErrCannotApply},
		{"not JSON", `{"a": }`, Op{Replace, "/a", `1`}, ErrInvalid},
		{"two values in a file", `{"a": 1} {"b": 2}`, Op{Replace, "/a", `1`}, ErrInvalid},
		{"a trailing comma", `{"a": 1,}`, Op{Replace, "/a", `1`}, ErrInvalid},
		{"a value that is not JSON", `{"a": 1}`, Op{Replace, "/a", `{x`}, ErrInvalid},
		{"UTF-16", "\xff\xfe{\x00}\x00", Op{Replace, "/a", `1`}, ErrInvalid},
		{"the whole document cannot be removed", `{"a": 1}`, Op{Remove, "", ``}, ErrCannotApply},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Apply([]byte(tc.src), tc.op)
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestNothingElseChanges(t *testing.T) {
	// A document with odd but valid spelling: escapes, exponents, spacing, an
	// empty map. Only the edited number changes.
	src := "{ \"a\" : [ 1.50 , 2E+2 , \"\\u00e9\\n\" ] ,\r\n\t\"b\":{ } , \"c\" :  -0.0 }\n"
	got, err := Apply([]byte(src), Op{Replace, "/c", `1`})
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.Replace(src, "-0.0", "1", 1); string(got) != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestDuplicateKeysOffThePathAreLeftAlone(t *testing.T) {
	src := `{"x": 1, "x": 2, "a": {"b": 1}}`
	got, err := Apply([]byte(src), Op{Replace, "/a/b", `2`})
	if err != nil || string(got) != `{"x": 1, "x": 2, "a": {"b": 2}}` {
		t.Errorf("got %q, %v", got, err)
	}
}

func TestSingleMemberAndElementStyles(t *testing.T) {
	for name, tc := range map[string]struct {
		src  string
		op   Op
		want string
	}{
		"one member, spaced":   {`{"a": 1}`, Op{Add, "/b", `2`}, `{"a": 1, "b": 2}`},
		"one member, compact":  {`{"a":1}`, Op{Add, "/b", `2`}, `{"a":1,"b":2}`},
		"one element, compact": {`[1]`, Op{Add, "/-", `2`}, `[1,2]`},
		"elements, spaced":     {`[1, 2]`, Op{Add, "/2", `3`}, `[1, 2, 3]`},
		"empty array, compact": {`{"l":[]}`, Op{Add, "/l/-", `1`}, `{"l":[1]}`},
		"remove only element":  {"{\n  \"l\": [\n    1\n  ]\n}", Op{Remove, "/l/0", ``}, "{\n  \"l\": []\n}"},
		"remove first, spaced": {`{"a": 1, "b": 2}`, Op{Remove, "/a", ``}, `{"b": 2}`},
	} {
		got, err := Apply([]byte(tc.src), tc.op)
		if err != nil || string(got) != tc.want {
			t.Errorf("%s: got %q, %v; want %q", name, got, err, tc.want)
		}
	}
}
