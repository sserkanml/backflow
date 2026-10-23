// Package rendermap is the part of mapping a drifted field back to Git that does
// not depend on the kind of source: it renders edited copies of the source files
// through Argo CD and accepts an edit only when the render reproduces the drift
// exactly. A source kind (Helm, Kustomize) supplies the candidates and the file
// writer.
package rendermap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/sserkanml/backflow/internal/argocd"
)

// Errors of rendering.
var (
	// ErrRenderFailed means the chart does not render with the given files.
	ErrRenderFailed = errors.New("the chart does not render")
	// ErrFilesTooLarge means the files do not fit the limits of Argo CD's
	// repo server.
	ErrFilesTooLarge = errors.New("the files are too large to render with")
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
// laid out like the repository and take the place of the
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
