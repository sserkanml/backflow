package controller

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	backflowv1alpha1 "github.com/sserkanml/backflow/api/v1alpha1"
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
		policy *backflowv1alpha1.BackflowPolicy
		conn   *backflowv1alpha1.ScmConnection
	)

	BeforeEach(func() {
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "sweep-token", Namespace: ns}, StringData: map[string]string{"token": "tok"},
		})).To(Succeed())
		conn = &backflowv1alpha1.ScmConnection{
			ObjectMeta: metav1.ObjectMeta{Name: "sweep-scm", Namespace: ns},
			Spec: backflowv1alpha1.ScmConnectionSpec{
				Provider: backflowv1alpha1.ScmProviderGitLab, URL: "https://git.test",
				TokenSecretRef: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "sweep-token"}, Key: "token"},
			},
		}
		Expect(k8sClient.Create(ctx, conn)).To(Succeed())
		policy = &backflowv1alpha1.BackflowPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "sweep-policy", Namespace: ns},
			Spec: backflowv1alpha1.BackflowPolicySpec{
				ArgoCDNamespace: ns, Applications: backflowv1alpha1.ApplicationSelector{Names: []string{"sweep-app"}},
				Mode: backflowv1alpha1.ModeMergeRequest,
			},
		}
		Expect(k8sClient.Create(ctx, policy)).To(Succeed())
		policy.Status.Applications = []backflowv1alpha1.ApplicationSummary{{
			Name: "sweep-app", ScmConnection: "sweep-scm",
			Sources: []backflowv1alpha1.SourceSummary{
				{Position: 1, RepoURL: "ghcr.io/x", Chart: "c"},
				{Position: 2, RepoURL: "https://git.test/a/values.git", Ref: "values", ScmConnection: "sweep-scm"},
				{Position: 3, RepoURL: "https://git.test/a/nokey.git", Ref: "nokey"},
			},
		}}
		Expect(k8sClient.Status().Update(ctx, policy)).To(Succeed())
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

	It("leaves a policy that does not allow scratch branches alone", func() {
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(policy), policy)).To(Succeed())
		policy.Spec.Mapping = &backflowv1alpha1.MappingOptions{ScratchBranches: backflowv1alpha1.ScratchBranchesNever}
		Expect(k8sClient.Update(ctx, policy)).To(Succeed())
		git := &sweepGit{}
		(&ScratchSweeper{Client: k8sClient, Git: git}).Sweep(ctx)
		Expect(git.listed).To(BeEmpty())
	})
})
