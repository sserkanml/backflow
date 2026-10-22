//go:build coverage

package coverage

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing/fstest"
)

// chartCase is one chart under test, deployed the way an Application would:
// the chart in a Git repository next to an environment values file.
type chartCase struct {
	name string
	// archive is a vendored chart package under testdata/charts, extracted to
	// charts/<name>/ of the repository. dir is a chart kept in this repository
	// instead (testdata/charts/<dir>).
	archive, dir string
	// env is the environment values file under testdata/envs; it is stored at
	// envRepoPath and listed as the Application's helm.valueFiles.
	env         string
	envRepoPath string
	valueFiles  []string // as written in the Application (relative to the chart, or /absolute)
	release     string
	namespace   string
}

var charts = []chartCase{
	{name: "nginx", archive: "nginx-25.2.8.tgz", env: "nginx.yaml", envRepoPath: "envs/prod/nginx.yaml",
		valueFiles: []string{"/envs/prod/nginx.yaml"}, release: "nginx-prod", namespace: "web"},
	{name: "redis", archive: "redis-28.3.1.tgz", env: "redis.yaml", envRepoPath: "envs/prod/redis.yaml",
		valueFiles: []string{"/envs/prod/redis.yaml"}, release: "redis-prod", namespace: "cache"},
	{name: "postgresql", archive: "postgresql-18.12.14.tgz", env: "postgresql.yaml", envRepoPath: "envs/prod/postgresql.yaml",
		valueFiles: []string{"/envs/prod/postgresql.yaml"}, release: "postgresql-prod", namespace: "db"},
	{name: "webapp", dir: "webapp", env: "webapp.yaml", envRepoPath: "charts/webapp/values-prod.yaml",
		valueFiles: []string{"values-prod.yaml"}, release: "webapp-prod", namespace: "apps"},
}

// chartPath is the chart directory in the repository.
func (c chartCase) chartPath() string { return "charts/" + c.name }

// repo builds the repository the Application reads from.
func (c chartCase) repo() (fstest.MapFS, error) {
	m := fstest.MapFS{}
	add := func(name string, data []byte) { m[name] = &fstest.MapFile{Data: data, Mode: 0o644} }
	switch {
	case c.archive != "":
		f, err := os.Open(filepath.Join("testdata", "charts", c.archive))
		if err != nil {
			return nil, err
		}
		defer func() { _ = f.Close() }()
		if err := untar(f, c.chartPath(), add); err != nil {
			return nil, fmt.Errorf("%s: %w", c.archive, err)
		}
	default:
		root := filepath.Join("testdata", "charts", c.dir)
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			add(path.Join(c.chartPath(), filepath.ToSlash(rel)), data)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	env, err := os.ReadFile(filepath.Join("testdata", "envs", c.env))
	if err != nil {
		return nil, err
	}
	add(c.envRepoPath, env)
	return m, nil
}

// untar adds the files of a chart package under dest, without the package's
// top directory.
func untar(r io.Reader, dest string, add func(name string, data []byte)) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		_, rest, ok := strings.Cut(h.Name, "/")
		if !ok || rest == "" {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return err
		}
		add(path.Join(dest, rest), data)
	}
}
