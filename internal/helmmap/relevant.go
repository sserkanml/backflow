package helmmap

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"
)

// ErrNotAChart means a directory a Helm source points to has no Chart.yaml
// at this revision.
var ErrNotAChart = errors.New("the directory has no Chart.yaml")

// chartYAMLName and requirementsName name the files that list a chart's dependencies.
const (
	chartYAMLName    = "Chart.yaml"
	requirementsName = "requirements.yaml"
)

// chartFiles are the files of a chart directory that Helm always reads.
var chartFiles = []string{
	chartYAMLName, "Chart.lock", requirementsName, "requirements.lock",
	"values.yaml", "values.schema.json", ".helmignore",
}

// chartDirs are the directories of a chart whose whole content Helm reads.
var chartDirs = []string{"templates", "charts", "crds"}

// Relevance tells which files of a repository, at one revision, can change
// what a Helm Application renders. Anything else (another service's values
// file next to a shared chart, another environment's values file, a README)
// cannot.
type Relevance struct {
	exact    map[string]bool
	prefixes []string // directories, with a trailing slash: everything below counts
	roots    []filesRoot
}

// filesRoot is a chart directory whose other files count too, because a
// template reads them with .Files.
type filesRoot struct {
	dir    string
	ignore func(rel string) bool
}

// Matches reports whether a changed path is one the render can depend on.
func (r *Relevance) Matches(p string) bool {
	if r.exact[p] {
		return true
	}
	for _, pre := range r.prefixes {
		// The directory itself becoming a file, or the reverse, counts too.
		if strings.HasPrefix(p, pre) || p+"/" == pre {
			return true
		}
	}
	for _, root := range r.roots {
		rel := p
		if root.dir != "." {
			if p == root.dir {
				return true
			}
			var ok bool
			if rel, ok = strings.CutPrefix(p, root.dir+"/"); !ok {
				continue
			}
		}
		if !root.ignore(rel) {
			return true
		}
	}
	return false
}

// Merge adds what o matches to r.
func (r *Relevance) Merge(o *Relevance) {
	for p := range o.exact {
		r.exact[p] = true
	}
	r.prefixes = append(r.prefixes, o.prefixes...)
	r.roots = append(r.roots, o.roots...)
}

// RelevantFiles works out, for the repository at fsys (one revision), which
// files the Helm source can depend on:
//   - per chart directory (the Application's, and every directory a file://
//     dependency points to, recursively): Chart.yaml, Chart.lock,
//     requirements.yaml and .lock, values.yaml, values.schema.json,
//     .helmignore, and everything under templates/, charts/ and crds/;
//   - the other files of such a directory only when a template uses .Files,
//     except those its .helmignore excludes;
//   - the value files the Application resolves to (globs expanded at this
//     revision) and the files of its fileParameters, wherever they are.
//
// Where it cannot tell, it fails and the caller must not assume anything: a
// symbolic link in any of these (ErrSymlinkInChart, ErrSymlinkedValueFile), a
// chart directory without Chart.yaml (ErrNotAChart), a dependency outside the
// repository (ErrDependencyOutsideRepository), a value file that does not
// exist, or a tree that cannot be read.
func RelevantFiles(fsys fs.FS, spec Spec) (*Relevance, error) {
	src, err := NewSource(fsys, spec)
	if err != nil {
		return nil, err
	}
	rel := &Relevance{exact: map[string]bool{}}
	for _, f := range append(append([]string{}, src.ValueFiles...), src.FileParameterFiles...) {
		rel.exact[f] = true
	}

	visited := map[string]bool{}
	queue := []string{src.ChartDir}
	for len(queue) > 0 {
		dir := queue[0]
		queue = queue[1:]
		if visited[dir] {
			continue
		}
		visited[dir] = true
		deps, err := rel.addChart(fsys, dir)
		if err != nil {
			return nil, err
		}
		queue = append(queue, deps...)
	}
	return rel, nil
}

