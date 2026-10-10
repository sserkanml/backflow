package mapping

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/gobwas/glob"
	"go.yaml.in/yaml/v3"
)

// DirectoryOptions are the spec.source.directory options of an Application.
// Like in Argo CD, Recurse is false by default.
type DirectoryOptions struct {
	Recurse bool
	// Include and Exclude are glob patterns matched against the file path
	// relative to the Application path, with the same library and settings
	// Argo CD uses (github.com/gobwas/glob, no separators): "*" and "**"
	// match any run of characters including "/", "?" one character, "[abc]",
	// "[!a-c]" classes and "{a,b}" alternatives, "\" escapes. An empty
	// Include includes every file. A pattern that does not compile is an
	// error (ErrInvalidGlob); Argo CD would silently match nothing.
	Include string
	Exclude string
}

// ResourceID identifies a resource the way Argo CD reports it.
type ResourceID struct {
	Group     string
	Kind      string
	Namespace string
	Name      string
}

// Location is the document that defines a resource.
type Location struct {
	// File path inside the file system (relative to the repository root).
	File string
	// Document is the index of the YAML document in the file, counting every
	// document delimited by "---", including empty ones.
	Document int
}

func (l Location) String() string { return fmt.Sprintf("%s (document %d)", l.File, l.Document) }

// Locate finds the single document under appPath that defines res.
//
// A document without metadata.namespace belongs to destNamespace, which is
// where Argo CD applies it. A cluster-scoped resource (empty namespace) also
// matches a document without a namespace. Zero matches yield ErrNotFound and
// more than one ErrAmbiguous; Locate never picks one.
func Locate(fsys fs.FS, appPath string, opts DirectoryOptions, destNamespace string, res ResourceID) (*Location, error) {
	root := path.Clean(strings.Trim(appPath, "/"))
	if !fs.ValidPath(root) {
		return nil, fmt.Errorf("%w: invalid path %q", ErrNotFound, appPath)
	}
	files, err := manifestFiles(fsys, root, opts)
	if err != nil {
		return nil, err
	}

	var matches []Location
	var unparsable []string
	for _, file := range files {
		data, err := fs.ReadFile(fsys, file)
		if err != nil {
			return nil, err
		}
		text := string(data)
		for i, seg := range splitDocuments(text) {
			node, err := parseDocument(text, seg)
			if err != nil {
				unparsable = append(unparsable, file)
				break
			}
			if documentDefines(node, destNamespace, res) {
				matches = append(matches, Location{File: file, Document: i})
			}
		}
	}

	switch len(matches) {
	case 1:
		return &matches[0], nil
	case 0:
		msg := fmt.Sprintf("no document under %q defines %s", appPath, describe(res))
		if len(unparsable) > 0 {
			msg += fmt.Sprintf(" (could not parse: %s)", strings.Join(unparsable, ", "))
		}
		return nil, fmt.Errorf("%w: %s", ErrNotFound, msg)
	}
	where := make([]string, len(matches))
	for i, m := range matches {
		where[i] = m.String()
	}
	return nil, fmt.Errorf("%w: %s is defined in %s", ErrAmbiguous, describe(res), strings.Join(where, ", "))
}

func describe(r ResourceID) string {
	s := r.Kind
	if r.Group != "" {
		s += "." + r.Group
	}
	if r.Namespace != "" {
		s += " " + r.Namespace + "/" + r.Name
	} else {
		s += " " + r.Name
	}
	return s
}

// documentDefines reports whether a parsed document is the resource.
func documentDefines(root *yaml.Node, destNamespace string, res ResourceID) bool {
	if root == nil || root.Kind != yaml.MappingNode {
		return false
	}
	apiVersion := scalarOf(mapGet(root, "apiVersion"))
	group := ""
	if i := strings.IndexByte(apiVersion, '/'); i >= 0 {
		group = apiVersion[:i]
	}
	meta := mapGet(root, "metadata")
	if scalarOf(mapGet(root, "kind")) != res.Kind || group != res.Group || scalarOf(mapGet(meta, "name")) != res.Name {
		return false
	}
	ns := scalarOf(mapGet(meta, "namespace"))
	if ns == "" && res.Namespace != "" {
		ns = destNamespace
	}
	return ns == res.Namespace
}

// manifestExtensions are the file extensions Argo CD reads as manifests
// (matched case-sensitively). Jsonnet is not read: its output cannot be
// traced back to a YAML document.
var manifestExtensions = map[string]bool{".yaml": true, ".yml": true, ".json": true}

// compileGlob compiles an include/exclude pattern like Argo CD does.
func compileGlob(pattern string) (glob.Glob, error) {
	if pattern == "" {
		return nil, nil
	}
	g, err := glob.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("%w: %q: %v", ErrInvalidGlob, pattern, err)
	}
	return g, nil
}

// manifestFiles lists the manifest files Argo CD would read for a Directory
// source, in a stable order.
func manifestFiles(fsys fs.FS, root string, opts DirectoryOptions) ([]string, error) {
	include, err := compileGlob(opts.Include)
	if err != nil {
		return nil, err
	}
	exclude, err := compileGlob(opts.Exclude)
	if err != nil {
		return nil, err
	}

	var files []string
	add := func(p string, d fs.DirEntry) {
		if d.IsDir() || d.Type()&fs.ModeSymlink != 0 || !manifestExtensions[path.Ext(p)] {
			return
		}
		rel := p
		if root != "." {
			rel = strings.TrimPrefix(p, root+"/")
		}
		if include != nil && !include.Match(rel) {
			return
		}
		if exclude != nil && exclude.Match(rel) {
			return
		}
		files = append(files, p)
	}

	if opts.Recurse {
		err := fs.WalkDir(fsys, root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			add(p, d)
			return nil
		})
		if err != nil {
			return nil, readError(root, err)
		}
	} else {
		entries, err := fs.ReadDir(fsys, root)
		if err != nil {
			return nil, readError(root, err)
		}
		for _, d := range entries {
			add(path.Join(root, d.Name()), d)
		}
	}
	sort.Strings(files)
	return files, nil
}

// readError reports a directory that cannot be listed. A directory that is not
// there is a verdict about the source (ErrNotFound); any other failure is only
// a failure to look, which carries no sentinel so callers retry it.
func readError(root string, err error) error {
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrInvalid) {
		return fmt.Errorf("%w: reading %q: %v", ErrNotFound, root, err)
	}
	return fmt.Errorf("reading %q: %w", root, err)
}
