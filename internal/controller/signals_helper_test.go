package controller

import (
	"context"
	"slices"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"sigs.k8s.io/controller-runtime/pkg/client"

	backflowv1 "github.com/sserkanml/backflow/api/v1"
)

// The keys of the signals the policy and drift controllers leave in status.signals, as the tests
// spell them. They used to be annotations; the tests keep the names.
const (
	annotationSupersededBy  = "supersededBy"
	annotationReverted      = "reverted"
	annotationLiveReverted  = "liveReverted"
	annotationAccessRevoked = "accessRevoked"
)

// setSignalKey sets one signal by its test key.
func setSignalKey(s *backflowv1.Signals, key, value string) {
	switch key {
	case annotationSupersededBy:
		s.SupersededBy = value
	case annotationReverted:
		s.Reverted = value
	case annotationLiveReverted:
		s.LiveRevertedAtRevision = value
	case annotationAccessRevoked:
		s.AccessRevoked = value != ""
	default:
		panic("unknown signal " + key)
	}
}

// signalMap lists the signals that are set, by test key.
func signalMap(dp *backflowv1.DriftProposal) map[string]string {
	out := map[string]string{}
	sg := dp.Status.Signal()
	for k, v := range map[string]string{
		annotationSupersededBy: sg.SupersededBy, annotationReverted: sg.Reverted, annotationLiveReverted: sg.LiveRevertedAtRevision,
	} {
		if v != "" {
			out[k] = v
		}
	}
	if sg.AccessRevoked {
		out[annotationAccessRevoked] = "true"
	}
	return out
}

// signalProposal does what the drift controller does: it records a signal in the status of the proposal, with a
// merge patch of status.signals only.
func signalProposal(ctx context.Context, c client.Client, dp *backflowv1.DriftProposal, key, value string) {
	GinkgoHelper()
	orig := dp.DeepCopy()
	setSignalKey(dp.Status.EnsureSignals(), key, value)
	Expect(c.Status().Patch(ctx, dp, client.MergeFrom(orig))).To(Succeed())
}

// appNames are the names in status.applications.
func appNames(st backflowv1.BackflowPolicyStatus) []string {
	names := make([]string, 0, len(st.Applications))
	for _, a := range st.Applications {
		names = append(names, a.Name)
	}
	return names
}

// hiddenReason is the reason in status.argoCD.hiddenApplications, empty when none are hidden.
func hiddenReason(st backflowv1.BackflowPolicyStatus) string {
	if st.ArgoCD == nil || len(st.ArgoCD.HiddenApplications) == 0 {
		return ""
	}
	return string(st.ArgoCD.HiddenApplications[0].Reason)
}

func init() {
	// The addresses the tests name in policies and connections are allowed, as an installer would
	// allow them; the allowlist tests set their own.
	SetArgoCDAllowedServers([]string{"https://argocd", "https://argocd.test", "https://argocd.example", "https://argocd-server.argocd.svc"})
	SetScmAllowedHosts([]string{"git.test", "gitlab.com", "gitlab.example.com", "github.com", "example.com", "127.0.0.1", "localhost"})
}

// allowArgoServer lets policies name the (test) Argo CD server at url for the rest of the spec.
func allowArgoServer(url string) {
	GinkgoHelper()
	allowed := argoServers.Load()
	previous := []string(nil)
	if allowed != nil {
		previous = append(previous, *allowed...)
	}
	SetArgoCDAllowedServers(append(slices.Clone(previous), url))
	DeferCleanup(SetArgoCDAllowedServers, previous)
}
