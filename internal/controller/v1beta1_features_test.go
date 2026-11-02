package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	backflowv1beta1 "github.com/sserkanml/backflow/api/v1beta1"
	"github.com/sserkanml/backflow/internal/gitrepo"
)

// allowOnly sets the allowlists for one test and puts the suite's back afterwards.
func allowOnly(t interface {
	Helper()
	Cleanup(func())
}, servers, hosts []string) {
	t.Helper()
	oldServers, oldHosts := argoServers.Load(), scmHosts.Load()
	SetArgoCDAllowedServers(servers)
	SetScmAllowedHosts(hosts)
	t.Cleanup(func() {
		argoServers.Store(oldServers)
		scmHosts.Store(oldHosts)
	})
}

func TestArgoCDServerAllowed(t *testing.T) {
	def := &ArgoCDDefaults{Server: "https://openshift-gitops-server.openshift-gitops.svc"}
	tests := []struct {
		name    string
		d       *ArgoCDDefaults
		servers []string
		url     string
		want    bool
	}{
		{"the operator's own server is allowed without a list", def, nil, "https://openshift-gitops-server.openshift-gitops.svc", true},
		{"with another path", def, nil, "https://openshift-gitops-server.openshift-gitops.svc/api", true},
		{"another server is not, without a list", def, nil, "https://argocd.example.com", false},
		{"nor with a list that does not name it", def, []string{"https://other.example.com"}, "https://argocd.example.com", false},
		{"a server in the list", def, []string{"https://argocd.example.com"}, "https://argocd.example.com", true},
		{"a wildcard in the list", def, []string{"https://*.argo.corp:8443"}, "https://team-a.argo.corp:8443", true},
		{"the port decides", def, []string{"https://argocd.example.com"}, "https://argocd.example.com:8443", false},
		{"the scheme decides", def, []string{"https://argocd.example.com"}, "http://argocd.example.com", false},
		{"no operator default and no list: nothing", nil, nil, "https://argocd.example.com", false},
		{"no operator default, a list", nil, []string{"https://argocd.example.com"}, "https://argocd.example.com", true},
		{"the address of a cloud metadata service", def, []string{"https://argocd.example.com"}, "http://169.254.169.254", false},
		{"an empty URL is the built-in address of the CRD default", &ArgoCDDefaults{}, []string{defaultArgoCDURL}, "", true},
		{"and is not allowed unless that address is", &ArgoCDDefaults{}, nil, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			allowOnly(t, tt.servers, nil)
			if got := tt.d.ServerAllowed(tt.url); got != tt.want {
				t.Errorf("ServerAllowed(%q) = %v, want %v", tt.url, got, tt.want)
			}
		})
	}
}

func TestArgoCDConfigNeverReadsTheSecretsOfAServerThatIsNotAllowed(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = backflowv1beta1.AddToScheme(scheme)
	policy := &backflowv1beta1.BackflowPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "p"},
		Spec: backflowv1beta1.BackflowPolicySpec{ArgoCD: backflowv1beta1.ArgoCDSpec{Server: &backflowv1beta1.ArgoCDServer{
			URL: "http://169.254.169.254", TokenSecretRef: backflowv1beta1.SecretKeyReference{Name: "tok", Key: "token"},
		}}},
	}
	secrets := &secretReadCounter{Reader: fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "tok"}, Data: map[string][]byte{"token": []byte("t")}},
	).Build()}

	allowOnly(t, []string{"https://argocd.example.com"}, nil)
	_, err := argoCDConfig(context.Background(), secrets, policy, &ArgoCDDefaults{})
	var check *checkError
	if !errors.As(err, &check) || check.reason != backflowv1beta1.ReasonServerNotAllowed {
		t.Fatalf("err = %v, want ServerNotAllowed", err)
	}
	if secrets.gets != 0 {
		t.Errorf("%d Secret(s) were read for a server that is not allowed", secrets.gets)
	}

	policy.Spec.ArgoCD.Server.URL = "https://argocd.example.com"
	cfg, err := argoCDConfig(context.Background(), secrets, policy, &ArgoCDDefaults{})
	if err != nil || cfg.URL != "https://argocd.example.com" || secrets.gets == 0 {
		t.Errorf("an allowed server: cfg %+v, err %v, secret reads %d", cfg, err, secrets.gets)
	}
}

// secretReadCounter counts the Secrets that are read.
type secretReadCounter struct {
	client.Reader
	gets int
}

