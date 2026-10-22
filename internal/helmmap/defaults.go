package helmmap

import (
	"path"
	"strings"

	"go.yaml.in/yaml/v3"
)

// depDefaults is the values.yaml of a dependency of the chart, with the key
// prefix its values have in the parent chart: the dependency's alias, or its
// name. Its global.* keys keep their place.
type depDefaults struct {
	file   string
	prefix string
}

// dependencyDefaults lists the values.yaml files of the dependencies that are
// part of the files of the render: a subchart unpacked under charts/, or a
// file:// dependency. A dependency that is a packaged .tgz is not read.
func dependencyDefaults(files map[string][]byte, chartDir string) []depDefaults {
	var out []depDefaults
	seen := map[string]bool{}
	for _, name := range []string{chartYAMLName, requirementsName} {
		data, ok := files[path.Join(chartDir, name)]
		if !ok {
			continue
		}
		var doc struct {
			Dependencies []struct {
				Name       string `yaml:"name"`
				Alias      string `yaml:"alias"`
				Repository string `yaml:"repository"`
			} `yaml:"dependencies"`
		}
		if yaml.Unmarshal(data, &doc) != nil {
			continue
		}
		for _, d := range doc.Dependencies {
			dir := path.Join(chartDir, "charts", d.Name)
			if rel, ok := strings.CutPrefix(d.Repository, "file://"); ok {
				dir = path.Join(chartDir, rel)
			}
			file := path.Join(dir, "values.yaml")
			if _, ok := files[file]; !ok || seen[file+"\x00"+d.Alias] {
				continue
			}
			seen[file+"\x00"+d.Alias] = true
			prefix := d.Name
			if d.Alias != "" {
				prefix = d.Alias
			}
			out = append(out, depDefaults{file: file, prefix: prefix})
		}
	}
	return out
}

// moveInto puts the definitions of a dependency's values.yaml into the
// namespace of the parent chart: under the dependency's name or alias, except
// global, which is shared.
func moveInto(defs []definition, prefix string) []definition {
	out := make([]definition, len(defs))
	for i, d := range defs {
		out[i] = d
		out[i].local = d.path
		if len(d.path) > 0 && d.path[0] == "global" {
			continue
		}
		out[i].path = append(KeyPath{prefix}, d.path...)
	}
	return out
}
