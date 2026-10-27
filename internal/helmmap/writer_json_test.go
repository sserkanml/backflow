package helmmap

import (
	"strings"
	"testing"

	"github.com/sserkanml/backflow/internal/rendermap"
)

const jsonValues = `{
  "replicaCount": 2,
  "image": {
    "repository": "nginx",
    "tag": "1.27-alpine"
  },
  "podAnnotations": {
    "owner": "platform"
  },
  "extraEnvs": [
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
`

func TestValueFilesInJSONAreEditedAsJSON(t *testing.T) {
	file := "charts/app/values-prod.json"
	t.Run("a scalar keeps its type and its line", func(t *testing.T) {
		w, err := SetValue(file, []byte(jsonValues), rendermap.KeyPath{"replicaCount"}, "5")
		if err != nil {
			t.Fatal(err)
		}
		if want := strings.Replace(jsonValues, `"replicaCount": 2`, `"replicaCount": 5`, 1); string(w.Edited) != want {
			t.Errorf("edited:\n%s", w.Edited)
		}
		if w.Line != 2 || strings.TrimSpace(w.Text) != `"replicaCount": 5,` {
			t.Errorf("line %d: %q", w.Line, w.Text)
		}
	})
	t.Run("a string below a key", func(t *testing.T) {
		w, err := SetValue(file, []byte(jsonValues), rendermap.KeyPath{"image", "tag"}, "1.28-alpine")
		if err != nil || string(w.Edited) != strings.Replace(jsonValues, "1.27-alpine", "1.28-alpine", 1) {
			t.Errorf("%s, %v", w.Edited, err)
		}
	})
	t.Run("an override is added after the last key, in the file's indentation", func(t *testing.T) {
		w, err := AddValue(file, []byte(jsonValues), rendermap.KeyPath{"ui", "color"}, "red", "!!str")
		if err != nil {
			t.Fatal(err)
		}
		want := strings.Replace(jsonValues, "    }\n  ]\n}\n", "    }\n  ],\n  \"ui\": {\n    \"color\": \"red\"\n  }\n}\n", 1)
		if string(w.Edited) != want {
			t.Errorf("edited:\n%s", w.Edited)
		}
	})
	t.Run("an element of a list", func(t *testing.T) {
		w, err := SetValue(file, []byte(jsonValues), rendermap.KeyPath{"extraEnvs", "1", "value"}, "3")
		if err != nil || string(w.Edited) != strings.Replace(jsonValues, `"value": "2"`, `"value": "3"`, 1) {
			t.Errorf("%s, %v", w.Edited, err)
		}
	})
	t.Run("a whole list is replaced in the file's style", func(t *testing.T) {
		w, err := SetNode(file, []byte(jsonValues), rendermap.KeyPath{"extraEnvs"}, []any{map[string]any{"name": "A", "value": "1"}})
		if err != nil {
			t.Fatal(err)
		}
		want := strings.Replace(jsonValues, "    },\n    {\n      \"name\": \"B\",\n      \"value\": \"2\"\n    }\n", "    }\n", 1)
		if string(w.Edited) != want {
			t.Errorf("edited:\n%s", w.Edited)
		}
	})
	t.Run("a key is removed with its comma", func(t *testing.T) {
		w, err := RemoveKey(file, []byte(jsonValues), rendermap.KeyPath{"podAnnotations"})
		if err != nil {
			t.Fatal(err)
		}
		want := strings.Replace(jsonValues, "  \"podAnnotations\": {\n    \"owner\": \"platform\"\n  },\n", "", 1)
		if string(w.Edited) != want {
			t.Errorf("edited:\n%s", w.Edited)
		}
	})
	t.Run("an empty .json file is not given YAML", func(t *testing.T) {
		if w, err := AddValue(file, nil, rendermap.KeyPath{"a"}, "b", "!!str"); err == nil {
			t.Errorf("wrote %q into an empty JSON file", w.Edited)
		}
	})
}