func (c *secretReadCounter) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*corev1.Secret); ok {
		c.gets++
	}
	return c.Reader.Get(ctx, key, obj, opts...)
}

func TestScmAccessNeverReadsTheTokenOfAHostThatIsNotAllowed(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = backflowv1beta1.AddToScheme(scheme)
	conn := &backflowv1beta1.ScmConnection{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "evil"},
		Spec: backflowv1beta1.ScmConnectionSpec{Provider: backflowv1beta1.ScmProviderGitLab, URL: "https://evil.example.net",
			TokenSecretRef: backflowv1beta1.SecretKeyReference{Name: "tok", Key: "token"}},
	}
	secrets := &secretReadCounter{Reader: fake.NewClientBuilder().WithScheme(scheme).WithObjects(conn,
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "tok"}, Data: map[string][]byte{"token": []byte("t")}},
	).Build()}

	allowOnly(t, nil, []string{"gitlab.com"})
	_, err := scmAccessByName(context.Background(), secrets, "team-a", "evil")
	if !errors.Is(err, gitrepo.ErrAuth) {
		t.Fatalf("err = %v, want an authentication error that holds the proposal", err)
	}
	if secrets.gets != 0 {
		t.Errorf("%d Secret(s) were read for a host that is not allowed", secrets.gets)
	}

	allowOnly(t, nil, []string{"evil.example.net"})
	if acc, err := scmAccessByName(context.Background(), secrets, "team-a", "evil"); err != nil || acc == nil {
		t.Errorf("an allowed host: %v, %v", acc, err)
	}
}

func TestScmHostAllowed(t *testing.T) {
	allowOnly(t, nil, []string{"gitlab.com", "*.corp.example.com"})
	for url, want := range map[string]bool{
		"https://gitlab.com": true, "https://gitlab.com/api/v4": true, "https://git.corp.example.com": true,
		"https://corp.example.com": false, "https://gitlab.com.evil.io": false, "http://127.0.0.1:8080": false, "": false,
	} {
		if got := scmHostAllowed(url); got != want {
			t.Errorf("scmHostAllowed(%q) = %v, want %v", url, got, want)
		}
	}
	allowOnly(t, nil, nil)
	if scmHostAllowed("https://gitlab.com") {
		t.Error("an empty list allows nothing")
	}
}

