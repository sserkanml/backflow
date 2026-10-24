package controller

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.yaml.in/yaml/v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	backflowv1alpha1 "github.com/sserkanml/backflow/api/v1alpha1"
	"github.com/sserkanml/backflow/internal/argocd"
	"github.com/sserkanml/backflow/internal/gitrepo"
	"github.com/sserkanml/backflow/internal/scm"
)

const podinfoDefaults = "# the chart's own values\nreplicaCount: 1\nui:\n  color: \"#34577c\"\n  message: \"\"\npodAnnotations: {}\n"

const msValues = "# environment values\nreplicaCount: 2\nregion: eu-central-1\n"

// fakeChartArgo stands in for Argo CD: the values of a chart, and a render of
// the Application by revision. It reads the value file at the revision of the
// values source from the repository, so a scratch commit changes the render.
type fakeChartArgo struct {
	repo *gitFixture

	mu         sync.Mutex
	queries    []argocd.RevisionsQuery
	details    *argocd.HelmDetails
	detailsErr error
	renderErr  error
	// valueFile is the file of the repository that holds the values.
	valueFile string
}

func (f *fakeChartArgo) GetHelmDetails(context.Context, string, string, string, map[string]any, int) (*argocd.HelmDetails, error) {
	return f.details, f.detailsErr
}

