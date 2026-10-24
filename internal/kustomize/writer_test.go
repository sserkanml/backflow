package kustomize

import (
	"maps"
	"strings"
	"testing"

	"github.com/sserkanml/backflow/api/v1alpha1"
	"github.com/sserkanml/backflow/internal/rendermap"
)

func cand(file string, e *Edit) rendermap.Candidate {
	return rendermap.Candidate{File: file, Path: rendermap.KeyPathFromPointer(e.Holder + e.Pointer), Variant: e.Kind, Meta: e}
}

func TestWriter(t *testing.T) {
	const kfile = "o/kustomization.yaml"
	tests := []struct {
		name    string
		file    string
		content string
		edit    *Edit
		value   string // for a fragment
		want    string
	}{
		{
			name: "image tag keeps comments and quoting", file: kfile,
			content: "resources:\n- ../base\nimages:\n- name: nginx # the web image\n  newTag: \"1.0\"\n",
			edit:    &Edit{Kind: KindSetting, Pointer: "/images/0/newTag", Op: v1alpha1.OpReplace, Live: `"1.1"`},
			want:    "resources:\n- ../base\nimages:\n- name: nginx # the web image\n  newTag: \"1.1\"\n",
		},
		{
			name: "replicas count stays a number", file: kfile,
			content: "replicas:\n- name: web\n  count: 2\n",
			edit:    &Edit{Kind: KindSetting, Pointer: "/replicas/0/count", Op: v1alpha1.OpReplace, Live: `3`},
			want:    "replicas:\n- name: web\n  count: 3\n",
		},
		{
			name: "a fragment of the image string is written as the tag", file: kfile,
			content: "images:\n- name: nginx\n  newTag: 1.0.0\n",
			edit:    &Edit{Kind: KindSetting, Pointer: "/images/0/newTag", Op: v1alpha1.OpReplace},
			value:   "1.27-alpine",
			want:    "images:\n- name: nginx\n  newTag: 1.27-alpine\n",
		},
		{
			name: "a strategic merge patch file", file: "o/patch.yaml",
			content: "# patch\napiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\nspec:\n  template:\n    spec:\n      containers:\n      - name: web\n        env:\n        - name: A\n          value: \"1\" # one\n",
			edit:    &Edit{Kind: KindPatchFile, Pointer: "/spec/template/spec/containers/0/env/0/value", Op: v1alpha1.OpReplace, Live: `"2"`},
			want:    "# patch\napiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\nspec:\n  template:\n    spec:\n      containers:\n      - name: web\n        env:\n        - name: A\n          value: \"2\" # one\n",
		},
		{
			name: "an inline patch keeps its block scalar", file: kfile,
			content: "patches:\n- target:\n    kind: Deployment\n  patch: |-\n    apiVersion: apps/v1\n    kind: Deployment\n    metadata:\n      name: web\n    spec:\n      replicas: 3\n",
			edit:    &Edit{Kind: KindPatchInline, Holder: "/patches/0/patch", Pointer: "/spec/replicas", Op: v1alpha1.OpReplace, Live: `4`},
			want:    "patches:\n- target:\n    kind: Deployment\n  patch: |-\n    apiVersion: apps/v1\n    kind: Deployment\n    metadata:\n      name: web\n    spec:\n      replicas: 4\n",
		},
		{
			name: "a JSON 6902 file (a list) keeps its layout", file: "o/ops.yaml",
			content: "# ops\n- op: replace\n  path: /spec/replicas   # keep\n  value: 3\n- op: add\n  path: /metadata/labels/x\n  value: \"a\"\n",
			edit:    &Edit{Kind: KindJSON6902File, Pointer: "/0/value", Op: v1alpha1.OpReplace, Live: `5`},
			want:    "# ops\n- op: replace\n  path: /spec/replicas   # keep\n  value: 5\n- op: add\n  path: /metadata/labels/x\n  value: \"a\"\n",
		},
		{
			name: "a JSON 6902 file string keeps its quotes", file: "o/ops.yaml",
			content: "- op: add\n  path: /metadata/labels/x\n  value: \"a\"\n- op: replace\n  path: /a\n  value: 'b'\n",
			edit:    &Edit{Kind: KindJSON6902File, Pointer: "/1/value", Op: v1alpha1.OpReplace, Live: `"it's"`},
			want:    "- op: add\n  path: /metadata/labels/x\n  value: \"a\"\n- op: replace\n  path: /a\n  value: 'it''s'\n",
		},
		{
			name: "an inline JSON 6902 patch as YAML in a block scalar", file: kfile,
			content: "patches:\n- target:\n    kind: Deployment\n  patch: |\n    - op: replace\n      path: /spec/replicas\n      value: 3\n",
			edit:    &Edit{Kind: KindJSON6902Inline, Holder: "/patches/0/patch", Pointer: "/0/value", Op: v1alpha1.OpReplace, Live: `6`},
			want:    "patches:\n- target:\n    kind: Deployment\n  patch: |\n    - op: replace\n      path: /spec/replicas\n      value: 6\n",
		},
		{
			name: "a JSON 6902 patch that is a JSON string is written compactly", file: kfile,
			content: "patches:\n- target:\n    kind: Deployment\n  patch: '[{\"op\": \"replace\", \"path\": \"/spec/replicas\", \"value\": 3}]'\n",
			edit:    &Edit{Kind: KindJSON6902Inline, Holder: "/patches/0/patch", Pointer: "/0/value", Op: v1alpha1.OpReplace, Live: `7`},
			want:    "patches:\n- target:\n    kind: Deployment\n  patch: '[{\"op\":\"replace\",\"path\":\"/spec/replicas\",\"value\":7}]'\n",
		},
		{
			name: "a resource file", file: "o/deploy.yaml",
			content: "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\nspec:\n  replicas: 1 # one\n",
			edit:    &Edit{Kind: KindResource, Pointer: "/spec/replicas", Op: v1alpha1.OpReplace, Desired: `1`, Live: `2`},
			want:    "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\nspec:\n  replicas: 2 # one\n",
		},
		{
			name: "a field removed from a resource file", file: "o/deploy.yaml",
			content: "kind: Deployment\nspec:\n  paused: true\n  replicas: 1\n",
			edit:    &Edit{Kind: KindResource, Pointer: "/spec/paused", Op: v1alpha1.OpRemove, Desired: `true`},
			want:    "kind: Deployment\nspec:\n  replicas: 1\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := cand(tc.file, tc.edit)
			c.Fragment = tc.value != ""
			w, err := Writer{}.Write(c, []byte(tc.content), tc.value)
			if err != nil {
				t.Fatal(err)
			}
			if got := string(w.Edited); got != tc.want {
				t.Errorf("got:\n%s\nwant:\n%s", got, tc.want)
			}
			if len(w.Edits) != 1 || w.Edits[0].File != tc.file {
				t.Errorf("edits = %+v", w.Edits)
			}
		})
	}
}

