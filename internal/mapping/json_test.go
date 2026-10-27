package mapping

import (
	"errors"
	"strings"
	"testing"

	"github.com/sserkanml/backflow/api/v1alpha1"
)

const jsonConfigMap = `{
  "apiVersion": "v1",
  "kind": "ConfigMap",
  "metadata": {
    "name": "demo-config",
    "namespace": "demo"
  },
  "data": {
    "LOG_LEVEL": "info",
    "REGION": "eu"
  }
}
`

const jsonList = `{
  "apiVersion": "v1",
  "kind": "List",
  "items": [
    {
      "apiVersion": "v1",
      "kind": "ConfigMap",
      "metadata": {"name": "first", "namespace": "demo"},
      "data": {"k": "1"}
    },
    {
      "apiVersion": "v1",
      "kind": "ConfigMap",
      "metadata": {"name": "demo-config", "namespace": "demo"},
      "data": {
        "LOG_LEVEL": "info"
      }
    }
  ]
}
`

var demoConfig = ResourceID{Kind: "ConfigMap", Namespace: "demo", Name: "demo-config"}

func TestLocateJSONAsArgoCDReadsIt(t *testing.T) {
	loc := func(repo map[string]string) (*Location, error) {
		return Locate(files(repo), "a", DirectoryOptions{}, "demo", demoConfig)
	}
	t.Run("a single object", func(t *testing.T) {
		l, err := loc(map[string]string{"a/cm.json": jsonConfigMap})
		if err != nil || l.File != "a/cm.json" || l.Document != 0 {
			t.Errorf("%+v, %v", l, err)
		}
	})
	t.Run("the items of a list are the resources", func(t *testing.T) {
		l, err := loc(map[string]string{"a/list.json": jsonList})
		if err != nil || l.Document != 1 {
			t.Errorf("%+v, %v", l, err)
		}
	})
	t.Run("a list is a list whatever its kind, when it has an items array", func(t *testing.T) {
		l, err := loc(map[string]string{"a/list.json": strings.Replace(jsonList, `"kind": "List"`, `"kind": "ConfigMapList"`, 1)})
		if err != nil || l.Document != 1 {
			t.Errorf("%+v, %v", l, err)
		}
	})
	t.Run("a null list is empty", func(t *testing.T) {
		_, err := loc(map[string]string{"a/list.json": `{"apiVersion":"v1","kind":"ConfigMapList","items":null}`})
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("Argo CD rejects an array and two objects in a .json file, so they define nothing", func(t *testing.T) {
		for name, content := range map[string]string{
			"array": "[" + jsonConfigMap + "]", "two objects": jsonConfigMap + jsonConfigMap,
		} {
			_, err := loc(map[string]string{"a/cm.json": content})
			if !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "could not parse: a/cm.json") {
				t.Errorf("%s: err = %v", name, err)
			}
		}
	})
	t.Run("a .yaml file with JSON in it is read as YAML documents", func(t *testing.T) {
		l, err := loc(map[string]string{"a/cm.yaml": "---\n" + strings.TrimSpace(jsonConfigMap) + "\n"})
		if err != nil || l.File != "a/cm.yaml" {
			t.Errorf("%+v, %v", l, err)
		}
	})
	t.Run("the same resource in a YAML and a JSON file is ambiguous", func(t *testing.T) {
		yaml := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: demo-config\n  namespace: demo\n"
		_, err := loc(map[string]string{"a/cm.json": jsonConfigMap, "a/cm.yaml": yaml})
		if !errors.Is(err, ErrAmbiguous) {
			t.Errorf("err = %v", err)
		}
	})
}

func TestMapEditsJSONInPlace(t *testing.T) {
	logLevel := []v1alpha1.FieldChange{change("/data/LOG_LEVEL", v1alpha1.OpReplace, `"info"`, `"debug"`)}
	t.Run("a single object: the diff changes only the value line", func(t *testing.T) {
		res, err := Map(files(map[string]string{"a/cm.json": jsonConfigMap}), "a", DirectoryOptions{}, "demo", demoConfig, logLevel)
		if err != nil {
			t.Fatal(err)
		}
		if want := strings.Replace(jsonConfigMap, `"LOG_LEVEL": "info"`, `"LOG_LEVEL": "debug"`, 1); string(res.Edited) != want {
			t.Errorf("edited:\n%s", res.Edited)
		}
		var changed []string
		for l := range strings.SplitSeq(res.Diff, "\n") {
			if (strings.HasPrefix(l, "-") || strings.HasPrefix(l, "+")) && !strings.HasPrefix(l, "---") && !strings.HasPrefix(l, "+++") {
				changed = append(changed, l)
			}
		}
		if strings.Join(changed, "|") != `-    "LOG_LEVEL": "info",|+    "LOG_LEVEL": "debug",` {
			t.Errorf("diff changes %q:\n%s", changed, res.Diff)
		}
		if len(res.Edits) != 1 || res.Edits[0].Location != "/data/LOG_LEVEL" || res.Edits[0].File != "a/cm.json" {
			t.Errorf("edits = %+v", res.Edits)
		}
	})
	t.Run("an item of a list: the pointer is below the item, the other items are untouched", func(t *testing.T) {
		res, err := Map(files(map[string]string{"a/list.json": jsonList}), "a", DirectoryOptions{}, "demo", demoConfig, logLevel)
		if err != nil {
			t.Fatal(err)
		}
		if want := strings.Replace(jsonList, `"LOG_LEVEL": "info"`, `"LOG_LEVEL": "debug"`, 1); string(res.Edited) != want {
			t.Errorf("edited:\n%s", res.Edited)
		}
		if res.Document != 1 {
			t.Errorf("document = %d", res.Document)
		}
	})
	t.Run("an added and a removed field", func(t *testing.T) {
		res, err := Map(files(map[string]string{"a/cm.json": jsonConfigMap}), "a", DirectoryOptions{}, "demo", demoConfig, []v1alpha1.FieldChange{
			change("/data/NEW", v1alpha1.OpAdd, ``, `"x"`),
			change("/data/REGION", v1alpha1.OpRemove, `"eu"`, ``),
		})
		if err != nil {
			t.Fatal(err)
		}
		want := strings.Replace(jsonConfigMap, "    \"LOG_LEVEL\": \"info\",\n    \"REGION\": \"eu\"\n", "    \"LOG_LEVEL\": \"info\",\n    \"NEW\": \"x\"\n", 1)
		if string(res.Edited) != want {
			t.Errorf("edited:\n%s\nwant:\n%s", res.Edited, want)
		}
	})
	t.Run("a defaulted field the file does not spell out is added", func(t *testing.T) {
		dep := `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"demo","namespace":"demo"},"spec":{"replicas":1}}`
		res, err := Map(files(map[string]string{"a/d.json": dep}), "a", DirectoryOptions{}, "demo", ResourceID{Group: "apps", Kind: "Deployment", Namespace: "demo", Name: "demo"},
			[]v1alpha1.FieldChange{change("/spec/revisionHistoryLimit", v1alpha1.OpReplace, `10`, `3`)})
		if err != nil || !strings.Contains(string(res.Edited), `"spec":{"replicas":1,"revisionHistoryLimit":3}`) {
			t.Errorf("%s, %v", res.Edited, err)
		}
	})
}

func TestVerifyJSONRefusesWhatIsNotTheChange(t *testing.T) {
	logLevel := []v1alpha1.FieldChange{change("/data/LOG_LEVEL", v1alpha1.OpReplace, `"info"`, `"debug"`)}
	edited := strings.Replace(jsonList, `"LOG_LEVEL": "info"`, `"LOG_LEVEL": "debug"`, 1)
	if err := VerifyFile("l.json", []byte(jsonList), []byte(edited), 1, logLevel); err != nil {
		t.Fatalf("the right edit: %v", err)
	}
	other := strings.Replace(edited, `"k": "1"`, `"k": "2"`, 1)
	if err := VerifyFile("l.json", []byte(jsonList), []byte(other), 1, logLevel); !errors.Is(err, ErrVerificationFailed) {
		t.Errorf("another item changed: %v", err)
	}
	more := strings.Replace(edited, `"LOG_LEVEL": "debug"`, `"LOG_LEVEL": "debug", "X": "y"`, 1)
	if err := VerifyFile("l.json", []byte(jsonList), []byte(more), 1, logLevel); !errors.Is(err, ErrVerificationFailed) {
		t.Errorf("another field changed: %v", err)
	}
	if err := VerifyFile("l.json", []byte(jsonList), []byte(jsonList), 1, logLevel); !errors.Is(err, ErrVerificationFailed) {
		t.Errorf("nothing changed: %v", err)
	}
}
