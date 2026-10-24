package kustomize

import (
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"
)

// Source is a Kustomize Application in one repository revision: the directory
// it builds and the files that build reads.
type Source struct {
	// Dir is the Application's directory (spec.source.path), cleaned.
	Dir string
	// Options are the spec.source.kustomize options of the Application.
	Options Options
	rel     *Relevance
}

// NewSource works out the files a build of the Application reads. It returns
// an error wrapping ErrNotClosed, with the reasons in its text, when the set is
// not closed: a render of edited files could differ from Argo CD's.
func NewSource(fsys fs.FS, dir string, opts Options) (*Source, error) {
	clean, err := cleanRepoPath(dir)
	if err != nil {
		return nil, fmt.Errorf("%w: the Application path %q is %v", ErrNotClosed, dir, err)
	}
	rel, err := RelevantFiles(fsys, Spec{
		Path: clean, Components: opts.Components,
		IgnoreMissingComponents: opts.IgnoreMissingComponents, PatchFiles: opts.PatchFiles(),
	})
	if err != nil {
		return nil, err
	}
	if !rel.Closed() {
		return nil, fmt.Errorf("%w: %s", ErrNotClosed, strings.Join(rel.Reasons(), "; "))
	}
	return &Source{Dir: clean, Options: opts, rel: rel}, nil
}

// Relevance is the set of files the build reads.
func (s *Source) Relevance() *Relevance { return s.rel }

// InPath reports whether a repository file is inside the Application's path.
func (s *Source) InPath(file string) bool {
	return s.Dir == "." || file == s.Dir || strings.HasPrefix(file, s.Dir+"/")
}

// Rel returns a path relative to the Application's directory, as the
// config.kubernetes.io/origin annotation spells it.
func (s *Source) Rel(file string) string {
	return relPath(s.Dir, file)
}

// Join makes a repository path from one relative to the Application's directory.
func (s *Source) Join(rel string) (string, error) { return cleanRepoPath(path.Join(s.Dir, rel)) }

func relPath(base, target string) string {
	if base == "." {
		return target
	}
	bs, ts := strings.Split(base, "/"), strings.Split(target, "/")
	i := 0
	for i < len(bs) && i < len(ts) && bs[i] == ts[i] {
		i++
	}
	parts := make([]string, 0, len(bs)-i+len(ts)-i)
	for range bs[i:] {
		parts = append(parts, "..")
	}
	parts = append(parts, ts[i:]...)
	if len(parts) == 0 {
		return "."
	}
	return strings.Join(parts, "/")
}

// CollectFiles reads the files of the closed set that exist, as a map from
// repository path to content: the unmodified Git state. Directories and files
// that may appear but do not exist are left out.
func CollectFiles(fsys fs.FS, src *Source) (map[string][]byte, error) {
	out := map[string][]byte{}
	files := src.rel.Files()
	slices.Sort(files)
	for _, f := range files {
		info, err := fs.Stat(fsys, f)
		if err != nil || info.IsDir() {
			continue
		}
		data, err := fs.ReadFile(fsys, f)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", f, err)
		}
		out[f] = data
	}
	return out, nil
}
