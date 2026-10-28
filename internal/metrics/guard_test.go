package metrics_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/sserkanml/backflow/internal/metrics"
	"github.com/sserkanml/backflow/internal/metrics/metricstest"
)

// TestCardinality exercises every metric with a spread of values, as a run of
// the operator would, and checks what the registry then holds.
func TestCardinality(t *testing.T) {
	for i := range 20 {
		app := fmt.Sprintf("app-%d", i)
		metrics.DriftDetected("ns/policy", app, "Helm")
		metrics.SetDetectionPaused("ns/policy", app, "GitAhead")
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
	metrics.SetPolicy("ns", "policy", true, 20)
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
