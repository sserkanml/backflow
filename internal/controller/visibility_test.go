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

	backflowv1beta1 "github.com/sserkanml/backflow/api/v1beta1"
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

func check(t *testing.T, v *ApplicationVisibility, cfg argocd.Config, name string) Visibility {
	t.Helper()
	got, err := v.Check(context.Background(), "ns/p", cfg, name, "argocd")
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return got
}

func TestVisibilityHidesWhatTheTokenMayNotGet(t *testing.T) {
	now := time.Now()
	f := &fakeGetter{answers: map[string]error{
		"forbidden": fmt.Errorf("%w: HTTP 403", argocd.ErrUnauthorized),
		"missing":   fmt.Errorf("%w: HTTP 404", argocd.ErrNotFound),
	}}
	v := visibilityWith(f, &now)
	cfg := argocd.Config{URL: "https://argocd", Token: "token-a"}
	for name, want := range map[string]Visibility{"mine": VisibilityVisible, "forbidden": VisibilityHidden, "missing": VisibilityHidden} {
		if got := check(t, v, cfg, name); got != want {
			t.Errorf("%s: %v, want %v", name, got, want)
		}
	}
}

func TestVisibilityIsCachedPerTokenAndApplicationForTheTTL(t *testing.T) {
	now := time.Now()
	f := &fakeGetter{}
	v := visibilityWith(f, &now)
	a, b := argocd.Config{URL: "https://argocd", Token: "token-a"}, argocd.Config{URL: "https://argocd", Token: "token-b"}

	for range 3 {
		check(t, v, a, "app")
	}
	if got := f.count("app"); got != 1 {
		t.Errorf("asked %d times within the TTL, want 1", got)
	}
	check(t, v, b, "app")
	if got := f.count("app"); got != 2 {
		t.Errorf("another token is another question: asked %d times, want 2", got)
	}
	now = now.Add(DefaultVisibilityTTL + time.Second)
	check(t, v, a, "app")
	if got := f.count("app"); got != 3 {
		t.Errorf("asked %d times after the TTL, want 3", got)
	}
}

func TestAnOutageKeepsWhatWasConfirmedAndShowsNothingElse(t *testing.T) {
	now := time.Now()
	down := fmt.Errorf("%w: down", argocd.ErrUnreachable)
	f := &fakeGetter{answers: map[string]error{"hidden": fmt.Errorf("%w: HTTP 403", argocd.ErrUnauthorized)}}
	v := visibilityWith(f, &now)
	cfg := argocd.Config{URL: "https://argocd", Token: "t"}

	// Confirmed while Argo CD was up: one visible, one refused.
	if check(t, v, cfg, "seen") != VisibilityVisible || check(t, v, cfg, "hidden") != VisibilityHidden {
		t.Fatal("setup")
	}

	// Argo CD goes down, and the answers are older than the TTL.
	f.answers["seen"], f.answers["hidden"], f.answers["new"] = down, down, down
	now = now.Add(10 * DefaultVisibilityTTL)

	if got := check(t, v, cfg, "seen"); got != VisibilityVisible {
		t.Errorf("an Application confirmed visible stays visible during an outage, got %v", got)
	}
	if got := check(t, v, cfg, "hidden"); got != VisibilityHidden {
		t.Errorf("an Application confirmed hidden stays hidden during an outage, got %v", got)
	}
	if got := check(t, v, cfg, "new"); got != VisibilityUnknown {
		t.Errorf("an Application never confirmed is unknown, not visible, got %v", got)
	}
	if got := check(t, v, cfg, "new"); got != VisibilityUnknown {
		t.Errorf("an unknown answer is not remembered as visible, got %v", got)
	}

	// Argo CD is back: the answers are asked for again, and a refusal now revokes.
	f.answers["seen"], f.answers["hidden"], f.answers["new"] = fmt.Errorf("%w: HTTP 403", argocd.ErrUnauthorized), nil, nil
	if got := check(t, v, cfg, "seen"); got != VisibilityHidden {
		t.Errorf("a definite refusal after the outage hides it, got %v", got)
	}
	if got := check(t, v, cfg, "new"); got != VisibilityVisible {
		t.Errorf("a first definite answer after the outage, got %v", got)
	}
}

