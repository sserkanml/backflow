// Package kustomize works out which files of a repository a Kustomize source
// reads. It parses kustomization files and never renders: rendering is Argo
// CD's.
package kustomize

import (
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// kustomizationNames are the names kustomize looks for in a directory. All of
// them are relevant, present or not: a file that appears under one of the
// names changes the build.
var kustomizationNames = []string{"kustomization.yaml", "kustomization.yml", "Kustomization"}

// maxDirs bounds the directories followed, so a repository with a reference
// cycle or an enormous overlay tree cannot keep the check busy.
const maxDirs = 500

// Spec is what decides the files of a Kustomize Application: its directory and
// the options of spec.source.kustomize that name files.
type Spec struct {
	// Path is the Application's directory in the repository.
	Path string
	// Components are spec.source.kustomize.components, relative to Path.
	Components []string
	// IgnoreMissingComponents is spec.source.kustomize.ignoreMissingComponents.
	IgnoreMissingComponents bool
	// PatchFiles are the path of every spec.source.kustomize.patches entry that
	// has one, relative to Path.
	PatchFiles []string
}

// Relevance tells which files of a repository, at one revision, can change
// what a Kustomize Application renders.
type Relevance struct {
	files map[string]bool
	// reasons say why the set is not closed: what the build reads that could
	// not be followed.
	reasons []string
}

// Matches reports whether a changed path is a file the build reads, or a
// place where one it could read would appear (the kustomization file of a
// directory under another of its names).
func (r *Relevance) Matches(p string) bool {
	return r.files[p]
}

// Closed reports whether Files holds everything the build reads. When it does
// not, a change outside Files may still matter.
func (r *Relevance) Closed() bool { return len(r.reasons) == 0 }

// Reasons says why the set is not closed, empty when it is. Each is one
// sentence naming the file and what it references.
func (r *Relevance) Reasons() []string { return r.reasons }

// Files are the paths Matches accepts, in no particular order.
func (r *Relevance) Files() []string {
	out := make([]string, 0, len(r.files))
	for f := range r.files {
		out = append(out, f)
	}
	return out
}

// RelevantFiles reads the kustomization files of the Application at fsys (one
// revision) and follows what they reference, recursively: resources, bases,
// components, patches, generator files and envs, replacements, openapi, crds,
// configurations, generators and transformers. A directory reference is
// followed into its own kustomization; a file reference is a file of the set.
//
// What it cannot follow makes the set not closed (Relevance.Closed), with a
// reason: a remote reference (URL or git repository), helmCharts, a symbolic
// link, a path outside the repository, a reference that does not exist, a
// directory without a kustomization, a kustomization that does not parse.
// Only a tree that cannot be read is an error.
func RelevantFiles(fsys fs.FS, spec Spec) (*Relevance, error) {
	w := &walker{fsys: fsys, rel: &Relevance{files: map[string]bool{}}, visited: map[string]bool{}}
	root, err := cleanRepoPath(spec.Path)
	if err != nil {
		w.open("the Application path %q is %v", spec.Path, err)
		return w.rel, nil
	}
	if err := w.dir(root, "the Application"); err != nil {
		return nil, err
	}
	// Components of the Application: directories relative to its path, which
	// kustomize adds to the kustomization of the path.
	for _, c := range spec.Components {
		target, err := cleanRepoPath(path.Join(root, c))
		if err != nil {
			w.open("component %q of the Application is %v", c, err)
			continue
		}
		if spec.IgnoreMissingComponents && !w.exists(target) {
			w.rel.files[target] = true // it may appear
			continue
		}
		if err := w.ref(root, c, "component of the Application"); err != nil {
			return nil, err
		}
	}
	for _, p := range spec.PatchFiles {
		if err := w.file(root, p, "patch of the Application"); err != nil {
			return nil, err
		}
	}
	return w.rel, nil
}

type walker struct {
	fsys    fs.FS
	rel     *Relevance
	visited map[string]bool
}

func (w *walker) open(format string, args ...any) {
	w.rel.reasons = append(w.rel.reasons, fmt.Sprintf(format, args...))
}

func (w *walker) exists(p string) bool {
	_, err := fs.Stat(w.fsys, p)
	return err == nil
}

// kustomization is the part of a kustomization file that names files.
type kustomization struct {
	Resources             []string `yaml:"resources"`
	Bases                 []string `yaml:"bases"`
	Components            []string `yaml:"components"`
	Crds                  []string `yaml:"crds"`
	Configurations        []string `yaml:"configurations"`
	Generators            []string `yaml:"generators"`
	Transformers          []string `yaml:"transformers"`
	PatchesStrategicMerge []string `yaml:"patchesStrategicMerge"`
	Patches               []struct {
		Path string `yaml:"path"`
	} `yaml:"patches"`
	PatchesJson6902 []struct {
		Path string `yaml:"path"`
	} `yaml:"patchesJson6902"`
	Replacements []yaml.Node `yaml:"replacements"`
	Openapi      struct {
		Path string `yaml:"path"`
	} `yaml:"openapi"`
	CMG       []generator `yaml:"configMapGenerator"`
	SG        []generator `yaml:"secretGenerator"`
	HelmChart []yaml.Node `yaml:"helmCharts"`
}

type generator struct {
	Name  string   `yaml:"name"`
	Files []string `yaml:"files"`
	Envs  []string `yaml:"envs"`
	Env   string   `yaml:"env"`
}

// dir records the kustomization of a directory and what it references.
func (w *walker) dir(dir, from string) error {
	if w.visited[dir] {
		return nil
	}
	if len(w.visited) >= maxDirs {
		w.open("more than %d kustomization directories", maxDirs)
		return nil
	}
	w.visited[dir] = true

	var found string
	for _, n := range kustomizationNames {
		p := path.Join(dir, n)
		w.rel.files[p] = true
		if _, err := fs.Stat(w.fsys, p); err == nil && found == "" {
			found = p
		}
	}
	if found == "" {
		w.open("%s refers to %s, which has no kustomization file", from, displayDir(dir))
		return nil
	}
	if w.symlinked(found) {
		w.open("%s is a symbolic link", found)
		return nil
	}
	data, err := fs.ReadFile(w.fsys, found)
	if err != nil {
		return err
	}
	var k kustomization
	if err := yaml.Unmarshal(data, &k); err != nil {
		w.open("%s does not parse: %v", found, err)
		return nil
	}
	if len(k.HelmChart) > 0 {
		w.open("%s uses helmCharts, which are not supported", found)
	}
	refs, files := k.references()
	for _, ref := range refs {
		if err := w.ref(dir, ref, found); err != nil {
			return err
		}
	}
	for _, f := range files {
		if err := w.file(dir, f, found); err != nil {
			return err
		}
	}
	return nil
}

// references returns what a kustomization names: refs are files or directories
// (resources, bases, components, generators, transformers), files are files.
func (k *kustomization) references() (refs, files []string) {
	for _, list := range [][]string{k.Resources, k.Bases, k.Components, k.Generators, k.Transformers} {
		refs = append(refs, list...)
	}
	files = append(files, k.Crds...)
	files = append(files, k.Configurations...)
	for _, ref := range k.PatchesStrategicMerge {
		if !strings.ContainsAny(ref, "\n{") { // else an inline patch
			files = append(files, ref)
		}
	}
	for _, p := range k.Patches {
		files = append(files, p.Path)
	}
	for _, p := range k.PatchesJson6902 {
		files = append(files, p.Path)
	}
	for _, n := range k.Replacements {
		var r struct {
			Path string `yaml:"path"`
		}
		if n.Kind == yaml.MappingNode && n.Decode(&r) == nil {
			files = append(files, r.Path)
		}
	}
	files = append(files, k.Openapi.Path)
	for _, g := range append(k.CMG, k.SG...) {
		for _, f := range g.Files {
			// "key=path" names the key the file's content gets.
			if _, p, ok := strings.Cut(f, "="); ok {
				f = p
			}
			files = append(files, f)
		}
		files = append(files, g.Envs...)
		files = append(files, g.Env)
	}
	return refs, slices.DeleteFunc(files, func(f string) bool { return f == "" })
}

// ref follows a reference that is a file or a directory (resources, bases,
// components, generators, transformers).
func (w *walker) ref(dir, ref, from string) error {
	target, ok := w.resolve(dir, ref, from)
	if !ok {
		return nil
	}
	info, err := lstat(w.fsys, target)
	switch {
	case err != nil:
		w.missing(ref, from)
		return nil
	case info.Mode()&fs.ModeSymlink != 0:
		w.open("%s refers to %s, a symbolic link", from, target)
		return nil
	case info.IsDir():
		return w.dir(target, from)
	}
	w.rel.files[target] = true
	return nil
}

// file records a reference that is a file.
func (w *walker) file(dir, ref, from string) error {
	target, ok := w.resolve(dir, ref, from)
	if !ok {
		return nil
	}
	w.rel.files[target] = true // read now, or a file that may appear
	info, err := lstat(w.fsys, target)
	switch {
	case err != nil:
		w.missing(ref, from)
	case info.Mode()&fs.ModeSymlink != 0:
		w.open("%s refers to %s, a symbolic link", from, target)
	case info.IsDir():
		w.open("%s refers to %s, which is a directory, not a file", from, target)
	}
	return nil
}

// resolve returns the repository path a reference points to. ok is false, and
// the reason recorded, for a reference that cannot be followed.
func (w *walker) resolve(dir, ref, from string) (string, bool) {
	if isRemote(ref) && !w.exists(path.Join(dir, ref)) {
		w.open("%s refers to %s, which is remote", from, ref)
		return "", false
	}
	target, err := cleanRepoPath(path.Join(dir, ref))
	if err != nil || path.IsAbs(ref) {
		w.open("%s refers to %s, which is outside the repository", from, ref)
		return "", false
	}
	return target, true
}

func (w *walker) missing(ref, from string) {
	if isRemote(ref) {
		w.open("%s refers to %s, which is remote", from, ref)
		return
	}
	w.open("%s refers to %s, which does not exist", from, ref)
}

func (w *walker) symlinked(p string) bool {
	info, err := lstat(w.fsys, p)
	return err == nil && info.Mode()&fs.ModeSymlink != 0
}

// lstat describes a path without following a symbolic link where the file
// system can tell. A file system without symbolic links of its own, such as the
// view of a Git commit, reports a link as a file with the symlink mode.
func lstat(fsys fs.FS, name string) (fs.FileInfo, error) {
	if _, ok := fsys.(fs.ReadLinkFS); ok {
		return fs.Lstat(fsys, name)
	}
	return fs.Stat(fsys, name)
}

var shorthandHost = regexp.MustCompile(`^[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)+/`)

// isRemote reports whether a reference looks like a URL or a git repository,
// which kustomize fetches: scheme://, git@host:, git::, gh: and host.tld/org/repo.
func isRemote(ref string) bool {
	return strings.Contains(ref, "://") || strings.HasPrefix(ref, "git@") ||
		strings.HasPrefix(ref, "git::") || strings.HasPrefix(ref, "gh:") || shorthandHost.MatchString(ref)
}

// cleanRepoPath normalises a repository path and rejects one that leaves the
// repository.
func cleanRepoPath(p string) (string, error) {
	p = strings.TrimPrefix(strings.TrimSpace(p), "./")
	if path.IsAbs(p) {
		return "", fmt.Errorf("absolute")
	}
	c := path.Clean(p)
	if c == ".." || strings.HasPrefix(c, "../") {
		return "", fmt.Errorf("outside the repository")
	}
	return c, nil
}

func displayDir(dir string) string {
	if dir == "." {
		return "the repository root"
	}
	return dir
}
