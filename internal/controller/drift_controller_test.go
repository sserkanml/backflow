package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	backflowv1alpha1 "github.com/sserkanml/backflow/api/v1alpha1"
	"github.com/sserkanml/backflow/internal/argocd"
)

func TestProposalName(t *testing.T) {
	res := appResource{Kind: "ConfigMap", Name: "demo-config", Namespace: "demo"}
	changes := []backflowv1alpha1.FieldChange{{Path: "/data/a", Op: backflowv1alpha1.OpAdd, Live: `"1"`}}

	name := proposalName("demo-app", res, changes, 0)
	if !strings.HasPrefix(name, "demo-app-configmap-demo-config-") {
		t.Errorf("name = %q", name)
	}
	if name != proposalName("demo-app", res, changes, 0) {
		t.Error("name is not deterministic")
	}
	other := []backflowv1alpha1.FieldChange{{Path: "/data/a", Op: backflowv1alpha1.OpAdd, Live: `"2"`}}
	if name == proposalName("demo-app", res, other, 0) {
		t.Error("different changes produced the same name")
	}
	if name == proposalName("demo-app", res, changes, 1) {
		t.Error("attempt did not change the name")
	}

	long := appResource{Kind: "ClusterRoleBinding", Name: strings.Repeat("Very_Long.Name", 10)}
	for attempt := 0; attempt < 12; attempt++ {
		n := proposalName(strings.Repeat("app", 30), long, changes, attempt)
		if len(n) > 63 {
			t.Errorf("len(%q) = %d", n, len(n))
		}
		if n != strings.ToLower(n) || strings.ContainsAny(n, "_. ") || strings.HasPrefix(n, "-") || strings.Contains(n, "--") {
			t.Errorf("not DNS-1123 safe: %q", n)
		}
	}
}

func TestSafeLabel(t *testing.T) {
	if safeLabel("short") != "short" {
		t.Error("short value changed")
	}
	long := strings.Repeat("a", 100)
	if got := safeLabel(long); len(got) > 63 || got != safeLabel(long) {
		t.Errorf("safeLabel = %q", got)
	}
}

func TestApplicationSyncChanged(t *testing.T) {
	newApp := func(status, reconciledAt string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]interface{}{
			"status": map[string]interface{}{
				"reconciledAt": reconciledAt,
				"resources": []interface{}{map[string]interface{}{
					"version": "v1", "kind": "ConfigMap", "namespace": "demo", "name": "c", "status": status,
				}},
			},
		}}
	}
	tests := []struct {
		name     string
		old, new *unstructured.Unstructured
		want     bool
	}{
		{"sync status changed", newApp("Synced", "t1"), newApp("OutOfSync", "t1"), true},
		{"still OutOfSync, new comparison", newApp("OutOfSync", "t1"), newApp("OutOfSync", "t2"), true},
		{"Synced, new comparison", newApp("Synced", "t1"), newApp("Synced", "t2"), false},
		{"nothing changed", newApp("OutOfSync", "t1"), newApp("OutOfSync", "t1"), false},
	}
	p := applicationSyncChanged()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := p.Update(event.UpdateEvent{ObjectOld: tt.old, ObjectNew: tt.new}); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

type fakeArgo struct {
	items []argocd.ManagedResource
	err   error
	calls int
}

func (f *fakeArgo) GetManagedResources(context.Context, string, string) ([]argocd.ManagedResource, error) {
	f.calls++
	return f.items, f.err
}

func configMapState(level string) map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]interface{}{"name": "demo-config", "resourceVersion": level},
		"data":     map[string]interface{}{"LOG_LEVEL": level},
	}
}

