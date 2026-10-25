package helmmap

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"path"
	"slices"
	"strings"
	"unicode"

	"github.com/sserkanml/backflow/internal/rendermap"

	"go.yaml.in/yaml/v3"
)

// depDefaults is the values.yaml of a dependency of the chart, with the key
// prefix its values have in the parent chart: the dependency's alias, or its
// name. Its global.* keys keep their place.
type depDefaults struct {
	// file names where the values come from, for messages: a values.yaml, or
	// the values.yaml inside a packaged chart ("charts/redis-1.0.0.tgz!redis/values.yaml").
	file   string
	data   []byte
	prefix string
}

// maxPackagedValues bounds the values.yaml read from a packaged subchart.
const maxPackagedValues = 1 << 20

// dependencyLayers is dependencyDefaults for the chart of the source: a chart
// that is not in the repository has no files to read, and its subcharts'
// defaults are unavailable.
func (s *Source) dependencyLayers(files map[string][]byte) (out []depDefaults, missing []string) {
	if s.External != nil {
		return nil, slices.Clone(s.External.UnreadDependencies)
	}
	return dependencyDefaults(files, s.ChartDir)
}

// dependencyDefaults lists the values.yaml files of the dependencies that are
// part of the files of the render: a subchart unpacked under charts/, or a
// file:// dependency, or one packaged as charts/<name>-<version>.tgz. missing
// are the dependencies whose values are not in the files at all: Argo CD
// downloads them from their repository when it renders.
func dependencyDefaults(files map[string][]byte, chartDir string) (out []depDefaults, missing []string) {
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
			prefix := d.Name
			if d.Alias != "" {
				prefix = d.Alias
			}
			file := path.Join(dir, "values.yaml")
			data, ok := files[file]
			if !ok {
				file, data, ok = packagedValues(files, path.Join(chartDir, "charts"), d.Name)
			}
			if !ok {
				if !seen["missing\x00"+prefix] && !hasChartFiles(files, dir) {
					seen["missing\x00"+prefix] = true
					missing = append(missing, prefix)
				}
				continue
			}
			if seen[file+"\x00"+d.Alias] {
				continue
			}
			seen[file+"\x00"+d.Alias] = true
			out = append(out, depDefaults{file: file, data: data, prefix: prefix})
		}
	}
	return out, missing
}

// hasChartFiles reports whether the files hold anything of the chart in dir
// (an unpacked dependency without a values.yaml has no defaults, which is not
// the same as not being in the repository).
func hasChartFiles(files map[string][]byte, dir string) bool {
	_, ok := files[path.Join(dir, "Chart.yaml")]
	return ok
}

// packagedValues finds the dependency name packaged in dir (a file
// <name>-<version>.tgz) and returns the values.yaml inside it.
func packagedValues(files map[string][]byte, dir, name string) (file string, data []byte, ok bool) {
	for p, content := range files {
		base := path.Base(p)
		rest, found := strings.CutPrefix(base, name+"-")
		if !found || path.Dir(p) != dir || !strings.HasSuffix(base, ".tgz") || rest == "" ||
			(!unicode.IsDigit(rune(rest[0])) && rest[0] != 'v') {
			continue
		}
		if v, err := valuesInArchive(content); err == nil && v != nil {
			return p + "!" + name + "/values.yaml", v, true
		}
	}
	return "", nil, false
}

// valuesInArchive returns the values.yaml at the top of a chart package, or nil.
func valuesInArchive(archive []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		parts := strings.Split(path.Clean(h.Name), "/")
		if len(parts) == 2 && parts[1] == "values.yaml" {
			return io.ReadAll(io.LimitReader(tr, maxPackagedValues))
		}
	}
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
		out[i].path = append(rendermap.KeyPath{prefix}, d.path...)
	}
	return out
}
