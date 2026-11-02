package controller

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	backflowv1beta1 "github.com/sserkanml/backflow/api/v1beta1"
	"github.com/sserkanml/backflow/internal/gitrepo"
	"github.com/sserkanml/backflow/internal/scratch"
)

type sweepGit struct {
	scratch.Git
	listed   map[string]int
	branches []gitrepo.RemoteBranch
	deleted  []string
}

func (g *sweepGit) ListBranches(_ context.Context, repo, prefix string, _ *gitrepo.Auth) ([]gitrepo.RemoteBranch, error) {
	if g.listed == nil {
		g.listed = map[string]int{}
	}
	g.listed[repo+" "+prefix]++
	return g.branches, nil
}

func (g *sweepGit) DeleteBranches(_ context.Context, _ string, names []string, _ *gitrepo.Auth) error {
	g.deleted = append(g.deleted, names...)
	return nil
}

var _ = Describe("ScratchSweeper", func() {
	const ns = "default"
	var (
		ctx    = context.Background()
		policy *backflowv1beta1.BackflowPolicy
		conn   *backflowv1beta1.ScmConnection
	)

	// createApp creates an Argo CD Application with these sources, removed after the spec.
	createApp := func(name string, sources []any) {
		app := &unstructured.Unstructured{}
		app.SetGroupVersionKind(applicationGVK)
		app.SetName(name)
		app.SetNamespace(ns)
		app.SetLabels(map[string]string{"sweep-test": "yes"})
		ExpectWithOffset(1, unstructured.SetNestedSlice(app.Object, sources, "spec", "sources")).To(Succeed())
		ExpectWithOffset(1, k8sClient.Create(ctx, app)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, app) })
	}

	BeforeEach(func() {
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "sweep-token", Namespace: ns}, StringData: map[string]string{"token": "tok"},
		})).To(Succeed())
		conn = &backflowv1beta1.ScmConnection{
			ObjectMeta: metav1.ObjectMeta{Name: "sweep-scm", Namespace: ns},
			Spec: backflowv1beta1.ScmConnectionSpec{
				Provider: backflowv1beta1.ScmProviderGitLab, URL: "https://git.test",
				TokenSecretRef: backflowv1beta1.SecretKeyReference{Name: "sweep-token", Key: "token"},
			},
		}
		Expect(k8sClient.Create(ctx, conn)).To(Succeed())
		policy = &backflowv1beta1.BackflowPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "sweep-policy", Namespace: ns},
			Spec: backflowv1beta1.BackflowPolicySpec{
				ArgoCD: backflowv1beta1.ArgoCDSpec{Namespace: ns}, Applications: backflowv1beta1.ApplicationSelector{Names: []string{"sweep-app"}},
				Mode: backflowv1beta1.ModeMergeRequest,
			},
		}
		Expect(k8sClient.Create(ctx, policy)).To(Succeed())
		createApp("sweep-app", []any{
			map[string]any{"repoURL": "ghcr.io/x", "chart": "c", "targetRevision": "1.0.0"},
			map[string]any{"repoURL": "https://git.test/a/values.git", "targetRevision": "main", "ref": "values"},
			map[string]any{"repoURL": "https://other.example/a/nokey.git", "targetRevision": "main", "ref": "nokey"},
		})
	})

	AfterEach(func() {
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, policy))).To(Succeed())
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, conn))).To(Succeed())
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "sweep-token", Namespace: ns}}))).To(Succeed())
	})

	It("deletes the old scratch branches of the repositories of ref sources, and only those", func() {
		now := time.Now()
		git := &sweepGit{branches: []gitrepo.RemoteBranch{
			{Name: scratch.BranchPrefix + "old", When: now.Add(-3 * time.Hour)},
			{Name: scratch.BranchPrefix + "new", When: now.Add(-time.Minute)},
		}}
		s := &ScratchSweeper{Client: k8sClient, Git: git, Now: func() time.Time { return now }}
		s.Sweep(ctx)
		Expect(git.deleted).To(Equal([]string{scratch.BranchPrefix + "old"}))
		Expect(git.listed).To(Equal(map[string]int{"https://git.test/a/values.git " + scratch.BranchPrefix: 1}),
			"the chart repository and a repository without a connection are not touched")
	})

	It("sweeps the repositories of Applications beyond the first 100 of a policy, which status.applications does not list", func() {
		// 102 Applications, in name order: sweep-app, sweep-bulk-000 ... sweep-bulk-100, and one more after them all.
		bulk := []any{map[string]any{"repoURL": "https://git.test/a/values.git", "targetRevision": "main", "ref": "values"}}
		names := []string{"sweep-app"}
		for i := range 101 {
			name := fmt.Sprintf("sweep-bulk-%03d", i)
			createApp(name, bulk)
			names = append(names, name)
		}
		last := []any{map[string]any{"repoURL": "https://git.test/z/last-values.git", "targetRevision": "main", "ref": "values"}}
		createApp("sweep-zzz-last", last)
		names = append(names, "sweep-zzz-last")
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(policy), policy)).To(Succeed())
		// More than 100 Applications cannot be named, so the policy selects them by label.
		policy.Spec.Applications = backflowv1beta1.ApplicationSelector{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"sweep-test": "yes"}}}
		Expect(k8sClient.Update(ctx, policy)).To(Succeed())

		// What the policy controller writes: a status that stops at 100.
		listed := make([]backflowv1beta1.ApplicationSummary, 100)
		for i := range listed {
			listed[i] = backflowv1beta1.ApplicationSummary{Name: names[i]}
		}
		policy.Status.Applications, policy.Status.ApplicationCount, policy.Status.ApplicationsTruncated = listed, 103, true
		Expect(k8sClient.Status().Update(ctx, policy)).To(Succeed())

		now := time.Now()
		git := &sweepGit{branches: []gitrepo.RemoteBranch{{Name: scratch.BranchPrefix + "old", When: now.Add(-3 * time.Hour)}}}
		(&ScratchSweeper{Client: k8sClient, Git: git, Now: func() time.Time { return now }}).Sweep(ctx)
		Expect(git.listed).To(HaveKey("https://git.test/z/last-values.git "+scratch.BranchPrefix),
			"the 103rd Application is swept although the status lists 100")
		Expect(git.listed).To(HaveKey("https://git.test/a/values.git " + scratch.BranchPrefix))
		Expect(git.listed).To(HaveLen(2), "one pass per repository")
	})

	It("leaves a policy that does not allow scratch branches alone", func() {
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(policy), policy)).To(Succeed())
		policy.Spec.Mapping = &backflowv1beta1.MappingOptions{ScratchBranches: backflowv1beta1.ScratchBranchesNever}
		Expect(k8sClient.Update(ctx, policy)).To(Succeed())
		git := &sweepGit{}
		(&ScratchSweeper{Client: k8sClient, Git: git}).Sweep(ctx)
		Expect(git.listed).To(BeEmpty())
	})
})
