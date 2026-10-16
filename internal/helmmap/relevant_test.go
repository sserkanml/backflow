package helmmap

import (
	"errors"
	"io/fs"
	"testing"
	"testing/fstest"
)

func tree(files map[string]string) fstest.MapFS {
	m := fstest.MapFS{}
	for n, c := range files {
		m[n] = &fstest.MapFile{Data: []byte(c)}
	}
	return m
}

const chartYAML = "apiVersion: v2\nname: x\nversion: 0.1.0\n"

func TestRelevantFiles(t *testing.T) {
	sharedRoot := map[string]string{
		"Chart.yaml": chartYAML, "values.yaml": "a: 1\n", "templates/cm.yaml": "kind: ConfigMap\n",
		"services/alpha/dev.yaml": "a: 2\n", "services/beta/dev.yaml": "a: 3\n", "README.md": "docs\n",
	}
	perEnv := map[string]string{
		"helm/demo/Chart.yaml": chartYAML, "helm/demo/values.yaml": "a: 1\n", "helm/demo/templates/cm.yaml": "x\n",
		"helm/demo/values-dev.yaml": "a: 2\n", "helm/demo/values-prod.yaml": "a: 3\n", "helm/demo/README.md": "docs\n",
		"other/file.txt": "x\n",
	}
	withFiles := func(helmignore string) map[string]string {
		m := map[string]string{
			"c/Chart.yaml": chartYAML, "c/values.yaml": "a: 1\n",
			"c/templates/cm.yaml": "data: {{ .Files.Get \"conf/app.ini\" }}\n",
			"c/conf/app.ini":      "x\n", "c/README.md": "docs\n", "c/docs/guide.md": "docs\n", "elsewhere/x": "x\n",
		}
		if helmignore != "" {
			m["c/.helmignore"] = helmignore
		}
		return m
	}
	withDep := map[string]string{
		"app/Chart.yaml":  chartYAML + "dependencies:\n  - name: lib\n    version: 1.0.0\n    repository: file://../lib\n",
		"app/values.yaml": "a: 1\n", "app/templates/x.yaml": "x\n", "app/README.md": "docs\n",
		"lib/Chart.yaml":  chartYAML + "dependencies:\n  - name: deep\n    version: 1.0.0\n    repository: file://../deep\n",
		"lib/values.yaml": "b: 1\n", "lib/templates/y.yaml": "y\n", "lib/README.md": "docs\n",
		"deep/Chart.yaml": chartYAML, "deep/templates/z.yaml": "z\n", "deep/NOTES": "n\n", "unrelated/Chart.yaml": chartYAML,
	}
	tests := []struct {
		name  string
		files map[string]string
		spec  Spec
		yes   []string
		no    []string
	}{
		{
			name: "shared chart at the root, alpha", files: sharedRoot,
			spec: Spec{Path: ".", ValueFiles: []string{"services/alpha/dev.yaml"}},
			yes:  []string{"services/alpha/dev.yaml", "templates/cm.yaml", "templates/sub/new.yaml", "Chart.yaml", "values.yaml", "Chart.lock", "values.schema.json", "charts/dep-1.0.0.tgz", "crds/x.yaml", ".helmignore", "templates"},
			no:   []string{"services/beta/dev.yaml", "services/alpha/other.yaml", "README.md", "docs/x.md"},
		},
		{
			name: "shared chart at the root, beta", files: sharedRoot,
			spec: Spec{Path: ".", ValueFiles: []string{"/services/beta/dev.yaml"}},
			yes:  []string{"services/beta/dev.yaml", "templates/cm.yaml"},
			no:   []string{"services/alpha/dev.yaml", "README.md"},
		},
		{
			name: "per-environment values files", files: perEnv,
			spec: Spec{Path: "helm/demo", ValueFiles: []string{"values-dev.yaml"}},
			yes:  []string{"helm/demo/values-dev.yaml", "helm/demo/values.yaml", "helm/demo/templates/cm.yaml", "helm/demo/Chart.yaml"},
			no:   []string{"helm/demo/values-prod.yaml", "helm/demo/README.md", "other/file.txt", "values-dev.yaml"},
		},
		{
			name: "a chart using .Files counts its other files", files: withFiles(""),
			spec: Spec{Path: "c"},
			yes:  []string{"c/README.md", "c/conf/app.ini", "c/docs/guide.md", "c/templates/cm.yaml"},
			no:   []string{"elsewhere/x", "c2/README.md"},
		},
		{
			name: ".helmignore is honoured: a file", files: withFiles("README.md\n"),
			spec: Spec{Path: "c"},
			yes:  []string{"c/conf/app.ini", "c/docs/guide.md"},
			no:   []string{"c/README.md"},
		},
		{
			name: ".helmignore is honoured: a directory, a glob, a comment", files: withFiles("# docs\ndocs/\n*.md\n"),
			spec: Spec{Path: "c"},
			yes:  []string{"c/conf/app.ini"},
			no:   []string{"c/docs/guide.md", "c/README.md", "c/docs/deeper/x.txt"},
		},
		{
			name: ".helmignore with a negation ignores nothing", files: withFiles("*.md\n!keep.md\n"),
			spec: Spec{Path: "c"},
			yes:  []string{"c/README.md", "c/docs/guide.md"},
		},
		{
			name: ".helmignore with ** ignores nothing", files: withFiles("**/*.md\n"),
			spec: Spec{Path: "c"},
			yes:  []string{"c/README.md"},
		},
		{
			name: "file:// dependencies, recursively", files: withDep,
			spec: Spec{Path: "app"},
			yes:  []string{"lib/templates/y.yaml", "lib/Chart.yaml", "lib/values.yaml", "deep/templates/z.yaml", "deep/Chart.yaml", "app/templates/x.yaml"},
			no:   []string{"lib/README.md", "deep/NOTES", "app/README.md", "unrelated/Chart.yaml"},
		},
		{
			name:  "file parameters count wherever they are",
			files: map[string]string{"c/Chart.yaml": chartYAML, "secrets/tls.pem": "x\n", "secrets/other.pem": "x\n"},
			spec:  Spec{Path: "c", FileParameters: []FileParameter{{Name: "tls", Path: "/secrets/tls.pem"}}},
			yes:   []string{"secrets/tls.pem"},
			no:    []string{"secrets/other.pem"},
		},
		{
			name: "a glob of value files", files: map[string]string{"c/Chart.yaml": chartYAML, "c/env/a.yaml": "a: 1\n", "c/env/b.yaml": "a: 1\n", "c/env/c.txt": "x\n"},
			spec: Spec{Path: "c", ValueFiles: []string{"env/*.yaml"}},
			yes:  []string{"c/env/a.yaml", "c/env/b.yaml"},
			no:   []string{"c/env/c.txt", "c/env/d.yaml"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rel, err := RelevantFiles(tree(tt.files), tt.spec)
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range tt.yes {
				if !rel.Matches(p) {
					t.Errorf("%s does not count, but can change the render", p)
				}
			}
			for _, p := range tt.no {
				if rel.Matches(p) {
					t.Errorf("%s counts, but cannot change the render", p)
				}
			}
		})
	}
}

