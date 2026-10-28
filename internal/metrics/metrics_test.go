package metrics

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func delta(t *testing.T, c prometheus.Collector, do func()) float64 {
	t.Helper()
	before := testutil.ToFloat64(c)
	do()
	return testutil.ToFloat64(c) - before
}

func TestCounters(t *testing.T) {
	tests := []struct {
		name string
		c    prometheus.Collector
		do   func()
	}{
		{"drifts", driftsDetected.WithLabelValues("ns", "p", "app", "Helm"), func() { DriftDetected(Policy{"ns", "p"}, "app", "Helm") }},
		{"mapping", mappingResults.WithLabelValues("Helm", "HelmValues", "Mapped"), func() { MappingResult("Helm", "HelmValues", "Mapped") }},
		{"merge request", mergeRequests.WithLabelValues("gitlab", "opened"), func() { MergeRequest("gitlab", MROpened) }},
		{"render", renders.WithLabelValues("upload", "ok"), func() { Render(RendererUpload, nil, false, time.Second) }},
		{"render failed", renders.WithLabelValues("revisions", "failed"), func() { Render(RendererRevisions, errors.New("x"), true, time.Second) }},
		{"render error", renders.WithLabelValues("revisions", "error"), func() { Render(RendererRevisions, errors.New("x"), false, time.Second) }},
		{"scratch", scratchBranches.WithLabelValues("pushed"), func() { ScratchBranches(ScratchPushed, 3) }},
		{"argocd", argocdRequests.WithLabelValues("manifests_by_revisions", "5xx"), func() { ArgoCDRequest("manifests_by_revisions", 502, nil, time.Second) }},
		{"scm", scmRequests.WithLabelValues("gitlab", "gitlab.com", "2xx"), func() { SCMRequest("gitlab", "gitlab.com", 200, nil, time.Second) }},
		{"secrets", secretReads.WithLabelValues("forbidden"), func() { SecretRead("forbidden") }},
	}
	want := map[string]float64{"scratch": 3}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exp := 1.0
			if w, ok := want[tt.name]; ok {
				exp = w
			}
			if got := delta(t, tt.c, tt.do); got != exp {
				t.Errorf("delta = %v, want %v", got, exp)
			}
		})
	}
}

func TestHistogramsObserve(t *testing.T) {
	Verification("Helm", "mapped", 3*time.Second)
	Render(RendererUpload, nil, false, time.Second)
	ArgoCDRequest("managed_resources", 200, nil, time.Millisecond)
	SCMRequest("github", "github.com", 200, nil, time.Millisecond)
	for name, c := range map[string]prometheus.Collector{"render": renderDuration, "argocd": argocdDuration, "scm": scmDuration, "verification": verificationDuration} {
		if testutil.CollectAndCount(c) == 0 {
			t.Errorf("%s: no series", name)
		}
	}
}

func TestScratchBranchesIgnoresZero(t *testing.T) {
	if got := delta(t, scratchBranches.WithLabelValues("swept"), func() { ScratchBranches(ScratchSwept, 0); ScratchBranches(ScratchSwept, -1) }); got != 0 {
		t.Errorf("delta = %v", got)
	}
}

func TestCodeClass(t *testing.T) {
	tests := []struct {
		status int
		err    error
		want   string
	}{
		{200, nil, "2xx"}, {204, nil, "2xx"}, {301, nil, "3xx"}, {404, nil, "4xx"}, {429, nil, "4xx"}, {500, nil, "5xx"},
		{503, nil, "5xx"}, {0, errors.New("x"), "error"}, {0, nil, "error"}, {100, nil, "1xx"}, {-1, nil, "error"},
	}
	for _, tt := range tests {
		if got := CodeClass(tt.status, tt.err); got != tt.want {
			t.Errorf("CodeClass(%d, %v) = %s, want %s", tt.status, tt.err, got, tt.want)
		}
	}
}

func TestSecretResult(t *testing.T) {
	err := errors.New("x")
	for _, tt := range []struct {
		err       error
		forbidden bool
		notFound  bool
		want      string
	}{{nil, false, false, "ok"}, {err, true, false, "forbidden"}, {err, false, true, "notfound"}, {err, false, false, "error"}} {
		if got := SecretResult(tt.err, tt.forbidden, tt.notFound); got != tt.want {
			t.Errorf("%v: %s, want %s", tt, got, tt.want)
		}
	}
}

