package helmmap

import (
	"errors"
	"io/fs"
	"os"
	"reflect"
	"testing"
	"testing/fstest"

	"github.com/sserkanml/backflow/internal/rendermap"
)

// demoRepo is a copy of the Helm part of the demo repository.
func demoRepo(t *testing.T) fs.FS {
	t.Helper()
	return os.DirFS("testdata/repo")
}

func files(names ...string) fs.FS {
	m := fstest.MapFS{}
	for _, n := range names {
		m[n] = &fstest.MapFile{Data: []byte("x: 1\n")}
	}
	return m
}

func TestNewSourceValueFiles(t *testing.T) {
	tests := []struct {
		name    string
		fsys    fs.FS
		spec    Spec
		want    []string
		remote  []string
		wantErr error
	}{
		{
			name: "demo application",
			fsys: demoRepo(t),
			spec: Spec{Path: "helm/demo", ValueFiles: []string{"values-dev.yaml"}},
			want: []string{"helm/demo/values-dev.yaml"},
		},
		{
			name: "no value files",
			fsys: demoRepo(t),
			spec: Spec{Path: "helm/demo"},
			want: nil,
		},
		{
			name: "slash prefix is the repository root",
			fsys: demoRepo(t),
			spec: Spec{Path: "helm/shared", ValueFiles: []string{"/helm/shared/services/alpha/dev.yaml"}},
			want: []string{"helm/shared/services/alpha/dev.yaml"},
		},
		{
			name: "relative path may leave the chart but not the repository",
			fsys: demoRepo(t),
			spec: Spec{Path: "helm/demo", ValueFiles: []string{"../shared/services/beta/dev.yaml"}},
			want: []string{"helm/shared/services/beta/dev.yaml"},
		},
		{
			name: "order is kept",
			fsys: files("c/a.yaml", "c/b.yaml"),
			spec: Spec{Path: "c", ValueFiles: []string{"b.yaml", "a.yaml"}},
			want: []string{"c/b.yaml", "c/a.yaml"},
		},
		{
			name: "duplicate keeps first",
			fsys: files("c/a.yaml", "c/b.yaml"),
			spec: Spec{Path: "c", ValueFiles: []string{"a.yaml", "b.yaml", "./a.yaml"}},
			want: []string{"c/a.yaml", "c/b.yaml"},
		},
		{
			name: "glob in lexical order",
			fsys: demoRepo(t),
			spec: Spec{Path: "helm/shared", ValueFiles: []string{"services/*/dev.yaml"}},
			want: []string{"helm/shared/services/alpha/dev.yaml", "helm/shared/services/beta/dev.yaml"},
		},
		{
			name: "slash prefixed glob with **",
			fsys: demoRepo(t),
			spec: Spec{Path: "helm/demo", ValueFiles: []string{"/helm/shared/**/dev.yaml"}},
			want: []string{"helm/shared/services/alpha/dev.yaml", "helm/shared/services/beta/dev.yaml"},
		},
		{
			name: "explicit entry keeps its position against a glob",
			fsys: files("c/a.yaml", "c/b.yaml", "c/c.yaml"),
			spec: Spec{Path: "c", ValueFiles: []string{"*.yaml", "b.yaml"}},
			want: []string{"c/a.yaml", "c/c.yaml", "c/b.yaml"},
		},
		{
			name: "explicit entry before a glob",
			fsys: files("c/a.yaml", "c/b.yaml", "c/c.yaml"),
			spec: Spec{Path: "c", ValueFiles: []string{"c.yaml", "*.yaml"}},
			want: []string{"c/c.yaml", "c/a.yaml", "c/b.yaml"},
		},
		{
			name:    "glob with no match",
			fsys:    files("c/a.yaml"),
			spec:    Spec{Path: "c", ValueFiles: []string{"env-*.yaml"}},
			wantErr: ErrMissingValueFile,
		},
		{
			name: "glob with no match is skipped when missing files are ignored",
			fsys: files("c/a.yaml"),
			spec: Spec{Path: "c", ValueFiles: []string{"env-*.yaml", "a.yaml"}, IgnoreMissingValueFiles: true},
			want: []string{"c/a.yaml"},
		},
		{
			name:    "missing file",
			fsys:    files("c/a.yaml"),
			spec:    Spec{Path: "c", ValueFiles: []string{"a.yaml", "nope.yaml"}},
			wantErr: ErrMissingValueFile,
		},
		{
			name: "missing file is skipped when ignored",
			fsys: files("c/a.yaml"),
			spec: Spec{Path: "c", ValueFiles: []string{"a.yaml", "nope.yaml"}, IgnoreMissingValueFiles: true},
			want: []string{"c/a.yaml"},
		},
		{
			name:    "path leaving the repository",
			fsys:    files("c/a.yaml"),
			spec:    Spec{Path: "c", ValueFiles: []string{"../../etc/x.yaml"}},
			wantErr: ErrOutsideRepository,
		},
		{
			name:    "slash prefixed path leaving the repository",
			fsys:    files("c/a.yaml"),
			spec:    Spec{Path: "c", ValueFiles: []string{"/../x.yaml"}},
			wantErr: ErrOutsideRepository,
		},
		{
			name:    "the repository root itself",
			fsys:    files("c/a.yaml"),
			spec:    Spec{Path: "c", ValueFiles: []string{".."}},
			wantErr: ErrOutsideRepository,
		},
		{
			name:    "chart path leaving the repository",
			fsys:    files("c/a.yaml"),
			spec:    Spec{Path: "../c"},
			wantErr: ErrOutsideRepository,
		},
		{
			name: "env variables",
			fsys: files("env/prod.yaml", "env/dev.yaml"),
			spec: Spec{Path: "env", ValueFiles: []string{"$ARGOCD_APP_NAME.yaml", "${ARGOCD_APP_NAME}.yaml"},
				Env: map[string]string{"ARGOCD_APP_NAME": "prod"}},
			want: []string{"env/prod.yaml"},
		},
		{
			name: "source path variable defaults to the chart path",
			fsys: files("env/x/v.yaml"),
			spec: Spec{Path: "env/x", ValueFiles: []string{"/$ARGOCD_APP_SOURCE_PATH/v.yaml"}},
			want: []string{"env/x/v.yaml"},
		},
		{
			name: "dollar escape",
			fsys: files("c/$x.yaml"),
			spec: Spec{Path: "c", ValueFiles: []string{"$$x.yaml"}},
			want: []string{"c/$x.yaml"},
		},
		{
			name:    "reference to another source",
			fsys:    files("c/a.yaml"),
			spec:    Spec{Path: "c", ValueFiles: []string{"$values/a.yaml"}},
			wantErr: ErrUnsupportedValueFile,
		},
		{
			name:   "remote files are reported, not resolved",
			fsys:   files("c/a.yaml"),
			spec:   Spec{Path: "c", ValueFiles: []string{"https://example.com/v.yaml", "a.yaml"}},
			want:   []string{"c/a.yaml"},
			remote: []string{"https://example.com/v.yaml"},
		},
		{
			name:    "url scheme that is not allowed",
			fsys:    files("c/a.yaml"),
			spec:    Spec{Path: "c", ValueFiles: []string{"file:///etc/passwd"}},
			wantErr: ErrUnsupportedValueFile,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src, err := NewSource(tt.fsys, tt.spec)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(src.ValueFiles, tt.want) {
				t.Errorf("ValueFiles = %q, want %q", src.ValueFiles, tt.want)
			}
			if !reflect.DeepEqual(src.RemoteValueFiles, tt.remote) {
				t.Errorf("RemoteValueFiles = %q, want %q", src.RemoteValueFiles, tt.remote)
			}
		})
	}
}

