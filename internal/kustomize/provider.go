package kustomize

import "github.com/sserkanml/backflow/internal/rendermap"

var _ rendermap.Provider = (*Source)(nil)

// Layers implements rendermap.Provider. A kustomization has no layers of keys:
// the candidates of a field are made by FieldsFor.
func (s *Source) Layers(map[string][]byte) ([]rendermap.Layer, error) { return nil, nil }

// OverrideFile implements rendermap.Provider. Nothing receives an overriding
// key; a new patch is a tier of candidates instead.
func (s *Source) OverrideFile() (file, reason string) {
	return "", "a kustomization has no file that receives overrides; a new patch is tried as the last resort"
}

// Controlled implements rendermap.Provider.
func (s *Source) Controlled(rendermap.KeyPath) bool { return false }

// References implements rendermap.Provider: every key may feed a field.
func (s *Source) References(map[string][]byte) rendermap.References { return anything{} }

type anything struct{}

func (anything) Mentions(rendermap.KeyPath) bool { return true }
