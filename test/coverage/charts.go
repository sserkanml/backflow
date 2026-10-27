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
	// multi deploys the chart as an Application with several sources: the chart
	// comes from a Helm repository (it is not in Git, and its own defaults are
	// what Argo CD returns for it), and the repository holds the environment
	// values file only, as the ref source $values.
	multi bool
	// hooks renders the way Argo CD does: the chart's test hooks are in the
	// render (helm template --skip-tests would leave them out), and the render of
	// the same files is answered from a cache, as Argo CD does for a revision it
	// rendered before. The chart names its test Pods at random.
	hooks bool
}

// source is the source type the case is reported under.
func (c chartCase) source() string {
	if c.multi {
		return sourceHelmMulti
	}
	return sourceHelm
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
	// The same charts as an Application with a chart from a Helm repository and
	// its value files in a Git repository ($values/...).
	{name: "nginx-ms", archive: "nginx-25.2.8.tgz", env: "nginx.yaml", envRepoPath: "values/nginx.yaml",
		valueFiles: []string{"/values/nginx.yaml"}, release: "nginx-ms", namespace: "web", multi: true},
	{name: "webapp-ms", dir: "webapp", env: "webapp.yaml", envRepoPath: "values/webapp.yaml",
		valueFiles: []string{"/values/webapp.yaml"}, release: "webapp-ms", namespace: "apps", multi: true},
	// The webapp chart with its environment values in a JSON file, which Helm reads
	// as YAML and Backflow edits as JSON.
	{name: "webapp-json", dir: "webapp", env: "webapp-json.json", envRepoPath: "charts/webapp-json/values-prod.json",
		valueFiles: []string{"values-prod.json"}, release: "webapp-json", namespace: "apps"},
	// A chart shaped like podinfo's: test hooks named at random, a default that
	// only the chart sets (ui.color), podAnnotations and extraEnvs.
	{name: "podinfo", dir: "podinfo", env: "podinfo.yaml", envRepoPath: "charts/podinfo/values-prod.yaml",
		valueFiles: []string{"values-prod.yaml"}, release: "podinfo-prod", namespace: "apps", hooks: true},
	{name: "podinfo-ms", dir: "podinfo", env: "podinfo.yaml", envRepoPath: "values/podinfo.yaml",
		valueFiles: []string{"/values/podinfo.yaml"}, release: "podinfo-ms", namespace: "apps", multi: true, hooks: true},
}

// chartPath is the chart directory in the repository.
func (c chartCase) chartPath() string { return "charts/" + c.name }

// chartFiles are the files of the chart, under chartPath.
func (c chartCase) chartFiles() (map[string][]byte, error) {
	out := map[string][]byte{}
	add := func(name string, data []byte) { out[name] = data }
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
	return out, nil
}

// repo builds the repository the Application reads from: the chart and the
// environment values file, or for a chart that is not in Git the values file only.
func (c chartCase) repo() (fstest.MapFS, error) {
	m := fstest.MapFS{}
	if !c.multi {
		files, err := c.chartFiles()
		if err != nil {
			return nil, err
		}
		for name, data := range files {
			m[name] = &fstest.MapFile{Data: data, Mode: 0o644}
		}
	}
	env, err := os.ReadFile(filepath.Join("testdata", "envs", c.env))
	if err != nil {
		return nil, err
	}
	m[c.envRepoPath] = &fstest.MapFile{Data: env, Mode: 0o644}
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