func TestNewSourceChart(t *testing.T) {
	src, err := NewSource(demoRepo(t), Spec{Path: "./helm/demo/"})
	if err != nil {
		t.Fatal(err)
	}
	if src.ChartDir != "helm/demo" || src.ChartValues != "helm/demo/values.yaml" {
		t.Fatalf("chart = %q, values = %q", src.ChartDir, src.ChartValues)
	}
}

func TestNewSourceInline(t *testing.T) {
	tests := []struct {
		name    string
		spec    Spec
		want    []InlineValue
		wantErr error
	}{
		{
			name: "parameters",
			spec: Spec{Path: "c", Parameters: []Parameter{{Name: "config.feature", Value: "beta"}}},
			want: []InlineValue{{Layer: LayerParameters, Path: rendermap.KeyPath{"config", "feature"}, Value: "beta", HasValue: true}},
		},
		{
			name: "values string",
			spec: Spec{Path: "c", Values: "a:\n  b: 3\n  c: [1, 2]\n  d: null\n"},
			want: []InlineValue{
				{Layer: LayerValues, Path: rendermap.KeyPath{"a", "b"}, Value: "3", HasValue: true},
				{Layer: LayerValues, Path: rendermap.KeyPath{"a", "c"}},
				{Layer: LayerValues, Path: rendermap.KeyPath{"a", "d"}},
			},
		},
		{
			name: "valuesObject wins over values",
			spec: Spec{Path: "c", Values: "x: 1\n", ValuesObject: []byte(`{"y": true}`)},
			want: []InlineValue{{Layer: LayerValuesObject, Path: rendermap.KeyPath{"y"}, Value: "true", HasValue: true}},
		},
		{
			name: "order is values, set, set-string, set-file",
			spec: Spec{Path: "c",
				Values:         "v: 1\n",
				Parameters:     []Parameter{{Name: "s", Value: "1", ForceString: true}, {Name: "p", Value: "2"}},
				FileParameters: []FileParameter{{Name: "f", Path: "f.txt"}}},
			want: []InlineValue{
				{Layer: LayerValues, Path: rendermap.KeyPath{"v"}, Value: "1", HasValue: true},
				{Layer: LayerParameters, Path: rendermap.KeyPath{"p"}, Value: "2", HasValue: true},
				{Layer: LayerParameters, Path: rendermap.KeyPath{"s"}, Value: "1", HasValue: true},
				{Layer: LayerFileParameters, Path: rendermap.KeyPath{"f"}},
			},
		},
		{
			name: "parameter name with escaped dots",
			spec: Spec{Path: "c", Parameters: []Parameter{{Name: `labels.app\.io/name`, Value: "x"}}},
			want: []InlineValue{{Layer: LayerParameters, Path: rendermap.KeyPath{"labels", "app.io/name"}, Value: "x", HasValue: true}},
		},
		{
			name:    "bad parameter name",
			spec:    Spec{Path: "c", Parameters: []Parameter{{Name: "a[x]", Value: "1"}}},
			wantErr: ErrInvalidInline,
		},
		{
			name:    "bad values",
			spec:    Spec{Path: "c", Values: "a: [1\n"},
			wantErr: ErrInvalidInline,
		},
		{
			name:    "values that are not a map",
			spec:    Spec{Path: "c", Values: "- a\n"},
			wantErr: ErrInvalidInline,
		},
		{
			name:    "valuesObject that is not JSON",
			spec:    Spec{Path: "c", ValuesObject: []byte("a: 1\n")},
			wantErr: ErrInvalidInline,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src, err := NewSource(files("c/a.yaml", "c/f.txt"), tt.spec)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(src.Inline, tt.want) {
				t.Errorf("Inline = %+v\nwant %+v", src.Inline, tt.want)
			}
		})
	}
}

