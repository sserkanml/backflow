package kustomize_test

import (
	"context"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/sserkanml/backflow/internal/kustomize"
	"github.com/sserkanml/backflow/internal/kustomize/kbuild"
	"github.com/sserkanml/backflow/internal/rendermap"
)

// jsonFixture has its resources, a strategic merge patch and a JSON 6902 patch in
// .json files.
func jsonFixture() fstest.MapFS {
	f := func(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }
	return fstest.MapFS{
		"k/base/kustomization.yaml": f("resources:\n- deployment.json\n"),
		"k/base/deployment.json": f(`{
  "apiVersion": "apps/v1",
  "kind": "Deployment",
  "metadata": {
    "name": "web"
  },
  "spec": {
    "replicas": 1,
    "selector": {
      "matchLabels": {
        "app": "web"
      }
    },
    "template": {
      "metadata": {
        "labels": {
          "app": "web"
        }
      },
      "spec": {
        "containers": [
          {
            "name": "web",
            "image": "nginx:1.0",
            "env": [
              {
                "name": "A",
                "value": "base-a"
              },
              {
                "name": "B",
                "value": "base-b"
              }
            ]
          }
        ]
      }
    }
  }
}
`),
		"k/overlays/dev/kustomization.yaml": f(`resources:
- ../../base
- extra.json
patches:
- path: env.json
- path: ops.json
  target:
    kind: Deployment
    name: web
`),
		"k/overlays/dev/env.json": f(`{
	"apiVersion": "apps/v1",
	"kind": "Deployment",
	"metadata": {
		"name": "web"
	},
	"spec": {
		"template": {
			"spec": {
				"containers": [
					{
						"name": "web",
						"env": [
							{
								"name": "A",
								"value": "dev-a"
							}
						]
					}
				]
			}
		}
	}
}
`),
		"k/overlays/dev/ops.json": f("[\n  {\n    \"op\": \"replace\",\n    \"path\": \"/spec/replicas\",\n    \"value\": 3\n  }\n]\n"),
		"k/overlays/dev/extra.json": f(`{
  "apiVersion": "v1",
  "kind": "ConfigMap",
  "metadata": {
    "name": "extra"
  },
  "data": {
    "level": "one",
    "other": "x"
  }
}
`),
	}
}

func setupJSON(t *testing.T) *env {
	t.Helper()
	bin := kustomizeBin(t)
	fsys := jsonFixture()
	src, err := kustomize.NewSource(fsys, "k/overlays/dev", kustomize.Options{})
	if err != nil {
		t.Fatal(err)
	}
	files, err := kustomize.CollectFiles(fsys, src)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, fsys: fsys, src: src, files: files, app: rendermap.App{Name: "demo", DestNamespace: "demo"},
		rend: &kbuild.Renderer{Bin: bin, Dir: "k/overlays/dev"}}
	if e.loc, err = kustomize.Locate(context.Background(), e.rend, src, files, e.app); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestJSONFilesAreEditedAsJSON(t *testing.T) {
	e := setupJSON(t)
	dep := e.obj("Deployment", "web")
	t.Run("a value in a strategic merge patch file", func(t *testing.T) {
		out := e.verify(dep, func(o map[string]any) { container(o)["env"].([]any)[0].(map[string]any)["value"] = "live-a" }, defaults)
		wantMapped(t, out, "k/overlays/dev/env.json")
		want := strings.Replace(string(e.files["k/overlays/dev/env.json"]), `"value": "dev-a"`, `"value": "live-a"`, 1)
		if got := string(out.Results[0].Edited); got != want {
			t.Errorf("edited:\n%s", got)
		}
	})
	t.Run("a value of a JSON 6902 patch file", func(t *testing.T) {
		out := e.verify(dep, func(o map[string]any) { o["spec"].(map[string]any)["replicas"] = int64(5) }, defaults)
		wantMapped(t, out, "k/overlays/dev/ops.json")
		want := strings.Replace(string(e.files["k/overlays/dev/ops.json"]), `"value": 3`, `"value": 5`, 1)
		if got := string(out.Results[0].Edited); got != want {
			t.Errorf("edited:\n%s", got)
		}
	})
	t.Run("a resource file in the path", func(t *testing.T) {
		cm := e.obj("ConfigMap", "extra")
		out := e.verify(cm, func(o map[string]any) { o["data"].(map[string]any)["level"] = "two" }, defaults)
		wantMapped(t, out, "k/overlays/dev/extra.json")
		want := strings.Replace(string(e.files["k/overlays/dev/extra.json"]), `"level": "one"`, `"level": "two"`, 1)
		if got := string(out.Results[0].Edited); got != want {
			t.Errorf("edited:\n%s", got)
		}
		if strings.Contains(diffs(out), "\n-    \"other\"") {
			t.Errorf("diff touches another line:\n%s", diffs(out))
		}
	})
	t.Run("a resource file of a base, with editBases", func(t *testing.T) {
		out := e.verify(dep, func(o map[string]any) { container(o)["env"].([]any)[1].(map[string]any)["value"] = "live-b" },
			kustomize.Policy{EditBases: true})
		wantMapped(t, out, "k/base/deployment.json")
		want := strings.Replace(string(e.files["k/base/deployment.json"]), `"value": "base-b"`, `"value": "live-b"`, 1)
		if got := string(out.Results[0].Edited); got != want {
			t.Errorf("edited:\n%s", got)
		}
	})
	t.Run("a removed list item in a base resource file", func(t *testing.T) {
		out := e.verify(dep, func(o map[string]any) {
			c := container(o)
			c["env"] = c["env"].([]any)[:1]
		}, kustomize.Policy{EditBases: true})
		wantMapped(t, out, "k/base/deployment.json")
		if strings.Contains(string(out.Results[0].Edited), "base-b") || strings.Contains(string(out.Results[0].Edited), "},\n              {\n                \"name\": \"B\"") {
			t.Errorf("edited:\n%s", out.Results[0].Edited)
		}
	})
}
