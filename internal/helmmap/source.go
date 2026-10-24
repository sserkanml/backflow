package helmmap

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/sserkanml/backflow/internal/rendermap"

	"github.com/bmatcuk/doublestar/v4"
)

// Errors a Source can be rejected with. Each means the value files cannot be
// determined with certainty, so the drift stays unmapped.
var (
	// ErrOutsideRepository: a path leaves the repository, or is the root.
	ErrOutsideRepository = errors.New("path is outside the repository")
	// ErrMissingValueFile: a value file does not exist and the Application
	// does not set ignoreMissingValueFiles.
	ErrMissingValueFile = errors.New("value file does not exist")
	// ErrUnsupportedValueFile: a value file is a reference to another source
	// ($name/...) or a URL scheme Argo CD does not allow.
	ErrUnsupportedValueFile = errors.New("unsupported value file")
	// ErrSymlinkedValueFile: a value file is a symbolic link or lies under
	// one. Argo CD follows it; this package does not, so the file cannot be
	// told apart from a missing one without saying so.
	ErrSymlinkedValueFile = errors.New("value file is a symbolic link or under one")
	// ErrInvalidInline: helm.values, helm.valuesObject or a parameter name
	// cannot be parsed.
	ErrInvalidInline = errors.New("invalid inline Helm values")
)

// Layer names of the settings on an Application that sit on top of the value
// files, in the order Argo CD passes them to Helm: values or valuesObject (one
// extra --values file, last), then parameters (--set, --set-string) and
// fileParameters (--set-file).
const (
	LayerValues         = "helm.values"
	LayerValuesObject   = "helm.valuesObject"
	LayerParameters     = "helm.parameters"
	LayerFileParameters = "helm.fileParameters"
)

// Parameter is one entry of helm.parameters.
type Parameter struct {
	Name  string
	Value string
	// ForceString is set for --set-string.
	ForceString bool
}

// FileParameter is one entry of helm.fileParameters (--set-file). The file's
// content is the value.
type FileParameter struct {
	Name string
	Path string
}

// Spec is the Helm part of an Application source.
type Spec struct {
	// Path is spec.source.path: the chart directory in the repository.
	Path string
	// ValueFiles is helm.valueFiles, in order.
	ValueFiles []string
	// Values is helm.values, a YAML string.
	Values string
	// ValuesObject is helm.valuesObject as JSON.
	ValuesObject []byte
	Parameters   []Parameter
	// FileParameters is helm.fileParameters.
	FileParameters          []FileParameter
	IgnoreMissingValueFiles bool
	// External is set when the chart is not in the repository at all: it comes
	// from a Helm repository or a registry, and Path is not read. Its own
	// defaults are given instead.
	External *ExternalChart
	// Env holds the variables value file paths may use, the $ARGOCD_APP_*
	// set Argo CD defines. ARGOCD_APP_SOURCE_PATH defaults to Path.
	Env map[string]string
}

// ExternalChart describes a chart that Argo CD pulls from a Helm repository or a
// registry, as far as Backflow can know it without the chart's files.
type ExternalChart struct {
	// Name and Version name the chart, for messages and for the name of its
	// defaults.
	Name, Version string
	// Values is the text of the chart's own values.yaml (what Argo CD returns
	// for the chart; comments and all).
	Values []byte
	// UnreadDependencies are the dependencies whose defaults are not available:
	// Argo CD does not return the values.yaml of a subchart.
	UnreadDependencies []string
}

// InlineValue is what one inline layer sets at a key path.
type InlineValue struct {
	Layer string
	Path  rendermap.KeyPath
	// Value is the rendered scalar, when HasValue is set. Lists, nulls and
	// file parameters have none.
	Value    string
	HasValue bool
}

