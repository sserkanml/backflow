//go:build coverage

package coverage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	"github.com/sserkanml/backflow/internal/rendermap"
)

// clusterScoped kinds keep an empty namespace, as Argo CD reports them.
var clusterScoped = map[string]bool{
	"Namespace": true, "ClusterRole": true, "ClusterRoleBinding": true, "CustomResourceDefinition": true,
	"PersistentVolume": true, "StorageClass": true, "PriorityClass": true,
	"MutatingWebhookConfiguration": true, "ValidatingWebhookConfiguration": true,
}

// helmRenderer is a test-only rendermap.Renderer that shells out to `helm
// template`. In production Argo CD renders; this only stands in for it so the
// suite needs no cluster. It never caches: Verify renders the unmodified files
// twice to find fields that change on every render (generated certificates,
// passwords), and a cache would hide them.
type helmRenderer struct {
	chartDir   string   // repository path of the chart
	valueFiles []string // repository paths, in order
	release    string
	namespace  string
	// slots bounds the helm processes running at once, across all renderers.
	slots chan struct{}
}

var _ rendermap.Renderer = (*helmRenderer)(nil)

// Render implements rendermap.Renderer.
func (r *helmRenderer) Render(ctx context.Context, _ rendermap.App, files map[string][]byte) ([]*unstructured.Unstructured, error) {
	select {
	case r.slots <- struct{}{}:
		defer func() { <-r.slots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return r.helm(ctx, files)
}

func (r *helmRenderer) helm(ctx context.Context, files map[string][]byte) ([]*unstructured.Unstructured, error) {
	dir, err := os.MkdirTemp("", "backflow-coverage-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	for name, data := range files {
		dst := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(dst, data, 0o600); err != nil {
			return nil, err
		}
	}
	args := []string{"template", r.release, filepath.Join(dir, filepath.FromSlash(r.chartDir)), "--namespace", r.namespace, "--skip-tests"}
	for _, vf := range r.valueFiles {
		args = append(args, "-f", filepath.Join(dir, filepath.FromSlash(vf)))
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "helm", args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w: helm template: %v: %s", rendermap.ErrRenderFailed, err, stderr.String())
	}
	return parseManifests(stdout.Bytes(), r.namespace)
}

// parseManifests splits the output of helm template into objects. A namespaced
// object without a namespace gets the destination namespace, as Argo CD does.
func parseManifests(data []byte, destNamespace string) ([]*unstructured.Unstructured, error) {
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	var out []*unstructured.Unstructured
	for {
		var obj map[string]any
		if err := dec.Decode(&obj); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		if len(obj) == 0 {
			continue
		}
		u := &unstructured.Unstructured{Object: obj}
		if u.GetKind() == "" || u.GetName() == "" {
			continue
		}
		if u.GetNamespace() == "" && !clusterScoped[u.GetKind()] {
			u.SetNamespace(destNamespace)
		}
		out = append(out, u)
	}
	slices.SortStableFunc(out, func(a, b *unstructured.Unstructured) int {
		if a.GetKind() != b.GetKind() {
			if a.GetKind() < b.GetKind() {
				return -1
			}
			return 1
		}
		if a.GetName() < b.GetName() {
			return -1
		} else if a.GetName() > b.GetName() {
			return 1
		}
		return 0
	})
	return out, nil
}