var _ = Describe("Drift detection", func() {
	const ns = "default"
	var (
		ctx      = context.Background()
		policy   *backflowv1alpha1.BackflowPolicy
		app      *unstructured.Unstructured
		argo     *fakeArgo
		recorder *events.FakeRecorder
		r        *DriftReconciler
	)

	setResources := func(resources ...interface{}) {
		Expect(unstructured.SetNestedSlice(app.Object, resources, "status", "resources")).To(Succeed())
		Expect(k8sClient.Update(ctx, app)).To(Succeed())
	}
	configMapRes := func(status string) map[string]interface{} {
		return map[string]interface{}{
			"version": "v1", "kind": "ConfigMap", "namespace": "demo", "name": "demo-config", "status": status,
		}
	}
	reconcileOnce := func() {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: policy.Name}})
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
	}
	proposals := func() []backflowv1alpha1.DriftProposal {
		var list backflowv1alpha1.DriftProposalList
		ExpectWithOffset(1, k8sClient.List(ctx, &list, client.InNamespace(ns),
			client.MatchingLabels{labelPolicy: policy.Name})).To(Succeed())
		return list.Items
	}
	byPhase := func(phase backflowv1alpha1.ProposalPhase) []backflowv1alpha1.DriftProposal {
		var out []backflowv1alpha1.DriftProposal
		for _, p := range proposals() {
			if p.Status.Phase == phase {
				out = append(out, p)
			}
		}
		return out
	}

	BeforeEach(func() {
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "argocd-token-drift", Namespace: ns},
			StringData: map[string]string{"token": "tok"},
		})).To(Succeed())

		policy = &backflowv1alpha1.BackflowPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "drift-policy", Namespace: ns},
			Spec: backflowv1alpha1.BackflowPolicySpec{
				ArgoCDNamespace: ns,
				Applications:    backflowv1alpha1.ApplicationSelector{Names: []string{"drift-app"}},
				Mode:            backflowv1alpha1.ModeReportOnly,
				ArgoCD: &backflowv1alpha1.ArgoCDServer{
					URL: "https://argocd.test",
					TokenSecretRef: corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "argocd-token-drift"}, Key: "token",
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, policy)).To(Succeed())
		policy.Status.Conditions = []metav1.Condition{{
			Type: "Ready", Status: metav1.ConditionTrue, Reason: "Resolved", LastTransitionTime: metav1.Now(),
		}}
		policy.Status.Applications = []backflowv1alpha1.ApplicationSummary{{
			Name: "drift-app", RepoURL: "https://gitlab.com/x/y.git", Path: "apps/demo",
			TargetRevision: "main", SyncedRevision: "abc123", SourceType: "Directory",
		}}
		Expect(k8sClient.Status().Update(ctx, policy)).To(Succeed())

		app = &unstructured.Unstructured{}
		app.SetGroupVersionKind(applicationGVK)
		app.SetName("drift-app")
		app.SetNamespace(ns)
		Expect(k8sClient.Create(ctx, app)).To(Succeed())
		setResources(configMapRes("OutOfSync"))

		argo = &fakeArgo{items: []argocd.ManagedResource{{
			Kind: "ConfigMap", Namespace: "demo", Name: "demo-config",
			PredictedLiveState: configMapState("info"), NormalizedLiveState: configMapState("debug"),
		}}}
		recorder = events.NewFakeRecorder(50)
		r = &DriftReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: recorder,
			NewArgoClient: func(argocd.Config) (ManagedResourcesGetter, error) { return argo, nil },
		}
	})

	AfterEach(func() {
		for _, p := range proposals() {
			Expect(k8sClient.Delete(ctx, &p)).To(Succeed())
		}
		Expect(k8sClient.Delete(ctx, app)).To(Succeed())
		Expect(k8sClient.Delete(ctx, policy)).To(Succeed())
		Expect(k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "argocd-token-drift", Namespace: ns}})).To(Succeed())
	})

	It("creates one proposal per drift and follows it until it is reverted", func() {
		By("detecting the drift")
		reconcileOnce()
		Expect(proposals()).To(HaveLen(1))
		first := proposals()[0]
		Expect(first.Status.Phase).To(Equal(backflowv1alpha1.PhaseDetected))
		Expect(first.Spec.PolicyName).To(Equal(policy.Name))
		Expect(first.Spec.Application.Name).To(Equal("drift-app"))
		Expect(first.Spec.Resource.Kind).To(Equal("ConfigMap"))
		Expect(first.Spec.Resource.Version).To(Equal("v1"))
		Expect(first.Spec.Source.Revision).To(Equal("abc123"))
		Expect(first.Spec.Source.Type).To(Equal(backflowv1alpha1.SourceDirectory))
		Expect(first.Spec.Changes).To(Equal([]backflowv1alpha1.FieldChange{{
			Path: "/data/LOG_LEVEL", Op: backflowv1alpha1.OpReplace, Desired: `"info"`, Live: `"debug"`,
		}}))
		Expect(first.Spec.Actor).To(BeNil())
		Expect(first.Labels).To(HaveKeyWithValue(labelPolicy, policy.Name))
		Expect(first.Labels).To(HaveKeyWithValue(labelApplication, "drift-app"))
		Expect(first.Labels).To(HaveKey(labelResource))
		Expect(first.OwnerReferences).To(HaveLen(1))
		Expect(first.OwnerReferences[0].UID).To(Equal(policy.UID))

		By("not creating a duplicate for identical changes")
		reconcileOnce()
		Expect(proposals()).To(HaveLen(1))
		Expect(proposals()[0].Name).To(Equal(first.Name))

		By("superseding it when the changes differ")
		argo.items[0].NormalizedLiveState = configMapState("trace")
		reconcileOnce()
		Expect(proposals()).To(HaveLen(2))
		detected := byPhase(backflowv1alpha1.PhaseDetected)
		superseded := byPhase(backflowv1alpha1.PhaseSuperseded)
		Expect(detected).To(HaveLen(1))
		Expect(superseded).To(HaveLen(1))
		Expect(superseded[0].Name).To(Equal(first.Name))
		Expect(superseded[0].Status.SupersededBy).To(Equal(detected[0].Name))
		Expect(superseded[0].Spec).To(Equal(first.Spec), "spec of an existing proposal must not change")

		By("reverting once the resource is Synced again")
		setResources(configMapRes("Synced"))
		reconcileOnce()
		Expect(byPhase(backflowv1alpha1.PhaseReverted)).To(HaveLen(1))
		Expect(byPhase(backflowv1alpha1.PhaseDetected)).To(BeEmpty())

		By("creating a fresh proposal when the same drift comes back")
		argo.items[0].NormalizedLiveState = configMapState("debug")
		setResources(configMapRes("OutOfSync"))
		reconcileOnce()
		Expect(proposals()).To(HaveLen(3))
		Expect(byPhase(backflowv1alpha1.PhaseDetected)).To(HaveLen(1))
	})

	It("does not call Argo CD when nothing is OutOfSync and nothing is open", func() {
		setResources(configMapRes("Synced"))
		reconcileOnce()
		Expect(argo.calls).To(BeZero())
		Expect(proposals()).To(BeEmpty())
	})

	It("calls Argo CD once per reconcile for an OutOfSync application", func() {
		reconcileOnce()
		Expect(argo.calls).To(Equal(1))
		reconcileOnce()
		Expect(argo.calls).To(Equal(2))
	})

	It("counts open proposals in the policy status", func() {
		reconcileOnce()
		pr := &BackflowPolicyReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := pr.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: policy.Name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(policy), policy)).To(Succeed())
		Expect(policy.Status.OpenProposals).To(Equal(int32(1)))
	})

	It("honours ignoreFields, include/exclude and skips Secrets", func() {
		By("ignoring a drifted field")
		policy.Spec.IgnoreFields = []backflowv1alpha1.IgnoreRule{{
			KindSelector: backflowv1alpha1.KindSelector{Kind: "ConfigMap"},
			JSONPointers: []string{"/data/LOG_LEVEL"},
		}}
		Expect(k8sClient.Update(ctx, policy)).To(Succeed())
		reconcileOnce()
		Expect(proposals()).To(BeEmpty())
		Expect(argo.calls).To(Equal(1))

		By("not calling Argo CD for excluded kinds and Secrets")
		policy.Spec.IgnoreFields = nil
		policy.Spec.Exclude = []backflowv1alpha1.KindSelector{{Kind: "ConfigMap"}}
		Expect(k8sClient.Update(ctx, policy)).To(Succeed())
		setResources(configMapRes("OutOfSync"), map[string]interface{}{
			"version": "v1", "kind": "Secret", "namespace": "demo", "name": "s", "status": "OutOfSync",
		})
		reconcileOnce()
		Expect(proposals()).To(BeEmpty())
		Expect(argo.calls).To(Equal(1))
	})

	It("does nothing and emits an Event when Argo CD rejects the token", func() {
		argo.err = errors.Join(argocd.ErrUnauthorized, errors.New("HTTP 401"))
		reconcileOnce()
		Expect(proposals()).To(BeEmpty())
		Expect(recorder.Events).To(Receive(ContainSubstring("ArgoCDUnauthorized")))
	})

	It("keeps proposals when Argo CD is unreachable", func() {
		reconcileOnce()
		Expect(byPhase(backflowv1alpha1.PhaseDetected)).To(HaveLen(1))
		argo.err = argocd.ErrUnreachable
		reconcileOnce()
		Expect(byPhase(backflowv1alpha1.PhaseDetected)).To(HaveLen(1))
		Expect(byPhase(backflowv1alpha1.PhaseReverted)).To(BeEmpty())
	})

	It("leaves a proposal untouched and warns when its resource was deleted from the cluster", func() {
		reconcileOnce()
		Expect(byPhase(backflowv1alpha1.PhaseDetected)).To(HaveLen(1))

		By("deleting the resource: OutOfSync, but only Git has a state")
		argo.items[0].NormalizedLiveState = nil
		reconcileOnce()
		Expect(byPhase(backflowv1alpha1.PhaseDetected)).To(HaveLen(1))
		Expect(byPhase(backflowv1alpha1.PhaseReverted)).To(BeEmpty())
		Expect(proposals()).To(HaveLen(1))

		var warned bool
		for len(recorder.Events) > 0 {
			if e := <-recorder.Events; strings.Contains(e, "Warning") && strings.Contains(e, "ResourceDeleted") {
				warned = true
			}
		}
		Expect(warned).To(BeTrue(), "expected a ResourceDeleted warning Event")
	})

	It("skips policies without argoCD or that are not Ready", func() {
		policy.Spec.ArgoCD = nil
		Expect(k8sClient.Update(ctx, policy)).To(Succeed())
		reconcileOnce()
		Expect(argo.calls).To(BeZero())

		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(policy), policy)).To(Succeed())
		policy.Spec.ArgoCD = &backflowv1alpha1.ArgoCDServer{
			TokenSecretRef: corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "argocd-token-drift"}, Key: "token",
			},
		}
		Expect(k8sClient.Update(ctx, policy)).To(Succeed())
		policy.Status.Conditions[0].Status = metav1.ConditionFalse
		Expect(k8sClient.Status().Update(ctx, policy)).To(Succeed())
		reconcileOnce()
		Expect(argo.calls).To(BeZero())
	})

	It("reports a missing token Secret as an Event", func() {
		Expect(k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "argocd-token-drift", Namespace: ns}})).To(Succeed())
		reconcileOnce()
		Expect(argo.calls).To(BeZero())
		Expect(recorder.Events).To(Receive(ContainSubstring("not found")))
		// Recreated for AfterEach.
		Expect(k8sClient.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "argocd-token-drift", Namespace: ns}})).To(Succeed())
	})
})