func TestWriterErrors(t *testing.T) {
	if _, err := (Writer{}).Write(rendermap.Candidate{File: "a"}, nil, ""); err == nil {
		t.Error("a candidate without an edit")
	}
	c := cand("o/ops.yaml", &Edit{Kind: KindJSON6902File, Pointer: "/0/value", Op: v1alpha1.OpReplace, Live: `{"a":1}`})
	if _, err := (Writer{}).Write(c, []byte("- op: replace\n  path: /a\n  value: 1\n"), ""); err == nil {
		t.Error("a scalar must not become a collection")
	}
	c = cand("o/ops.yaml", &Edit{Kind: KindJSON6902File, Pointer: "/0/value", Op: v1alpha1.OpReplace, Live: `1`})
	if _, err := (Writer{}).Write(c, []byte("- op: replace\n  path: /a\n  value: |\n    x\n"), ""); err == nil {
		t.Error("a block scalar is not replaced in place")
	}
}

func TestNewPatch(t *testing.T) {
	const kfile = "o/kustomization.yaml"
	patchFile := "o/backflow-patches/deployment-web.yaml"
	edit := func(patch map[string]any) *Edit {
		return &Edit{Kind: KindNewPatch, Pointer: "/spec/replicas", Kustomization: kfile, Live: `3`, Patch: patch}
	}
	base := map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]any{"name": "web"}}
	with := func(extra map[string]any) map[string]any {
		out := map[string]any{}
		maps.Copy(out, base)
		maps.Copy(out, extra)
		return out
	}
	files := map[string][]byte{kfile: []byte("resources:\n- ../base\n")}
	c := rendermap.Candidate{File: patchFile, Path: rendermap.KeyPath{"spec", "replicas"}, Variant: KindNewPatch, NewFile: true,
		Meta: edit(with(map[string]any{"spec": map[string]any{"replicas": 3}}))}
	w, err := Writer{}.WriteWith(c, files, "")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(w.Edited), "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\nspec:\n  replicas: 3\n"; got != want {
		t.Errorf("patch file:\n%s", got)
	}
	if len(w.Also) != 1 || w.Also[0].File != kfile || string(w.Also[0].Edited) != "resources:\n- ../base\npatches:\n  - path: backflow-patches/deployment-web.yaml\n" {
		t.Errorf("kustomization = %+v", w.Also)
	}

	t.Run("a second field extends the file and lists it once", func(t *testing.T) {
		files := map[string][]byte{kfile: w.Also[0].Edited, patchFile: w.Edited}
		c2 := c
		c2.Path = rendermap.KeyPath{"spec", "paused"}
		c2.Meta = edit(with(map[string]any{"spec": map[string]any{"paused": true}}))
		w2, err := Writer{}.WriteWith(c2, files, "")
		if err != nil {
			t.Fatal(err)
		}
		if got := string(w2.Edited); !strings.Contains(got, "paused: true") || !strings.Contains(got, "replicas: 3") {
			t.Errorf("patch file:\n%s", got)
		}
		if string(w2.Also[0].Edited) != string(w.Also[0].Edited) {
			t.Errorf("listed twice:\n%s", w2.Also[0].Edited)
		}
	})
	t.Run("a merge keyed list item is merged by its key", func(t *testing.T) {
		container := func(extra map[string]any) map[string]any {
			item := map[string]any{"name": "web"}
			maps.Copy(item, extra)
			return with(map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"containers": []any{item}}}}})
		}
		existing, _ := marshalPatch(container(map[string]any{"imagePullPolicy": "Always"}))
		c3 := c
		c3.NewFile = false
		c3.Meta = edit(container(map[string]any{"resources": map[string]any{"limits": map[string]any{"cpu": "1"}}}))
		w3, err := Writer{}.WriteWith(c3, map[string][]byte{kfile: files[kfile], patchFile: existing}, "")
		if err != nil {
			t.Fatal(err)
		}
		got := string(w3.Edited)
		if strings.Count(got, "name: web") != 2 || !strings.Contains(got, "imagePullPolicy: Always") || !strings.Contains(got, "cpu: \"1\"") {
			t.Errorf("patch file:\n%s", got)
		}
	})
}