func TestAnotherTokenDoesNotInheritConfirmedAnswers(t *testing.T) {
	now := time.Now()
	f := &fakeGetter{}
	v := visibilityWith(f, &now)
	a, b := argocd.Config{URL: "https://argocd", Token: "token-a"}, argocd.Config{URL: "https://argocd", Token: "token-b"}
	check(t, v, a, "app")
	f.answers = map[string]error{"app": fmt.Errorf("%w: down", argocd.ErrUnreachable)}
	now = now.Add(time.Hour)
	if got := check(t, v, b, "app"); got != VisibilityUnknown {
		t.Errorf("a token that never saw the Application: %v, want unknown", got)
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
		policy := &backflowv1beta1.BackflowPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "vis-policy", Namespace: argoNS},
			Spec: backflowv1beta1.BackflowPolicySpec{
				Mode:         backflowv1beta1.ModeReportOnly,
				Applications: backflowv1beta1.ApplicationSelector{Names: []string{"vis-mine", "vis-theirs-qx17"}},
				ArgoCD: backflowv1beta1.ArgoCDSpec{Namespace: argoNS, Server: &backflowv1beta1.ArgoCDServer{
					TokenSecretRef: backflowv1beta1.SecretKeyReference{Name: "vis-token", Key: "token"},
				}},
			},
		}
		create(policy)

		f := &fakeGetter{answers: map[string]error{"vis-theirs-qx17": fmt.Errorf("%w: HTTP 403", argocd.ErrUnauthorized)}}
		r := &BackflowPolicyReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), ArgoCD: &ArgoCDDefaults{}, Visibility: visibilityWith(f, &now)}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(policy)})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(policy), policy)).To(Succeed())

		Expect(meta.FindStatusCondition(policy.Status.Conditions, "Ready").Status).To(Equal(metav1.ConditionTrue))
		Expect(appNames(policy.Status)).To(ConsistOf("vis-mine"))
		Expect(policy.Status.ArgoCD.HiddenCount()).To(BeEquivalentTo(1))
		Expect(hiddenReason(policy.Status)).To(Equal("NotVisibleToToken"))
		Expect(fmt.Sprint(policy.Status)).NotTo(ContainSubstring("qx17"))

		By("asking again within the TTL: Argo CD is not asked again")
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(policy)})
		Expect(err).NotTo(HaveOccurred())
		Expect(f.count("vis-mine")).To(Equal(1))
	})

	Context("when Argo CD cannot be reached", func() {
		var policy *backflowv1beta1.BackflowPolicy
		down := fmt.Errorf("%w: down", argocd.ErrUnreachable)
		BeforeEach(func() {
			for _, n := range []string{"out-confirmed", "out-new-zx42"} {
				app := &unstructured.Unstructured{}
				app.SetGroupVersionKind(applicationGVK)
				app.SetName(n)
				app.SetNamespace(argoNS)
				create(app)
			}
			create(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "out-token", Namespace: argoNS}, Data: map[string][]byte{"token": []byte("t")}})
			policy = &backflowv1beta1.BackflowPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "out-policy", Namespace: argoNS},
				Spec: backflowv1beta1.BackflowPolicySpec{
					Mode:         backflowv1beta1.ModeReportOnly,
					Applications: backflowv1beta1.ApplicationSelector{Names: []string{"out-confirmed", "out-new-zx42"}},
					ArgoCD: backflowv1beta1.ArgoCDSpec{Namespace: argoNS, Server: &backflowv1beta1.ArgoCDServer{
						TokenSecretRef: backflowv1beta1.SecretKeyReference{Name: "out-token", Key: "token"},
					}},
				},
			}
			create(policy)
		})
		reconcileWith := func(v *ApplicationVisibility) (*backflowv1beta1.BackflowPolicy, reconcile.Result) {
			r := &BackflowPolicyReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), ArgoCD: &ArgoCDDefaults{}, Visibility: v}
			res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(policy)})
			ExpectWithOffset(1, err).NotTo(HaveOccurred())
			got := &backflowv1beta1.BackflowPolicy{}
			ExpectWithOffset(1, k8sClient.Get(ctx, client.ObjectKeyFromObject(policy), got)).To(Succeed())
			return got, res
		}

		It("keeps what was confirmed, shows nothing else, and says when it looks again", func() {
			f := &fakeGetter{answers: map[string]error{"out-new-zx42": down}}
			v := visibilityWith(f, &now)
			got, _ := reconcileWith(v) // only out-confirmed is asked successfully; out-new is down from the start
			Expect(appNames(got.Status)).To(ConsistOf("out-confirmed"))

			f.answers["out-confirmed"] = down
			now = now.Add(time.Hour)
			got, res := reconcileWith(v)
			Expect(appNames(got.Status)).To(ConsistOf("out-confirmed"), "confirmed earlier: still visible")
			Expect(got.Status.ArgoCD.HiddenCount()).To(BeEquivalentTo(1))
			Expect(hiddenReason(got.Status)).To(Equal("VisibilityUnknown"))
			Expect(res.RequeueAfter).To(Equal(visibilityRetry))
			ready := meta.FindStatusCondition(got.Status.Conditions, "Ready")
			Expect(ready.Status).To(Equal(metav1.ConditionTrue))
			Expect(ready.Message).To(And(ContainSubstring("could not be asked"), ContainSubstring("looking again in 30s")))
			Expect(fmt.Sprint(got.Status)).NotTo(ContainSubstring("zx42"), "no name of an Application that was never confirmed")
		})

		It("shows none of them, and is not Ready, when nothing was ever confirmed", func() {
			f := &fakeGetter{answers: map[string]error{"out-confirmed": down, "out-new-zx42": down}}
			got, res := reconcileWith(visibilityWith(f, &now))
			Expect(got.Status.Applications).To(BeEmpty())
			Expect(got.Status.ArgoCD.HiddenCount()).To(BeEquivalentTo(2))
			ready := meta.FindStatusCondition(got.Status.Conditions, "Ready")
			Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			Expect(ready.Reason).To(Equal("ApplicationVisibilityUnknown"))
			Expect(res.RequeueAfter).To(Equal(visibilityRetry))
			Expect(fmt.Sprint(got.Status)).NotTo(ContainSubstring("out-confirmed"))
		})

		It("never retires a proposal for it, and does for a definite refusal", func() {
			dp := &backflowv1beta1.DriftProposal{
				ObjectMeta: metav1.ObjectMeta{Namespace: argoNS, GenerateName: "out-dp-", Labels: map[string]string{labelPolicy: safeLabel(policy.Name)}},
				Spec: backflowv1beta1.DriftProposalSpec{
					PolicyRef:      backflowv1beta1.LocalObjectRef{Name: policy.Name},
					ApplicationRef: backflowv1beta1.ObjectRef{Name: "out-new-zx42", Namespace: argoNS},
					ResourceRef:    backflowv1beta1.ResourceRef{Version: "v1", Kind: "ConfigMap", Namespace: "demo", Name: "cm"},
					Sources: []backflowv1beta1.ProposalSource{{Position: 1, Role: backflowv1beta1.RoleRender, RepoURL: "https://example.com/r.git", Revision: "abc",
						Path: "apps/demo", Type: backflowv1beta1.SourceDirectory}},
					Changes:    []backflowv1beta1.FieldChange{{Path: "/data/K", Op: backflowv1beta1.OpReplace, Desired: `"a"`, Live: `"b"`}},
					DetectedAt: metav1.Now(),
				},
			}
			create(dp)
			dp.Status.Phase = backflowv1beta1.PhaseMapping
			Expect(k8sClient.Status().Update(ctx, dp)).To(Succeed())

			f := &fakeGetter{answers: map[string]error{"out-confirmed": down, "out-new-zx42": down}}
			v := visibilityWith(f, &now)
			reconcileWith(v)
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(dp), dp)).To(Succeed())
			Expect(dp.Status.Signal().AccessRevoked).To(BeFalse(), "an outage never retires a proposal")

			f.answers["out-new-zx42"] = fmt.Errorf("%w: HTTP 403", argocd.ErrUnauthorized)
			now = now.Add(time.Hour)
			reconcileWith(v)
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(dp), dp)).To(Succeed())
			Expect(dp.Status.Signal().AccessRevoked).To(BeTrue(), "a definite 403 does")
		})
	})

	It("is not Ready with the reason SecretForbidden when its token Secret cannot be read", func() {
		policy := &backflowv1beta1.BackflowPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "vis-forbidden-policy", Namespace: argoNS},
			Spec: backflowv1beta1.BackflowPolicySpec{
				Applications: backflowv1beta1.ApplicationSelector{Names: []string{"anything"}},
				ArgoCD: backflowv1beta1.ArgoCDSpec{Namespace: argoNS, Server: &backflowv1beta1.ArgoCDServer{
					TokenSecretRef: backflowv1beta1.SecretKeyReference{Name: "locked", Key: "token"},
				}},
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
		policy := &backflowv1beta1.BackflowPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "vis-parent-policy", Namespace: argoNS},
			Spec: backflowv1beta1.BackflowPolicySpec{
				Applications: backflowv1beta1.ApplicationSelector{Names: []string{"vis-child"}},
				ArgoCD: backflowv1beta1.ArgoCDSpec{Namespace: argoNS, Server: &backflowv1beta1.ArgoCDServer{
					TokenSecretRef: backflowv1beta1.SecretKeyReference{Name: "vis-token3", Key: "token"},
				}},
			},
		}
		create(policy)
		dp := &backflowv1beta1.DriftProposal{Spec: backflowv1beta1.DriftProposalSpec{PolicyRef: backflowv1beta1.LocalObjectRef{Name: policy.Name}}}
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
