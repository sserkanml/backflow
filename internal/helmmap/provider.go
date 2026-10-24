package helmmap

import (
	"fmt"

	"go.yaml.in/yaml/v3"

	"github.com/sserkanml/backflow/internal/rendermap"
)

var _ rendermap.Provider = (*Source)(nil)

// Layers decodes the layers of values, lowest precedence first (the same
// layers as find reads): the defaults of the dependencies in the files, the
// chart's own values.yaml, then the value files in order.
func (s *Source) Layers(files map[string][]byte) ([]rendermap.Layer, error) {
	decode := func(name string, data []byte) (map[string]any, error) {
		var m map[string]any
		if err := yaml.Unmarshal(data, &m); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if m == nil {
			m = map[string]any{}
		}
		out, _ := rendermap.ToJSONValue(m).(map[string]any)
		return out, nil
	}
	var layers []rendermap.Layer
	deps, _ := s.dependencyLayers(files)
	for _, dd := range deps {
		m, err := decode(dd.file, dd.data)
		if err != nil {
			return nil, err
		}
		moved := map[string]any{}
		for k, v := range m {
			if k == "global" {
				moved[k] = v
			} else {
				sub, _ := moved[dd.prefix].(map[string]any)
				if sub == nil {
					sub = map[string]any{}
					moved[dd.prefix] = sub
				}
				sub[k] = v
			}
		}
		layers = append(layers, rendermap.Layer{Name: dd.file, Tree: moved, IsDefault: true, Prefix: dd.prefix})
	}
	if data, ok := files[s.ChartValues]; ok {
		m, err := decode(s.ChartValues, data)
		if err != nil {
			return nil, err
		}
		layers = append(layers, rendermap.Layer{Name: s.ChartValues, Tree: m, IsDefault: true})
	}
	for _, name := range s.ValueFiles {
		data, ok := files[name]
		if !ok {
			return nil, fmt.Errorf("value file %s was not provided", name)
		}
		m, err := decode(name, data)
		if err != nil {
			return nil, err
		}
		layers = append(layers, rendermap.Layer{Name: name, Tree: m})
	}
	return layers, nil
}

// OverrideFile implements rendermap.Provider: the last value file receives the
// keys that override a chart default.
func (s *Source) OverrideFile() (file, reason string) { return s.overrideFile() }

// Controlled implements rendermap.Provider: an inline setting of the Application
// decides the key, so editing a value file would not change the render.
func (s *Source) Controlled(key rendermap.KeyPath) bool {
	_, ok := s.inlineFor(key)
	return ok
}

// References implements rendermap.Provider: what the chart's templates read.
func (s *Source) References(files map[string][]byte) rendermap.References {
	return scanValueRefs(files)
}
