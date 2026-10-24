package kustomize

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/sserkanml/backflow/api/v1alpha1"
	"github.com/sserkanml/backflow/internal/rendermap"
)

// Located is the result of the locate render: the resources without the
// build metadata, and where each came from.
type Located struct {
	Objects map[rendermap.ResourceKey]*unstructured.Unstructured
	Origins map[rendermap.ResourceKey]*Origin
}

// Locate renders a copy of files whose kustomization has build metadata and
// reads where each resource comes from. The origins only order and seed
// candidates; every edit is verified by a render of the unmodified tree.
func Locate(ctx context.Context, r rendermap.Renderer, src *Source, files map[string][]byte, app rendermap.App) (*Located, error) {
	withMeta, err := src.WithBuildMetadata(files)
	if err != nil {
		return nil, err
	}
	objs, err := r.Render(ctx, app, withMeta)
	if err != nil {
		return nil, fmt.Errorf("locate render: %w", err)
	}
	clean, origins, err := src.ParseOrigins(objs)
	if err != nil {
		return nil, err
	}
	l := &Located{Objects: map[rendermap.ResourceKey]*unstructured.Unstructured{}, Origins: origins}
	for _, o := range clean {
		gv := o.GroupVersionKind()
		l.Objects[rendermap.ResourceKey{Group: gv.Group, Kind: o.GetKind(), Namespace: o.GetNamespace(), Name: o.GetName()}] = o
	}
	return l, nil
}

// Input is a proposal to map: the changes to one resource of a Kustomize
// Application.
type Input struct {
	App      rendermap.App
	Resource rendermap.ResourceKey
	Changes  []v1alpha1.FieldChange
	Source   *Source
	// Files are the unmodified files of the render (CollectFiles).
	Files   map[string][]byte
	Located *Located
	Policy  Policy
}

// Verify decides whether the changes can be traced back to edits of the
// kustomization files (see rendermap.Verify).
func Verify(ctx context.Context, r rendermap.Renderer, in Input, opts rendermap.Options) (*rendermap.Outcome, error) {
	obj := in.Located.Objects[in.Resource]
	if obj == nil {
		return nil, fmt.Errorf("the locate render has no %s %s", in.Resource.Kind, in.Resource.Name)
	}
	fields, err := FieldsFor(in.Source, in.Files, in.Located.Origins[in.Resource], obj, in.Policy, in.Changes)
	if err != nil {
		return nil, err
	}
	return rendermap.Verify(ctx, r, rendermap.Input{
		App: in.App, Resource: in.Resource, Fields: fields, Files: in.Files,
		Provider: in.Source, Writer: Writer{},
	}, opts)
}
