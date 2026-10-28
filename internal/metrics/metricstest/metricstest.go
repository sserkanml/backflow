// Package metricstest checks the metrics of Backflow in tests.
package metricstest

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/sserkanml/backflow/internal/metrics"
)

// Allowed are the label names a Backflow metric may have. Anything else fails
// the guard: a new label has to be a conscious, reviewed decision.
var Allowed = []string{
	"policy", "application", "source_type", "strategy", "reason", "phase", "renderer", "provider",
	"host", "endpoint", "code_class", "result", "action", "namespace", "name", "version", "commit", "le",
}

// Forbidden are names that would carry an unbounded value; they are called out in
// the message, which is more useful than "not allowed".
var Forbidden = []string{
	"resource", "proposal", "file", "path", "branch", "repo", "repository", "url", "token", "secret", "uid", "revision", "sha",
}

// Gather returns the metric families of Backflow (the names that start with
// backflow_) in the registry.
func Gather() ([]*dto.MetricFamily, error) {
	return GatherFrom(metrics.Registry)
}

// GatherFrom is Gather for another gatherer.
func GatherFrom(g prometheus.Gatherer) ([]*dto.MetricFamily, error) {
	all, err := g.Gather()
	if err != nil {
		return nil, err
	}
	var out []*dto.MetricFamily
	for _, f := range all {
		if strings.HasPrefix(f.GetName(), "backflow_") {
			out = append(out, f)
		}
	}
	return out, nil
}

// Problems checks the families: every name starts with backflow_, every label
// name is allowed (and none is forbidden), and no metric has more than maxSeries
// series. It returns one line per problem.
func Problems(families []*dto.MetricFamily, maxSeries int) []string {
	var out []string
	for _, f := range families {
		series := 0
		seen := map[string]bool{}
		for _, m := range f.GetMetric() {
			series++
			for _, l := range m.GetLabel() {
				name := l.GetName()
				switch {
				case slices.Contains(Forbidden, name):
					out = append(out, fmt.Sprintf("%s has the label %q, which carries an unbounded value", f.GetName(), name))
				case !slices.Contains(Allowed, name) && !seen[name]:
					out = append(out, fmt.Sprintf("%s has the label %q, which is not on the list of allowed labels", f.GetName(), name))
				}
				seen[name] = true
			}
		}
		// A histogram series is one per label set; its buckets are not counted.
		if series > maxSeries {
			out = append(out, fmt.Sprintf("%s has %d series, more than %d", f.GetName(), series, maxSeries))
		}
	}
	return out
}

// Guard fails the test on any problem of the registered metrics.
func Guard(t *testing.T, maxSeries int) {
	t.Helper()
	fams, err := Gather()
	if err != nil {
		t.Fatalf("gathering the metrics: %v", err)
	}
	for _, p := range Problems(fams, maxSeries) {
		t.Error(p)
	}
}

// Value is the value of the series with exactly these labels: a counter or a
// gauge, or the number of observations of a histogram. It is 0 when there is no
// such series, so a test can take it before and after.
func Value(name string, labels map[string]string) float64 {
	fams, err := Gather()
	if err != nil {
		return 0
	}
	for _, f := range fams {
		if f.GetName() != name {
			continue
		}
	series:
		for _, m := range f.GetMetric() {
			if len(m.GetLabel()) != len(labels) {
				continue
			}
			for _, l := range m.GetLabel() {
				if labels[l.GetName()] != l.GetValue() {
					continue series
				}
			}
			switch {
			case m.GetCounter() != nil:
				return m.GetCounter().GetValue()
			case m.GetGauge() != nil:
				return m.GetGauge().GetValue()
			case m.GetHistogram() != nil:
				return float64(m.GetHistogram().GetSampleCount())
			}
		}
	}
	return 0
}

// Series is how many series a metric has.
func Series(name string) int {
	fams, _ := Gather()
	for _, f := range fams {
		if f.GetName() == name {
			return len(f.GetMetric())
		}
	}
	return 0
}