func TestNewSourceSymlinks(t *testing.T) {
	link := func(target string) *fstest.MapFile {
		return &fstest.MapFile{Data: []byte(target), Mode: fs.ModeSymlink}
	}
	file := func() *fstest.MapFile { return &fstest.MapFile{Data: []byte("x: 1\n")} }
	fsys := fstest.MapFS{
		"c/real.yaml":       file(),
		"c/link.yaml":       link("real.yaml"),
		"c/dir-link":        link("real-dir"),
		"c/real-dir/a.yaml": file(),
		"c/envs/a/v.yaml":   file(),
		"c/envs/b":          link("a"),
	}
	tests := []struct {
		name       string
		valueFiles []string
		ignore     bool
	}{
		{name: "the file is a symlink", valueFiles: []string{"link.yaml"}},
		{name: "a directory above the file is a symlink", valueFiles: []string{"dir-link/a.yaml"}},
		{name: "a glob ends at a symlink", valueFiles: []string{"*.yaml"}},
		{name: "a glob walks through a symlinked directory", valueFiles: []string{"envs/*/v.yaml"}},
		{name: "ignoreMissingValueFiles does not hide it", valueFiles: []string{"link.yaml"}, ignore: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewSource(fsys, Spec{Path: "c", ValueFiles: tt.valueFiles, IgnoreMissingValueFiles: tt.ignore})
			if !errors.Is(err, ErrSymlinkedValueFile) {
				t.Fatalf("err = %v, want ErrSymlinkedValueFile", err)
			}
			if got := ReasonOf(err); got != "SymlinkedValueFile" {
				t.Errorf("reason = %q", got)
			}
		})
	}

	// A regular file next to the links still resolves.
	src, err := NewSource(fsys, Spec{Path: "c", ValueFiles: []string{"real.yaml"}})
	if err != nil || len(src.ValueFiles) != 1 {
		t.Fatalf("regular file: %v %v", src, err)
	}
}

func TestReasonOf(t *testing.T) {
	tests := map[error]string{
		ErrMissingValueFile:     ReasonMissingValueFile,
		ErrOutsideRepository:    ReasonValueFileOutsideRepo,
		ErrUnsupportedValueFile: ReasonUnsupportedValueFile,
		ErrInvalidInline:        ReasonInvalidInlineValues,
		errors.New("other"):     "",
	}
	for err, want := range tests {
		if got := ReasonOf(err); got != want {
			t.Errorf("ReasonOf(%v) = %q, want %q", err, got, want)
		}
	}
}

func TestNewSourceFileParameters(t *testing.T) {
	fsys := files("c/tls.pem", "c/real.yaml")
	src, err := NewSource(fsys, Spec{Path: "c", FileParameters: []FileParameter{{Name: "tls", Path: "tls.pem"}, {Name: "tls2", Path: "tls.pem"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(src.FileParameterFiles, []string{"c/tls.pem"}) {
		t.Errorf("FileParameterFiles = %v", src.FileParameterFiles)
	}
	for name, fp := range map[string]FileParameter{
		"missing": {Name: "x", Path: "nope.pem"},
		"outside": {Name: "x", Path: "../../x"},
		"url":     {Name: "x", Path: "https://example.com/x"},
	} {
		if _, err := NewSource(fsys, Spec{Path: "c", FileParameters: []FileParameter{fp}}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