// Source is the resolved Helm source of an Application: which files feed the
// chart and which key paths the inline settings control.
type Source struct {
	// ChartDir is the chart directory, relative to the repository root. Empty
	// when the chart is External.
	ChartDir string
	// External is the chart that is not in the repository, if it is not.
	External *ExternalChart
	// ChartValues is the chart's own values.yaml. Its defaults are never
	// edited; an Application overrides them instead.
	ChartValues string
	// ValueFiles are the value files in the order Helm merges them (a later
	// file wins), as repository paths. Missing files that the Application
	// ignores are left out.
	ValueFiles []string
	// RemoteValueFiles are http(s) value files. They cannot be edited, and
	// they are not read, so a value may come from one of them.
	RemoteValueFiles []string
	// FileParameterFiles are the files helm.fileParameters read (--set-file),
	// as repository paths. They are inputs of the render and are never edited.
	FileParameterFiles []string
	// Inline lists, in precedence order (a later entry wins), what
	// helm.values, helm.valuesObject, helm.parameters and
	// helm.fileParameters set.
	Inline []InlineValue
	// OverrideTarget is the value file an override of a chart default is added
	// to (spec.helm.targetValuesFile of the policy): a repository path, a path
	// relative to the chart directory, or as the Application writes it. Empty
	// means the last value file.
	OverrideTarget string
}

// overrideFile returns the value file that receives an override. When it
// cannot be one, reason says why.
func (s *Source) overrideFile() (file, reason string) {
	if len(s.ValueFiles) == 0 {
		return "", "the Application has no value file to add an override to"
	}
	if s.OverrideTarget == "" {
		return s.ValueFiles[len(s.ValueFiles)-1], ""
	}
	t := strings.TrimPrefix(s.OverrideTarget, "/")
	for _, vf := range s.ValueFiles {
		if vf == t || vf == path.Join(s.ChartDir, t) || vf == path.Clean(t) {
			return vf, ""
		}
	}
	return "", fmt.Sprintf("spec.helm.targetValuesFile %q is not one of the Application's value files (%s)",
		s.OverrideTarget, strings.Join(s.ValueFiles, ", "))
}

// NewSource resolves the Helm source of an Application against the
// repository at fsys.
//
// Value files follow Argo CD 3.5.3, reposerver/repository/repository.go
// getResolvedValueFiles and util/io/path/resolved.go ResolveValueFilePathOrUrl:
//   - $ARGOCD_APP_* variables are expanded (Env.Envsubst, os.Expand with $$
//     for a literal $; an undefined variable is empty);
//   - a URL with a scheme is a remote file, allowed for http and https;
//   - a path starting with "/" is relative to the repository root, any other
//     is relative to the chart directory, and the result must stay inside the
//     repository and not be the root itself;
//   - a path with *, ? or [ is a glob (doublestar, ** included) whose matches
//     keep lexical order; a file that is also listed without a glob keeps the
//     position of that explicit entry; duplicates are dropped;
//   - a missing file or a glob without a match is an error unless
//     ignoreMissingValueFiles is set, in which case it is skipped.
func NewSource(fsys fs.FS, spec Spec) (*Source, error) {
	var src *Source
	if ext := spec.External; ext != nil {
		src = &Source{External: ext, ChartValues: fmt.Sprintf("%s@%s/values.yaml", ext.Name, ext.Version)}
	} else {
		chart, err := cleanRepoPath(spec.Path, true)
		if err != nil {
			return nil, fmt.Errorf("chart path %q: %w", spec.Path, err)
		}
		src = &Source{ChartDir: chart, ChartValues: path.Join(chart, "values.yaml")}
	}

	env := map[string]string{"ARGOCD_APP_SOURCE_PATH": spec.Path}
	maps.Copy(env, spec.Env)
	if err := src.resolveValueFiles(fsys, spec, env); err != nil {
		return nil, err
	}
	if err := src.resolveFileParameters(fsys, spec, env); err != nil {
		return nil, err
	}
	if err := src.parseInline(spec); err != nil {
		return nil, err
	}
	return src, nil
}

// cleanRepoPath makes p a clean path relative to the repository root. The root
// itself is only allowed when allowRoot is set.
func cleanRepoPath(p string, allowRoot bool) (string, error) {
	c := path.Clean(strings.TrimPrefix(p, "/"))
	if c == ".." || strings.HasPrefix(c, "../") {
		return "", ErrOutsideRepository
	}
	if c == "." && !allowRoot {
		return "", ErrOutsideRepository
	}
	return c, nil
}

func isGlob(p string) bool { return strings.ContainsAny(p, "*?[") }

// refPrefix matches a leading $name, which Argo CD reads as a reference to a
// source with that ref when the Application has one.
var refPrefix = regexp.MustCompile(`^\$\{?([A-Za-z_][A-Za-z0-9_]*)`)

// resolvedFile is one value file entry after expansion of variables and URL
// and path resolution.
type resolvedFile struct {
	path   string // repository path, or URL when remote
	remote bool
}