var _ = Describe("The allowlists, with the API server", func() {
	const ns = "default"
	ctx := context.Background()

	It("keeps a ScmConnection to a host that is not allowed from being contacted", func() {
		var hits atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			_, _ = fmt.Fprint(w, `{"username":"backflow-bot"}`)
		}))
		DeferCleanup(server.Close)
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "allow-token", Namespace: ns}, StringData: map[string]string{"token": "t"},
		})).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "allow-token", Namespace: ns}})
		})
		conn := &backflowv1beta1.ScmConnection{
			ObjectMeta: metav1.ObjectMeta{Name: "allow-scm", Namespace: ns},
			Spec: backflowv1beta1.ScmConnectionSpec{Provider: backflowv1beta1.ScmProviderGitLab, URL: server.URL,
				TokenSecretRef: backflowv1beta1.SecretKeyReference{Name: "allow-token", Key: "token"}},
		}
		Expect(k8sClient.Create(ctx, conn)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, conn) })
		r := &ScmConnectionReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		reconcileConn := func() *metav1.Condition {
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(conn)})
			ExpectWithOffset(1, err).NotTo(HaveOccurred())
			ExpectWithOffset(1, k8sClient.Get(ctx, client.ObjectKeyFromObject(conn), conn)).To(Succeed())
			return meta.FindStatusCondition(conn.Status.Conditions, "Ready")
		}

		allowOnly(GinkgoT(), nil, []string{"gitlab.com"})
		ready := reconcileConn()
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal(backflowv1beta1.ReasonHostNotAllowed))
		Expect(ready.Message).To(ContainSubstring("--scm-allowed-hosts"))
		Expect(ready.ObservedGeneration).To(Equal(conn.Generation))
		Expect(hits.Load()).To(BeZero(), "no request is made to a host that is not allowed")
		Expect(conn.Status.AuthenticatedAs).To(BeEmpty())
		Expect(conn.Status.Hosts).To(Equal([]string{"127.0.0.1"}))

		allowOnly(GinkgoT(), nil, []string{"127.0.0.1"})
		ready = reconcileConn()
		Expect(ready.Status).To(Equal(metav1.ConditionTrue), ready.Message)
		Expect(hits.Load()).To(BeEquivalentTo(1))
		Expect(conn.Status.AuthenticatedAs).To(Equal("backflow-bot"))
	})

	It("keeps a BackflowPolicy that names a server that is not allowed from being Ready", func() {
		policy := &backflowv1beta1.BackflowPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "allow-policy", Namespace: ns},
			Spec: backflowv1beta1.BackflowPolicySpec{
				Applications: backflowv1beta1.ApplicationSelector{Names: []string{"allow-app"}},
				Mode:         backflowv1beta1.ModeReportOnly,
				ArgoCD: backflowv1beta1.ArgoCDSpec{Namespace: ns, Server: &backflowv1beta1.ArgoCDServer{
					URL: "https://argocd.not-allowed.example", TokenSecretRef: backflowv1beta1.SecretKeyReference{Name: "none", Key: "token"},
				}},
			},
		}
		Expect(k8sClient.Create(ctx, policy)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, policy) })
		r := &BackflowPolicyReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), ArgoCD: &ArgoCDDefaults{}}
		ready := func() *metav1.Condition {
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(policy)})
			ExpectWithOffset(1, err).NotTo(HaveOccurred())
			ExpectWithOffset(1, k8sClient.Get(ctx, client.ObjectKeyFromObject(policy), policy)).To(Succeed())
			return meta.FindStatusCondition(policy.Status.Conditions, "Ready")
		}

		allowOnly(GinkgoT(), []string{"https://argocd"}, nil)
		c := ready()
		Expect(c.Status).To(Equal(metav1.ConditionFalse))
		Expect(c.Reason).To(Equal(backflowv1beta1.ReasonServerNotAllowed))
		Expect(c.Message).To(ContainSubstring("--argocd-allowed-servers"))
		Expect(c.Message).To(ContainSubstring("no request is made"))
		Expect(policy.Status.Applications).To(BeEmpty())

		By("the operator's own server needs no entry in the list")
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(policy), policy)).To(Succeed())
		r.ArgoCD = &ArgoCDDefaults{Server: "https://argocd.not-allowed.example"}
		Expect(ready().Reason).NotTo(Equal(backflowv1beta1.ReasonServerNotAllowed))

		By("and a server in the list is allowed")
		r.ArgoCD = &ArgoCDDefaults{}
		allowOnly(GinkgoT(), []string{"https://argocd.not-allowed.example"}, nil)
		Expect(ready().Reason).NotTo(Equal(backflowv1beta1.ReasonServerNotAllowed))
	})
})

var _ = Describe("A policy that selects more Applications than its status lists", func() {
	const ns = "default"
	ctx := context.Background()

	It("lists 100, says how many there are, and still covers them all", func() {
		const total = 105
		for i := range total {
			app := &unstructured.Unstructured{}
			app.SetGroupVersionKind(applicationGVK)
			app.SetName(fmt.Sprintf("cap-%03d", i))
			app.SetNamespace(ns)
			app.SetLabels(map[string]string{"cap-test": "yes"})
			Expect(unstructured.SetNestedMap(app.Object, map[string]any{"repoURL": "https://gitlab.com/x/y.git", "path": "a"}, "spec", "source")).To(Succeed())
			Expect(k8sClient.Create(ctx, app)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, app) })
		}
		policy := &backflowv1beta1.BackflowPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "cap-policy", Namespace: ns},
			Spec: backflowv1beta1.BackflowPolicySpec{
				Applications: backflowv1beta1.ApplicationSelector{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"cap-test": "yes"}}},
				Mode:         backflowv1beta1.ModeReportOnly,
				ArgoCD: backflowv1beta1.ArgoCDSpec{Namespace: ns, Server: &backflowv1beta1.ArgoCDServer{
					URL: "https://argocd.test", TokenSecretRef: backflowv1beta1.SecretKeyReference{Name: "none", Key: "token"},
				}},
			},
		}
		Expect(k8sClient.Create(ctx, policy)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, policy) })

		r := &BackflowPolicyReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(policy)})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(policy), policy)).To(Succeed())
		Expect(policy.Status.Applications).To(HaveLen(100))
		Expect(policy.Status.ApplicationCount).To(BeEquivalentTo(total))
		Expect(policy.Status.ApplicationsTruncated).To(BeTrue())
		ready := meta.FindStatusCondition(policy.Status.Conditions, "Ready")
		Expect(ready.Message).To(ContainSubstring(fmt.Sprintf("Watching %d application(s)", total)))
		Expect(appNames(policy.Status)[0]).To(Equal("cap-000"))

		By("the resolution the drift controller uses has all of them")
		res, err := (&BackflowPolicyReconciler{Client: k8sClient}).resolver().resolve(ctx, policy)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.apps).To(HaveLen(total))
	})
})