// addChart records what one chart directory contributes and returns the
// directories of its file:// dependencies.
func (r *Relevance) addChart(fsys fs.FS, dir string) ([]string, error) {
	if info, err := lstat(fsys, path.Join(dir, chartYAMLName)); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrNotAChart, dir)
	} else if info.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: %s", ErrSymlinkInChart, path.Join(dir, chartYAMLName))
	}
	for _, name := range chartFiles {
		p := path.Join(dir, name)
		info, err := lstat(fsys, p)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				r.exact[p] = true // it may appear
				continue
			}
			return nil, err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return nil, fmt.Errorf("%w: %s", ErrSymlinkInChart, p)
		}
		r.exact[p] = true
	}
	for _, name := range chartDirs {
		p := path.Join(dir, name)
		r.prefixes = append(r.prefixes, p+"/")
		if err := noSymlinksBelow(fsys, p); err != nil {
			return nil, err
		}
	}

	files := map[string][]byte{}
	for _, name := range []string{chartYAMLName, requirementsName} {
		if data, err := fs.ReadFile(fsys, path.Join(dir, name)); err == nil {
			files[path.Join(dir, name)] = data
		}
	}
	deps, err := fileDependencies(files, dir)
	if err != nil {
		return nil, err
	}
	for _, d := range deps {
		if _, err := fs.Stat(fsys, d); err != nil {
			return nil, fmt.Errorf("%w: %s (from %s)", ErrDependencyOutsideRepository, d, dir)
		}
	}

	uses, err := usesFilesObject(fsys, path.Join(dir, "templates"))
	if err != nil {
		return nil, err
	}
	if uses {
		ignore, err := helmIgnore(fsys, path.Join(dir, ".helmignore"))
		if err != nil {
			return nil, err
		}
		if err := noSymlinksBelow(fsys, dir); err != nil {
			return nil, err
		}
		r.roots = append(r.roots, filesRoot{dir: dir, ignore: ignore})
	}
	return deps, nil
}

// noSymlinksBelow fails when anything at or below dir is a symbolic link. A
// directory that does not exist has none.
func noSymlinksBelow(fsys fs.FS, dir string) error {
	info, err := lstat(fsys, dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s", ErrSymlinkInChart, dir)
	}
	return fs.WalkDir(fsys, dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s", ErrSymlinkInChart, p)
		}
		return nil
	})
}

// usesFilesObject reports whether a template under dir mentions .Files.
func usesFilesObject(fsys fs.FS, dir string) (bool, error) {
	found := false
	err := fs.WalkDir(fsys, dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return fs.SkipAll
			}
			return err
		}
		if d.IsDir() || found {
			return nil
		}
		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		found = strings.Contains(string(data), ".Files")
		return nil
	})
	return found, err
}

// helmIgnore returns a function that tells whether a path inside the chart
// directory is excluded by .helmignore. It errs on the side of counting more
// files: a file is ignored only when a plain pattern clearly matches it, and
// as soon as the file has a negation (!) or a ** pattern nothing is ignored.
func helmIgnore(fsys fs.FS, name string) (func(rel string) bool, error) {
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return func(string) bool { return false }, nil
		}
		return nil, err
	}
	type rule struct {
		pattern  string
		dirOnly  bool
		anchored bool
	}
	var rules []rule
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "!") || strings.Contains(line, "**") {
			return func(string) bool { return false }, nil
		}
		r := rule{}
		r.dirOnly = strings.HasSuffix(line, "/")
		line = strings.TrimSuffix(line, "/")
		r.anchored = strings.HasPrefix(line, "/")
		r.pattern = strings.TrimPrefix(line, "/")
		rules = append(rules, r)
	}
	return func(rel string) bool {
		parts := strings.Split(rel, "/")
		for _, r := range rules {
			for i := range parts {
				last := i == len(parts)-1
				if r.dirOnly && last {
					continue // the entry is a file; the rule is for directories
				}
				var ok bool
				if r.anchored || strings.Contains(r.pattern, "/") {
					ok, _ = path.Match(r.pattern, strings.Join(parts[:i+1], "/"))
				} else {
					ok, _ = path.Match(r.pattern, parts[i])
				}
				if ok {
					return true
				}
			}
		}
		return false
	}, nil
}
