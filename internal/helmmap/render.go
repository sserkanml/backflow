package helmmap

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Errors of collecting files for a render.
var (
	// ErrSymlinkInChart: a chart directory holds a symbolic link, which is
	// not sent, so the render would not be the one Argo CD does.
	ErrSymlinkInChart = errors.New("a chart directory contains a symbolic link")
	// ErrDependencyOutsideRepository: a file:// dependency leaves the
	// repository or does not exist.
	ErrDependencyOutsideRepository = errors.New("a file:// dependency is outside the repository or missing")
	// ErrArgoSourceOverrides: the chart directory has .argocd-source*.yaml,
	// which Argo CD merges into the Application source. Its effect on value
	// files and parameters is not modelled.
	ErrArgoSourceOverrides = errors.New("the chart directory has .argocd-source overrides")
)

// CollectFiles reads what a render of src needs from the repository at fsys,
// as a map from repository path to content: the whole chart directory, every
// directory a file:// dependency points to (recursively, and inside the
// repository), the value files, and the files of helm.fileParameters. The
// result is the unmodified Git state; edits are applied to a copy.
func CollectFiles(fsys fs.FS, src *Source) (map[string][]byte, error) {
	if src.External != nil {
		return collectExternal(fsys, src)
	}
	out := map[string][]byte{}
	visited := map[string]bool{}
	queue := []string{src.ChartDir}
	for len(queue) > 0 {
		dir := queue[0]
		queue = queue[1:]
		if visited[dir] {
			continue
		}
		visited[dir] = true
		if err := readTree(fsys, dir, out); err != nil {
			return nil, err
		}
		deps, err := fileDependencies(out, dir)
		if err != nil {
			return nil, err
		}
		for _, d := range deps {
			if _, err := fs.Stat(fsys, d); err != nil {
				return nil, fmt.Errorf("%w: %s (from %s)", ErrDependencyOutsideRepository, d, dir)
			}
			queue = append(queue, d)
		}
	}
	for _, name := range slices.Concat(src.ValueFiles, src.FileParameterFiles) {
		if _, ok := out[name]; ok {
			continue
		}
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", name, err)
		}
		out[name] = data
	}
	for name := range out {
		if path.Dir(name) == src.ChartDir && isArgoSourceOverride(path.Base(name)) {
			return nil, fmt.Errorf("%w: %s", ErrArgoSourceOverrides, name)
		}
	}
	return out, nil
}

func isArgoSourceOverride(base string) bool {
	return base == ".argocd-source.yaml" || (strings.HasPrefix(base, ".argocd-source-") && strings.HasSuffix(base, ".yaml"))
}

// readTree adds every file under dir to out. A symbolic link fails: it is
// neither followed nor sent.
func readTree(fsys fs.FS, dir string, out map[string][]byte) error {
	return fs.WalkDir(fsys, dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			return fmt.Errorf("%w: %s", ErrSymlinkInChart, p)
		case d.IsDir():
			return nil
		case !d.Type().IsRegular():
			return nil
		}
		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		out[p] = data
		return nil
	})
}

// fileDependencies returns the directories the chart in dir depends on
// through file:// repositories, from Chart.yaml (apiVersion v2) and
// requirements.yaml (v1). They are already read into files.
func fileDependencies(files map[string][]byte, dir string) ([]string, error) {
	var deps []string
	for _, name := range []string{chartYAMLName, requirementsName} {
		data, ok := files[path.Join(dir, name)]
		if !ok {
			continue
		}
		var doc struct {
			Dependencies []struct {
				Repository string `yaml:"repository"`
			} `yaml:"dependencies"`
		}
		if err := yaml.Unmarshal(data, &doc); err != nil {
			return nil, fmt.Errorf("%s: %w", path.Join(dir, name), err)
		}
		for _, d := range doc.Dependencies {
			rel, ok := strings.CutPrefix(d.Repository, "file://")
			if !ok {
				continue
			}
			target := path.Join(dir, rel)
			if target == ".." || strings.HasPrefix(target, "../") || path.IsAbs(rel) {
				return nil, fmt.Errorf("%w: %s (from %s)", ErrDependencyOutsideRepository, d.Repository, dir)
			}
			deps = append(deps, target)
		}
	}
	return deps, nil
}

// collectExternal reads what a render of a chart that is not in the repository
// needs from the repository: the value files and the files of the file
// parameters. The chart's defaults are added under the name of Source.ChartValues,
// which is not a path of the repository.
func collectExternal(fsys fs.FS, src *Source) (map[string][]byte, error) {
	out := map[string][]byte{src.ChartValues: src.External.Values}
	for _, name := range slices.Concat(src.ValueFiles, src.FileParameterFiles) {
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", name, err)
		}
		out[name] = data
	}
	return out, nil
}
