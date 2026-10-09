package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

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
	"github.com/sserkanml/backflow/internal/gitrepo"
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

func TestGitAheadSettling(t *testing.T) {
	at := func(h, m, sec int) string { return time.Date(2026, 3, 4, h, m, sec, 0, time.UTC).Format(time.RFC3339) }
	app := func(mutate func(o map[string]interface{})) *unstructured.Unstructured {
		o := map[string]interface{}{}
		_ = unstructured.SetNestedField(o, "bbb", "status", "sync", "revision")
		_ = unstructured.SetNestedField(o, "bbb", "status", "operationState", "syncResult", "revision")
		if mutate != nil {
			mutate(o)
		}
		return &unstructured.Unstructured{Object: o}
	}
	tests := []struct {
		name     string
		mutate   func(o map[string]interface{})
		settling bool
	}{
		{"idle", nil, false},
		{"a sync is running", func(o map[string]interface{}) {
			_ = unstructured.SetNestedField(o, "Running", "status", "operationState", "phase")
		}, true},
		{"a sync is being cancelled", func(o map[string]interface{}) {
			_ = unstructured.SetNestedField(o, "Terminating", "status", "operationState", "phase")
		}, true},
		{"a sync is requested", func(o map[string]interface{}) {
			_ = unstructured.SetNestedField(o, map[string]interface{}{"sync": map[string]interface{}{}}, "operation")
		}, true},
		{"finished sync, not compared again yet", func(o map[string]interface{}) {
			_ = unstructured.SetNestedField(o, "Succeeded", "status", "operationState", "phase")
			_ = unstructured.SetNestedField(o, at(10, 0, 5), "status", "operationState", "finishedAt")
			_ = unstructured.SetNestedField(o, at(10, 0, 3), "status", "reconciledAt")
		}, true},
		{"compared after the sync", func(o map[string]interface{}) {
			_ = unstructured.SetNestedField(o, "Succeeded", "status", "operationState", "phase")
			_ = unstructured.SetNestedField(o, at(10, 0, 5), "status", "operationState", "finishedAt")
			_ = unstructured.SetNestedField(o, at(10, 0, 6), "status", "reconciledAt")
		}, false},
		{"compared in the same second", func(o map[string]interface{}) {
			_ = unstructured.SetNestedField(o, at(10, 0, 5), "status", "operationState", "finishedAt")
			_ = unstructured.SetNestedField(o, at(10, 0, 5), "status", "reconciledAt")
		}, false},
		{"times missing or unreadable: nothing to judge", func(o map[string]interface{}) {
			_ = unstructured.SetNestedField(o, "yesterday", "status", "operationState", "finishedAt")
			_ = unstructured.SetNestedField(o, at(10, 0, 3), "status", "reconciledAt")
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := gitAhead(app(tt.mutate)).settling; got != tt.settling {
				t.Errorf("settling = %v, want %v", got, tt.settling)
			}
		})
	}
}

func TestCountUnderPath(t *testing.T) {
	tests := []struct {
		name    string
		changed []string
		path    string
		recurse bool
		want    int
	}{
		{"file directly in the directory", []string{"apps/demo/configmap.yaml"}, "apps/demo", false, 1},
		{"unrelated file", []string{"README.md"}, "apps/demo", false, 0},
		{"sibling with the same prefix", []string{"apps/demo2/x.yaml", "apps/demo-extra/y.yaml", "apps/demos"}, "apps/demo", true, 0},
		{"parent directory file", []string{"apps/x.yaml"}, "apps/demo", true, 0},
		{"nested file without recurse is not part of the source", []string{"apps/demo/sub/x.yaml"}, "apps/demo", false, 0},
		{"nested file with recurse is", []string{"apps/demo/sub/x.yaml"}, "apps/demo", true, 1},
		{"counts each file", []string{"apps/demo/a.yaml", "apps/demo/b.yaml", "README.md"}, "apps/demo", false, 2},
		{"path with ./ and trailing slash", []string{"apps/demo/a.yaml"}, "./apps/demo/", false, 1},
		{"path with a leading slash", []string{"apps/demo/a.yaml"}, "/apps/demo", false, 1},
		{"the directory itself replaced by a file", []string{"apps/demo"}, "apps/demo", false, 1},
		{"repository root, top-level file", []string{"a.yaml"}, ".", false, 1},
		{"repository root, nested file without recurse", []string{"sub/a.yaml"}, "", false, 0},
		{"repository root, nested file with recurse", []string{"sub/a.yaml"}, "", true, 1},
		{"nothing changed", nil, "apps/demo", true, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := countUnderPath(tt.changed, tt.path, tt.recurse); got != tt.want {
				t.Errorf("countUnderPath(%v, %q, %v) = %d, want %d", tt.changed, tt.path, tt.recurse, got, tt.want)
			}
		})
	}
}

