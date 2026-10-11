package controller

import (
	"context"
	"encoding/json"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	backflowv1alpha1 "github.com/sserkanml/backflow/api/v1alpha1"
	"github.com/sserkanml/backflow/internal/argocd"
)

var _ = Describe("Repository URLs with credentials", func() {
	const ns = "default"
	secrets := []string{"glpat-secret", "s3cret", "ghp_leaked", "tok3n"}
	urls := map[string]string{
		"cred-https":     "https://oauth2:glpat-secret@gitlab.com/x/y.git",
		"cred-ssh-pw":    "ssh://git:s3cret@gitlab.com/x/y.git",
		"cred-token":     "https://ghp_leaked@github.com/o/r.git",
		"cred-scp-pw":    "git:tok3n@gitlab.com:x/y.git",
		"cred-user-only": "git@gitlab.com:x/y.git",
		"cred-ssh-user":  "ssh://git@gitlab.com/x/y.git",
		"cred-clean":     "https://gitlab.com/x/y.git",
	}
	blocked := []string{"cred-https", "cred-ssh-pw", "cred-token", "cred-scp-pw", "cred-multi"}
	var (
		ctx      = context.Background()
		policy   *backflowv1alpha1.BackflowPolicy
		appObjs  []*unstructured.Unstructured
		recorder *events.FakeRecorder
		pr       *BackflowPolicyReconciler
	)

	newApp := func(name string, source map[string]any, sources ...any) *unstructured.Unstructured {
		app := &unstructured.Unstructured{}
		app.SetGroupVersionKind(applicationGVK)
		app.SetName(name)
		app.SetNamespace(ns)
		if source != nil {
			ExpectWithOffset(1, unstructured.SetNestedMap(app.Object, source, "spec", "source")).To(Succeed())
		}
		if sources != nil {
			ExpectWithOffset(1, unstructured.SetNestedSlice(app.Object, sources, "spec", "sources")).To(Succeed())
		}
		ExpectWithOffset(1, k8sClient.Create(ctx, app)).To(Succeed())
		appObjs = append(appObjs, app)
		return app
	}
	reconcilePolicy := func() *backflowv1alpha1.BackflowPolicy {
		_, err := pr.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(policy)})
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		got := &backflowv1alpha1.BackflowPolicy{}
		ExpectWithOffset(1, k8sClient.Get(ctx, client.ObjectKeyFromObject(policy), got)).To(Succeed())
		return got
	}
	summaryOf := func(p *backflowv1alpha1.BackflowPolicy, name string) backflowv1alpha1.ApplicationSummary {
		for _, s := range p.Status.Applications {
			if s.Name == name {
				return s
			}
		}
		Fail("no summary for " + name)
		return backflowv1alpha1.ApplicationSummary{}
	}
	ready := func(p *backflowv1alpha1.BackflowPolicy) *metav1.Condition {
		return meta.FindStatusCondition(p.Status.Conditions, "Ready")
	}
	drain := func() []string {
		var out []string
		for len(recorder.Events) > 0 {
			out = append(out, <-recorder.Events)
		}
		return out
	}

	BeforeEach(func() {
		appObjs = nil
		recorder = events.NewFakeRecorder(100)
		pr = &BackflowPolicyReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: recorder}
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "cred-argocd-token", Namespace: ns}, StringData: map[string]string{"token": "tok"},
		})).To(Succeed())

		for name, raw := range urls {
			newApp(name, map[string]any{"repoURL": raw, "path": "apps/demo", "targetRevision": "main"})
		}
		// The credential is in the second of several sources.
		newApp("cred-multi", nil,
			map[string]any{"repoURL": "https://gitlab.com/x/y.git", "path": "a"},
			map[string]any{"repoURL": "https://u:glpat-secret@gitlab.com/x/other.git", "path": "b"})
		names := make([]string, 0, len(appObjs))
		for _, a := range appObjs {
			names = append(names, a.GetName())
		}
		policy = &backflowv1alpha1.BackflowPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "cred-policy", Namespace: ns},
			Spec: backflowv1alpha1.BackflowPolicySpec{
				ArgoCDNamespace: ns,
				Applications:    backflowv1alpha1.ApplicationSelector{Names: names},
				Mode:            backflowv1alpha1.ModeReportOnly,
				ArgoCD: &backflowv1alpha1.ArgoCDServer{
					URL: "https://argocd.test",
					TokenSecretRef: corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "cred-argocd-token"}, Key: "token",
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, policy)).To(Succeed())
	})

	AfterEach(func() {
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, policy))).To(Succeed())
		for _, a := range appObjs {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, a))).To(Succeed())
		}
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "cred-argocd-token", Namespace: ns}}))).To(Succeed())
	})

	It("does not track an Application whose repository URL has a credential, and stores only the redacted URL", func() {
		got := reconcilePolicy()
		for _, name := range blocked {
			s := summaryOf(got, name)
			Expect(s.Reason).To(Equal("CredentialsInRepoURL"), name)
			Expect(s.RepoURL).To(ContainSubstring("REDACTED@"), name)
			Expect(s.Path).To(BeEmpty(), "nothing else of it is stored: "+name)
			Expect(s.ScmConnection).To(BeEmpty())
		}
		Expect(summaryOf(got, "cred-https").RepoURL).To(Equal("https://REDACTED@gitlab.com/x/y.git"))
		Expect(summaryOf(got, "cred-ssh-pw").RepoURL).To(Equal("ssh://REDACTED@gitlab.com/x/y.git"))
		Expect(summaryOf(got, "cred-scp-pw").RepoURL).To(Equal("REDACTED@gitlab.com:x/y.git"))
		Expect(summaryOf(got, "cred-multi").RepoURL).To(Equal("https://REDACTED@gitlab.com/x/other.git"))

		By("URLs with a plain user name still work, as do clean ones")
		for _, name := range []string{"cred-user-only", "cred-ssh-user", "cred-clean"} {
			s := summaryOf(got, name)
			Expect(s.Reason).To(BeEmpty(), name)
			Expect(s.RepoURL).To(Equal(urls[name]), name)
			Expect(s.Path).To(Equal("apps/demo"))
		}
		Expect(got.Status.MatchedApplications).To(HaveLen(len(urls) + 1))
		Expect(ready(got).Status).To(Equal(metav1.ConditionTrue))
		Expect(ready(got).Message).To(ContainSubstring("not tracked because a repository URL contains credentials"))
	})

	It("says so once per Application, and nothing unredacted reaches status or events", func() {
		reconcilePolicy()
		got := reconcilePolicy()
		reconcilePolicy()
		sent := drain()
		Expect(sent).To(HaveLen(len(blocked)), "one Event per Application, however often it is reconciled")
		for _, name := range blocked {
			n := 0
			for _, e := range sent {
				if strings.Contains(e, "Application "+name+" ") {
					n++
					Expect(e).To(ContainSubstring("CredentialsInRepoURL"))
				}
			}
			Expect(n).To(Equal(1), name)
		}
		status, err := json.Marshal(got.Status)
		Expect(err).NotTo(HaveOccurred())
		for _, text := range append(sent, string(status)) {
			for _, secret := range secrets {
				Expect(text).NotTo(ContainSubstring(secret))
			}
		}
	})

	It("reports an Application again when the credential comes back", func() {
		reconcilePolicy()
		drain()
		var app *unstructured.Unstructured
		for _, a := range appObjs {
			if a.GetName() == "cred-https" {
				app = a
			}
		}
		Expect(app).NotTo(BeNil())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(app), app)).To(Succeed())
		original, _, _ := unstructured.NestedString(app.Object, "spec", "source", "repoURL")
		Expect(unstructured.SetNestedField(app.Object, "https://gitlab.com/x/y.git", "spec", "source", "repoURL")).To(Succeed())
		Expect(k8sClient.Update(ctx, app)).To(Succeed())
		got := reconcilePolicy()
		Expect(summaryOf(got, app.GetName()).Reason).To(BeEmpty())

		Expect(unstructured.SetNestedField(app.Object, original, "spec", "source", "repoURL")).To(Succeed())
		Expect(k8sClient.Update(ctx, app)).To(Succeed())
		reconcilePolicy()
		Expect(drain()).To(HaveLen(1))
	})

	It("is skipped by the drift controller: nothing is read from Argo CD or proposed for it", func() {
		got := reconcilePolicy()
		Expect(ready(got).Status).To(Equal(metav1.ConditionTrue), "the drift controller works on a Ready policy")

		// A blocked Application with drift: it must never reach the Argo CD API.
		for _, a := range appObjs {
			if summaryOf(got, a.GetName()).Reason == "" {
				continue
			}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(a), a)).To(Succeed())
			Expect(unstructured.SetNestedSlice(a.Object, []any{map[string]any{
				"version": "v1", "kind": "ConfigMap", "namespace": "demo", "name": "demo-config", "status": "OutOfSync",
			}}, fieldStatus, "resources")).To(Succeed())
			Expect(k8sClient.Update(ctx, a)).To(Succeed())
		}

		argo := &fakeArgo{items: []argocd.ManagedResource{{
			Kind: "ConfigMap", Namespace: "demo", Name: "demo-config",
			PredictedLiveState: configMapState("info"), NormalizedLiveState: configMapState("debug"),
		}}}
		dr := &DriftReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: recorder,
			NewArgoClient: func(argocd.Config) (ManagedResourcesGetter, error) { return argo, nil },
		}
		for range 3 {
			_, err := dr.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: policy.Name}})
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(argo.calls).To(BeZero(), "no Argo CD API call for an Application that is not tracked")
		var list backflowv1alpha1.DriftProposalList
		Expect(k8sClient.List(ctx, &list, client.InNamespace(ns), client.MatchingLabels{labelPolicy: policy.Name})).To(Succeed())
		Expect(list.Items).To(BeEmpty(), "so no proposal can carry the URL")
	})
})
