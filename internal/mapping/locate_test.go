package mapping

import (
	"errors"
	"strings"
	"testing"
	"testing/fstest"
)

func manifest(apiVersion, kind, name, namespace string) string {
	s := "apiVersion: " + apiVersion + "\nkind: " + kind + "\nmetadata:\n  name: " + name + "\n"
	if namespace != "" {
		s += "  namespace: " + namespace + "\n"
	}
	return s
}

func files(m map[string]string) fstest.MapFS {
	fsys := fstest.MapFS{}
	for name, content := range m {
		fsys[name] = &fstest.MapFile{Data: []byte(content)}
	}
	return fsys
}

func TestLocate(t *testing.T) {
	cm := ResourceID{Kind: "ConfigMap", Namespace: "demo", Name: "demo-config"}
	deploy := ResourceID{Group: "apps", Kind: "Deployment", Namespace: "demo", Name: "demo"}

	tests := []struct {
		name    string
		fs      map[string]string
		path    string
		opts    DirectoryOptions
		destNS  string
		res     ResourceID
		want    string // "file#document", empty when an error is expected
		wantErr error
	}{
		{
			name: "single document",
			fs:   map[string]string{"apps/demo/cm.yaml": manifest("v1", "ConfigMap", "demo-config", "demo")},
			path: "apps/demo", destNS: "demo", res: cm, want: "apps/demo/cm.yaml#0",
		},
		{
			name: "document inside a multi-document file",
			fs: map[string]string{"apps/demo/all.yaml": manifest("v1", "Service", "demo", "demo") + "---\n" +
				manifest("apps/v1", "Deployment", "demo", "demo") + "---\n" + manifest("v1", "ConfigMap", "demo-config", "demo")},
			path: "apps/demo", destNS: "demo", res: deploy, want: "apps/demo/all.yaml#1",
		},
		{
			name: "empty documents count towards the index",
			fs:   map[string]string{"a/all.yaml": "---\n---\n" + manifest("v1", "ConfigMap", "demo-config", "demo")},
			path: "a", destNS: "demo", res: cm, want: "a/all.yaml#1",
		},
		{
			name: "no namespace in the document means the destination namespace",
			fs:   map[string]string{"a/cm.yaml": manifest("v1", "ConfigMap", "demo-config", "")},
			path: "a", destNS: "demo", res: cm, want: "a/cm.yaml#0",
		},
		{
			name: "no namespace in the document does not match another namespace",
			fs:   map[string]string{"a/cm.yaml": manifest("v1", "ConfigMap", "demo-config", "")},
			path: "a", destNS: "other", res: cm, wantErr: ErrNotFound,
		},
		{
			name: "an explicit namespace wins over the destination namespace",
			fs:   map[string]string{"a/cm.yaml": manifest("v1", "ConfigMap", "demo-config", "other")},
			path: "a", destNS: "demo", res: cm, wantErr: ErrNotFound,
		},
		{
			name: "cluster-scoped resource",
			fs:   map[string]string{"a/ns.yaml": manifest("v1", "Namespace", "demo", "")},
			path: "a", destNS: "demo", res: ResourceID{Kind: "Namespace", Name: "demo"}, want: "a/ns.yaml#0",
		},
		{
			name: "the group must match",
			fs:   map[string]string{"a/d.yaml": manifest("extensions/v1beta1", "Deployment", "demo", "demo")},
			path: "a", destNS: "demo", res: deploy, wantErr: ErrNotFound,
		},
		{
			name: "the kind must match",
			fs:   map[string]string{"a/s.yaml": manifest("v1", "Service", "demo-config", "demo")},
			path: "a", destNS: "demo", res: cm, wantErr: ErrNotFound,
		},
		{
			name: "not recursive by default",
			fs:   map[string]string{"a/sub/cm.yaml": manifest("v1", "ConfigMap", "demo-config", "demo")},
			path: "a", destNS: "demo", res: cm, wantErr: ErrNotFound,
		},
		{
			name: "recursive",
			fs:   map[string]string{"a/sub/deeper/cm.yml": manifest("v1", "ConfigMap", "demo-config", "demo")},
			path: "a", opts: DirectoryOptions{Recurse: true}, destNS: "demo", res: cm, want: "a/sub/deeper/cm.yml#0",
		},
		{
			name: "files outside the application path are not read",
			fs:   map[string]string{"b/cm.yaml": manifest("v1", "ConfigMap", "demo-config", "demo")},
			path: "a", opts: DirectoryOptions{Recurse: true}, destNS: "demo", res: cm, wantErr: ErrNotFound,
		},
		{
			name: "only manifest extensions are read",
			fs:   map[string]string{"a/cm.txt": manifest("v1", "ConfigMap", "demo-config", "demo"), "a/cm.yaml.bak": manifest("v1", "ConfigMap", "demo-config", "demo")},
			path: "a", destNS: "demo", res: cm, wantErr: ErrNotFound,
		},
		{
			name: "json files are located",
			fs:   map[string]string{"a/cm.json": `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"demo-config","namespace":"demo"}}`},
			path: "a", destNS: "demo", res: cm, want: "a/cm.json#0",
		},
		{
			name: "include",
			fs: map[string]string{
				"a/keep.yaml": manifest("v1", "ConfigMap", "demo-config", "demo"),
				"a/skip.yml":  manifest("v1", "ConfigMap", "demo-config", "demo"),
			},
			path: "a", opts: DirectoryOptions{Include: "*.yaml"}, destNS: "demo", res: cm, want: "a/keep.yaml#0",
		},
		{
			name: "exclude",
			fs: map[string]string{
				"a/keep.yaml":    manifest("v1", "ConfigMap", "demo-config", "demo"),
				"a/test-cm.yaml": manifest("v1", "ConfigMap", "demo-config", "demo"),
			},
			path: "a", opts: DirectoryOptions{Exclude: "test-*"}, destNS: "demo", res: cm, want: "a/keep.yaml#0",
		},
		{
			name: "include with alternatives and exclude together",
			fs: map[string]string{
				"a/cm.yaml":    manifest("v1", "ConfigMap", "demo-config", "demo"),
				"a/other.yml":  manifest("v1", "ConfigMap", "demo-config", "demo"),
				"a/other.json": manifest("v1", "ConfigMap", "demo-config", "demo"),
			},
			path: "a", opts: DirectoryOptions{Include: "{*.yaml,*.yml}", Exclude: "other.*"}, destNS: "demo", res: cm, want: "a/cm.yaml#0",
		},
		{
			name: "patterns are relative to the application path",
			fs: map[string]string{
				"a/sub/cm.yaml": manifest("v1", "ConfigMap", "demo-config", "demo"),
			},
			path: "a", opts: DirectoryOptions{Recurse: true, Exclude: "sub/*"}, destNS: "demo", res: cm, wantErr: ErrNotFound,
		},
		{
			name: "the same resource in two files is ambiguous",
			fs: map[string]string{
				"a/one.yaml": manifest("v1", "ConfigMap", "demo-config", "demo"),
				"a/two.yaml": manifest("v1", "ConfigMap", "demo-config", "demo"),
			},
			path: "a", destNS: "demo", res: cm, wantErr: ErrAmbiguous,
		},
		{
			name: "the same resource twice in one file is ambiguous",
			fs: map[string]string{
				"a/one.yaml": manifest("v1", "ConfigMap", "demo-config", "demo") + "---\n" + manifest("v1", "ConfigMap", "demo-config", "demo"),
			},
			path: "a", destNS: "demo", res: cm, wantErr: ErrAmbiguous,
		},
		{
			name: "a missing directory",
			fs:   map[string]string{"a/cm.yaml": manifest("v1", "ConfigMap", "demo-config", "demo")},
			path: "nope", destNS: "demo", res: cm, wantErr: ErrNotFound,
		},
		{
			name: "the repository root",
			fs:   map[string]string{"cm.yaml": manifest("v1", "ConfigMap", "demo-config", "demo")},
			path: ".", destNS: "demo", res: cm, want: "cm.yaml#0",
		},
		{
			name: "leading and trailing slashes in the path",
			fs:   map[string]string{"a/cm.yaml": manifest("v1", "ConfigMap", "demo-config", "demo")},
			path: "/a/", destNS: "demo", res: cm, want: "a/cm.yaml#0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			loc, err := Locate(files(tt.fs), tt.path, tt.opts, tt.destNS, tt.res)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Replace(loc.String(), " (document ", "#", 1); strings.TrimSuffix(got, ")") != tt.want {
				t.Errorf("location = %s, want %s", loc, tt.want)
			}
		})
	}
}

