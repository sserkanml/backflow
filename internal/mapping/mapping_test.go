package mapping

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/sserkanml/backflow/api/v1alpha1"
	"github.com/sserkanml/backflow/internal/drift"
)

// The demo repository, as in apps/demo.
const demoConfigMap = `apiVersion: v1
kind: ConfigMap
metadata:
  name: demo-config
  namespace: demo
data:
  # Log level of the demo app.
  LOG_LEVEL: info
  GREETING: "hello"
`

const demoDeployment = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: demo
  namespace: demo
spec:
  # Keep it small.
  replicas: 1
  selector:
    matchLabels:
      app: demo
  template:
    metadata:
      labels:
        app: demo
    spec:
      containers:
        - name: demo
          image: nginx:1.27
`

func demoRepo() map[string]string {
	return map[string]string{
		"apps/demo/configmap.yaml":  demoConfigMap,
		"apps/demo/deployment.yaml": demoDeployment,
		"README.md":                 "not a manifest",
	}
}

func TestMapLogLevel(t *testing.T) {
	changes := []v1alpha1.FieldChange{change("/data/LOG_LEVEL", v1alpha1.OpReplace, `"info"`, `"debug"`)}
	res, err := Map(files(demoRepo()), "apps/demo", DirectoryOptions{}, "demo",
		ResourceID{Kind: "ConfigMap", Namespace: "demo", Name: "demo-config"}, changes)
	if err != nil {
		t.Fatal(err)
	}
	if res.File != "apps/demo/configmap.yaml" || res.Document != 0 {
		t.Errorf("location = %s#%d", res.File, res.Document)
	}
	if len(res.Edits) != 1 || res.Edits[0] != (Edit{File: "apps/demo/configmap.yaml", Location: "/data/LOG_LEVEL", Value: `"debug"`, ChangeIndex: 0}) {
		t.Errorf("edits = %+v", res.Edits)
	}
	if string(res.Edited) != strings.Replace(demoConfigMap, "LOG_LEVEL: info", "LOG_LEVEL: debug", 1) {
		t.Errorf("edited =\n%s", res.Edited)
	}
	want := "--- a/apps/demo/configmap.yaml\n+++ b/apps/demo/configmap.yaml\n" +
		"@@ -5,5 +5,5 @@\n" +
		"   namespace: demo\n" +
		" data:\n" +
		"   # Log level of the demo app.\n" +
		"-  LOG_LEVEL: info\n" +
		"+  LOG_LEVEL: debug\n" +
		"   GREETING: \"hello\"\n"
	if !strings.Contains(res.Diff, "-  LOG_LEVEL: info\n+  LOG_LEVEL: debug\n") {
		t.Errorf("diff =\n%s", res.Diff)
	}
	for _, l := range strings.Split(res.Diff, "\n") {
		if (strings.HasPrefix(l, "+") || strings.HasPrefix(l, "-")) && strings.Contains(l, "# Log level") {
			t.Errorf("the comment line is part of the change: %q", l)
		}
	}
	if res.Diff != want {
		t.Errorf("diff =\n%s\nwant\n%s", res.Diff, want)
	}
}

func TestMapReplicas(t *testing.T) {
	changes := []v1alpha1.FieldChange{change("/spec/replicas", v1alpha1.OpReplace, `1`, `3`)}
	res, err := Map(files(demoRepo()), "apps/demo", DirectoryOptions{}, "demo",
		ResourceID{Group: "apps", Kind: "Deployment", Namespace: "demo", Name: "demo"}, changes)
	if err != nil {
		t.Fatal(err)
	}
	if res.File != "apps/demo/deployment.yaml" || res.Edits[0].Location != "/spec/replicas" || res.Edits[0].Value != "3" {
		t.Errorf("result = %+v", res)
	}
	if !strings.Contains(res.Diff, "-  replicas: 1\n+  replicas: 3\n") || strings.Contains(res.Diff, "Keep it small.\n+") {
		t.Errorf("diff =\n%s", res.Diff)
	}
}

func TestMapFailures(t *testing.T) {
	cm := ResourceID{Kind: "ConfigMap", Namespace: "demo", Name: "demo-config"}
	logLevel := []v1alpha1.FieldChange{change("/data/LOG_LEVEL", v1alpha1.OpReplace, `"info"`, `"debug"`)}

	t.Run("not found", func(t *testing.T) {
		_, err := Map(files(demoRepo()), "apps/other", DirectoryOptions{}, "demo", cm, logLevel)
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("ambiguous", func(t *testing.T) {
		repo := demoRepo()
		repo["apps/demo/copy.yaml"] = demoConfigMap
		_, err := Map(files(repo), "apps/demo", DirectoryOptions{}, "demo", cm, logLevel)
		if !errors.Is(err, ErrAmbiguous) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("a replace below a scalar cannot be applied", func(t *testing.T) {
		_, err := Map(files(demoRepo()), "apps/demo", DirectoryOptions{}, "demo", cm,
			[]v1alpha1.FieldChange{change("/data/LOG_LEVEL/x", v1alpha1.OpReplace, `"a"`, `"b"`)})
		if !errors.Is(err, ErrCannotApply) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("a JSON manifest is located but not edited", func(t *testing.T) {
		repo := map[string]string{"apps/demo/cm.json": `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"demo-config","namespace":"demo"},"data":{"LOG_LEVEL":"info"}}`}
		_, err := Map(files(repo), "apps/demo", DirectoryOptions{}, "demo", cm, logLevel)
		if !errors.Is(err, ErrUnsupportedFileFormat) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("an edit that changes nothing fails verification", func(t *testing.T) {
		// The file already holds the live value, so the edit is a no-op.
		_, err := Map(files(demoRepo()), "apps/demo", DirectoryOptions{}, "demo", cm,
			[]v1alpha1.FieldChange{change("/data/GREETING", v1alpha1.OpReplace, `"hi"`, `"hello"`)})
		if !errors.Is(err, ErrVerificationFailed) {
			t.Errorf("err = %v", err)
		}
	})
}

// The proposals Backflow really creates come from drift.Diff. For many shapes
// of live change, the mapped edit must verify and reproduce the live state.
func TestMapReproducesLiveState(t *testing.T) {
	tests := []struct {
		name   string
		file   string
		res    ResourceID
		mutate func(m map[string]interface{})
	}{
		{"changed config value", "apps/demo/configmap.yaml", ResourceID{Kind: "ConfigMap", Namespace: "demo", Name: "demo-config"},
			func(m map[string]interface{}) { m["data"].(map[string]interface{})["LOG_LEVEL"] = "trace" }},
		{"new config key", "apps/demo/configmap.yaml", ResourceID{Kind: "ConfigMap", Namespace: "demo", Name: "demo-config"},
			func(m map[string]interface{}) { m["data"].(map[string]interface{})["EXTRA"] = "1" }},
		{"removed config key", "apps/demo/configmap.yaml", ResourceID{Kind: "ConfigMap", Namespace: "demo", Name: "demo-config"},
			func(m map[string]interface{}) { delete(m["data"].(map[string]interface{}), "GREETING") }},
		{"new label map", "apps/demo/configmap.yaml", ResourceID{Kind: "ConfigMap", Namespace: "demo", Name: "demo-config"},
			func(m map[string]interface{}) {
				m["metadata"].(map[string]interface{})["labels"] = map[string]interface{}{"app.kubernetes.io/name": "demo", "team": "x"}
			}},
		{"replicas", "apps/demo/deployment.yaml", ResourceID{Group: "apps", Kind: "Deployment", Namespace: "demo", Name: "demo"},
			func(m map[string]interface{}) { m["spec"].(map[string]interface{})["replicas"] = float64(3) }},
		{"container image", "apps/demo/deployment.yaml", ResourceID{Group: "apps", Kind: "Deployment", Namespace: "demo", Name: "demo"},
			func(m map[string]interface{}) {
				c := m["spec"].(map[string]interface{})["template"].(map[string]interface{})["spec"].(map[string]interface{})["containers"].([]interface{})
				c[0].(map[string]interface{})["image"] = "nginx:1.28"
			}},
		{"container added (list length change)", "apps/demo/deployment.yaml", ResourceID{Group: "apps", Kind: "Deployment", Namespace: "demo", Name: "demo"},
			func(m map[string]interface{}) {
				sp := m["spec"].(map[string]interface{})["template"].(map[string]interface{})["spec"].(map[string]interface{})
				sp["containers"] = append(sp["containers"].([]interface{}), map[string]interface{}{"name": "side", "image": "busybox"})
			}},
		{"container env added", "apps/demo/deployment.yaml", ResourceID{Group: "apps", Kind: "Deployment", Namespace: "demo", Name: "demo"},
			func(m map[string]interface{}) {
				c := m["spec"].(map[string]interface{})["template"].(map[string]interface{})["spec"].(map[string]interface{})["containers"].([]interface{})
				c[0].(map[string]interface{})["env"] = []interface{}{map[string]interface{}{"name": "A", "value": "1"}}
			}},
		{"selector label changed", "apps/demo/deployment.yaml", ResourceID{Group: "apps", Kind: "Deployment", Namespace: "demo", Name: "demo"},
			func(m map[string]interface{}) {
				m["spec"].(map[string]interface{})["selector"].(map[string]interface{})["matchLabels"].(map[string]interface{})["app"] = "other"
			}},
	}
	repo := demoRepo()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var desired map[string]interface{}
			doc, _ := Locate(files(repo), "apps/demo", DirectoryOptions{}, "demo", tt.res)
			if doc == nil || doc.File != tt.file {
				t.Fatalf("location = %v", doc)
			}
			desired, err := documentMap(repo[tt.file], segment{0, len(repo[tt.file])})
			if err != nil {
				t.Fatal(err)
			}
			live, _ := documentMap(repo[tt.file], segment{0, len(repo[tt.file])})
			tt.mutate(live)
			changes := drift.Diff(desired, live)
			if len(changes) == 0 {
				t.Fatal("the mutation produced no change")
			}

			res, err := Map(files(repo), "apps/demo", DirectoryOptions{}, "demo", tt.res, changes)
			if err != nil {
				t.Fatalf("%v\nchanges: %+v", err, changes)
			}
			// The edited file, parsed, is the live state (nothing else drifted).
			var edited map[string]interface{}
			if err := yaml.Unmarshal(res.Edited, &edited); err != nil {
				t.Fatal(err)
			}
			if rest := drift.Diff(live, mustMap(t, res.Edited)); len(rest) != 0 {
				t.Errorf("edited file still differs from live: %+v\n%s", rest, res.Edited)
			}
			if res.Diff == "" {
				t.Error("empty diff")
			}
		})
	}
}

func mustMap(t *testing.T, data []byte) map[string]interface{} {
	t.Helper()
	m, err := documentMap(string(data), segment{0, len(data)})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// Argo CD's predicted state contains defaulted fields the manifest never
// spelled out. With real drift.Diff output (a Replace) the mapping adds the
// field, and the edit verifies.
func TestMapDefaultedFields(t *testing.T) {
	deploy := ResourceID{Group: "apps", Kind: "Deployment", Namespace: "demo", Name: "demo"}
	spec := func(m map[string]interface{}) map[string]interface{} { return m["spec"].(map[string]interface{}) }
	container := func(m map[string]interface{}) map[string]interface{} {
		return spec(m)["template"].(map[string]interface{})["spec"].(map[string]interface{})["containers"].([]interface{})[0].(map[string]interface{})
	}
	noReplicas := strings.Replace(demoDeployment, "  # Keep it small.\n  replicas: 1\n", "", 1)

	tests := []struct {
		name      string
		manifest  string
		defaults  func(m map[string]interface{}) // what Argo CD's predicted state adds
		mutate    func(m map[string]interface{}) // what changed live
		wantPath  string
		wantEdit  string // text the edited file must contain
		wantAbsOK string // text that must not appear (the edit stays minimal)
	}{
		{
			name: "revisionHistoryLimit", manifest: demoDeployment,
			defaults: func(m map[string]interface{}) { spec(m)["revisionHistoryLimit"] = float64(10) },
			mutate:   func(m map[string]interface{}) { spec(m)["revisionHistoryLimit"] = float64(5) },
			wantPath: "/spec/revisionHistoryLimit", wantEdit: "  revisionHistoryLimit: 5\n",
		},
		{
			name: "replicas missing from the manifest", manifest: noReplicas,
			defaults: func(m map[string]interface{}) { spec(m)["replicas"] = float64(1) },
			mutate:   func(m map[string]interface{}) { spec(m)["replicas"] = float64(3) },
			wantPath: "/spec/replicas", wantEdit: "  replicas: 3\n",
		},
		{
			name: "imagePullPolicy of a container", manifest: demoDeployment,
			defaults: func(m map[string]interface{}) { container(m)["imagePullPolicy"] = "IfNotPresent" },
			mutate:   func(m map[string]interface{}) { container(m)["imagePullPolicy"] = "Always" },
			wantPath: "/spec/template/spec/containers/0/imagePullPolicy", wantEdit: "          imagePullPolicy: Always\n",
		},
		{
			name: "a nested default below a missing parent", manifest: demoDeployment,
			defaults: func(m map[string]interface{}) {
				spec(m)["strategy"] = map[string]interface{}{
					"type":          "RollingUpdate",
					"rollingUpdate": map[string]interface{}{"maxSurge": "25%", "maxUnavailable": "25%"},
				}
			},
			mutate: func(m map[string]interface{}) {
				spec(m)["strategy"].(map[string]interface{})["rollingUpdate"].(map[string]interface{})["maxSurge"] = "50%"
			},
			wantPath: "/spec/strategy/rollingUpdate/maxSurge", wantEdit: "  strategy:\n    rollingUpdate:\n      maxSurge: 50%\n",
		},
		{
			name: "a default and a real change together", manifest: demoDeployment,
			defaults: func(m map[string]interface{}) { spec(m)["revisionHistoryLimit"] = float64(10) },
			mutate: func(m map[string]interface{}) {
				spec(m)["revisionHistoryLimit"] = float64(5)
				spec(m)["replicas"] = float64(3)
			},
			wantPath: "/spec/revisionHistoryLimit", wantEdit: "  replicas: 3\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := map[string]string{"apps/demo/deployment.yaml": tt.manifest}
			whole := segment{0, len(tt.manifest)}
			predicted, err := documentMap(tt.manifest, whole)
			if err != nil {
				t.Fatal(err)
			}
			tt.defaults(predicted)
			live, _ := documentMap(tt.manifest, whole)
			tt.defaults(live)
			tt.mutate(live)

			changes := drift.Diff(predicted, live)
			var replaced bool
			for _, c := range changes {
				if c.Path == tt.wantPath && c.Op == v1alpha1.OpReplace {
					replaced = true
				}
			}
			if !replaced {
				t.Fatalf("drift.Diff has no Replace at %s: %+v", tt.wantPath, changes)
			}

			res, err := Map(files(repo), "apps/demo", DirectoryOptions{}, "demo", deploy, changes)
			if err != nil {
				t.Fatalf("%v\nchanges: %+v", err, changes)
			}
			if !strings.Contains(string(res.Edited), tt.wantEdit) {
				t.Errorf("edited file lacks %q:\n%s", tt.wantEdit, res.Edited)
			}
			// For every path the proposal names, the edited file holds the live value.
			editedMap := mustMap(t, res.Edited)
			for _, c := range changes {
				segs, _ := parsePointer(c.Path)
				var want interface{}
				if err := json.Unmarshal([]byte(c.Live), &want); err != nil {
					t.Fatal(err)
				}
				if got, ok := lookup(editedMap, segs); !ok || !reflect.DeepEqual(got, want) {
					t.Errorf("%s = %v (present: %v), want %v", c.Path, got, ok, want)
				}
			}
			// Every edit records the live value of its change.
			for i, e := range res.Edits {
				if e.Value != changes[i].Live || e.Location != changes[i].Path || e.ChangeIndex != i {
					t.Errorf("edit %d = %+v", i, e)
				}
			}
		})
	}
}
