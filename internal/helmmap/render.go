package helmmap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/sserkanml/backflow/internal/argocd"
)

// Errors of collecting and rendering.
var (
	// ErrRenderFailed means the chart does not render with the given files.
	ErrRenderFailed = errors.New("the chart does not render")
	// ErrFilesTooLarge means the files do not fit the limits of Argo CD's
	// repo server.
	ErrFilesTooLarge = errors.New("the files are too large to render with")
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

// App identifies the Application to render.
type App struct {
	Name      string
	Namespace string
	Project   string
	// DestNamespace is spec.destination.namespace. Namespaced resources that
	// a chart leaves without a namespace get it, as Argo CD does when it
	// applies them.
	DestNamespace string
	// ReleaseName is the Helm release name: helm.releaseName, else the
	// Application's name. Argo CD renders with the Application's own
	// settings, so ArgoRenderer does not send it; a renderer that runs Helm
	// itself needs it.
	ReleaseName string
	// Resources are the resources Argo CD lists in the Application's
	// status.resources. They decide which rendered resources are namespaced.
	Resources []ListedResource
}

// ListedResource is an entry of an Application's status.resources. Namespace
// is empty for a cluster-scoped resource.
type ListedResource struct {
	Group, Kind, Namespace, Name string
}

// Renderer renders an Application's source from the given files, which are
// laid out like the repository (see CollectFiles) and take the place of the
// files in Git.
type Renderer interface {
	Render(ctx context.Context, app App, files map[string][]byte) ([]*unstructured.Unstructured, error)
}

// ManifestsClient is the part of the Argo CD client the renderer needs.
type ManifestsClient interface {
	GetManifestsWithFiles(ctx context.Context, q argocd.ManifestsQuery) (*argocd.ManifestsResponse, error)
}

// ArgoRenderer renders through Argo CD's API server, so the chart is rendered
// by the Helm version and the settings of the Argo CD that deploys it.
type ArgoRenderer struct {
	Client ManifestsClient
}

var _ Renderer = (*ArgoRenderer)(nil)

// CloseIdleConnections closes the connections of the client, when it keeps any.
func (r *ArgoRenderer) CloseIdleConnections() {
	if c, ok := r.Client.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

// Render implements Renderer. Errors from Argo CD that a retry can fix
// (argocd.ErrUnauthorized, ErrNotFound, ErrUnreachable) are returned as they
// are; a chart that does not render wraps ErrRenderFailed, and files over
// the limits wrap ErrFilesTooLarge.
func (r *ArgoRenderer) Render(ctx context.Context, app App, files map[string][]byte) ([]*unstructured.Unstructured, error) {
	resp, err := r.Client.GetManifestsWithFiles(ctx, argocd.ManifestsQuery{
		AppName: app.Name, AppNamespace: app.Namespace, Project: app.Project, Files: files,
	})
	switch {
	case errors.Is(err, argocd.ErrRenderFailed):
		return nil, fmt.Errorf("%w: %w", ErrRenderFailed, err)
	case errors.Is(err, argocd.ErrArchiveTooLarge):
		return nil, fmt.Errorf("%w: %w", ErrFilesTooLarge, err)
	case err != nil:
		return nil, err
	}
	out := make([]*unstructured.Unstructured, 0, len(resp.Manifests))
	for i, m := range resp.Manifests {
		var obj map[string]any
		if err := json.Unmarshal([]byte(m), &obj); err != nil {
			return nil, fmt.Errorf("manifest %d is not JSON: %w", i, err)
		}
		if len(obj) == 0 {
			continue
		}
		u := &unstructured.Unstructured{Object: obj}
		if u.GetKind() == "" || u.GetName() == "" {
			return nil, fmt.Errorf("manifest %d has no kind or name", i)
		}
		u.SetNamespace(namespaceFor(app, u))
		out = append(out, u)
	}
	return out, nil
}

// namespaceFor returns the namespace a rendered resource has in the cluster.
// Whether it is namespaced is read from the Application's status.resources,
// which lists every resource Argo CD manages with its namespace (empty for a
// cluster-scoped one): the render itself leaves a namespaced resource without
// a namespace when the chart does not set one, and Argo CD adds the
// destination namespace when it applies it.
//
// A resource that Argo CD does not list yet, because only the candidate
// render has it, cannot be looked up. For it the manifest's own namespace
// stands, and without one the destination namespace is used. That is right
// for namespaced kinds, which are nearly all of them; a new cluster-scoped
// resource would be given a namespace it does not have, which at worst makes
// it look like a different resource and so a change, never a match.
func namespaceFor(app App, u *unstructured.Unstructured) string {
	group := u.GroupVersionKind().Group
	var listed []string
	for _, r := range app.Resources {
		if r.Group == group && r.Kind == u.GetKind() && r.Name == u.GetName() {
			if !slices.Contains(listed, r.Namespace) {
				listed = append(listed, r.Namespace)
			}
		}
	}
	switch {
	case len(listed) == 1:
		return listed[0]
	case len(listed) > 1 && slices.Contains(listed, u.GetNamespace()):
		return u.GetNamespace()
	case u.GetNamespace() != "":
		return u.GetNamespace()
	}
	return app.DestNamespace
}

// CollectFiles reads what a render of src needs from the repository at fsys,
// as a map from repository path to content: the whole chart directory, every
// directory a file:// dependency points to (recursively, and inside the
// repository), the value files, and the files of helm.fileParameters. The
// result is the unmodified Git state; edits are applied to a copy.
func CollectFiles(fsys fs.FS, src *Source) (map[string][]byte, error) {
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