func TestStateGaugesAndForgetting(t *testing.T) {
	SetSCMConnection("ns", "gitlab", true)
	if got := testutil.ToFloat64(scmConnectionReady.WithLabelValues("ns", "gitlab")); got != 1 {
		t.Errorf("connection ready = %v", got)
	}
	SetSCMConnection("ns", "gitlab", false)
	if got := testutil.ToFloat64(scmConnectionReady.WithLabelValues("ns", "gitlab")); got != 0 {
		t.Errorf("connection ready = %v", got)
	}
	ForgetSCMConnection("ns", "gitlab")
	if n := testutil.CollectAndCount(scmConnectionReady); n != 0 {
		t.Errorf("series left after the connection was deleted: %d", n)
	}

	SetPolicy(Policy{"ns", "p1"}, true, 3)
	SetPolicy(Policy{"ns", "p2"}, false, 0)
	if got := testutil.ToFloat64(policyApplications.WithLabelValues("ns", "p1")); got != 3 {
		t.Errorf("applications = %v", got)
	}
	DriftDetected(Policy{"ns", "p1"}, "app", "Helm")
	SetDetectionPaused(Policy{"ns", "p1"}, "app", "GitAhead")
	ForgetPolicy(Policy{"ns", "p1"})
	if n := testutil.CollectAndCount(policyReady); n != 1 {
		t.Errorf("policy_ready series = %d, want only p2", n)
	}
	if n := testutil.CollectAndCount(detectionPaused); n != 0 {
		t.Errorf("detection_paused series left for a deleted policy: %d", n)
	}
	ForgetPolicy(Policy{"ns", "p2"})
}

func TestDetectionPausedHasOnlyTheCurrentReason(t *testing.T) {
	p := Policy{"ns", "p"}
	SetDetectionPaused(p, "app", "GitAhead")
	SetDetectionPaused(p, "app", "ApplicationChanged")
	SetDetectionPaused(p, "other", "Settling")
	if n := testutil.CollectAndCount(detectionPaused); n != 2 {
		t.Fatalf("series = %d, want one per application", n)
	}
	if got := testutil.ToFloat64(detectionPaused.WithLabelValues("ns", "p", "app", "ApplicationChanged")); got != 1 {
		t.Errorf("value = %v", got)
	}
	SetDetectionPaused(p, "app", "")
	if n := testutil.CollectAndCount(detectionPaused); n != 1 {
		t.Errorf("series = %d after the Application is no longer paused", n)
	}
	ForgetApplication(p, "other")
	if n := testutil.CollectAndCount(detectionPaused); n != 0 {
		t.Errorf("series = %d after the Application is forgotten", n)
	}
}

func TestBuildInfoHasOneSeries(t *testing.T) {
	SetBuildInfo("v1", "abc")
	SetBuildInfo("v2", "def")
	if n := testutil.CollectAndCount(buildInfo); n != 1 {
		t.Errorf("series = %d", n)
	}
	if got := testutil.ToFloat64(buildInfo.WithLabelValues("v2", "def")); got != 1 {
		t.Errorf("value = %v", got)
	}
}

func TestBoundLimitsTheValuesOfALabel(t *testing.T) {
	if got := bound("l1", ""); got != "none" {
		t.Errorf("empty = %q", got)
	}
	if got := bound("l1", strings.Repeat("x", 200)); len(got) != maxLabelLength {
		t.Errorf("long value has %d characters", len(got))
	}
	for i := range maxValuesPerLabel + 50 {
		bound("l2", fmt.Sprintf("v%d", i))
	}
	if got := bound("l2", "a-new-one"); got != "other" {
		t.Errorf("beyond the bound: %q", got)
	}
	if got := bound("l2", "v1"); got != "v1" {
		t.Errorf("a value seen before is kept: %q", got)
	}
}

func TestAMetricNeverPanics(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic: %v", r)
		}
	}()
	safe(func() { panic("boom") })
	// A label count that does not fit would panic in the client library.
	safe(func() { driftsDetected.WithLabelValues("only one") })
}