func (f *fakeChartArgo) GetManifestsByRevisions(_ context.Context, q argocd.RevisionsQuery) (*argocd.ManifestsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries = append(f.queries, q)
	if f.renderErr != nil {
		return nil, f.renderErr
	}
	values := map[string]any{}
	var defaults map[string]any
	if err := yaml.Unmarshal([]byte(f.details.Values), &defaults); err != nil {
		return nil, err
	}
	deepMergeValues(values, defaults)
	var layer map[string]any
	if err := yaml.Unmarshal([]byte(f.repo.file(q.Revisions[1], f.valueFile)), &layer); err != nil {
		return nil, fmt.Errorf("%w: %v", argocd.ErrRenderFailed, err)
	}
	deepMergeValues(values, layer)
	cm := fmt.Sprintf(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"ms-config"},"data":{"REPLICAS":%q,"COLOR":%q,"REGION":%q}}`,
		nested(values, "replicaCount"), nested(values, "ui", "color"), nested(values, "region"))
	return &argocd.ManifestsResponse{Manifests: []string{cm}}, nil
}

// refusingWriter fails the push of scratch branches.
type refusingWriter struct {
	RepositoryWriter
	err error
}

func (w refusingWriter) PushBranches(context.Context, string, map[string]string, map[string]string, *gitrepo.Auth) error {
	return w.err
}

var _ = Describe("DriftProposal of a multi-source Application", func() {
	const ns = "default"
	var (
		ctx      = context.Background()
		repo     *gitFixture
		initial  string
		provider *fakeProvider
		cacheDir string
		argo     *fakeChartArgo
		r        *DriftProposalReconciler
		policy   *backflowv1alpha1.BackflowPolicy
		app      *unstructured.Unstructured
		dp       *backflowv1alpha1.DriftProposal
		conn     *backflowv1alpha1.ScmConnection
	)

	replicas := backflowv1alpha1.FieldChange{Path: "/data/REPLICAS", Op: backflowv1alpha1.OpReplace, Desired: `"2"`, Live: `"5"`}
	reconcileDP := func() error {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(dp)})
		return err
	}
	latest := func() *backflowv1alpha1.DriftProposal {
		got := &backflowv1alpha1.DriftProposal{}
		ExpectWithOffset(1, k8sClient.Get(ctx, client.ObjectKeyFromObject(dp), got)).To(Succeed())
		return got
	}
	mapped := func() *metav1.Condition { return meta.FindStatusCondition(latest().Status.Conditions, conditionMapped) }
	settle := func(phase backflowv1alpha1.ProposalPhase) {
		for range 5 {
			ExpectWithOffset(1, reconcileDP()).To(Succeed())
			if latest().Status.Phase == phase {
				return
			}
		}
		Fail(fmt.Sprintf("the proposal did not reach %s: %s (%v)", phase, latest().Status.Phase, mapped()))
	}
	recreate := func(mutate func(spec *backflowv1alpha1.DriftProposalSpec)) {
		ExpectWithOffset(1, k8sClient.Delete(ctx, dp)).To(Succeed())
		fresh := &backflowv1alpha1.DriftProposal{ObjectMeta: metav1.ObjectMeta{Name: dp.Name, Namespace: dp.Namespace}, Spec: *dp.Spec.DeepCopy()}
		mutate(&fresh.Spec)
		ExpectWithOffset(1, k8sClient.Create(ctx, fresh)).To(Succeed())
		dp = fresh
	}
	setPolicy := func(mutate func(*backflowv1alpha1.BackflowPolicy)) {
		ExpectWithOffset(1, k8sClient.Get(ctx, client.ObjectKeyFromObject(policy), policy)).To(Succeed())
		mutate(policy)
		ExpectWithOffset(1, k8sClient.Update(ctx, policy)).To(Succeed())
	}
	scratchBranches := func() []string {
		var out []string
		refs, err := repo.open().References()
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		_ = refs.ForEach(func(ref *plumbing.Reference) error {
			if strings.HasPrefix(ref.Name().Short(), "backflow/scratch/") {
				out = append(out, ref.Name().Short())
			}
			return nil
		})
		return out
	}
	setHelm := func(helm map[string]any) {
		sources, _, _ := unstructured.NestedSlice(app.Object, "spec", "sources")
		sources[0].(map[string]any)["helm"] = helm
		ExpectWithOffset(1, unstructured.SetNestedSlice(app.Object, sources, "spec", "sources")).To(Succeed())
		ExpectWithOffset(1, k8sClient.Update(ctx, app)).To(Succeed())
	}

	BeforeEach(func() {
		repo = newGitFixture(map[string]string{"env/values.yaml": msValues, "env/extra.yaml": "podAnnotations:\n  team: a\n", "README.md": "values\n"})
		initial = repo.branch("main")
		provider = newFakeProvider()
		provider.tip = repo.branch
		argo = &fakeChartArgo{repo: repo, valueFile: "env/values.yaml", details: &argocd.HelmDetails{
			Values: podinfoDefaults, ValueFiles: []string{"values.yaml", "charts/redis/values.yaml"},
		}}

		var err error
		cacheDir, err = os.MkdirTemp("", "backflow-multi-cache-*")
		Expect(err).NotTo(HaveOccurred())
		cache := gitrepo.NewCache(cacheDir)
		r = &DriftProposalReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Repos: CacheReader{Cache: cache}, Writer: cache,
			NewProvider:  func(scm.Config) (scm.Provider, error) { return provider, nil },
			NewChartArgo: func(argocd.Config) (ChartArgo, error) { return argo, nil },
		}

		for _, name := range []string{"ms-token", "ms-argo-token"} {
			Expect(k8sClient.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, StringData: map[string]string{"token": "tok-" + name},
			})).To(Succeed())
		}
		conn = &backflowv1alpha1.ScmConnection{
			ObjectMeta: metav1.ObjectMeta{Name: "ms-scm", Namespace: ns},
			Spec: backflowv1alpha1.ScmConnectionSpec{
				Provider: backflowv1alpha1.ScmProviderGitLab, URL: "https://git.test",
				TokenSecretRef: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "ms-token"}, Key: "token"},
			},
		}
		Expect(k8sClient.Create(ctx, conn)).To(Succeed())

		policy = &backflowv1alpha1.BackflowPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "ms-policy", Namespace: ns},
			Spec: backflowv1alpha1.BackflowPolicySpec{
				ArgoCDNamespace: ns, Applications: backflowv1alpha1.ApplicationSelector{Names: []string{"ms-app"}},
				Mode: backflowv1alpha1.ModeMergeRequest,
				ArgoCD: &backflowv1alpha1.ArgoCDServer{
					TokenSecretRef: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "ms-argo-token"}, Key: "token"},
				},
			},
		}
		Expect(k8sClient.Create(ctx, policy)).To(Succeed())
		policy.Status.Applications = []backflowv1alpha1.ApplicationSummary{{
			Name: "ms-app", RepoURL: "ghcr.io/stefanprodan/charts", ScmConnection: "ms-scm",
			Sources: []backflowv1alpha1.SourceSummary{
				{Position: 1, RepoURL: "ghcr.io/stefanprodan/charts", Chart: "podinfo"},
				{Position: 2, RepoURL: testRepoURL, Ref: "values", ScmConnection: "ms-scm"},
			},
		}}
		Expect(k8sClient.Status().Update(ctx, policy)).To(Succeed())

		app = &unstructured.Unstructured{}
		app.SetGroupVersionKind(applicationGVK)
		app.SetName("ms-app")
		app.SetNamespace(ns)
		Expect(unstructured.SetNestedField(app.Object, "default", "spec", "project")).To(Succeed())
		Expect(unstructured.SetNestedField(app.Object, "ms", "spec", "destination", "namespace")).To(Succeed())
		Expect(unstructured.SetNestedSlice(app.Object, []any{
			map[string]any{"repoURL": "ghcr.io/stefanprodan/charts", "chart": "podinfo", "targetRevision": "6.7.*",
				"helm": map[string]any{"valueFiles": []any{"$values/env/values.yaml"}}},
			map[string]any{"repoURL": testRepoURL, "targetRevision": "main", "ref": "values"},
		}, "spec", "sources")).To(Succeed())
		Expect(k8sClient.Create(ctx, app)).To(Succeed())

		dp = &backflowv1alpha1.DriftProposal{
			ObjectMeta: metav1.ObjectMeta{Name: "ms-dp", Namespace: ns},
			Spec: backflowv1alpha1.DriftProposalSpec{
				PolicyName:  "ms-policy",
				Application: backflowv1alpha1.ObjectRef{Name: "ms-app", Namespace: ns},
				Resource:    backflowv1alpha1.ResourceRef{Version: "v1", Kind: "ConfigMap", Namespace: "ms", Name: "ms-config"},
				Source: backflowv1alpha1.SourceRef{
					RepoURL: "ghcr.io/stefanprodan/charts", Revision: "6.7.1," + initial, TargetRevision: "6.7.*",
					Path: "podinfo", Type: backflowv1alpha1.SourceHelm,
				},
				Sources: []backflowv1alpha1.ApplicationSource{
					{Position: 1, Role: backflowv1alpha1.RoleRender, RepoURL: "ghcr.io/stefanprodan/charts", Revision: "6.7.1",
						TargetRevision: "6.7.*", Chart: "podinfo", Type: backflowv1alpha1.SourceHelm},
					{Position: 2, Role: backflowv1alpha1.RoleValues, RepoURL: testRepoURL, Revision: initial, TargetRevision: "main", Ref: "values"},
				},
				Changes:    []backflowv1alpha1.FieldChange{replicas},
				Actor:      &backflowv1alpha1.Actor{Username: "alice", UserAgent: "kubectl"},
				DetectedAt: metav1.NewTime(time.Now().Add(-time.Hour)),
			},
		}
		Expect(k8sClient.Create(ctx, dp)).To(Succeed())
	})

	AfterEach(func() {
		for _, o := range []client.Object{dp, policy, app, conn,
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "ms-token", Namespace: ns}},
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "ms-argo-token", Namespace: ns}},
		} {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, o))).To(Succeed())
		}
		repo.close()
		_ = os.RemoveAll(cacheDir)
	})

	It("maps a drift to the value file of the ref source and proposes it in that repository", func() {
		settle(backflowv1alpha1.PhaseProposed)
		got := latest()
		Expect(mapped().Status).To(Equal(metav1.ConditionTrue))
		Expect(got.Status.Mapping.Strategy).To(Equal(backflowv1alpha1.StrategyHelmValues))
		Expect(got.Status.Mapping.RepoURL).To(Equal(testRepoURL), "the repository the edits are written to")
		Expect(got.Status.Mapping.Edits).To(Equal([]backflowv1alpha1.SourceEdit{{
			File: "env/values.yaml", Location: "replicaCount", Value: "5", ChangeIndex: 0,
		}}))
		Expect(got.Status.Mapping.Diff).To(ContainSubstring("-replicaCount: 2\n+replicaCount: 5\n"))
		Expect(mapped().Message).To(ContainSubstring("env/values.yaml of " + testRepoURL))

		By("verifying by renders that pin every source, and the values source to scratch commits")
		Expect(argo.queries).NotTo(BeEmpty())
		for _, q := range argo.queries {
			Expect(q.Revisions[0]).To(Equal("6.7.1"), "the chart version, never the range")
			Expect(q.SourcePositions).To(Equal([]int{1, 2}))
			Expect(q.AppName).To(Equal("ms-app"))
		}
		Expect(argo.queries[0].Revisions[1]).To(Equal(initial), "the unmodified files are rendered at the commit the Application compared against")

		By("leaving no scratch branch behind, and the target branch where it was")
		Expect(scratchBranches()).To(BeEmpty())
		Expect(repo.branch("main")).To(Equal(initial))

		By("the merge request is made in the values repository, and says where the sources are")
		branch := got.Status.MergeRequest.Branch
		tip := repo.branch(branch)
		Expect(repo.file(tip, "env/values.yaml")).To(Equal(strings.Replace(msValues, "replicaCount: 2", "replicaCount: 5", 1)))
		body := provider.created[0].Body
		Expect(body).To(ContainSubstring("**Sources:** chart podinfo 6.7.1 from ghcr.io/stefanprodan/charts"))
		Expect(body).To(ContainSubstring("values in " + testRepoURL + " (`$values`"))
	})

	It("adds a key that overrides a default the chart's values.yaml gives, read from Argo CD", func() {
		recreate(func(s *backflowv1alpha1.DriftProposalSpec) {
			s.Changes = []backflowv1alpha1.FieldChange{{Path: "/data/COLOR", Op: backflowv1alpha1.OpReplace, Desired: `"#34577c"`, Live: `"#ff0000"`}}
		})
		settle(backflowv1alpha1.PhaseProposed)
		got := latest()
		Expect(got.Status.Mapping.Edits).To(Equal([]backflowv1alpha1.SourceEdit{{
			File: "env/values.yaml", Location: "ui.color", Value: `"#ff0000"`, ChangeIndex: 0,
		}}))
		Expect(got.Status.Mapping.Diff).To(ContainSubstring("+ui:\n+  color:"))
		Expect(provider.created[0].Body).To(ContainSubstring("Overrides a chart default"))
		Expect(provider.created[0].Body).To(ContainSubstring("podinfo@6.7.1/values.yaml: ui.color"))
		Expect(scratchBranches()).To(BeEmpty())
	})

	Context("scratch branches", func() {
		It("are not used in ReportOnly mode by default: Unmapped, naming the field", func() {
			setPolicy(func(p *backflowv1alpha1.BackflowPolicy) { p.Spec.Mode = backflowv1alpha1.ModeReportOnly })
			settle(backflowv1alpha1.PhaseUnmapped)
			Expect(mapped().Reason).To(Equal(reasonScratchBranchesDisabled))
			Expect(mapped().Message).To(ContainSubstring("spec.mapping.scratchBranches"))
			Expect(argo.queries).To(BeEmpty())
			Expect(scratchBranches()).To(BeEmpty())
		})

		It("are used in ReportOnly mode when the policy says Always", func() {
			setPolicy(func(p *backflowv1alpha1.BackflowPolicy) {
				p.Spec.Mode = backflowv1alpha1.ModeReportOnly
				p.Spec.Mapping = &backflowv1alpha1.MappingOptions{ScratchBranches: backflowv1alpha1.ScratchBranchesAlways}
			})
			for range 2 {
				Expect(reconcileDP()).To(Succeed())
			}
			Expect(latest().Status.Phase).To(Equal(backflowv1alpha1.PhaseMapping))
			Expect(mapped().Status).To(Equal(metav1.ConditionTrue))
			Expect(scratchBranches()).To(BeEmpty())
		})

		It("are refused by Never, also in MergeRequest mode", func() {
			setPolicy(func(p *backflowv1alpha1.BackflowPolicy) {
				p.Spec.Mapping = &backflowv1alpha1.MappingOptions{ScratchBranches: backflowv1alpha1.ScratchBranchesNever}
			})
			settle(backflowv1alpha1.PhaseUnmapped)
			Expect(mapped().Reason).To(Equal(reasonScratchBranchesDisabled))
		})

		It("a push the repository refuses is Unmapped, and nothing is left", func() {
			cache := gitrepo.NewCache(cacheDir)
			r.Writer = refusingWriter{RepositoryWriter: cache, err: gitrepo.ErrPushRejected}
			settle(backflowv1alpha1.PhaseUnmapped)
			Expect(mapped().Reason).To(Equal(reasonScratchBranchRejected))
			Expect(mapped().Message).To(ContainSubstring("backflow/scratch/"))
			Expect(scratchBranches()).To(BeEmpty())
		})
	})

	Context("what stays Unmapped", func() {
		It("a value file inside the chart", func() {
			setHelm(map[string]any{"valueFiles": []any{"values-prod.yaml", "$values/env/values.yaml"}})
			settle(backflowv1alpha1.PhaseUnmapped)
			Expect(mapped().Reason).To(Equal(reasonMultipleValueRepositories))
			Expect(mapped().Message).To(ContainSubstring("inside the chart"))
		})

		It("value files from two repositories", func() {
			sources, _, _ := unstructured.NestedSlice(app.Object, "spec", "sources")
			sources = append(sources, map[string]any{"repoURL": "https://git.test/other/values.git", "targetRevision": "main", "ref": "more"})
			sources[0].(map[string]any)["helm"] = map[string]any{"valueFiles": []any{"$values/env/values.yaml", "$more/x.yaml"}}
			Expect(unstructured.SetNestedSlice(app.Object, sources, "spec", "sources")).To(Succeed())
			Expect(k8sClient.Update(ctx, app)).To(Succeed())
			recreate(func(s *backflowv1alpha1.DriftProposalSpec) {
				s.Sources = append(s.Sources, backflowv1alpha1.ApplicationSource{Position: 3, Role: backflowv1alpha1.RoleValues,
					RepoURL: "https://git.test/other/values.git", Revision: initial, Ref: "more"})
			})
			settle(backflowv1alpha1.PhaseUnmapped)
			Expect(mapped().Reason).To(Equal(reasonMultipleValueRepositories))
			Expect(mapped().Message).To(ContainSubstring("2 repositories"))
		})

		It("a value that the Application sets inline", func() {
			setHelm(map[string]any{"valueFiles": []any{"$values/env/values.yaml"}, "parameters": []any{map[string]any{"name": "replicaCount", "value": "2"}}})
			settle(backflowv1alpha1.PhaseUnmapped)
			Expect(mapped().Message).To(ContainSubstring("in the Application"))
			Expect(mapped().Message).To(ContainSubstring("roadmap B6"))
		})

		It("an Application outside the supported shape", func() {
			Expect(unstructured.SetNestedSlice(app.Object, []any{
				map[string]any{"repoURL": testRepoURL, "path": "a"}, map[string]any{"repoURL": testRepoURL, "path": "b"},
			}, "spec", "sources")).To(Succeed())
			Expect(k8sClient.Update(ctx, app)).To(Succeed())
			settle(backflowv1alpha1.PhaseUnmapped)
			Expect(mapped().Reason).To(Equal(reasonUnsupportedSourceType))
			Expect(mapped().Message).To(ContainSubstring("not a Helm source"))
		})

		It("an Argo CD that cannot render by revision", func() {
			argo.renderErr = fmt.Errorf("%w: unknown field", argocd.ErrUnsupported)
			settle(backflowv1alpha1.PhaseUnmapped)
			Expect(mapped().Reason).To(Equal(reasonArgoCDUnsupported))
			Expect(mapped().Message).To(ContainSubstring(minArgoCDVersion))
			Expect(scratchBranches()).To(BeEmpty())
		})
	})

	Context("what is tried again", func() {
		It("a chart Argo CD does not let the account read", func() {
			argo.detailsErr = fmt.Errorf("%w: permission denied", argocd.ErrUnauthorized)
			for range 2 {
				Expect(reconcileDP()).To(Succeed())
			}
			Expect(latest().Status.Phase).To(Equal(backflowv1alpha1.PhaseMapping))
			Expect(mapped().Status).To(Equal(metav1.ConditionFalse))
			Expect(mapped().Reason).To(Equal(reasonChartDefaultsUnavailable))
			Expect(mapped().Message).To(ContainSubstring("repositories, get"))
		})

		It("a repository without a ScmConnection", func() {
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(policy), policy)).To(Succeed())
			policy.Status.Applications[0].Sources[1].ScmConnection = ""
			Expect(k8sClient.Status().Update(ctx, policy)).To(Succeed())
			for range 2 {
				Expect(reconcileDP()).To(Succeed())
			}
			Expect(latest().Status.Phase).To(Equal(backflowv1alpha1.PhaseMapping))
		})
	})
})
