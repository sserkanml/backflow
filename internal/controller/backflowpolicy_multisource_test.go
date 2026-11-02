package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	backflowv1 "github.com/sserkanml/backflow/api/v1"
)

var _ = Describe("BackflowPolicy and multi-source Applications", func() {
	const ns = "default"
	var (
		ctx    = context.Background()
		policy *backflowv1.BackflowPolicy
		app    *unstructured.Unstructured
		conns  []*backflowv1.ScmConnection
	)

	connection := func(name, url string) {
		c := &backflowv1.ScmConnection{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: backflowv1.ScmConnectionSpec{
				Provider: backflowv1.ScmProviderGitLab, URL: url,
				TokenSecretRef: backflowv1.SecretKeyReference{Name: "ms-token", Key: "token"},
			},
		}
		ExpectWithOffset(1, k8sClient.Create(ctx, c)).To(Succeed())
		conns = append(conns, c)
	}
	reconcilePolicy := func() *backflowv1.BackflowPolicy {
		r := &BackflowPolicyReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(policy)})
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		got := &backflowv1.BackflowPolicy{}
		ExpectWithOffset(1, k8sClient.Get(ctx, client.ObjectKeyFromObject(policy), got)).To(Succeed())
		return got
	}

	BeforeEach(func() {
		conns = nil
		app = &unstructured.Unstructured{}
		app.SetGroupVersionKind(applicationGVK)
		app.SetName("ms-app")
		app.SetNamespace(ns)
		Expect(unstructured.SetNestedSlice(app.Object, []any{
			chartSource,
			valuesRef,
			map[string]any{"repoURL": "https://github.example.org/x/other.git", "targetRevision": "main", "ref": "other"},
		}, "spec", "sources")).To(Succeed())
		Expect(unstructured.SetNestedStringSlice(app.Object, []string{"6.7.1", "aaaa", "cccc"}, "status", "sync", "revisions")).To(Succeed())
		Expect(unstructured.SetNestedStringSlice(app.Object, []string{"6.7.0", "bbbb", "cccc"}, "status", "operationState", "syncResult", "revisions")).To(Succeed())
		Expect(unstructured.SetNestedStringSlice(app.Object, []string{"Helm", "", ""}, "status", "sourceTypes")).To(Succeed())
		Expect(k8sClient.Create(ctx, app)).To(Succeed())

		policy = &backflowv1.BackflowPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "ms-policy", Namespace: ns},
			Spec: backflowv1.BackflowPolicySpec{
				ArgoCD:       backflowv1.ArgoCDSpec{Namespace: ns},
				Applications: backflowv1.ApplicationSelector{Names: []string{"ms-app"}},
				Mode:         backflowv1.ModeMergeRequest,
			},
		}
		Expect(k8sClient.Create(ctx, policy)).To(Succeed())
	})

	AfterEach(func() {
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, policy))).To(Succeed())
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, app))).To(Succeed())
		for _, c := range conns {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, c))).To(Succeed())
		}
	})

	It("lists every source and matches a ScmConnection to each Git repository by host", func() {
		connection("ms-gitlab", "https://gitlab.com")
		connection("ms-example", "https://github.example.org")
		got := reconcilePolicy()

		Expect(meta.IsStatusConditionTrue(got.Status.Conditions, "Ready")).To(BeTrue(), "%+v", got.Status.Conditions)
		Expect(got.Status.Applications).To(HaveLen(1))
		s := got.Status.Applications[0]
		Expect(s.First().RepoURL).To(Equal("ghcr.io/stefanprodan/charts"), "the first source is the first entry")
		Expect(s.Sources).To(HaveLen(3))
		Expect(s.Sources[0]).To(Equal(backflowv1.SourceSummary{
			Position: 1, RepoURL: "ghcr.io/stefanprodan/charts", Chart: "podinfo", TargetRevision: "6.7.*",
			Revision: "6.7.1", SyncedRevision: "6.7.0", Type: "Helm",
		}), "a chart from a registry has no ScmConnection")
		Expect(s.Sources[1].Ref).To(Equal("values"))
		Expect(s.Sources[1].ScmConnectionName).To(Equal("ms-gitlab"))
		Expect(s.Sources[2].ScmConnectionName).To(Equal("ms-example"), "matched by its own host")
	})

	It("is MissingScmConnection when a Git repository of the Application has none, not for a chart repository", func() {
		connection("ms-gitlab", "https://gitlab.com")
		got := reconcilePolicy()
		cond := meta.FindStatusCondition(got.Status.Conditions, "Ready")
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("MissingScmConnection"))
		Expect(cond.Message).To(ContainSubstring("ms-app"))
		Expect(got.Status.Applications[0].Sources[2].ScmConnectionName).To(BeEmpty())
	})

	It("lists one source for an Application with one source", func() {
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, app))).To(Succeed())
		app = &unstructured.Unstructured{}
		app.SetGroupVersionKind(applicationGVK)
		app.SetName("ms-app")
		app.SetNamespace(ns)
		Expect(unstructured.SetNestedMap(app.Object, map[string]any{"repoURL": "https://gitlab.com/x/y.git", "path": "a"}, "spec", "source")).To(Succeed())
		Expect(k8sClient.Create(ctx, app)).To(Succeed())
		connection("ms-gitlab", "https://gitlab.com")
		got := reconcilePolicy()
		Expect(got.Status.Applications[0].Sources).To(HaveLen(1))
		Expect(got.Status.Applications[0].First().RepoURL).To(Equal("https://gitlab.com/x/y.git"))
		Expect(got.Status.Applications[0].First().ScmConnectionName).To(Equal("ms-gitlab"))
	})
})
