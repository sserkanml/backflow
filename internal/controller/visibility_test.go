package controller

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	backflowv1alpha1 "github.com/sserkanml/backflow/api/v1alpha1"
	"github.com/sserkanml/backflow/internal/argocd"
)

// fakeGetter answers per Application name and counts the questions.
type fakeGetter struct {
	mu      sync.Mutex
	answers map[string]error
	asked   map[string]int
}

func (f *fakeGetter) GetApplication(_ context.Context, name, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.asked == nil {
		f.asked = map[string]int{}
	}
	f.asked[name]++
	return f.answers[name]
}

func (f *fakeGetter) count(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.asked[name]
}

func visibilityWith(f *fakeGetter, now *time.Time) *ApplicationVisibility {
	return &ApplicationVisibility{
		New: func(argocd.Config) (ApplicationGetter, error) { return f, nil },
		Now: func() time.Time { return *now },
	}
}

func TestVisibilityHidesWhatTheTokenMayNotGet(t *testing.T) {
	now := time.Now()
	f := &fakeGetter{answers: map[string]error{
		"forbidden": fmt.Errorf("%w: HTTP 403", argocd.ErrUnauthorized),
		"missing":   fmt.Errorf("%w: HTTP 404", argocd.ErrNotFound),
	}}
	v := visibilityWith(f, &now)
	cfg := argocd.Config{URL: "https://argocd", Token: "token-a"}
	for name, want := range map[string]bool{"mine": true, "forbidden": false, "missing": false} {
		got, err := v.Visible(context.Background(), "ns/p", cfg, name, "argocd")
		if err != nil || got != want {
			t.Errorf("%s: visible %v, error %v; want %v", name, got, err, want)
		}
	}
}

