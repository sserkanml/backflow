package kustomize

import (
	"fmt"
	"maps"
	"path"
	"strings"

	"go.yaml.in/yaml/v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/sserkanml/backflow/api/v1alpha1"
	"github.com/sserkanml/backflow/internal/mapping"
	"github.com/sserkanml/backflow/internal/rendermap"
)

// The annotations kustomize adds with buildMetadata.
const (
	originAnnotation          = "config.kubernetes.io/origin"
	transformationsAnnotation = "alpha.config.kubernetes.io/transformations"
)

// WithBuildMetadata returns a copy of files whose kustomization of the
// Application has buildMetadata: [originAnnotations,
// transformerAnnotations]. Only that file differs: it is the files of the
// locate render. Nothing of it is written to Git, and verification renders use
// the unmodified files.
func (s *Source) WithBuildMetadata(files map[string][]byte) (map[string][]byte, error) {
	for _, n := range kustomizationNames {
		file := path.Join(s.Dir, n)
		content, ok := files[file]
		if !ok {
			continue
		}
		edited, err := mapping.Apply(file, content, 0, []v1alpha1.FieldChange{{
			Path: "/buildMetadata", Op: v1alpha1.OpAdd,
			Live: `["originAnnotations","transformerAnnotations"]`,
		}})
		if err != nil {
			return nil, fmt.Errorf("adding buildMetadata to %s: %w", file, err)
		}
		out := maps.Clone(files)
		out[file] = edited
		return out, nil
	}
	return nil, fmt.Errorf("%w: %s has no kustomization file", ErrNotClosed, displayDir(s.Dir))
}

// Origin says where kustomize got a rendered resource from.
type Origin struct {
	// File is the repository path of the file that declares the resource; ""
	// for a generated resource.
	File string
	// Index is the document of File (0 unless the file holds several).
	Index int
	// Generated is set for the output of a configMapGenerator or
	// secretGenerator; ConfiguredIn is then the kustomization that has it
	// (repository path).
	Generated    bool
	ConfiguredIn string
	// Transformers are the files that configured a transformer that touched
	// the resource, with its kind (PatchTransformer, ImageTagTransformer, ...).
	// They are hints to order candidates, never proof: a component's patch is
	// listed on resources it did not change.
	Transformers []Transformer
}

// Transformer is an entry of the transformations annotation.
type Transformer struct {
	// ConfiguredIn is the repository path of the kustomization (or file).
	ConfiguredIn string
	Kind         string
}

// ParseOrigins reads the origin and transformer annotations of the locate
// render and returns the resources without them, which are the objects to
// compare, together with where each came from.
func (s *Source) ParseOrigins(objs []*unstructured.Unstructured) ([]*unstructured.Unstructured, map[rendermap.ResourceKey]*Origin, error) {
	origins := map[rendermap.ResourceKey]*Origin{}
	clean := make([]*unstructured.Unstructured, 0, len(objs))
	for _, o := range objs {
		o = o.DeepCopy()
		ann := o.GetAnnotations()
		og, err := s.parseOrigin(ann[originAnnotation], ann[transformationsAnnotation])
		if err != nil {
			return nil, nil, fmt.Errorf("%s %s: %w", o.GetKind(), o.GetName(), err)
		}
		StripMetadata(o)
		gv := o.GroupVersionKind()
		origins[rendermap.ResourceKey{Group: gv.Group, Kind: o.GetKind(), Namespace: o.GetNamespace(), Name: o.GetName()}] = og
		clean = append(clean, o)
	}
	return clean, origins, nil
}

// StripMetadata removes the annotations the locate render adds, so the object
// compares with one from a render without them.
func StripMetadata(o *unstructured.Unstructured) {
	ann := o.GetAnnotations()
	if _, ok := ann[originAnnotation]; !ok {
		if _, ok := ann[transformationsAnnotation]; !ok {
			return
		}
	}
	delete(ann, originAnnotation)
	delete(ann, transformationsAnnotation)
	if len(ann) == 0 {
		ann = nil
	}
	o.SetAnnotations(ann)
}

func (s *Source) parseOrigin(origin, transformations string) (*Origin, error) {
	og := &Origin{}
	if origin != "" {
		var o struct {
			Path         string `yaml:"path"`
			Index        int    `yaml:"index"`
			ConfiguredIn string `yaml:"configuredIn"`
			ConfiguredBy struct {
				Kind string `yaml:"kind"`
			} `yaml:"configuredBy"`
		}
		if err := yaml.Unmarshal([]byte(origin), &o); err != nil {
			return nil, fmt.Errorf("origin annotation: %w", err)
		}
		switch {
		case o.Path != "":
			file, err := s.Join(o.Path)
			if err != nil {
				return nil, fmt.Errorf("origin %q: %w", o.Path, err)
			}
			og.File, og.Index = file, o.Index
		case o.ConfiguredIn != "":
			og.Generated = strings.HasSuffix(o.ConfiguredBy.Kind, "Generator")
			in, err := s.Join(o.ConfiguredIn)
			if err != nil {
				return nil, fmt.Errorf("origin %q: %w", o.ConfiguredIn, err)
			}
			og.ConfiguredIn = in
		}
	}
	if transformations != "" {
		var ts []struct {
			ConfiguredIn string `yaml:"configuredIn"`
			ConfiguredBy struct {
				Kind string `yaml:"kind"`
			} `yaml:"configuredBy"`
		}
		if err := yaml.Unmarshal([]byte(transformations), &ts); err != nil {
			return nil, fmt.Errorf("transformations annotation: %w", err)
		}
		for _, t := range ts {
			in, err := s.Join(t.ConfiguredIn)
			if err != nil {
				continue
			}
			og.Transformers = append(og.Transformers, Transformer{ConfiguredIn: in, Kind: t.ConfiguredBy.Kind})
		}
	}
	return og, nil
}
