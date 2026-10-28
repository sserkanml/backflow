package metrics_test

import (
	"fmt"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"google.golang.org/protobuf/proto"

	"github.com/sserkanml/backflow/internal/metrics"
	"github.com/sserkanml/backflow/internal/metrics/metricstest"
)

// TestCardinality exercises every metric with a spread of values, as a run of
// the operator would, and checks what the registry then holds.
func TestCardinality(t *testing.T) {
	for i := range 20 {
		app := fmt.Sprintf("app-%d", i)
		metrics.DriftDetected(metrics.Policy{Namespace: "ns", Name: "policy"}, app, "Helm")
		metrics.SetDetectionPaused(metrics.Policy{Namespace: "ns", Name: "policy"}, app, "GitAhead")
		metrics.MappingResult("Helm", "HelmValues", "Mapped")
		metrics.MappingResult("Helm", "Unmapped", "NoVerifiedCandidate")
	}
	metrics.MergeRequest("gitlab", metrics.MROpened)
	metrics.Verification("Helm", "mapped", time.Second)
	metrics.Render(metrics.RendererUpload, nil, false, time.Second)
	metrics.ScratchBranches(metrics.ScratchPushed, 2)
	metrics.ArgoCDRequest("manifests_with_files", 200, nil, time.Second)
	metrics.SCMRequest("gitlab", "gitlab.com", 200, nil, time.Second)
	metrics.SecretRead("ok")
	metrics.SetPolicy(metrics.Policy{Namespace: "ns", Name: "policy"}, true, 20)
	metrics.SetSCMConnection("ns", "gitlab", true)
	metrics.SetBuildInfo("test", "abc")

	metricstest.Guard(t, 100)
}

func TestGuardCatchesWhatItShould(t *testing.T) {
	// A fake family with a forbidden label and one with too many series.
	fams, err := metricstest.Gather()
	if err != nil || len(fams) == 0 {
		t.Fatalf("families = %d, err = %v", len(fams), err)
	}
	if problems := metricstest.Problems(fams, 1); len(problems) == 0 {
		t.Error("a series bound of 1 finds nothing wrong")
	}
}

func TestGuardCatchesInconsistentPolicyLabels(t *testing.T) {
	family := func(labels ...[2]string) []*dto.MetricFamily {
		m := &dto.Metric{}
		for _, l := range labels {
			m.Label = append(m.Label, &dto.LabelPair{Name: proto.String(l[0]), Value: proto.String(l[1])})
		}
		return []*dto.MetricFamily{{Name: proto.String("backflow_x"), Metric: []*dto.Metric{m}}}
	}
	for name, tt := range map[string]struct {
		fams []*dto.MetricFamily
		want int
	}{
		"both labels":       {family([2]string{"policy_namespace", "ns"}, [2]string{"policy", "p"}), 0},
		"a joined value":    {family([2]string{"policy_namespace", "ns"}, [2]string{"policy", "ns/p"}), 1},
		"the name alone":    {family([2]string{"policy", "p"}), 1},
		"a bare name":       {family([2]string{"name", "p"}, [2]string{"namespace", "ns"}), 2},
		"both scm labels":   {family([2]string{"scm_namespace", "ns"}, [2]string{"scm", "c"}), 0},
		"the scm namespace": {family([2]string{"scm_namespace", "ns"}), 1},
	} {
		if got := len(metricstest.Problems(tt.fams, 10)); got != tt.want {
			t.Errorf("%s: %d problems, want %d: %v", name, got, tt.want, metricstest.Problems(tt.fams, 10))
		}
	}
}