func TestRelevantFilesMergeCoversBothRevisions(t *testing.T) {
	before := tree(map[string]string{"c/Chart.yaml": chartYAML, "c/env/a.yaml": "a: 1\n"})
	after := tree(map[string]string{"c/Chart.yaml": chartYAML, "c/env/a.yaml": "a: 1\n", "c/env/b.yaml": "a: 2\n"})
	spec := Spec{Path: "c", ValueFiles: []string{"env/*.yaml"}}
	a, err := RelevantFiles(before, spec)
	if err != nil {
		t.Fatal(err)
	}
	b, err := RelevantFiles(after, spec)
	if err != nil {
		t.Fatal(err)
	}
	if a.Matches("c/env/b.yaml") {
		t.Fatal("the new file counts at the old revision")
	}
	a.Merge(b)
	if !a.Matches("c/env/b.yaml") || !a.Matches("c/env/a.yaml") {
		t.Error("the union misses a file")
	}
}

func TestRelevantFilesVariablesPerRevision(t *testing.T) {
	files := tree(map[string]string{"c/Chart.yaml": chartYAML, "c/v-abc.yaml": "a: 1\n", "c/v-def.yaml": "a: 1\n"})
	for rev, want := range map[string]string{"abc": "c/v-abc.yaml", "def": "c/v-def.yaml"} {
		rel, err := RelevantFiles(files, Spec{Path: "c", ValueFiles: []string{"v-$ARGOCD_APP_REVISION.yaml"}, Env: map[string]string{"ARGOCD_APP_REVISION": rev}})
		if err != nil {
			t.Fatal(err)
		}
		if !rel.Matches(want) {
			t.Errorf("revision %s: %s does not count", rev, want)
		}
	}
}

