package helmmap

import (
	"path"
	"regexp"
	"slices"

	"go.yaml.in/yaml/v3"
)

// DependencyNames lists the dependencies a Chart.yaml (or requirements.yaml)
// declares, by the key their values have in the parent chart: the alias, else
// the name.
func DependencyNames(chartYAML []byte) []string {
	var doc struct {
		Dependencies []struct {
			Name  string `yaml:"name"`
			Alias string `yaml:"alias"`
		} `yaml:"dependencies"`
	}
	if yaml.Unmarshal(chartYAML, &doc) != nil {
		return nil
	}
	var out []string
	for _, d := range doc.Dependencies {
		name := d.Alias
		if name == "" {
			name = d.Name
		}
		if name != "" && !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	return out
}

var subchartValues = regexp.MustCompile(`^charts/([^/]+)/values\.ya?ml$`)

// SubchartNames returns the subcharts whose values.yaml is in a list of value
// files, as Argo CD reports them for a chart (charts/<name>/values.yaml):
// their defaults exist but are not available to Backflow.
func SubchartNames(valueFiles []string) []string {
	var out []string
	for _, f := range valueFiles {
		if m := subchartValues.FindStringSubmatch(path.Clean(f)); m != nil && !slices.Contains(out, m[1]) {
			out = append(out, m[1])
		}
	}
	return out
}
