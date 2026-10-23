package helmmap

import (
	"errors"
	"io/fs"
	"maps"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

func sortedKeys(m map[string][]byte) []string { return slices.Sorted(maps.Keys(m)) }

func TestCollectFilesDemo(t *testing.T) {
	fsys := demoRepo(t)
	src, err := NewSource(fsys, Spec{Path: "helm/demo", ValueFiles: []string{"values-dev.yaml", "/helm/shared/services/alpha/dev.yaml"}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := CollectFiles(fsys, src)
	if err != nil {
		t.Fatal(err)
	}
	names := strings.Join(sortedKeys(got), "\n")
	for _, want := range []string{
		"helm/demo/Chart.yaml", "helm/demo/values.yaml", "helm/demo/values-dev.yaml", "helm/demo/values-prod.yaml", // the whole chart directory
		"helm/demo-sidecar/Chart.yaml", "helm/demo-sidecar/values.yaml", // the file:// dependency
		"helm/shared/services/alpha/dev.yaml", // a value file outside the chart
	} {
		if !strings.Contains(names, want+"\n") && !strings.HasSuffix(names, want) {
			t.Errorf("%s is not collected:\n%s", want, names)
		}
	}
	if _, ok := got["helm/shared/Chart.yaml"]; ok {
		t.Error("a directory that is only used for a value file was collected as a whole")
	}
}

func TestCollectFilesDependencies(t *testing.T) {
	file := func(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }
	dep := func(repo string) string {
		return "apiVersion: v2\nname: x\ndependencies:\n  - name: d\n    version: 1.0.0\n    repository: " + repo + "\n"
	}
	tests := []struct {
		name    string
		fsys    fstest.MapFS
		wantErr error
		want    []string
	}{
		{
			name: "dependencies of dependencies",
			fsys: fstest.MapFS{
				"a/Chart.yaml": file(dep("file://../b")), "b/Chart.yaml": file(dep("file://../c")), "c/Chart.yaml": file("name: c\n"),
			},
			want: []string{"a/Chart.yaml", "b/Chart.yaml", "c/Chart.yaml", "a/values.yaml"},
		},
		{
			name: "a dependency cycle ends",
			fsys: fstest.MapFS{"a/Chart.yaml": file(dep("file://../b")), "b/Chart.yaml": file(dep("file://../a"))},
			want: []string{"a/Chart.yaml", "b/Chart.yaml", "a/values.yaml"},
		},
		{
			name: "requirements.yaml of an apiVersion v1 chart",
			fsys: fstest.MapFS{
				"a/Chart.yaml": file("name: a\n"), "a/requirements.yaml": file("dependencies:\n  - name: d\n    repository: file://../b\n"),
				"b/Chart.yaml": file("name: b\n"),
			},
			want: []string{"a/Chart.yaml", "a/requirements.yaml", "b/Chart.yaml", "a/values.yaml"},
		},
		{
			name: "remote dependencies are left to Argo CD",
			fsys: fstest.MapFS{"a/Chart.yaml": file(dep("https://charts.example.com"))},
			want: []string{"a/Chart.yaml", "a/values.yaml"},
		},
		{
			name:    "dependency leaving the repository",
			fsys:    fstest.MapFS{"a/Chart.yaml": file(dep("file://../../x"))},
			wantErr: ErrDependencyOutsideRepository,
		},
		{
			name:    "missing dependency directory",
			fsys:    fstest.MapFS{"a/Chart.yaml": file(dep("file://../nope"))},
			wantErr: ErrDependencyOutsideRepository,
		},
		{
			name: "symlink in the chart",
			fsys: fstest.MapFS{
				"a/Chart.yaml": file("name: a\n"),
				"a/link":       &fstest.MapFile{Data: []byte("Chart.yaml"), Mode: fs.ModeSymlink},
			},
			wantErr: ErrSymlinkInChart,
		},
		{
			name:    "argocd source overrides",
			fsys:    fstest.MapFS{"a/Chart.yaml": file("name: a\n"), "a/.argocd-source-demo.yaml": file("helm:\n  parameters: []\n")},
			wantErr: ErrArgoSourceOverrides,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.fsys["a/values.yaml"] = file("x: 1\n")
			src, err := NewSource(tt.fsys, Spec{Path: "a", ValueFiles: []string{"values.yaml"}})
			if err != nil {
				t.Fatal(err)
			}
			got, err := CollectFiles(tt.fsys, src)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := slices.Sorted(slices.Values(tt.want))
			if g := strings.Join(sortedKeys(got), ","); g != strings.Join(want, ",") {
				t.Errorf("files = %s, want %s", g, strings.Join(want, ","))
			}
		})
	}
}
