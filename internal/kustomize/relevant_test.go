package kustomize

import (
	"io/fs"
	"os"
	"strings"
	"testing"
	"testing/fstest"
)

func mapFS(files map[string]string) fstest.MapFS {
	m := fstest.MapFS{}
	for name, content := range files {
		m[name] = &fstest.MapFile{Data: []byte(content)}
	}
	return m
}

func TestRelevantFiles(t *testing.T) {
	tests := []struct {
		name string
		fsys fs.FS
		spec Spec
		// in and out are changed paths that must and must not count.
		in, out []string
		// open are fragments of the reasons the set is not closed; none means closed.
		open []string
	}{
		{
			name: "an overlay and its base outside the Application path",
			fsys: os.DirFS("testdata/overlay-base"),
			spec: Spec{Path: "apps/web/overlays/dev"},
			in: []string{
				"apps/web/overlays/dev/kustomization.yaml", "apps/web/overlays/dev/replicas.yaml",
				"apps/web/base/kustomization.yaml", "apps/web/base/deployment.yaml", "apps/web/base/service.yaml",
				"apps/web/overlays/dev/kustomization.yml", "apps/web/base/Kustomization", // another name of the file
			},
			out: []string{
				"apps/web/overlays/dev/notes.txt", "apps/web/base/README.md", "apps/other/thing.yaml", "docs/a.md",
				"apps/web/overlays/prod/kustomization.yaml",
			},
		},
		{
			name: "components, and an inline patch that is not a file",
			fsys: os.DirFS("testdata/components"),
			spec: Spec{Path: "app/overlay"},
			in: []string{
				"app/overlay/kustomization.yaml", "app/base/kustomization.yaml", "app/base/deploy.yaml",
				"app/comp/logging/kustomization.yaml", "app/comp/logging/sidecar.yaml",
			},
			out: []string{"app/extra/kustomization.yaml"},
		},
		{
			name: "components of the Application and patch files of its options",
			fsys: os.DirFS("testdata/components"),
			spec: Spec{Path: "app/overlay", Components: []string{"../extra"}, PatchFiles: []string{"../base/deploy.yaml"}},
			in:   []string{"app/extra/kustomization.yaml", "app/base/deploy.yaml"},
		},
		{
			name: "a missing component of the Application is ignored when asked, and may appear",
			fsys: os.DirFS("testdata/components"),
			spec: Spec{Path: "app/overlay", Components: []string{"../later"}, IgnoreMissingComponents: true},
			in:   []string{"app/later"},
		},
		{
			name: "a missing component of the Application is not closed otherwise",
			fsys: os.DirFS("testdata/components"),
			spec: Spec{Path: "app/overlay", Components: []string{"../later"}},
			open: []string{"../later, which does not exist"},
		},
		{
			name: "generator files and envs, replacements, openapi, crds and configurations",
			fsys: os.DirFS("testdata/generators"),
			spec: Spec{Path: "app"},
			in: []string{
				"app/kustomization.yaml", "app/cm.yaml", "app/app.properties", "app/config/settings.json", "app/app.env",
				"app/secret.env", "app/repl.yaml", "app/schema.json", "app/crd.yaml", "app/nameref.yaml",
			},
			out: []string{"app/config/other.json", "app/settings.json", "renamed"},
		},
		{
			name: "a remote base is not closed, the local one is still followed",
			fsys: os.DirFS("testdata/remote"),
			spec: Spec{Path: "app"},
			in:   []string{"app/kustomization.yaml", "local/a.yaml", "local/kustomization.yaml"},
			open: []string{"github.com/org/repo//base?ref=v1, which is remote", "https://example.com/x.yaml, which is remote"},
		},
		{
			name: "helmCharts are not supported",
			fsys: mapFS(map[string]string{"a/kustomization.yaml": "resources: [x.yaml]\nhelmCharts:\n  - name: redis\n", "a/x.yaml": ""}),
			spec: Spec{Path: "a"},
			in:   []string{"a/x.yaml"},
			open: []string{"helmCharts, which are not supported"},
		},
		{
			name: "a reference that leaves the repository",
			fsys: mapFS(map[string]string{"a/kustomization.yaml": "resources: [../../x]\n"}),
			spec: Spec{Path: "a"},
			open: []string{"../../x, which is outside the repository"},
		},
		{
			name: "a reference that does not exist, and a directory without a kustomization",
			fsys: mapFS(map[string]string{"a/kustomization.yaml": "resources: [gone.yaml, ../empty]\n", "empty/f.yaml": ""}),
			spec: Spec{Path: "a"},
			open: []string{"gone.yaml, which does not exist", "empty, which has no kustomization file"},
		},
		{
			name: "a kustomization that does not parse",
			fsys: mapFS(map[string]string{"a/kustomization.yaml": "resources: [unterminated\n"}),
			spec: Spec{Path: "a"},
			open: []string{"a/kustomization.yaml does not parse"},
		},
		{
			name: "an Application path without a kustomization",
			fsys: mapFS(map[string]string{"a/deploy.yaml": ""}),
			spec: Spec{Path: "a"},
			in:   []string{"a/kustomization.yaml", "a/Kustomization"}, // one of them may be added
			open: []string{"has no kustomization file"},
		},
		{
			name: "a symbolic link",
			fsys: fstest.MapFS{
				"a/kustomization.yaml": &fstest.MapFile{Data: []byte("resources: [link.yaml]\n")},
				"a/link.yaml":          &fstest.MapFile{Data: []byte("../x.yaml"), Mode: fs.ModeSymlink},
			},
			spec: Spec{Path: "a"},
			open: []string{"link.yaml, a symbolic link"},
		},
		{
			name: "a reference cycle ends",
			fsys: mapFS(map[string]string{"a/kustomization.yaml": "resources: [../b]\n", "b/kustomization.yaml": "resources: [../a]\n"}),
			spec: Spec{Path: "a"},
			in:   []string{"a/kustomization.yaml", "b/kustomization.yaml"},
		},
		{
			name: "the repository root as the Application path",
			fsys: mapFS(map[string]string{"kustomization.yaml": "resources: [deploy.yaml, sub]\n", "deploy.yaml": "", "sub/kustomization.yaml": ""}),
			spec: Spec{Path: "."},
			in:   []string{"kustomization.yaml", "deploy.yaml", "sub/kustomization.yaml"},
			out:  []string{"other.yaml"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rel, err := RelevantFiles(tt.fsys, tt.spec)
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range tt.in {
				if !rel.Matches(p) {
					t.Errorf("%s should count", p)
				}
			}
			for _, p := range tt.out {
				if rel.Matches(p) {
					t.Errorf("%s should not count", p)
				}
			}
			if rel.Closed() != (len(tt.open) == 0) {
				t.Errorf("closed = %v, reasons = %q", rel.Closed(), rel.Reasons())
			}
			all := strings.Join(rel.Reasons(), "\n")
			for _, frag := range tt.open {
				if !strings.Contains(all, frag) {
					t.Errorf("reasons %q lack %q", rel.Reasons(), frag)
				}
			}
		})
	}
}