func (s *Source) resolve(raw string, env map[string]string) (resolvedFile, error) {
	if m := refPrefix.FindStringSubmatch(raw); m != nil {
		if _, ok := env[m[1]]; !ok {
			return resolvedFile{}, fmt.Errorf("%w: %q refers to another source", ErrUnsupportedValueFile, raw)
		}
	}
	expanded := os.Expand(raw, func(name string) string {
		if name == "$" {
			return "$"
		}
		return env[name]
	})
	if u, err := url.Parse(expanded); err == nil && u.Scheme != "" {
		switch u.Scheme {
		case "http", "https":
			return resolvedFile{path: expanded, remote: true}, nil
		}
		return resolvedFile{}, fmt.Errorf("%w: the URL scheme %q of %q is not allowed", ErrUnsupportedValueFile, u.Scheme, raw)
	}
	var rel string
	if strings.HasPrefix(expanded, "/") {
		rel = expanded
	} else {
		rel = path.Join(s.ChartDir, expanded)
	}
	p, err := cleanRepoPath(rel, false)
	if err != nil {
		return resolvedFile{}, fmt.Errorf("value file %q: %w", raw, err)
	}
	return resolvedFile{path: p}, nil
}

func (s *Source) resolveValueFiles(fsys fs.FS, spec Spec, env map[string]string) error {
	// Explicit entries claim their own position; a glob leaves them out.
	explicit := map[string]bool{}
	for _, raw := range spec.ValueFiles {
		if r, err := s.resolve(raw, env); err == nil && !r.remote && !isGlob(r.path) {
			explicit[r.path] = true
		}
	}
	seen := map[string]bool{}
	add := func(p string) {
		if !seen[p] {
			seen[p] = true
			s.ValueFiles = append(s.ValueFiles, p)
		}
	}
	for _, raw := range spec.ValueFiles {
		r, err := s.resolve(raw, env)
		if err != nil {
			return err
		}
		switch {
		case r.remote:
			s.RemoteValueFiles = append(s.RemoteValueFiles, r.path)
		case isGlob(r.path):
			if err := checkGlobSymlinks(fsys, r.path); err != nil {
				return fmt.Errorf("value file glob %q: %w", raw, err)
			}
			matches, err := doublestar.Glob(fsys, r.path, doublestar.WithFilesOnly())
			if err != nil {
				return fmt.Errorf("value file glob %q: %w", raw, err)
			}
			if len(matches) == 0 {
				if spec.IgnoreMissingValueFiles {
					continue
				}
				return fmt.Errorf("%w: the glob %q matches no file", ErrMissingValueFile, raw)
			}
			slices.Sort(matches)
			for _, m := range matches {
				if !explicit[m] {
					add(m)
				}
			}
		default:
			if err := checkNoSymlink(fsys, r.path); err != nil {
				return fmt.Errorf("value file %q: %w", raw, err)
			}
			if _, err := fs.Stat(fsys, r.path); err != nil {
				if !errors.Is(err, fs.ErrNotExist) {
					return fmt.Errorf("value file %q: %w", raw, err)
				}
				if spec.IgnoreMissingValueFiles {
					continue
				}
				return fmt.Errorf("%w: %s", ErrMissingValueFile, r.path)
			}
			add(r.path)
		}
	}
	return nil
}

// resolveFileParameters finds the files of helm.fileParameters. They must
// exist: Helm fails without them.
func (s *Source) resolveFileParameters(fsys fs.FS, spec Spec, env map[string]string) error {
	for _, fp := range spec.FileParameters {
		r, err := s.resolve(fp.Path, env)
		if err != nil {
			return fmt.Errorf("file parameter %q: %w", fp.Name, err)
		}
		if r.remote {
			return fmt.Errorf("%w: file parameter %q is a URL", ErrUnsupportedValueFile, fp.Name)
		}
		if err := checkNoSymlink(fsys, r.path); err != nil {
			return fmt.Errorf("file parameter %q: %w", fp.Name, err)
		}
		if _, err := fs.Stat(fsys, r.path); err != nil {
			return fmt.Errorf("%w: file parameter %q: %s", ErrMissingValueFile, fp.Name, r.path)
		}
		if !slices.Contains(s.FileParameterFiles, r.path) {
			s.FileParameterFiles = append(s.FileParameterFiles, r.path)
		}
	}
	return nil
}

