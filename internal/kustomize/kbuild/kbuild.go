// Package kbuild is a stand-in for Argo CD's Kustomize render, for tests and
// the coverage suite: it runs the kustomize binary on the files. In production
// Argo CD renders.
package kbuild

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	"github.com/sserkanml/backflow/internal/kustomize"
	"github.com/sserkanml/backflow/internal/rendermap"
)

const (
	cmdEdit = "edit"
	cmdSet  = "set"
)

// Renderer runs `kustomize build` in the directory of an Application.
type Renderer struct {
	// Bin is the kustomize binary (v5.8.1 is what Argo CD uses).
	Bin string
	// Dir is the Application's path in the repository.
	Dir string
	// Options are applied the way Argo CD does, with `kustomize edit`.
	Options kustomize.Options
	// Slots bounds the processes running at once; nil means no bound.
	Slots chan struct{}
}

var _ rendermap.Renderer = (*Renderer)(nil)

// Render implements rendermap.Renderer. It never caches.
func (r *Renderer) Render(ctx context.Context, app rendermap.App, files map[string][]byte) ([]*unstructured.Unstructured, error) {
	bin, err := filepath.Abs(r.Bin)
	if err != nil {
		return nil, err
	}
	if r.Slots != nil {
		select {
		case r.Slots <- struct{}{}:
			defer func() { <-r.Slots }()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	root, err := os.MkdirTemp("", "backflow-kbuild-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(root) }()
	for name, data := range files {
		dst := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(dst, data, 0o600); err != nil {
			return nil, err
		}
	}
	dir := filepath.Join(root, filepath.FromSlash(r.Dir))
	if err := r.edit(ctx, bin, dir); err != nil {
		return nil, err
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, "build", dir)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w: kustomize build: %v: %s", rendermap.ErrRenderFailed, err, stderr.String())
	}
	return parse(stdout.Bytes(), app.DestNamespace)
}

// edit applies the Application's options to the copy, as Argo CD does.
func (r *Renderer) edit(ctx context.Context, bin, dir string) error {
	o := r.Options
	var cmds [][]string
	for _, img := range o.Images {
		cmds = append(cmds, []string{cmdEdit, cmdSet, "image", img})
	}
	if o.NamePrefix != "" {
		cmds = append(cmds, []string{cmdEdit, cmdSet, "nameprefix", o.NamePrefix})
	}
	if o.NameSuffix != "" {
		cmds = append(cmds, []string{cmdEdit, cmdSet, "namesuffix", o.NameSuffix})
	}
	for _, name := range slices.Sorted(keys(o.Replicas)) {
		cmds = append(cmds, []string{cmdEdit, cmdSet, "replicas", name + "=" + o.Replicas[name]})
	}
	for _, c := range cmds {
		cmd := exec.CommandContext(ctx, bin, c...)
		cmd.Dir = dir
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%w: kustomize %s: %v: %s", rendermap.ErrRenderFailed, strings.Join(c, " "), err, stderr.String())
		}
	}
	return nil
}

func keys(m map[string]string) func(yield func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// clusterScoped kinds keep an empty namespace, as Argo CD reports them.
var clusterScoped = map[string]bool{
	"Namespace": true, "ClusterRole": true, "ClusterRoleBinding": true, "CustomResourceDefinition": true,
	"PersistentVolume": true, "StorageClass": true, "PriorityClass": true,
}

func parse(data []byte, destNamespace string) ([]*unstructured.Unstructured, error) {
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
		return strings.Compare(a.GetKind()+"/"+a.GetName(), b.GetKind()+"/"+b.GetName())
	})
	return out, nil
}