var _ = Describe("assignActor", func() {
	const ns = "default"
	ctx := context.Background()

	It("can be set to false from Go, and is true when it is left out", func() {
		for name, assign := range map[string]*bool{"assign-off": new(false), "assign-on": new(true), "assign-unset": nil} {
			policy := &backflowv1beta1.BackflowPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
				Spec: backflowv1beta1.BackflowPolicySpec{
					Applications: backflowv1beta1.ApplicationSelector{Names: []string{"x"}},
					Git:          backflowv1beta1.GitOptions{MergeRequest: backflowv1beta1.MergeRequestOptions{AssignActor: assign}},
				},
			}
			Expect(k8sClient.Create(ctx, policy)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, policy) })
			got := &backflowv1beta1.BackflowPolicy{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(policy), got)).To(Succeed())
			Expect(got.Spec.Git.MergeRequest.AssignActor).NotTo(BeNil(), name)
			Expect(got.Spec.AssignsActor()).To(Equal(assign == nil || *assign), name)
			Expect(*got.Spec.Git.MergeRequest.AssignActor).To(Equal(assign == nil || *assign), name)
		}
	})
})

var _ = Describe("The retention of finished proposals", func() {
	const ns = "default"
	ctx := context.Background()
	start := time.Now().UTC().Truncate(time.Second)
	var now time.Time

	newPolicy := func(name string, retention *time.Duration) {
		policy := &backflowv1beta1.BackflowPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       backflowv1beta1.BackflowPolicySpec{Applications: backflowv1beta1.ApplicationSelector{Names: []string{"x"}}},
		}
		if retention != nil {
			policy.Spec.Retention.FinishedProposals = &metav1.Duration{Duration: *retention}
		}
		ExpectWithOffset(1, k8sClient.Create(ctx, policy)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, policy) })
	}
	// proposal creates a proposal of the policy in a phase that was reached finishedAgo before now.
	proposal := func(name, policy string, phase backflowv1beta1.ProposalPhase, finishedAgo time.Duration) *backflowv1beta1.DriftProposal {
		dp := &backflowv1beta1.DriftProposal{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: backflowv1beta1.DriftProposalSpec{
				PolicyRef:      backflowv1beta1.LocalObjectRef{Name: policy},
				ApplicationRef: backflowv1beta1.ObjectRef{Name: "x", Namespace: "argocd"},
				ResourceRef:    backflowv1beta1.ResourceRef{Version: "v1", Kind: "ConfigMap", Namespace: "demo", Name: name},
				Sources:        []backflowv1beta1.ProposalSource{{Position: 1, Role: backflowv1beta1.RoleRender, RepoURL: "https://gitlab.com/x/y.git", Revision: "abc", Type: backflowv1beta1.SourceDirectory}},
				Changes:        []backflowv1beta1.FieldChange{{Path: "/data/a", Op: backflowv1beta1.OpReplace, Desired: `"1"`, Live: `"2"`}},
				DetectedAt:     metav1.NewTime(start),
			},
		}
		ExpectWithOffset(1, k8sClient.Create(ctx, dp)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, dp) })
		dp.Status.Phase = phase
		if finishedAgo >= 0 {
			finished := metav1.NewTime(now.Add(-finishedAgo))
			dp.Status.FinishedAt = &finished
		}
		ExpectWithOffset(1, k8sClient.Status().Update(ctx, dp)).To(Succeed())
		return dp
	}
	reconcileDP := func(dp *backflowv1beta1.DriftProposal) (reconcile.Result, bool) {
		r := &DriftProposalReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Now: func() time.Time { return now }}
		res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(dp)})
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		err = k8sClient.Get(ctx, client.ObjectKeyFromObject(dp), &backflowv1beta1.DriftProposal{})
		if apierrors.IsNotFound(err) {
			return res, false
		}
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		return res, true
	}
	BeforeEach(func() { now = start })

	It("deletes a finished proposal after the retention, and keeps it until then", func() {
		retention := 2 * time.Hour
		newPolicy("ret-short", &retention)
		for _, phase := range []backflowv1beta1.ProposalPhase{
			backflowv1beta1.PhaseMerged, backflowv1beta1.PhaseRejected, backflowv1beta1.PhaseReverted,
			backflowv1beta1.PhaseSuperseded, backflowv1beta1.PhaseFailed,
		} {
			name := "ret-" + strings.ToLower(string(phase))
			young := proposal(name+"-young", "ret-short", phase, time.Hour)
			res, exists := reconcileDP(young)
			Expect(exists).To(BeTrue(), "%s: one hour old, kept for two", phase)
			Expect(res.RequeueAfter).To(BeNumerically("~", time.Hour, 5*time.Second), "it looks again when the time is up")

			old := proposal(name+"-old", "ret-short", phase, 3*time.Hour)
			_, exists = reconcileDP(old)
			Expect(exists).To(BeFalse(), "%s: three hours old, past the retention", phase)
		}
	})

	It("never deletes a proposal that is open, or Unmapped, however old", func() {
		retention := time.Hour
		newPolicy("ret-open", &retention)
		for _, phase := range []backflowv1beta1.ProposalPhase{
			backflowv1beta1.PhaseDetected, backflowv1beta1.PhaseMapping, backflowv1beta1.PhaseProposed, backflowv1beta1.PhaseUnmapped,
		} {
			dp := proposal("ret-open-"+strings.ToLower(string(phase)), "ret-open", phase, 1000*time.Hour)
			now = start.Add(1000 * time.Hour)
			_, exists := reconcileDP(dp)
			Expect(exists).To(BeTrue(), "%s is not finished", phase)
		}
	})

	It("keeps finished proposals for ever with 0s", func() {
		zero := time.Duration(0)
		newPolicy("ret-forever", &zero)
		dp := proposal("ret-forever-merged", "ret-forever", backflowv1beta1.PhaseMerged, 100000*time.Hour)
		res, exists := reconcileDP(dp)
		Expect(exists).To(BeTrue())
		Expect(res.IsZero()).To(BeTrue(), "nothing to wait for")
	})

	It("keeps them 14 days when the policy does not say", func() {
		newPolicy("ret-default", nil)
		got := &backflowv1beta1.BackflowPolicy{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "ret-default"}, got)).To(Succeed())
		Expect(got.Spec.Retention.FinishedProposals.Duration).To(Equal(14*24*time.Hour), "the API server fills in the default")
		_, exists := reconcileDP(proposal("ret-default-13d", "ret-default", backflowv1beta1.PhaseMerged, 13*24*time.Hour))
		Expect(exists).To(BeTrue())
		_, exists = reconcileDP(proposal("ret-default-15d", "ret-default", backflowv1beta1.PhaseMerged, 15*24*time.Hour))
		Expect(exists).To(BeFalse())
	})

	It("counts from the moment the proposal finished, which is recorded when it does", func() {
		newPolicy("ret-stamp", new(time.Hour))
		dp := proposal("ret-stamp-dp", "ret-stamp", backflowv1beta1.PhaseProposed, -1)
		r := &DriftProposalReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Now: func() time.Time { return now }}
		// The proposal controller's own status write stamps finishedAt when the phase is a final one.
		orig := dp.DeepCopy()
		dp.Status.Phase = backflowv1beta1.PhaseRejected
		Expect(r.patchStatus(ctx, dp, orig)).To(Succeed())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(dp), dp)).To(Succeed())
		Expect(dp.Status.FinishedAt).NotTo(BeNil())
		Expect(dp.Status.FinishedAt.Time).To(BeTemporally("~", now, time.Second))
		_, exists := reconcileDP(dp)
		Expect(exists).To(BeTrue())
		now = now.Add(2 * time.Hour)
		_, exists = reconcileDP(dp)
		Expect(exists).To(BeFalse())
	})

	It("counts from its last change for a proposal that finished before the field existed", func() {
		newPolicy("ret-legacy", new(time.Hour))
		dp := proposal("ret-legacy-dp", "ret-legacy", backflowv1beta1.PhaseMerged, -1)
		Expect(dp.Status.FinishedAt).To(BeNil())
		now = start.Add(30 * time.Minute)
		_, exists := reconcileDP(dp)
		Expect(exists).To(BeTrue(), "the clock starts when it is first looked at, not at once")
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(dp), dp)).To(Succeed())
		Expect(dp.Status.FinishedAt).NotTo(BeNil(), "and is kept")
	})

	It("leaves a proposal whose policy is gone to the owner reference", func() {
		dp := proposal("ret-orphan", "ret-no-such-policy", backflowv1beta1.PhaseMerged, 1000*time.Hour)
		_, exists := reconcileDP(dp)
		Expect(exists).To(BeTrue())
	})
})