// parseInline records which key paths the inline layers set. Only the paths
// (and scalar values) are needed, never to rebuild the values.
func (s *Source) parseInline(spec Spec) error {
	// Argo CD passes valuesObject when it is set, and values otherwise.
	var layer string
	var doc []byte
	switch {
	case len(spec.ValuesObject) > 0 && string(spec.ValuesObject) != "null":
		layer, doc = LayerValuesObject, spec.ValuesObject
	case strings.TrimSpace(spec.Values) != "":
		layer, doc = LayerValues, []byte(spec.Values)
	}
	if doc != nil {
		if layer == LayerValuesObject && !json.Valid(doc) {
			return fmt.Errorf("%w: %s is not JSON", ErrInvalidInline, layer)
		}
		defs, err := parseValues(doc)
		if err != nil {
			return fmt.Errorf("%w: %s: %v", ErrInvalidInline, layer, err)
		}
		for _, d := range defs {
			if d.kind == defList {
				// A list is one unit; its items add nothing.
				s.Inline = append(s.Inline, InlineValue{Layer: layer, Path: d.path})
				continue
			}
			s.Inline = append(s.Inline, InlineValue{Layer: layer, Path: d.path, Value: d.value, HasValue: d.kind == defScalar})
		}
		s.Inline = dropListItems(s.Inline)
	}

	// Helm applies --set, then --set-string, then --set-file.
	for _, forceString := range []bool{false, true} {
		for _, p := range spec.Parameters {
			if p.ForceString != forceString {
				continue
			}
			kp, err := parseStrvals(p.Name)
			if err != nil {
				return fmt.Errorf("%w: %v", ErrInvalidInline, err)
			}
			s.Inline = append(s.Inline, InlineValue{Layer: LayerParameters, Path: kp, Value: p.Value, HasValue: true})
		}
	}
	for _, p := range spec.FileParameters {
		kp, err := parseStrvals(p.Name)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidInline, err)
		}
		s.Inline = append(s.Inline, InlineValue{Layer: LayerFileParameters, Path: kp})
	}
	return nil
}

// dropListItems removes the entries below a list entry of the same layer: the
// list is replaced as a whole, so only its own path matters.
func dropListItems(in []InlineValue) []InlineValue {
	var lists []InlineValue
	out := in[:0:0]
	for _, v := range in {
		below := false
		for _, l := range lists {
			if l.Layer == v.Layer && v.Path.HasPrefix(l.Path) && !v.Path.Equal(l.Path) {
				below = true
				break
			}
		}
		if below {
			continue
		}
		if !v.HasValue {
			lists = append(lists, v)
		}
		out = append(out, v)
	}
	return out
}

// lstat describes a path without following a symbolic link where the file
// system can tell. A file system without symbolic links of its own, such as
// the view of a Git commit, reports a link as a file with the symlink mode.
func lstat(fsys fs.FS, name string) (fs.FileInfo, error) {
	if _, ok := fsys.(fs.ReadLinkFS); ok {
		return fs.Lstat(fsys, name)
	}
	return fs.Stat(fsys, name)
}

// checkNoSymlink fails when name, or a directory above it, is a symbolic
// link. A part that does not exist ends the check: the caller reports a
// missing file.
func checkNoSymlink(fsys fs.FS, name string) error {
	parts := strings.Split(name, "/")
	for i := range parts {
		prefix := path.Join(parts[:i+1]...)
		info, err := lstat(fsys, prefix)
		if err != nil {
			return nil //nolint:nilerr // missing is reported by the caller
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s", ErrSymlinkedValueFile, prefix)
		}
	}
	return nil
}

// checkGlobSymlinks fails when the glob walks through, or ends at, a symbolic
// link. Each leading part of the pattern is matched on its own, so a linked
// directory the full pattern would not enter is still found.
func checkGlobSymlinks(fsys fs.FS, pattern string) error {
	parts := strings.Split(pattern, "/")
	for i := range parts {
		matches, err := doublestar.Glob(fsys, path.Join(parts[:i+1]...))
		if err != nil {
			return err
		}
		for _, m := range matches {
			if info, err := lstat(fsys, m); err == nil && info.Mode()&fs.ModeSymlink != 0 {
				return fmt.Errorf("%w: %s", ErrSymlinkedValueFile, m)
			}
		}
	}
	return nil
}