func TestRegisterPatch(t *testing.T) {
	const kfile = "o/kustomization.yaml"
	const pf = "o/backflow-patches/a.yaml"
	for name, tc := range map[string]struct{ in, want string }{
		"after the last item and before the next key": {
			"patches:\n  - path: x.yaml\n  - path: y.yaml\n\n# images\nimages: []\n",
			"patches:\n  - path: x.yaml\n  - path: y.yaml\n  - path: backflow-patches/a.yaml\n\n# images\nimages: []\n"},
		"at the end of the file": {
			"resources:\n- b\npatches:\n- path: x.yaml\n  target:\n    kind: Deployment\n",
			"resources:\n- b\npatches:\n- path: x.yaml\n  target:\n    kind: Deployment\n- path: backflow-patches/a.yaml\n"},
		"an empty flow list": {"patches: []\nresources:\n- b\n", ""},
		"already listed":     {"patches:\n- path: backflow-patches/a.yaml\n", "patches:\n- path: backflow-patches/a.yaml\n"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := registerPatch(kfile, []byte(tc.in), pf)
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if !strings.Contains(string(got), "backflow-patches/a.yaml") || !strings.Contains(string(got), "resources:") {
					t.Errorf("got:\n%s", got)
				}
				return
			}
			if string(got) != tc.want {
				t.Errorf("got:\n%s\nwant:\n%s", got, tc.want)
			}
		})
	}
}