func TestVisibilityIsCachedPerTokenAndApplicationForTheTTL(t *testing.T) {
	now := time.Now()
	f := &fakeGetter{}
	v := visibilityWith(f, &now)
	ctx := context.Background()
	a, b := argocd.Config{URL: "https://argocd", Token: "token-a"}, argocd.Config{URL: "https://argocd", Token: "token-b"}

	for range 3 {
		if _, err := v.Visible(ctx, "ns/p", a, "app", "argocd"); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.count("app"); got != 1 {
		t.Errorf("asked %d times within the TTL, want 1", got)
	}
	if _, err := v.Visible(ctx, "ns/q", b, "app", "argocd"); err != nil {
		t.Fatal(err)
	}
	if got := f.count("app"); got != 2 {
		t.Errorf("another token is another question: asked %d times, want 2", got)
	}
	now = now.Add(DefaultVisibilityTTL + time.Second)
	if _, err := v.Visible(ctx, "ns/p", a, "app", "argocd"); err != nil {
		t.Fatal(err)
	}
	if got := f.count("app"); got != 3 {
		t.Errorf("asked %d times after the TTL, want 3", got)
	}
}

func TestVisibilityDoesNotRememberAnOutage(t *testing.T) {
	now := time.Now()
	f := &fakeGetter{answers: map[string]error{"app": fmt.Errorf("%w: down", argocd.ErrUnreachable)}}
	v := visibilityWith(f, &now)
	cfg := argocd.Config{URL: "https://argocd", Token: "t"}
	if _, err := v.Visible(context.Background(), "ns/p", cfg, "app", "argocd"); err == nil {
		t.Fatal("an unreachable Argo CD is an error, not a hidden Application")
	}
	f.answers["app"] = nil
	if ok, err := v.Visible(context.Background(), "ns/p", cfg, "app", "argocd"); err != nil || !ok {
		t.Errorf("after the outage: visible %v, error %v", ok, err)
	}
}

var _ = Describe("A policy with its own Argo CD token", func() {
	const argoNS = "default"
	var (
		ctx     = context.Background()
		created []client.Object
		now     = time.Now()
	)
	create := func(o client.Object) {
		ExpectWithOffset(1, k8sClient.Create(ctx, o)).To(Succeed())
		created = append(created, o)
	}
	AfterEach(func() {
		for _, o := range created {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, o))).To(Succeed())
		}
		created = nil
	})

	It("sees only the Applications its token may get, and counts the others", func() {
		for _, n := range []string{"vis-mine", "vis-theirs-qx17"} {
			app := &unstructured.Unstructured{}
			app.SetGroupVersionKind(applicationGVK)
			app.SetName(n)
			app.SetNamespace(argoNS)
			create(app)
		}
		create(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "vis-token", Namespace: argoNS}, Data: map[string][]byte{"token": []byte("t")}})
		policy := &backflowv1alpha1.BackflowPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "vis-policy", Namespace: argoNS},
			Spec: backflowv1alpha1.BackflowPolicySpec{
				ArgoCDNamespace: argoNS, Mode: backflowv1alpha1.ModeReportOnly,
				Applications: backflowv1alpha1.ApplicationSelector{Names: []string{"vis-mine", "vis-theirs-qx17"}},
				ArgoCD: &backflowv1alpha1.ArgoCDServer{
					TokenSecretRef: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "vis-token"}, Key: "token"},
				},
			},
		}
		create(policy)

		f := &fakeGetter{answers: map[string]error{"vis-theirs-qx17": fmt.Errorf("%w: HTTP 403", argocd.ErrUnauthorized)}}
		r := &BackflowPolicyReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), ArgoCD: &ArgoCDDefaults{}, Visibility: visibilityWith(f, &now)}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(policy)})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(policy), policy)).To(Succeed())

		Expect(meta.FindStatusCondition(policy.Status.Conditions, "Ready").Status).To(Equal(metav1.ConditionTrue))
		Expect(policy.Status.MatchedApplications).To(ConsistOf("vis-mine"))
		Expect(policy.Status.ExcludedApplications).To(BeEquivalentTo(1))
		Expect(policy.Status.ExcludedReason).To(Equal("NotVisibleToToken"))
		Expect(fmt.Sprint(policy.Status)).NotTo(ContainSubstring("qx17"))

		By("asking again within the TTL: Argo CD is not asked again")
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(policy)})
		Expect(err).NotTo(HaveOccurred())
		Expect(f.count("vis-mine")).To(Equal(1))
	})

	It("does not hide anything while Argo CD cannot be reached", func() {
		app := &unstructured.Unstructured{}
		app.SetGroupVersionKind(applicationGVK)
		app.SetName("vis-outage")
		app.SetNamespace(argoNS)
		create(app)
		create(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "vis-token2", Namespace: argoNS}, Data: map[string][]byte{"token": []byte("t")}})
		policy := &backflowv1alpha1.BackflowPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "vis-outage-policy", Namespace: argoNS},
			Spec: backflowv1alpha1.BackflowPolicySpec{
				ArgoCDNamespace: argoNS, Applications: backflowv1alpha1.ApplicationSelector{Names: []string{"vis-outage"}},
				ArgoCD: &backflowv1alpha1.ArgoCDServer{
					TokenSecretRef: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "vis-token2"}, Key: "token"},
				},
			},
		}
		create(policy)
		f := &fakeGetter{answers: map[string]error{"vis-outage": fmt.Errorf("%w: down", argocd.ErrUnreachable)}}
		r := &BackflowPolicyReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), ArgoCD: &ArgoCDDefaults{}, Visibility: visibilityWith(f, &now)}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(policy)})
		Expect(err).To(HaveOccurred(), "retried with a backoff; the status stays as it was")
	})

	It("is not Ready with the reason SecretForbidden when its token Secret cannot be read", func() {
		policy := &backflowv1alpha1.BackflowPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "vis-forbidden-policy", Namespace: argoNS},
			Spec: backflowv1alpha1.BackflowPolicySpec{
				ArgoCDNamespace: argoNS, Applications: backflowv1alpha1.ApplicationSelector{Names: []string{"anything"}},
				ArgoCD: &backflowv1alpha1.ArgoCDServer{
					TokenSecretRef: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "locked"}, Key: "token"},
				},
			},
		}
		create(policy)
		f := &fakeGetter{}
		r := &BackflowPolicyReconciler{
			Client: NewSecretClient(k8sClient, forbiddingReader{}, 0), Scheme: k8sClient.Scheme(),
			ArgoCD: &ArgoCDDefaults{}, Visibility: visibilityWith(f, &now),
		}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(policy)})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(policy), policy)).To(Succeed())
		Expect(meta.FindStatusCondition(policy.Status.Conditions, "Ready").Reason).To(Equal("SecretForbidden"))
	})

	It("does not see a parent Application its token may not get", func() {
		parent := &unstructured.Unstructured{}
		parent.SetGroupVersionKind(applicationGVK)
		parent.SetName("vis-parent")
		parent.SetNamespace(argoNS)
		Expect(unstructured.SetNestedSlice(parent.Object, []any{map[string]any{
			"group": "argoproj.io", "kind": "Application", "namespace": argoNS, "name": "vis-child"}}, "status", "resources")).To(Succeed())
		create(parent)
		child := &unstructured.Unstructured{}
		child.SetGroupVersionKind(applicationGVK)
		child.SetName("vis-child")
		child.SetNamespace(argoNS)
		child.SetAnnotations(map[string]string{"argocd.argoproj.io/tracking-id": "vis-parent:argoproj.io/Application:default/vis-child"})
		create(child)
		create(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "vis-token3", Namespace: argoNS}, Data: map[string][]byte{"token": []byte("t")}})
		policy := &backflowv1alpha1.BackflowPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "vis-parent-policy", Namespace: argoNS},
			Spec: backflowv1alpha1.BackflowPolicySpec{
				ArgoCDNamespace: argoNS, Applications: backflowv1alpha1.ApplicationSelector{Names: []string{"vis-child"}},
				ArgoCD: &backflowv1alpha1.ArgoCDServer{
					TokenSecretRef: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "vis-token3"}, Key: "token"},
				},
			},
		}
		create(policy)
		dp := &backflowv1alpha1.DriftProposal{Spec: backflowv1alpha1.DriftProposalSpec{PolicyName: policy.Name}}
		dp.Namespace = argoNS

		f := &fakeGetter{answers: map[string]error{"vis-parent": fmt.Errorf("%w: HTTP 403", argocd.ErrUnauthorized)}}
		r := &DriftProposalReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), ArgoCD: &ArgoCDDefaults{}, Visibility: visibilityWith(f, &now)}
		parents, hidden, err := r.parentsOf(ctx, dp, child)
		Expect(err).NotTo(HaveOccurred())
		Expect(hidden).To(BeTrue())
		Expect(parents).To(BeNil())

		f.answers["vis-parent"] = nil
		now = now.Add(DefaultVisibilityTTL + time.Second)
		parents, hidden, err = r.parentsOf(ctx, dp, child)
		Expect(err).NotTo(HaveOccurred())
		Expect(hidden).To(BeFalse())
		Expect(parents).To(HaveLen(1))
	})
})