func TestIsFullSHA(t *testing.T) {
	for in, want := range map[string]bool{
		strings.Repeat("a", 40): true, strings.Repeat("0", 40): true,
		strings.Repeat("A", 40): false, strings.Repeat("a", 39): false, "main": false, "": false,
		strings.Repeat("g", 40): false,
	} {
		if got := isFullSHA(in); got != want {
			t.Errorf("isFullSHA(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestGitAhead(t *testing.T) {
	app := func(compared, synced string, multi bool) *unstructured.Unstructured {
		o := &unstructured.Unstructured{Object: map[string]interface{}{}}
		set := func(v string, path ...string) {
			if v == "" {
				return
			}
			if multi {
				path[len(path)-1] += "s"
				_ = unstructured.SetNestedStringSlice(o.Object, strings.Split(v, ","), path...)
				return
			}
			_ = unstructured.SetNestedField(o.Object, v, path...)
		}
		set(compared, "status", "sync", "revision")
		set(synced, "status", "operationState", "syncResult", "revision")
		return o
	}
	tests := []struct {
		name             string
		compared, synced string
		multi            bool
		paused           bool
	}{
		{"caught up", "aaa", "aaa", false, false},
		{"git ahead", "bbb", "aaa", false, true},
		{"never synced", "bbb", "", false, true},
		{"nothing compared yet", "", "aaa", false, false},
		{"nothing at all", "", "", false, false},
		{"multi source caught up", "a,b", "a,b", true, false},
		{"multi source, one source ahead", "a,c", "a,b", true, true},
		{"multi source never synced", "a,b", "", true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := gitAhead(app(tt.compared, tt.synced, tt.multi))
			if got.paused != tt.paused {
				t.Errorf("paused = %v, want %v (%+v)", got.paused, tt.paused, got)
			}
		})
	}
}

func TestShortRevision(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef01234567"
	for in, want := range map[string]string{
		sha:             "0123456",
		sha + "," + sha: "0123456,0123456",
		"main":          "main",
		"v1.2.3":        "v1.2.3",
		"":              "",
	} {
		if got := shortRevision(in); got != want {
			t.Errorf("shortRevision(%q) = %q, want %q", in, got, want)
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

type fakeDiffer struct {
	paths []string
	err   error
	calls int
	last  [2]string
	// linked lists the revisions that have a symbolic link below the directory.
	linked      map[string]bool
	symlinkErr  error
	symlinkCall int
}

func (f *fakeDiffer) HasSymlink(_ context.Context, _, sha, _ string, _ *gitrepo.Auth) (bool, error) {
	f.symlinkCall++
	return f.linked[sha], f.symlinkErr
}

func (f *fakeDiffer) ChangedPaths(_ context.Context, _, from, to string, _ *gitrepo.Auth) ([]string, error) {
	f.calls++
	f.last = [2]string{from, to}
	return f.paths, f.err
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
	// setRevisions sets the revision Argo CD compares against and the revision
	// of its last sync, like Argo CD does. An empty synced means never synced.
	setRevisions := func(compared, synced string) {
		if compared != "" {
			ExpectWithOffset(1, unstructured.SetNestedField(app.Object, compared, "status", "sync", "revision")).To(Succeed())
		}
		if synced != "" {
			ExpectWithOffset(1, unstructured.SetNestedField(app.Object, synced, "status", "operationState", "syncResult", "revision")).To(Succeed())
		} else {
			unstructured.RemoveNestedField(app.Object, "status", "operationState")
		}
		ExpectWithOffset(1, k8sClient.Update(ctx, app)).To(Succeed())
	}
	configMapRes := func(status string) map[string]interface{} {
		return map[string]interface{}{
			"version": "v1", "kind": "ConfigMap", "namespace": "demo", "name": "demo-config", "status": status,
		}
	}
	// The drift controller reads time from this clock, which tests move.
	var (
		clockBase   time.Time
		clockOffset time.Duration
	)
	clock := func() time.Time { return clockBase.Add(clockOffset) }
	advance := func(d time.Duration) { clockOffset += d }
	// settle moves the clock past the batch window of the policy.
	settle := func() { advance(policy.Spec.EffectiveBatchWindow() + time.Second) }

	// reconcileDrift runs only the drift controller, which never writes status.
	reconcileDrift := func() {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: policy.Name}})
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
	}
	// reconcileOnce runs the drift controller, then the DriftProposal
	// controller for every proposal, like the manager would.
	reconcileOnce := func() {
		// A drift becomes a proposal after two observations a batch window
		// apart, so this looks twice with the clock moved on in between.
		reconcileDrift()
		settle()
		reconcileDrift()
		var list backflowv1alpha1.DriftProposalList
		ExpectWithOffset(1, k8sClient.List(ctx, &list, client.InNamespace(ns),
			client.MatchingLabels{labelPolicy: policy.Name})).To(Succeed())
		pr := &DriftProposalReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		for _, p := range list.Items {
			_, err := pr.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&p)})
			ExpectWithOffset(1, err).NotTo(HaveOccurred())
		}
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
		// The last sync is at the revision Argo CD compares against.
		setRevisions("abc123", "abc123")
		setResources(configMapRes("OutOfSync"))

		argo = &fakeArgo{items: []argocd.ManagedResource{{
			Kind: "ConfigMap", Namespace: "demo", Name: "demo-config",
			PredictedLiveState: configMapState("info"), NormalizedLiveState: configMapState("debug"),
		}}}
		recorder = events.NewFakeRecorder(50)
		clockBase, clockOffset = time.Now(), 0
		r = &DriftReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: recorder, Now: clock,
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
		By("creating the proposal without touching its status")
		reconcileDrift()
		Expect(proposals()).To(BeEmpty(), "seen once is not enough")
		settle()
		reconcileDrift()
		Expect(proposals()).To(HaveLen(1))
		Expect(proposals()[0].Status.Phase).To(BeEmpty())

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
		Expect(superseded[0].Annotations).To(HaveKeyWithValue(annotationSupersededBy, detected[0].Name))
		Expect(superseded[0].Spec).To(Equal(first.Spec), "spec of an existing proposal must not change")

		By("reverting once the resource is Synced again")
		setResources(configMapRes("Synced"))
		reconcileOnce()
		Expect(byPhase(backflowv1alpha1.PhaseReverted)).To(HaveLen(1))
		Expect(byPhase(backflowv1alpha1.PhaseReverted)[0].Annotations).To(HaveKey(annotationReverted))
		Expect(byPhase(backflowv1alpha1.PhaseDetected)).To(BeEmpty())

		By("creating a fresh proposal when the same drift comes back")
		argo.items[0].NormalizedLiveState = configMapState("debug")
		setResources(configMapRes("OutOfSync"))
		reconcileOnce()
		Expect(proposals()).To(HaveLen(3))
		Expect(byPhase(backflowv1alpha1.PhaseDetected)).To(HaveLen(1))
	})

	Context("a drift that was already handled and ended without a fix", func() {
		// retire puts the only proposal into a terminal phase, as its controller would.
		retire := func(phase backflowv1alpha1.ProposalPhase, mutate func(st *backflowv1alpha1.DriftProposalStatus)) {
			p := proposals()[0]
			p.Status.Phase = phase
			if mutate != nil {
				mutate(&p.Status)
			}
			ExpectWithOffset(1, k8sClient.Status().Update(ctx, &p)).To(Succeed())
		}
		rejected := func(st *backflowv1alpha1.DriftProposalStatus) {
			st.MergeRequest = &backflowv1alpha1.MergeRequestRef{URL: "https://git.test/-/merge_requests/7", Number: 7, State: "closed"}
		}
		drainEvents := func() []string {
			var out []string
			for {
				select {
				case e := <-recorder.Events:
					out = append(out, e)
				default:
					return out
				}
			}
		}
		count := func(events []string, reason string) int {
			n := 0
			for _, e := range events {
				if strings.Contains(e, reason) {
					n++
				}
			}
			return n
		}
		setRevision := func(rev string) {
			ExpectWithOffset(1, k8sClient.Get(ctx, client.ObjectKeyFromObject(policy), policy)).To(Succeed())
			policy.Status.Applications[0].SyncedRevision = rev
			ExpectWithOffset(1, k8sClient.Status().Update(ctx, policy)).To(Succeed())
		}

		BeforeEach(func() {
			reconcileOnce()
			Expect(proposals()).To(HaveLen(1))
		})

		It("does not reopen a Rejected proposal, and reports the rejection once", func() {
			retire(backflowv1alpha1.PhaseRejected, rejected)
			drainEvents()

			var seen []string
			for i := 0; i < 4; i++ {
				reconcileOnce()
				seen = append(seen, drainEvents()...)
			}
			Expect(proposals()).To(HaveLen(1), "no new proposal and no new merge request")
			Expect(count(seen, "ChangeRejected")).To(Equal(1), "reported once, not on every cycle")
			Expect(seen).To(ContainElement(And(
				ContainSubstring("Normal"), ContainSubstring("ChangeRejected"),
				ContainSubstring("merge_requests/7"), ContainSubstring("Sync Application drift-app in Argo CD"))))
			Expect(proposals()[0].Annotations).To(HaveKey(annotationRejectionReported))
		})

		It("does not reopen a Failed proposal, and stays quiet about it", func() {
			retire(backflowv1alpha1.PhaseFailed, func(st *backflowv1alpha1.DriftProposalStatus) {
				st.Message = "targetRevision is not a branch"
			})
			drainEvents()
			for i := 0; i < 3; i++ {
				reconcileOnce()
			}
			Expect(proposals()).To(HaveLen(1))
			Expect(count(drainEvents(), "ChangeRejected")).To(BeZero())
		})

		It("does not reopen a proposal superseded because Git changed", func() {
			retire(backflowv1alpha1.PhaseSuperseded, func(st *backflowv1alpha1.DriftProposalStatus) {
				st.Message = "The source changed in Git after the drift was detected."
			})
			for i := 0; i < 3; i++ {
				reconcileOnce()
			}
			Expect(proposals()).To(HaveLen(1))
		})

		It("proposes again when the changes differ", func() {
			retire(backflowv1alpha1.PhaseRejected, rejected)
			reconcileOnce()
			Expect(proposals()).To(HaveLen(1))

			argo.items[0].NormalizedLiveState = configMapState("trace")
			reconcileOnce()
			Expect(proposals()).To(HaveLen(2))
			Expect(byPhase(backflowv1alpha1.PhaseDetected)).To(HaveLen(1))
		})

		It("proposes again when Argo CD synced a new revision", func() {
			retire(backflowv1alpha1.PhaseRejected, rejected)
			reconcileOnce()
			Expect(proposals()).To(HaveLen(1))

			setRevision("def456")
			reconcileOnce()
			Expect(proposals()).To(HaveLen(2))
			fresh := byPhase(backflowv1alpha1.PhaseDetected)
			Expect(fresh).To(HaveLen(1))
			Expect(fresh[0].Spec.Source.Revision).To(Equal("def456"))
		})

		It("proposes again for a Failed or Git-superseded one when a new revision is synced", func() {
			retire(backflowv1alpha1.PhaseFailed, nil)
			setRevision("def456")
			reconcileOnce()
			Expect(proposals()).To(HaveLen(2))
		})

		It("reports a second rejection of a different drift again", func() {
			retire(backflowv1alpha1.PhaseRejected, rejected)
			reconcileOnce()
			Expect(count(drainEvents(), "ChangeRejected")).To(Equal(1))

			argo.items[0].NormalizedLiveState = configMapState("trace")
			reconcileOnce()
			second := byPhase(backflowv1alpha1.PhaseDetected)
			Expect(second).To(HaveLen(1))
			second[0].Status.Phase = backflowv1alpha1.PhaseRejected
			Expect(k8sClient.Status().Update(ctx, &second[0])).To(Succeed())
			drainEvents()

			reconcileOnce()
			Expect(count(drainEvents(), "ChangeRejected")).To(Equal(1), "once for the new proposal, none for the old")
			reconcileOnce()
			Expect(count(drainEvents(), "ChangeRejected")).To(BeZero())
		})

		It("does not let a Merged proposal block the same drift coming back", func() {
			retire(backflowv1alpha1.PhaseMerged, nil)
			reconcileOnce()
			Expect(proposals()).To(HaveLen(2))
			Expect(byPhase(backflowv1alpha1.PhaseDetected)).To(HaveLen(1))
		})

		It("does not let a Reverted proposal block the same drift coming back", func() {
			retire(backflowv1alpha1.PhaseReverted, nil)
			reconcileOnce()
			Expect(proposals()).To(HaveLen(2))
		})

		It("does not let a proposal superseded by a newer drift block its own changes coming back", func() {
			By("a newer drift supersedes the first")
			argo.items[0].NormalizedLiveState = configMapState("trace")
			reconcileOnce()
			Expect(byPhase(backflowv1alpha1.PhaseSuperseded)).To(HaveLen(1))
			Expect(byPhase(backflowv1alpha1.PhaseSuperseded)[0].Status.SupersededBy).NotTo(BeEmpty())

			By("the original drift returns")
			argo.items[0].NormalizedLiveState = configMapState("debug")
			reconcileOnce()
			Expect(proposals()).To(HaveLen(3))
			Expect(byPhase(backflowv1alpha1.PhaseDetected)).To(HaveLen(1))
		})
	})

	Context("Git is ahead of the last sync of the Application", func() {
		eventsOf := func(reason string) []string {
			var out []string
			for {
				select {
				case e := <-recorder.Events:
					if strings.Contains(e, reason) {
						out = append(out, e)
					}
				default:
					return out
				}
			}
		}
		const sha1 = "1111111111111111111111111111111111111111"
		const sha2 = "2222222222222222222222222222222222222222"

		It("creates no proposal and says so once", func() {
			setRevisions(sha2, sha1) // a commit that the Application has not synced yet
			for i := 0; i < 3; i++ {
				reconcileOnce()
			}
			Expect(proposals()).To(BeEmpty(), "an OutOfSync resource may only differ because of the pending commit")
			got := eventsOf("GitAhead")
			Expect(got).To(HaveLen(1), "one Event for this combination of revisions")
			Expect(got[0]).To(And(ContainSubstring("Normal"),
				ContainSubstring("Git is ahead of the last sync of drift-app (2222222 vs 1111111); drift detection for it is paused until Argo CD syncs.")))
		})

		It("reports a new combination of revisions again", func() {
			setRevisions(sha2, sha1)
			reconcileOnce()
			Expect(eventsOf("GitAhead")).To(HaveLen(1))

			setRevisions("3333333333333333333333333333333333333333", sha1)
			reconcileOnce()
			Expect(eventsOf("GitAhead")).To(HaveLen(1))
		})

		It("says nothing when nothing is OutOfSync", func() {
			setResources(configMapRes("Synced"))
			setRevisions(sha2, sha1)
			reconcileOnce()
			Expect(eventsOf("GitAhead")).To(BeEmpty(), "an unrelated commit must not make noise")
		})

		It("proposes again once the sync has caught up and a real drift is there", func() {
			setRevisions(sha2, sha1)
			reconcileOnce()
			Expect(proposals()).To(BeEmpty())
			eventsOf("GitAhead")

			setRevisions(sha2, sha2)
			reconcileOnce()
			Expect(proposals()).To(HaveLen(1))
			Expect(eventsOf("GitAhead")).To(BeEmpty())
		})

		It("creates no proposal for an Application that was never synced", func() {
			setRevisions(sha2, "")
			reconcileOnce()
			Expect(proposals()).To(BeEmpty())
			got := eventsOf("GitAhead")
			Expect(got).To(HaveLen(1))
			Expect(got[0]).To(ContainSubstring("(2222222 vs never synced)"))
		})

		It("judges nothing when Argo CD reports no compared revision", func() {
			ExpectWithOffset(1, unstructured.SetNestedField(app.Object, "", "status", "sync", "revision")).To(Succeed())
			Expect(k8sClient.Update(ctx, app)).To(Succeed())
			reconcileOnce()
			Expect(proposals()).To(HaveLen(1))
		})

		It("leaves an open proposal as it is, and neither supersedes nor duplicates it", func() {
			reconcileOnce()
			Expect(proposals()).To(HaveLen(1))
			first := proposals()[0]

			By("the same drift while Git is ahead")
			setRevisions(sha2, sha1)
			reconcileOnce()
			Expect(proposals()).To(HaveLen(1))

			By("a different drift while Git is ahead")
			argo.items[0].NormalizedLiveState = configMapState("trace")
			reconcileOnce()
			Expect(proposals()).To(HaveLen(1), "a different drift is not proposed, so nothing is superseded")
			Expect(proposals()[0].Name).To(Equal(first.Name))
			Expect(proposals()[0].Annotations).NotTo(HaveKey(annotationSupersededBy))
			Expect(proposals()[0].Annotations).NotTo(HaveKey(annotationReverted))

			By("it is retired as usual when the resource is in sync again")
			setResources(configMapRes("Synced"))
			reconcileOnce()
			Expect(byPhase(backflowv1alpha1.PhaseReverted)).To(HaveLen(1))
		})

		It("follows multi-source Applications by their lists of revisions", func() {
			setRevisions("", "")
			unstructured.RemoveNestedField(app.Object, "status", "sync", "revision")
			Expect(unstructured.SetNestedStringSlice(app.Object, []string{sha1, "feedface"}, "status", "sync", "revisions")).To(Succeed())
			Expect(unstructured.SetNestedStringSlice(app.Object, []string{sha1, "feedface"}, "status", "operationState", "syncResult", "revisions")).To(Succeed())
			Expect(k8sClient.Update(ctx, app)).To(Succeed())
			reconcileOnce()
			Expect(proposals()).To(HaveLen(1), "same revisions: not ahead")

			Expect(unstructured.SetNestedStringSlice(app.Object, []string{sha2, "feedface"}, "status", "sync", "revisions")).To(Succeed())
			Expect(k8sClient.Update(ctx, app)).To(Succeed())
			Expect(k8sClient.Delete(ctx, &proposals()[0])).To(Succeed())
			reconcileOnce()
			Expect(proposals()).To(BeEmpty(), "the first source moved ahead")
		})
	})

	Context("Git is ahead and the Application's directory is checked", func() {
		const sha1 = "1111111111111111111111111111111111111111"
		const sha2 = "2222222222222222222222222222222222222222"
		var differ *fakeDiffer
		gitAheadEvents := func() []string {
			var out []string
			for {
				select {
				case e := <-recorder.Events:
					if strings.Contains(e, "GitAhead") {
						out = append(out, e)
					}
				default:
					return out
				}
			}
		}
		setSourceType := func(t string) {
			ExpectWithOffset(1, k8sClient.Get(ctx, client.ObjectKeyFromObject(policy), policy)).To(Succeed())
			policy.Status.Applications[0].SourceType = t
			ExpectWithOffset(1, k8sClient.Status().Update(ctx, policy)).To(Succeed())
		}

		BeforeEach(func() {
			differ = &fakeDiffer{}
			r.Git = differ
			setRevisions(sha2, sha1) // a commit the Application has not synced
		})

		It("proposes for a real drift when the commit is outside the directory", func() {
			differ.paths = []string{"README.md", "apps/other/deployment.yaml"}
			reconcileOnce()
			Expect(proposals()).To(HaveLen(1), "an unrelated commit must not pause detection")
			Expect(gitAheadEvents()).To(BeEmpty())
			Expect(differ.calls).To(Equal(1))
			Expect(differ.last).To(Equal([2]string{sha1, sha2}), "from the last sync to the compared revision")
		})

		It("pauses, and says how many files changed, when the commit is under the directory", func() {
			differ.paths = []string{"README.md", "apps/demo/configmap.yaml", "apps/demo/deployment.yaml"}
			reconcileOnce()
			Expect(proposals()).To(BeEmpty())
			got := gitAheadEvents()
			Expect(got).To(HaveLen(1))
			Expect(got[0]).To(ContainSubstring(
				"Git is ahead of the last sync of drift-app and 2 file(s) under apps/demo changed (2222222 vs 1111111); drift detection for it is paused until Argo CD syncs."))
		})

		It("does not count a sibling folder that shares the prefix", func() {
			differ.paths = []string{"apps/demo2/configmap.yaml", "apps/demo-extra/x.yaml"}
			reconcileOnce()
			Expect(proposals()).To(HaveLen(1))
			Expect(gitAheadEvents()).To(BeEmpty())
		})

		It("counts nested files only with directory.recurse", func() {
			differ.paths = []string{"apps/demo/sub/x.yaml"}
			reconcileOnce()
			Expect(proposals()).To(HaveLen(1), "without recurse the nested file is not part of the source")

			Expect(k8sClient.Delete(ctx, &proposals()[0])).To(Succeed())
			Expect(unstructured.SetNestedField(app.Object, true, "spec", "source", "directory", "recurse")).To(Succeed())
			Expect(k8sClient.Update(ctx, app)).To(Succeed())
			reconcileOnce()
			Expect(proposals()).To(BeEmpty())
			Expect(gitAheadEvents()).To(HaveLen(1))
		})

		DescribeTable("pauses whenever the directory cannot be trusted to hold everything",
			func(prepare func(), wantCalls int, wantMessage string) {
				differ.paths = []string{"README.md"} // outside the directory, so a check would let it through
				prepare()
				reconcileOnce()
				Expect(proposals()).To(BeEmpty())
				Expect(differ.calls).To(Equal(wantCalls))
				got := gitAheadEvents()
				Expect(got).To(HaveLen(1))
				Expect(got[0]).To(ContainSubstring(wantMessage))
				Expect(got[0]).NotTo(ContainSubstring("file(s) under"))
			},
			Entry("Kustomize", func() { setSourceType("Kustomize") }, 0, "Git is ahead of the last sync of drift-app ("),
			Entry("Helm", func() { setSourceType("Helm") }, 0, "Git is ahead of the last sync of drift-app ("),
			Entry("several sources", func() {
				Expect(unstructured.SetNestedSlice(app.Object, []interface{}{map[string]interface{}{"path": "apps/demo"}}, "spec", "sources")).To(Succeed())
				Expect(k8sClient.Update(ctx, app)).To(Succeed())
			}, 0, "Git is ahead of the last sync of drift-app ("),
			Entry("never synced", func() { setRevisions(sha2, "") }, 0, "(2222222 vs never synced)"),
			Entry("a compared revision that is not a commit", func() { setRevisions("main", sha1) }, 0, "(main vs 1111111)"),
			// reconcileOnce looks twice, a batch window apart; a failed look is only remembered for 30s.
			Entry("an unreadable repository", func() { differ.err = errors.New("repository unavailable") }, 2, "(2222222 vs 1111111)"),
			Entry("no way to read the repository at all", func() { r.Git = nil }, 0, "(2222222 vs 1111111)"),
		)

		It("pauses when a symbolic link is below the directory at either revision, as for an unreadable repository", func() {
			differ.paths = []string{"README.md"} // would let the commit through
			differ.linked = map[string]bool{sha2: true}
			reconcileOnce()
			Expect(proposals()).To(BeEmpty())
			got := gitAheadEvents()
			Expect(got).To(HaveLen(1))
			Expect(got[0]).To(ContainSubstring("Git is ahead of the last sync of drift-app (2222222 vs 1111111); drift detection for it is paused until Argo CD syncs."))
			Expect(got[0]).NotTo(ContainSubstring("file(s) under"))
			Expect(differ.calls).To(BeZero(), "no diff is trusted once a link is found")

			By("at the last synced revision too, and the answer is remembered")
			differ.linked = map[string]bool{sha1: true}
			setRevisions("3333333333333333333333333333333333333333", sha1)
			reconcileOnce()
			reconcileOnce()
			Expect(proposals()).To(BeEmpty())
			Expect(differ.calls).To(BeZero())
			Expect(gitAheadEvents()).To(HaveLen(1))
			calls := differ.symlinkCall
			reconcileOnce()
			Expect(differ.symlinkCall).To(Equal(calls), "a link does not go away by looking again")
		})

		It("pauses when the symlink check itself fails", func() {
			differ.paths = []string{"README.md"}
			differ.symlinkErr = errors.New("repository unavailable")
			reconcileOnce()
			Expect(proposals()).To(BeEmpty())
			Expect(gitAheadEvents()).To(HaveLen(1))
			Expect(differ.calls).To(BeZero())
		})

		It("looks for links before it looks at paths", func() {
			differ.paths = []string{"README.md"}
			reconcileOnce()
			Expect(differ.symlinkCall).To(Equal(2), "the synced and the compared revision")
			Expect(proposals()).To(HaveLen(1))
		})

		It("looks at the repository once per combination of revisions", func() {
			differ.paths = []string{"README.md"}
			for i := 0; i < 4; i++ {
				reconcileOnce()
			}
			Expect(differ.calls).To(Equal(1))

			setRevisions("3333333333333333333333333333333333333333", sha1)
			reconcileOnce()
			Expect(differ.calls).To(Equal(2), "another compared revision is another look")
		})

		It("tries an unreadable repository again, but not at once", func() {
			differ.err = errors.New("repository unavailable")

			reconcileDrift()
			advance(5 * time.Second)
			reconcileDrift()
			Expect(differ.calls).To(Equal(1), "a failure is remembered for a moment")
			Expect(proposals()).To(BeEmpty())

			advance(aheadErrorTTL + time.Second)
			differ.err = nil
			differ.paths = []string{"README.md"}
			reconcileDrift() // looks again; the commit is outside the directory, so the drift is first seen now
			Expect(differ.calls).To(Equal(2))
			settle()
			reconcileDrift()
			Expect(proposals()).To(HaveLen(1), "once readable, an unrelated commit does not pause")
		})
	})

	Context("an Application whose sync has just finished", func() {
		const sha = "1111111111111111111111111111111111111111"
		setOperation := func(phase, finishedAt, reconciledAt string) {
			ExpectWithOffset(1, unstructured.SetNestedField(app.Object, sha, "status", "sync", "revision")).To(Succeed())
			ExpectWithOffset(1, unstructured.SetNestedField(app.Object, sha, "status", "operationState", "syncResult", "revision")).To(Succeed())
			if phase != "" {
				ExpectWithOffset(1, unstructured.SetNestedField(app.Object, phase, "status", "operationState", "phase")).To(Succeed())
			}
			if finishedAt != "" {
				ExpectWithOffset(1, unstructured.SetNestedField(app.Object, finishedAt, "status", "operationState", "finishedAt")).To(Succeed())
			}
			if reconciledAt != "" {
				ExpectWithOffset(1, unstructured.SetNestedField(app.Object, reconciledAt, "status", "reconciledAt")).To(Succeed())
			}
			ExpectWithOffset(1, k8sClient.Update(ctx, app)).To(Succeed())
		}

		It("does not propose for a status that was stale for a moment after the sync", func() {
			setOperation("Succeeded", "2026-03-04T10:00:05Z", "2026-03-04T10:00:06Z")
			reconcileDrift() // first seen: Argo CD still shows the resource OutOfSync
			advance(8 * time.Second)
			setResources(configMapRes("Synced")) // the next comparison catches up
			reconcileDrift()
			settle()
			reconcileDrift()
			Expect(proposals()).To(BeEmpty(), "it never stayed the same for the whole window")
		})

		It("makes no proposal while the sync runs or its result is not compared yet, and no noise either", func() {
			setOperation("Running", "", "")
			reconcileOnce()
			Expect(proposals()).To(BeEmpty())

			setOperation("Succeeded", "2026-03-04T10:00:05Z", "2026-03-04T10:00:03Z")
			reconcileOnce()
			Expect(proposals()).To(BeEmpty(), "the resource statuses still describe the state before the sync")

			for {
				select {
				case e := <-recorder.Events:
					Expect(e).NotTo(ContainSubstring("GitAhead"), "this is not Git being ahead")
				default:
					goto drained
				}
			}
		drained:
			setOperation("Succeeded", "2026-03-04T10:00:05Z", "2026-03-04T10:00:06Z")
			reconcileOnce()
			Expect(proposals()).To(HaveLen(1), "compared after the sync: a real drift is proposed")
		})
	})

	Context("a drift has to stay the same before it becomes a proposal", func() {
		window := func() time.Duration { return policy.Spec.EffectiveBatchWindow() }
		resultOf := func() reconcile.Result {
			res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: policy.Name}})
			ExpectWithOffset(1, err).NotTo(HaveOccurred())
			return res
		}

		It("uses 30s for a policy built in Go without the field", func() {
			Expect(policy.Spec.BatchWindow).NotTo(BeNil(), "the API server filled in its default; omitempty let it")
			Expect(policy.Spec.BatchWindow.Duration).To(Equal(30 * time.Second))
			Expect(window()).To(Equal(30 * time.Second))

			By("and for one that never went through the API server")
			Expect(backflowv1alpha1.BackflowPolicySpec{}.EffectiveBatchWindow()).To(Equal(backflowv1alpha1.DefaultBatchWindow))
		})

		It("keeps an explicit 0s", func() {
			zero := &backflowv1alpha1.BackflowPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "window-zero", Namespace: ns},
				Spec: backflowv1alpha1.BackflowPolicySpec{
					Applications: backflowv1alpha1.ApplicationSelector{Names: []string{"x"}},
					BatchWindow:  &metav1.Duration{},
				},
			}
			Expect(k8sClient.Create(ctx, zero)).To(Succeed())
			defer func() { Expect(k8sClient.Delete(ctx, zero)).To(Succeed()) }()
			var got backflowv1alpha1.BackflowPolicy
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(zero), &got)).To(Succeed())
			Expect(got.Spec.BatchWindow).NotTo(BeNil())
			Expect(got.Spec.EffectiveBatchWindow()).To(BeZero(), "0s is a choice, not a missing value")
		})

		It("defaults the window for a manifest that leaves it out", func() {
			raw := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "backflow.io/v1alpha1", "kind": "BackflowPolicy",
				"metadata": map[string]interface{}{"name": "window-default", "namespace": ns},
				"spec": map[string]interface{}{
					"applications": map[string]interface{}{"names": []interface{}{"x"}},
				},
			}}
			Expect(k8sClient.Create(ctx, raw)).To(Succeed())
			defer func() { Expect(k8sClient.Delete(ctx, raw)).To(Succeed()) }()
			var got backflowv1alpha1.BackflowPolicy
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(raw), &got)).To(Succeed())
			Expect(got.Spec.EffectiveBatchWindow()).To(Equal(30 * time.Second))
		})

		It("makes no proposal for a drift that disappears within the window", func() {
			reconcileDrift()
			advance(10 * time.Second)
			setResources(configMapRes("Synced"))
			reconcileDrift()
			advance(window())
			reconcileDrift()
			Expect(proposals()).To(BeEmpty())
		})

		It("starts the window again when the drift comes back", func() {
			reconcileDrift()
			advance(10 * time.Second)
			setResources(configMapRes("Synced"))
			reconcileDrift()
			advance(10 * time.Second)

			setResources(configMapRes("OutOfSync"))
			reconcileDrift() // first seen again
			advance(window() - time.Second)
			reconcileDrift()
			Expect(proposals()).To(BeEmpty(), "less than a window since it came back")
			advance(2 * time.Second)
			reconcileDrift()
			Expect(proposals()).To(HaveLen(1))
		})

		It("makes one proposal for the final state of quick edits", func() {
			reconcileDrift() // debug
			advance(10 * time.Second)
			argo.items[0].NormalizedLiveState = configMapState("trace")
			reconcileDrift()
			advance(10 * time.Second)
			argo.items[0].NormalizedLiveState = configMapState("warn")
			reconcileDrift()
			Expect(proposals()).To(BeEmpty())

			advance(window() - 5*time.Second) // the final value has been there for less than the window
			reconcileDrift()
			Expect(proposals()).To(BeEmpty(), "each edit restarts the window")

			advance(10 * time.Second)
			reconcileDrift()
			Expect(proposals()).To(HaveLen(1))
			Expect(proposals()[0].Spec.Changes).To(Equal([]backflowv1alpha1.FieldChange{{
				Path: "/data/LOG_LEVEL", Op: backflowv1alpha1.OpReplace, Desired: `"info"`, Live: `"warn"`,
			}}), "the final state, not an intermediate one")
		})

		It("proposes a stable drift once the window has passed, after a second observation", func() {
			reconcileDrift()
			Expect(proposals()).To(BeEmpty(), "one observation is never enough")
			advance(window() - time.Second)
			reconcileDrift()
			Expect(proposals()).To(BeEmpty(), "two observations, but the window is not over")
			advance(2 * time.Second)
			reconcileDrift()
			Expect(proposals()).To(HaveLen(1))
		})

		It("still needs a second observation when the window is zero", func() {
			Expect(k8sClient.Patch(ctx, policy, client.RawPatch(types.MergePatchType,
				[]byte(`{"spec":{"batchWindow":"0s"}}`)))).To(Succeed())
			reconcileDrift()
			Expect(proposals()).To(BeEmpty())
			reconcileDrift()
			Expect(proposals()).To(HaveLen(1))
		})

		It("starts over when the synced revision moves inside the window", func() {
			reconcileDrift()
			advance(window() - 5*time.Second)
			setRevisions("def456", "def456") // a sync of another commit; the drift is still there
			reconcileDrift()
			advance(10 * time.Second) // more than a window since the first look, less since the revision moved
			reconcileDrift()
			Expect(proposals()).To(BeEmpty())

			advance(window())
			reconcileDrift()
			Expect(proposals()).To(HaveLen(1))
			Expect(proposals()[0].Spec.Source.Revision).To(Equal("abc123"), "the revision comes from the policy status")
		})

		It("starts over when only the compared revision moves", func() {
			const sha1 = "1111111111111111111111111111111111111111"
			const sha2 = "2222222222222222222222222222222222222222"
			const sha3 = "3333333333333333333333333333333333333333"
			r.Git = &fakeDiffer{paths: []string{"README.md"}} // commits outside the directory do not pause
			setRevisions(sha2, sha1)
			reconcileDrift()
			advance(window() - 5*time.Second)
			setRevisions(sha3, sha1)
			reconcileDrift()
			advance(10 * time.Second)
			reconcileDrift()
			Expect(proposals()).To(BeEmpty())
		})

		It("asks to be looked at again when the window is over", func() {
			res := resultOf()
			Expect(res.RequeueAfter).To(BeNumerically(">", 0))
			Expect(res.RequeueAfter).To(BeNumerically("<=", window()), "not the two minutes of the periodic resync")
			advance(10 * time.Second)
			res = resultOf()
			Expect(res.RequeueAfter).To(BeNumerically("<=", window()-10*time.Second))
			Expect(res.RequeueAfter).To(BeNumerically(">", 0))
		})

		It("leaves an open proposal alone while a different drift proves stable, then supersedes it", func() {
			reconcileOnce()
			Expect(proposals()).To(HaveLen(1))
			first := proposals()[0]

			argo.items[0].NormalizedLiveState = configMapState("trace")
			reconcileDrift()
			Expect(proposals()).To(HaveLen(1))
			Expect(proposals()[0].Annotations).NotTo(HaveKey(annotationSupersededBy), "not before the new drift is stable")

			advance(window() + time.Second)
			reconcileDrift()
			Expect(proposals()).To(HaveLen(2))
			Expect(byPhase("")).NotTo(BeEmpty())
			var got backflowv1alpha1.DriftProposal
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(&first), &got)).To(Succeed())
			Expect(got.Annotations).To(HaveKey(annotationSupersededBy))
		})

		It("loses what it has seen on a restart, which only delays the proposal", func() {
			reconcileDrift()
			advance(window() - time.Second)
			restarted := &DriftReconciler{
				Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: recorder, Now: clock,
				NewArgoClient: func(argocd.Config) (ManagedResourcesGetter, error) { return argo, nil },
			}
			r = restarted
			reconcileDrift() // first observation of the new process
			advance(2 * time.Second)
			reconcileDrift()
			Expect(proposals()).To(BeEmpty(), "the window starts again")
			advance(window())
			reconcileDrift()
			Expect(proposals()).To(HaveLen(1))
		})

		It("forgets a resource that stopped drifting while another one still does", func() {
			svc := map[string]interface{}{"version": "v1", "kind": "Service", "namespace": "demo", "name": "demo", "status": "OutOfSync"}
			setResources(configMapRes("OutOfSync"), svc)
			argo.items = append(argo.items, argocd.ManagedResource{
				Kind: "Service", Namespace: "demo", Name: "demo",
				PredictedLiveState:  map[string]interface{}{"spec": map[string]interface{}{"port": "80"}},
				NormalizedLiveState: map[string]interface{}{"spec": map[string]interface{}{"port": "81"}},
			})
			reconcileDrift()
			Expect(r.pending).To(HaveLen(2))

			By("the Service difference goes away (still reported OutOfSync for a moment), the ConfigMap still drifts")
			argo.items[1].NormalizedLiveState = argo.items[1].PredictedLiveState
			reconcileDrift()
			Expect(r.pending).To(HaveLen(1))
		})

		It("forgets a drift whose resource is gone from the Application", func() {
			reconcileDrift()
			Expect(r.pending).To(HaveLen(1))
			// The ConfigMap left the Application; only a Service is left, and it is in sync.
			setResources(map[string]interface{}{"version": "v1", "kind": "Service", "namespace": "demo", "name": "demo", "status": "Synced"})
			reconcileDrift()
			Expect(r.pending).To(BeEmpty())
		})
	})

	Context("a resource that is in sync again while its merge request is open", func() {
		const syncedAt = "abc123" // the revision the proposals of these specs were made at
		// A sync of rev: Argo CD compares against it and has synced it.
		setSyncedRevision := func(rev string) { setRevisions(rev, rev) }
		// proposed puts the only proposal into Proposed with an open merge request.
		proposed := func() backflowv1alpha1.DriftProposal {
			p := proposals()[0]
			p.Status.Phase = backflowv1alpha1.PhaseProposed
			p.Status.MergeRequest = &backflowv1alpha1.MergeRequestRef{
				URL: "https://git.test/-/merge_requests/7", Number: 7, Branch: "backflow/x", State: "open",
			}
			ExpectWithOffset(1, k8sClient.Status().Update(ctx, &p)).To(Succeed())
			return p
		}
		annotations := func() map[string]string { return proposals()[0].Annotations }

		BeforeEach(func() {
			setSyncedRevision(syncedAt)
			reconcileOnce()
			Expect(proposals()).To(HaveLen(1))
			proposed()
			// Argo CD reset the resource: it is Synced and holds Git's value.
			setResources(configMapRes("Synced"))
			argo.items[0].NormalizedLiveState = configMapState("info")
			argo.calls = 0
		})

		It("keeps the change in its merge request when a sync of a new revision reset the cluster", func() {
			setSyncedRevision("def456")
			for i := 0; i < 3; i++ {
				reconcileDrift()
			}
			Expect(annotations()).To(HaveKeyWithValue(annotationLiveReverted, "def456"))
			Expect(annotations()).NotTo(HaveKey(annotationReverted), "not retired: its merge request still holds the change")
			Expect(proposals()).To(HaveLen(1))
			Expect(argo.calls).To(Equal(1), "the live state is read once, then the annotation is enough")
		})

		It("still counts the proposal as open, so the same drift does not create a second one", func() {
			setSyncedRevision("def456")
			reconcileDrift()
			Expect(isOpen(&proposals()[0])).To(BeTrue())

			By("the change is made again in the cluster")
			argo.items[0].NormalizedLiveState = configMapState("debug")
			setResources(configMapRes("OutOfSync"))
			reconcileDrift()
			Expect(proposals()).To(HaveLen(1), "the proposal that waits in its merge request is the one")
			Expect(annotations()).NotTo(HaveKey(annotationLiveReverted), "the change is live again")
		})

		It("retires the proposal when the cluster was reset in place, at the same revision", func() {
			reconcileDrift()
			Expect(annotations()).To(HaveKey(annotationReverted))
			Expect(annotations()).NotTo(HaveKey(annotationLiveReverted))
			Expect(argo.calls).To(BeZero(), "no API call: the status alone settles it")
		})

		It("retires the proposal when Git adopted the change at a new revision", func() {
			setSyncedRevision("def456")
			argo.items[0].NormalizedLiveState = configMapState("debug") // the recorded live value, now also in Git
			reconcileDrift()
			Expect(annotations()).To(HaveKey(annotationReverted))
			Expect(annotations()).NotTo(HaveKey(annotationLiveReverted))
		})

		It("retires the proposal when the field holds neither value", func() {
			setSyncedRevision("def456")
			argo.items[0].NormalizedLiveState = configMapState("trace")
			reconcileDrift()
			Expect(annotations()).To(HaveKey(annotationReverted))
			Expect(annotations()).NotTo(HaveKey(annotationLiveReverted))
		})

		It("waits instead of guessing when the live state cannot be read", func() {
			setSyncedRevision("def456")
			argo.err = errors.New("argo is down")
			reconcileDrift()
			Expect(annotations()).NotTo(HaveKey(annotationReverted))
			Expect(annotations()).NotTo(HaveKey(annotationLiveReverted))

			argo.err = nil
			reconcileDrift()
			Expect(annotations()).To(HaveKey(annotationLiveReverted))
		})

		It("retires a proposal that has no merge request, whatever reset the cluster", func() {
			p := proposals()[0]
			p.Status.Phase = backflowv1alpha1.PhaseMapping
			p.Status.MergeRequest = nil
			Expect(k8sClient.Status().Update(ctx, &p)).To(Succeed())
			setSyncedRevision("def456")

			reconcileDrift()
			Expect(annotations()).To(HaveKey(annotationReverted))
			Expect(annotations()).NotTo(HaveKey(annotationLiveReverted))
		})
	})

	It("does not call Argo CD when nothing is OutOfSync and nothing is open", func() {
		setResources(configMapRes("Synced"))
		reconcileOnce()
		Expect(argo.calls).To(BeZero())
		Expect(proposals()).To(BeEmpty())
	})

	It("calls Argo CD once per reconcile for an OutOfSync application", func() {
		reconcileDrift()
		Expect(argo.calls).To(Equal(1))
		reconcileDrift()
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
		reconcileDrift()
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