func TestRelevantFilesErrors(t *testing.T) {
	link := func() *fstest.MapFile { return &fstest.MapFile{Data: []byte("x"), Mode: fs.ModeSymlink} }
	base := func() fstest.MapFS {
		return tree(map[string]string{"c/Chart.yaml": chartYAML, "c/values-dev.yaml": "a: 1\n", "c/templates/x.yaml": "x\n"})
	}
	tests := []struct {
		name string
		fsys func() fstest.MapFS
		spec Spec
		want error
	}{
		{"a symlink among the templates", func() fstest.MapFS { m := base(); m["c/templates/l.yaml"] = link(); return m }, Spec{Path: "c"}, ErrSymlinkInChart},
		{"a symlinked Chart.yaml is no chart file", func() fstest.MapFS { m := base(); m["c/Chart.yaml"] = link(); return m }, Spec{Path: "c"}, ErrSymlinkInChart},
		{"a symlinked values.yaml", func() fstest.MapFS { m := base(); m["c/values.yaml"] = link(); return m }, Spec{Path: "c"}, ErrSymlinkInChart},
		{"a symlinked value file", func() fstest.MapFS { m := base(); m["c/values-dev.yaml"] = link(); return m }, Spec{Path: "c", ValueFiles: []string{"values-dev.yaml"}}, ErrSymlinkedValueFile},
		{"a symlink in a chart that uses .Files", func() fstest.MapFS {
			m := base()
			m["c/templates/x.yaml"] = &fstest.MapFile{Data: []byte("{{ .Files.Get \"a\" }}")}
			m["c/docs/l"] = link()
			return m
		}, Spec{Path: "c"}, ErrSymlinkInChart},
		{"a symlink in a dependency", func() fstest.MapFS {
			m := base()
			m["c/Chart.yaml"] = &fstest.MapFile{Data: []byte(chartYAML + "dependencies:\n  - name: d\n    version: 1.0.0\n    repository: file://../d\n")}
			m["d/Chart.yaml"] = &fstest.MapFile{Data: []byte(chartYAML)}
			m["d/templates/l"] = link()
			return m
		}, Spec{Path: "c"}, ErrSymlinkInChart},
		{"no Chart.yaml", func() fstest.MapFS { m := base(); delete(m, "c/Chart.yaml"); return m }, Spec{Path: "c"}, ErrNotAChart},
		{"a missing value file", base, Spec{Path: "c", ValueFiles: []string{"nope.yaml"}}, ErrMissingValueFile},
		{"a dependency that is gone", func() fstest.MapFS {
			m := base()
			m["c/Chart.yaml"] = &fstest.MapFile{Data: []byte(chartYAML + "dependencies:\n  - name: d\n    version: 1.0.0\n    repository: file://../nope\n")}
			return m
		}, Spec{Path: "c"}, ErrDependencyOutsideRepository},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := RelevantFiles(tt.fsys(), tt.spec); !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}