func TestLocateNeverGuesses(t *testing.T) {
	res := ResourceID{Kind: "ConfigMap", Namespace: "demo", Name: "demo-config"}

	t.Run("ambiguity names every candidate", func(t *testing.T) {
		_, err := Locate(files(map[string]string{
			"a/one.yaml": manifest("v1", "ConfigMap", "demo-config", "demo"),
			"a/two.yaml": manifest("v1", "ConfigMap", "demo-config", "demo"),
		}), "a", DirectoryOptions{}, "demo", res)
		if !errors.Is(err, ErrAmbiguous) || !strings.Contains(err.Error(), "a/one.yaml") || !strings.Contains(err.Error(), "a/two.yaml") {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("a file that cannot be parsed is reported when nothing matches", func(t *testing.T) {
		_, err := Locate(files(map[string]string{
			"a/broken.yaml": "kind: [unclosed\n",
		}), "a", DirectoryOptions{}, "demo", res)
		if !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "a/broken.yaml") {
			t.Errorf("err = %v", err)
		}
	})
}

func TestGlobsFollowArgoCD(t *testing.T) {
	tests := []struct {
		pattern, s string
		want       bool
	}{
		{"*.yaml", "cm.yaml", true},
		{"*.yaml", "sub/cm.yaml", true}, // no separators: * also matches /
		{"*.yaml", "cm.yml", false},
		{"c?.yaml", "cm.yaml", true},
		{"c?.yaml", "cmm.yaml", false},
		{"[ab].yaml", "a.yaml", true},
		{"[!ab].yaml", "a.yaml", false},
		{"[a-c].yaml", "b.yaml", true},
		{"**/*.yaml", "x/y/z.yaml", true},
		{`a\*.yaml`, "a*.yaml", true},
		{`a\*.yaml`, "ab.yaml", false},
		{"{a,b}.yaml", "b.yaml", true},
		{"{a,b}.yaml", "c.yaml", false},
		{"{*.yaml,*.yml}", "x.yml", true},
		// The example from the Argo CD documentation.
		{"{config.json,env-usw2/*}", "config.json", true},
		{"{config.json,env-usw2/*}", "env-usw2/cm.yaml", true},
		{"{config.json,env-usw2/*}", "env-usw2/deeper/cm.yaml", true},
		{"{config.json,env-usw2/*}", "sub/config.json", false},
		{"{config.json,env-usw2/*}", "other.yaml", false},
		{"{config.json,env-usw2/*}", "config.json.bak", false},
		{"a.yaml", "a.yaml", true},
		{"a.yaml", "ayaml", false},
	}
	for _, tt := range tests {
		g, err := compileGlob(tt.pattern)
		if err != nil {
			t.Fatalf("%q: %v", tt.pattern, err)
		}
		if got := g.Match(tt.s); got != tt.want {
			t.Errorf("%q matches %q = %v, want %v", tt.pattern, tt.s, got, tt.want)
		}
	}
}

func TestLocateDocumentedExcludeExample(t *testing.T) {
	repo := files(map[string]string{
		"app/cm.yaml":              manifest("v1", "ConfigMap", "demo-config", "demo"),
		"app/config.json":          `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"demo-config","namespace":"demo"}}`,
		"app/env-usw2/cm.yaml":     manifest("v1", "ConfigMap", "demo-config", "demo"),
		"app/env-use1/cm-use1.yml": manifest("v1", "ConfigMap", "demo-config", "demo"),
		"app/nested/config.json":   `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"demo-config","namespace":"demo"}}`,
	})
	cm := ResourceID{Kind: "ConfigMap", Namespace: "demo", Name: "demo-config"}

	// Without the exclude all five files define the ConfigMap.
	if _, err := Locate(repo, "app", DirectoryOptions{Recurse: true}, "demo", cm); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("err = %v", err)
	}
	// config.json (top level only), and everything under env-usw2/ are excluded;
	// env-use1/ and nested/config.json remain: still ambiguous, and the message names exactly those.
	_, err := Locate(repo, "app", DirectoryOptions{Recurse: true, Exclude: "{config.json,env-usw2/*}"}, "demo", cm)
	if !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("err = %v", err)
	}
	for _, want := range []string{"app/cm.yaml", "app/env-use1/cm-use1.yml", "app/nested/config.json"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%s missing from %v", want, err)
		}
	}
	for _, gone := range []string{"app/config.json", "app/env-usw2/cm.yaml"} {
		if strings.Contains(err.Error(), gone) {
			t.Errorf("%s should be excluded: %v", gone, err)
		}
	}
}

func TestLocateRejectsPatternsItCannotCompile(t *testing.T) {
	repo := files(map[string]string{"a/cm.yaml": manifest("v1", "ConfigMap", "demo-config", "demo")})
	cm := ResourceID{Kind: "ConfigMap", Namespace: "demo", Name: "demo-config"}
	for _, opts := range []DirectoryOptions{
		{Include: "["}, {Exclude: "["}, {Include: "[z-a]"}, {Exclude: "[z-a]"},
	} {
		if _, err := Locate(repo, "a", opts, "demo", cm); !errors.Is(err, ErrInvalidGlob) {
			t.Errorf("%+v: err = %v, want ErrInvalidGlob (Argo CD would silently match nothing)", opts, err)
		}
	}
}

func TestLocateExtensionsAreCaseSensitiveLikeArgoCD(t *testing.T) {
	repo := files(map[string]string{"a/cm.YAML": manifest("v1", "ConfigMap", "demo-config", "demo")})
	_, err := Locate(repo, "a", DirectoryOptions{}, "demo", ResourceID{Kind: "ConfigMap", Namespace: "demo", Name: "demo-config"})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v", err)
	}
}
