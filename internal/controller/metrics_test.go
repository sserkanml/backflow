package controller

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	backflowv1alpha1 "github.com/sserkanml/backflow/api/v1alpha1"
	"github.com/sserkanml/backflow/internal/metrics"
	"github.com/sserkanml/backflow/internal/metrics/metricstest"
)

func TestProposalCountsByPolicyAndPhase(t *testing.T) {
	proposal := func(name, policy string, phase backflowv1alpha1.ProposalPhase) *backflowv1alpha1.DriftProposal {
		return &backflowv1alpha1.DriftProposal{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: name},
			Spec:       backflowv1alpha1.DriftProposalSpec{PolicyName: policy},
			Status:     backflowv1alpha1.DriftProposalStatus{Phase: phase},
		}
	}
	sch := runtime.NewScheme()
	if err := backflowv1alpha1.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(
		proposal("a", "p", backflowv1alpha1.PhaseUnmapped),
		proposal("b", "p", backflowv1alpha1.PhaseUnmapped),
		proposal("c", "p", ""),
		proposal("d", "q", backflowv1alpha1.PhaseProposed),
	).Build()
	got := map[string]int{}
	for _, p := range ProposalCounts(c)() {
		got[p.Policy.Namespace+"/"+p.Policy.Name+"/"+p.Phase] = p.Count
	}
	want := map[string]int{"ns/p/Unmapped": 2, "ns/p/Pending": 1, "ns/q/Proposed": 1}
	if len(got) != len(want) {
		t.Fatalf("counts = %v, want %v", got, want)
	}
	for k, n := range want {
		if got[k] != n {
			t.Errorf("%s = %d, want %d", k, got[k], n)
		}
	}
}

func TestVerificationResult(t *testing.T) {
	for _, tt := range []struct {
		err  error
		want string
	}{
		{nil, "mapped"},
		{&verdictError{reason: reasonNotFound, err: errors.New("x")}, "unmapped"},
		{&heldError{reason: reasonRepositoryUnavailable, cause: errors.New("x")}, "retry"},
		{context.DeadlineExceeded, "retry"},
	} {
		if got := verificationResult(tt.err); got != tt.want {
			t.Errorf("verificationResult(%v) = %q, want %q", tt.err, got, tt.want)
		}
	}
}

func TestProviderOf(t *testing.T) {
	if got := providerOf(nil); got != "" {
		t.Errorf("nil access = %q", got)
	}
	conn := &backflowv1alpha1.ScmConnection{Spec: backflowv1alpha1.ScmConnectionSpec{Provider: backflowv1alpha1.ScmProviderGitHub}}
	if got := providerOf(&scmAccess{conn: conn}); got != "github" {
		t.Errorf("provider = %q", got)
	}
}

func TestDetectionPausedMetricFollowsTheApplication(t *testing.T) {
	value := func(reason string) float64 {
		return metricstest.Value("backflow_detection_paused", map[string]string{"policy_namespace": "ns", "policy": "p", "application": "app", "reason": reason})
	}
	metrics.SetDetectionPaused(metrics.Policy{Namespace: "ns", Name: "p"}, "app", metrics.PausedGitAhead)
	if value("GitAhead") != 1 {
		t.Fatal("no series while paused")
	}
	metrics.SetDetectionPaused(metrics.Policy{Namespace: "ns", Name: "p"}, "app", metrics.PausedSettling)
	if value("GitAhead") != 0 || value("Settling") != 1 {
		t.Error("only the current reason has a series")
	}
	var r DriftReconciler
	r.forgetApplication("ns/p", "app")
	if value("Settling") != 0 {
		t.Error("the series of an Application that is gone stays")
	}
}
