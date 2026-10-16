package controller

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

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
	"github.com/sserkanml/backflow/internal/helmmap"
	"github.com/sserkanml/backflow/internal/scm"
)

// fakeHelmRenderer stands in for Argo CD: it renders a small chart (one
// ConfigMap) from the files it is given, so a change to a value file in a
// commit changes the render.
type fakeHelmRenderer struct {
	mu    sync.Mutex
	calls int
	// failures are returned by the first renders, one each.
	failures []error
	// err is returned by every render when set.
	err error
	// lastApp is the Application of the last render.
	lastApp helmmap.App
}

func deepMergeValues(dst, src map[string]any) {
	for k, v := range src {
		sm, sIsMap := v.(map[string]any)
		dm, dIsMap := dst[k].(map[string]any)
		if sIsMap && dIsMap {
			deepMergeValues(dm, sm)
			continue
		}
		dst[k] = v
	}
}

func nested(m map[string]any, path ...string) string {
	var cur any = m
	for _, k := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = mm[k]
	}
	return fmt.Sprint(cur)
}

func (f *fakeHelmRenderer) Render(_ context.Context, app helmmap.App, files map[string][]byte) ([]*unstructured.Unstructured, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastApp = app
	if len(f.failures) > 0 {
		err := f.failures[0]
		f.failures = f.failures[1:]
		return nil, err
	}
	if f.err != nil {
		return nil, f.err
	}
	values := map[string]any{}
	for _, name := range []string{"helm/demo/values.yaml", "helm/demo/values-dev.yaml", "helm/demo/values-extra.yaml"} {
		var layer map[string]any
		if err := yaml.Unmarshal(files[name], &layer); err != nil {
			return nil, fmt.Errorf("%w: %s: %v", helmmap.ErrRenderFailed, name, err)
		}
		deepMergeValues(values, layer)
	}
	minA, minB := nested(values, "config", "minA"), nested(values, "config", "minB")
	level := nested(values, "config", "logLevel")
	cm := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": "demo-helm-config", "namespace": app.DestNamespace},
		"data": map[string]any{
			"LOG_LEVEL":    level,
			"LOG_LEVEL_UP": strings.ToUpper(level),
			"REGION":       nested(values, "global", "region"),
			"TIMEOUT":      nested(values, "config", "timeout"),
			"AMBIGUOUS":    max(minA, minB),
		},
	}}
	return []*unstructured.Unstructured{cm}, nil
}

const (
	helmChartYAML = "apiVersion: v2\nname: demo\nversion: 0.1.0\n"
	helmValues    = "# Chart defaults.\nconfig:\n  logLevel: info\n  timeout: 30\n  minA: 1\n  minB: 1\nglobal:\n  region: eu-west-1\n"
	helmValuesDev = `# Development environment.
config:
  # Log verbosity.
  logLevel: debug
  minA: 3
  minB: 3
global:
  region: eu-central-1
`
)

func helmFiles() map[string]string {
	return map[string]string{
		"helm/demo/Chart.yaml":        helmChartYAML,
		"helm/demo/values.yaml":       helmValues,
		"helm/demo/values-dev.yaml":   helmValuesDev,
		"helm/demo/values-prod.yaml":  "global:\n  region: us-east-1\n",
		"helm/demo/templates/cm.yaml": "kind: ConfigMap\n",
		"helm/demo/README.md":         "demo chart\n",
	}
}

var _ = Describe("DriftProposal of a Helm Application", func() {
	const ns = "default"
	var (
		ctx      = context.Background()
		repo     *gitFixture
		initial  string
		provider *fakeProvider
		cacheDir string
		renderer *fakeHelmRenderer
		r        *DriftProposalReconciler
		policy   *backflowv1alpha1.BackflowPolicy
		app      *unstructured.Unstructured
		dp       *backflowv1alpha1.DriftProposal
		conn     *backflowv1alpha1.ScmConnection
	)

	regionChange := backflowv1alpha1.FieldChange{
		Path: "/data/REGION", Op: backflowv1alpha1.OpReplace, Desired: `"eu-central-1"`, Live: `"us-west-2"`,
	}
	reconcileDP := func() (reconcile.Result, error) {
		return r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(dp)})
	}
	latest := func() *backflowv1alpha1.DriftProposal {
		got := &backflowv1alpha1.DriftProposal{}
		ExpectWithOffset(1, k8sClient.Get(ctx, client.ObjectKeyFromObject(dp), got)).To(Succeed())
		return got
	}
	condition := func(t string) *metav1.Condition { return meta.FindStatusCondition(latest().Status.Conditions, t) }
	// settle reconciles until the proposal reaches the phase, or fails.
	settle := func(phase backflowv1alpha1.ProposalPhase) {
		for range 5 {
			_, err := reconcileDP()
			ExpectWithOffset(1, err).NotTo(HaveOccurred())
			if latest().Status.Phase == phase {
				return
			}
		}
		Fail(fmt.Sprintf("the proposal did not reach %s: %s", phase, latest().Status.Phase))
	}
	recreate := func(mutate func(spec *backflowv1alpha1.DriftProposalSpec)) {
		ExpectWithOffset(1, k8sClient.Delete(ctx, dp)).To(Succeed())
		fresh := &backflowv1alpha1.DriftProposal{
			ObjectMeta: metav1.ObjectMeta{Name: dp.Name, Namespace: dp.Namespace},
			Spec:       *dp.Spec.DeepCopy(),
		}
		mutate(&fresh.Spec)
		ExpectWithOffset(1, k8sClient.Create(ctx, fresh)).To(Succeed())
		dp = fresh
	}
	setMode := func(mode backflowv1alpha1.BackflowMode) {
		ExpectWithOffset(1, k8sClient.Get(ctx, client.ObjectKeyFromObject(policy), policy)).To(Succeed())
		policy.Spec.Mode = mode
		ExpectWithOffset(1, k8sClient.Update(ctx, policy)).To(Succeed())
	}
	setHelm := func(helm map[string]any) {
		ExpectWithOffset(1, unstructured.SetNestedField(app.Object, helm, "spec", "source", "helm")).To(Succeed())
		ExpectWithOffset(1, k8sClient.Update(ctx, app)).To(Succeed())
	}

	BeforeEach(func() {
		repo = newGitFixture(helmFiles())
		initial = repo.branch("main")
		provider = newFakeProvider()
		provider.tip = repo.branch
		renderer = &fakeHelmRenderer{}

		var err error
		cacheDir, err = os.MkdirTemp("", "backflow-helm-cache-*")
		Expect(err).NotTo(HaveOccurred())
		cache := gitrepo.NewCache(cacheDir)
		r = &DriftProposalReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Repos: CacheReader{Cache: cache}, Writer: cache,
			NewProvider: func(scm.Config) (scm.Provider, error) { return provider, nil },
			NewRenderer: func(argocd.Config) (helmmap.Renderer, error) { return renderer, nil },
		}

		for _, name := range []string{"helm-token", "argo-token"} {
			Expect(k8sClient.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
				StringData: map[string]string{"token": "tok-" + name},
			})).To(Succeed())
		}
		conn = &backflowv1alpha1.ScmConnection{
			ObjectMeta: metav1.ObjectMeta{Name: "helm-scm", Namespace: ns},
			Spec: backflowv1alpha1.ScmConnectionSpec{
				Provider: backflowv1alpha1.ScmProviderGitLab, URL: "https://git.test",
				TokenSecretRef: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "helm-token"}, Key: "token"},
			},
		}
		Expect(k8sClient.Create(ctx, conn)).To(Succeed())

		policy = &backflowv1alpha1.BackflowPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "helm-policy", Namespace: ns},
			Spec: backflowv1alpha1.BackflowPolicySpec{
				ArgoCDNamespace: ns,
				Applications:    backflowv1alpha1.ApplicationSelector{Names: []string{"helm-app"}},
				Mode:            backflowv1alpha1.ModeMergeRequest,
				ArgoCD: &backflowv1alpha1.ArgoCDServer{
					TokenSecretRef: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "argo-token"}, Key: "token"},
				},
			},
		}
		Expect(k8sClient.Create(ctx, policy)).To(Succeed())
		policy.Status.Applications = []backflowv1alpha1.ApplicationSummary{{Name: "helm-app", RepoURL: testRepoURL, ScmConnection: "helm-scm"}}
		Expect(k8sClient.Status().Update(ctx, policy)).To(Succeed())

		app = &unstructured.Unstructured{}
		app.SetGroupVersionKind(applicationGVK)
		app.SetName("helm-app")
		app.SetNamespace(ns)
		Expect(unstructured.SetNestedField(app.Object, "demo-helm", "spec", "destination", "namespace")).To(Succeed())
		Expect(unstructured.SetNestedField(app.Object, "helm/demo", "spec", "source", "path")).To(Succeed())
		Expect(unstructured.SetNestedStringSlice(app.Object, []string{"values-dev.yaml"}, "spec", "source", "helm", "valueFiles")).To(Succeed())
		Expect(k8sClient.Create(ctx, app)).To(Succeed())

		dp = &backflowv1alpha1.DriftProposal{
			ObjectMeta: metav1.ObjectMeta{Name: "helm-dp", Namespace: ns},
			Spec: backflowv1alpha1.DriftProposalSpec{
				PolicyName:  "helm-policy",
				Application: backflowv1alpha1.ObjectRef{Name: "helm-app", Namespace: ns},
				Resource:    backflowv1alpha1.ResourceRef{Version: "v1", Kind: "ConfigMap", Namespace: "demo-helm", Name: "demo-helm-config"},
				Source: backflowv1alpha1.SourceRef{
					RepoURL: testRepoURL, Revision: initial, TargetRevision: "main",
					Path: "helm/demo", Type: backflowv1alpha1.SourceHelm,
				},
				Changes:    []backflowv1alpha1.FieldChange{regionChange},
				Actor:      &backflowv1alpha1.Actor{Username: "alice", UserAgent: "kubectl"},
				DetectedAt: metav1.NewTime(time.Now().Add(-time.Hour)),
			},
		}
		Expect(k8sClient.Create(ctx, dp)).To(Succeed())
	})

	AfterEach(func() {
		for _, o := range []client.Object{dp, policy, app, conn,
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "helm-token", Namespace: ns}},
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "argo-token", Namespace: ns}},
		} {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, o))).To(Succeed())
		}
		repo.close()
		_ = os.RemoveAll(cacheDir)
	})

	It("goes from Detected through Mapping to Proposed with an edit of the value file", func() {
		Expect(latest().Status.Phase).To(BeEmpty())
		settle(backflowv1alpha1.PhaseProposed)

		got := latest()
		Expect(condition(conditionMapped).Status).To(Equal(metav1.ConditionTrue))
		Expect(got.Status.Mapping.Strategy).To(Equal(backflowv1alpha1.StrategyHelmValues))
		Expect(got.Status.Mapping.Verified).To(BeTrue())
		Expect(got.Status.Mapping.Edits).To(Equal([]backflowv1alpha1.SourceEdit{{
			File: "helm/demo/values-dev.yaml", Location: "global.region", Value: `"us-west-2"`, ChangeIndex: 0,
		}}))
		Expect(got.Status.Mapping.Diff).To(ContainSubstring("-  region: eu-central-1\n+  region: us-west-2\n"))
		Expect(got.Status.Message).To(ContainSubstring("Merge request"))

		By("rendering with the Application as Argo CD knows it")
		Expect(renderer.lastApp.Name).To(Equal("helm-app"))
		Expect(renderer.lastApp.Namespace).To(Equal(ns))
		Expect(renderer.lastApp.DestNamespace).To(Equal("demo-helm"))
		Expect(renderer.lastApp.ReleaseName).To(Equal("helm-app"), "the release is named after the Application unless helm.releaseName says otherwise")
		Expect(renderer.calls).To(Equal(2*(2+1)), "mapped once, and again at the head of the target branch: the unmodified files twice and the one candidate each time")

		By("the branch holds the edited value file, comments intact, and nothing else changed")
		branch := got.Status.MergeRequest.Branch
		tip := repo.branch(branch)
		Expect(repo.file(tip, "helm/demo/values-dev.yaml")).To(Equal(strings.Replace(helmValuesDev, "region: eu-central-1", "region: us-west-2", 1)))
		Expect(repo.file(tip, "helm/demo/values.yaml")).To(Equal(helmValues))
		Expect(repo.branch("main")).To(Equal(initial))

		By("the commit message and the merge request say what was edited and how it was verified")
		msg := repo.commitAt(tip).Message
		for _, want := range []string{
			"Value file: helm/demo/values-dev.yaml\n", "- global.region: eu-central-1 -> us-west-2\n",
			"Verified by rendering through Argo CD: this edit changes exactly the drifted fields (/data/REGION) and nothing else.",
			"Proposal: helm-dp\n", "Changed-by: alice\n",
		} {
			Expect(msg).To(ContainSubstring(want))
		}
		body := provider.created[0].Body
		for _, want := range []string{
			"**Helm values**", "| ` helm/demo/values-dev.yaml ` | ` global.region ` | ` eu-central-1 ` | ` us-west-2 ` |",
			"Verified by rendering through Argo CD", "(/data/REGION)", "-  region: eu-central-1\n+  region: us-west-2\n",
		} {
			Expect(body).To(ContainSubstring(want))
		}
	})

	It("is held in Mapping by ReportOnly mode, with the mapping done", func() {
		setMode(backflowv1alpha1.ModeReportOnly)
		for range 2 {
			_, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
		}
		got := latest()
		Expect(got.Status.Phase).To(Equal(backflowv1alpha1.PhaseMapping))
		Expect(condition(conditionMapped).Status).To(Equal(metav1.ConditionTrue))
		Expect(got.Status.Mapping.Strategy).To(Equal(backflowv1alpha1.StrategyHelmValues))
		Expect(condition(conditionProposed).Reason).To(Equal(reasonReportOnly))
		Expect(provider.created).To(BeEmpty())
		Expect(repo.branch("main")).To(Equal(initial))
	})

	It("maps two drifted fields that one key feeds to that key", func() {
		recreate(func(s *backflowv1alpha1.DriftProposalSpec) {
			s.Changes = []backflowv1alpha1.FieldChange{
				{Path: "/data/LOG_LEVEL", Op: backflowv1alpha1.OpReplace, Desired: `"debug"`, Live: `"trace"`},
				{Path: "/data/LOG_LEVEL_UP", Op: backflowv1alpha1.OpReplace, Desired: `"DEBUG"`, Live: `"TRACE"`},
			}
		})
		settle(backflowv1alpha1.PhaseProposed)
		got := latest()
		Expect(got.Status.Mapping.Edits).To(Equal([]backflowv1alpha1.SourceEdit{{
			File: "helm/demo/values-dev.yaml", Location: "config.logLevel", Value: `"trace"`, ChangeIndex: 0,
		}}))
		Expect(provider.created[0].Body).To(ContainSubstring("(/data/LOG_LEVEL, /data/LOG_LEVEL_UP)"))
	})

	It("edits two value files in one commit when the fields come from different files", func() {
		head := repo.commit(map[string]string{"helm/demo/values-extra.yaml": "# Overrides.\nconfig:\n  timeout: 99\n"}, "add the extra values")
		setHelm(map[string]any{"valueFiles": []any{"values-dev.yaml", "values-extra.yaml"}})
		recreate(func(s *backflowv1alpha1.DriftProposalSpec) {
			s.Source.Revision = head
			s.Changes = []backflowv1alpha1.FieldChange{
				regionChange,
				{Path: "/data/TIMEOUT", Op: backflowv1alpha1.OpReplace, Desired: `"99"`, Live: `"120"`},
			}
		})
		settle(backflowv1alpha1.PhaseProposed)

		got := latest()
		Expect(got.Status.Mapping.Edits).To(ConsistOf(
			backflowv1alpha1.SourceEdit{File: "helm/demo/values-dev.yaml", Location: "global.region", Value: `"us-west-2"`, ChangeIndex: 0},
			backflowv1alpha1.SourceEdit{File: "helm/demo/values-extra.yaml", Location: "config.timeout", Value: "120", ChangeIndex: 1},
		), "the integer stays an integer")
		tip := repo.branch(got.Status.MergeRequest.Branch)
		Expect(repo.commitAt(tip).ParentHashes[0].String()).To(Equal(head))
		Expect(repo.file(tip, "helm/demo/values-dev.yaml")).To(ContainSubstring("region: us-west-2"))
		Expect(repo.file(tip, "helm/demo/values-extra.yaml")).To(Equal("# Overrides.\nconfig:\n  timeout: 120\n"))
		Expect(provider.created[0].Body).To(ContainSubstring("helm/demo/values-extra.yaml"))
		Expect(provider.created[0].Body).To(ContainSubstring("(/data/REGION, /data/TIMEOUT)"))
	})

	DescribeTable("is Unmapped with the reason in the Mapped condition",
		func(mutate func(), reason string, message ...string) {
			mutate()
			_, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			_, err = reconcileDP()
			Expect(err).NotTo(HaveOccurred())

			got := latest()
			Expect(got.Status.Phase).To(Equal(backflowv1alpha1.PhaseUnmapped))
			c := condition(conditionMapped)
			Expect(c.Status).To(Equal(metav1.ConditionFalse))
			Expect(c.Reason).To(Equal(reason))
			Expect(got.Status.Mapping.Strategy).To(Equal(backflowv1alpha1.StrategyUnmapped))
			for _, m := range message {
				Expect(c.Message).To(ContainSubstring(m))
			}
			Expect(provider.created).To(BeEmpty())
		},
		Entry("the key is only in the chart's own values.yaml", func() {
			recreate(func(s *backflowv1alpha1.DriftProposalSpec) {
				s.Changes = []backflowv1alpha1.FieldChange{{Path: "/data/TIMEOUT", Op: backflowv1alpha1.OpReplace, Desired: `"30"`, Live: `"45"`}}
			})
		}, helmmap.ReasonNoVerifiedCandidate, "chart's own defaults", "config.timeout"),
		Entry("a derived field: the nearest edit also moves another field", func() {
			recreate(func(s *backflowv1alpha1.DriftProposalSpec) {
				s.Changes = []backflowv1alpha1.FieldChange{{Path: "/data/LOG_LEVEL", Op: backflowv1alpha1.OpReplace, Desired: `"debug"`, Live: `"trace"`}}
			})
		}, helmmap.ReasonNoVerifiedCandidate, "config.logLevel", "would reproduce /data/LOG_LEVEL", "/data/LOG_LEVEL_UP", `current "DEBUG"`),
		Entry("two keys give the same result", func() {
			recreate(func(s *backflowv1alpha1.DriftProposalSpec) {
				s.Changes = []backflowv1alpha1.FieldChange{{Path: "/data/AMBIGUOUS", Op: backflowv1alpha1.OpReplace, Desired: `"3"`, Live: `"5"`}}
			})
		}, helmmap.ReasonAmbiguousCandidates, "config.minA", "config.minB"),
		Entry("the render does not give the value Git is expected to", func() {
			recreate(func(s *backflowv1alpha1.DriftProposalSpec) {
				s.Changes = []backflowv1alpha1.FieldChange{{Path: "/data/REGION", Op: backflowv1alpha1.OpReplace, Desired: `"mars"`, Live: `"venus"`}}
			})
		}, helmmap.ReasonRenderMismatch, `"eu-central-1"`),
		Entry("the drifted resource is a Secret", func() {
			recreate(func(s *backflowv1alpha1.DriftProposalSpec) { s.Resource.Kind, s.Resource.Name = "Secret", "tls" })
		}, helmmap.ReasonSecretNotVerifiable, "Secret"),
		Entry("the chart does not render", func() { renderer.err = fmt.Errorf("%w: template: bad", helmmap.ErrRenderFailed) },
			helmmap.ReasonRenderFailed, "does not render"),
		Entry("a value file does not exist", func() { setHelm(map[string]any{"valueFiles": []any{"values-gone.yaml"}}) },
			helmmap.ReasonMissingValueFile, "values-gone.yaml"),
		Entry("a value file leaves the repository", func() { setHelm(map[string]any{"valueFiles": []any{"../../../x.yaml"}}) },
			helmmap.ReasonValueFileOutsideRepo),
		Entry("the chart comes from a Helm repository", func() {
			Expect(unstructured.SetNestedField(app.Object, "nginx", "spec", "source", "chart")).To(Succeed())
			Expect(k8sClient.Update(ctx, app)).To(Succeed())
		}, reasonUnsupportedSourceType, "Helm repository or registry"),
		Entry("the Application has several sources", func() {
			Expect(unstructured.SetNestedSlice(app.Object, []any{map[string]any{"repoURL": "x"}, map[string]any{"repoURL": "y"}}, "spec", "sources")).To(Succeed())
			Expect(k8sClient.Update(ctx, app)).To(Succeed())
		}, reasonUnsupportedSourceType, "several sources"),
	)

	Context("when Argo CD cannot render right now", func() {
		It("retries with the backoff, then maps", func() {
			renderer.failures = []error{fmt.Errorf("%w: connection refused", argocd.ErrUnreachable)}
			res, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(Equal(retryMin))
			got := latest()
			Expect(got.Status.Phase).To(Equal(backflowv1alpha1.PhaseMapping))
			c := condition(conditionMapped)
			Expect(c.Status).To(Equal(metav1.ConditionFalse))
			Expect(c.Reason).To(Equal(reasonArgoCDUnreachable))
			Expect(c.Message).To(ContainSubstring("connection refused"))
			Expect(got.Status.Mapping).To(BeNil())

			settle(backflowv1alpha1.PhaseProposed)
			Expect(condition(conditionMapped).Status).To(Equal(metav1.ConditionTrue))
			Expect(latest().Status.Mapping.Strategy).To(Equal(backflowv1alpha1.StrategyHelmValues))
		})

		It("tells a rejected token from an unreachable server", func() {
			renderer.err = fmt.Errorf("%w: HTTP 401", argocd.ErrUnauthorized)
			_, _ = reconcileDP()
			_, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(latest().Status.Phase).To(Equal(backflowv1alpha1.PhaseMapping), "a token that is fixed later makes it work again")
			Expect(condition(conditionMapped).Reason).To(Equal(reasonArgoCDUnauthorized))
		})

		It("waits for the Argo CD token Secret", func() {
			Expect(k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "argo-token", Namespace: ns}})).To(Succeed())
			_, _ = reconcileDP()
			res, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(BeNumerically(">", 0))
			Expect(condition(conditionMapped).Reason).To(Equal(reasonArgoCDCredentials))
			Expect(latest().Status.Phase).To(Equal(backflowv1alpha1.PhaseMapping))
		})
	})

	Context("when the target branch moved after the proposal was mapped", func() {
		mapOnly := func() {
			setMode(backflowv1alpha1.ModeReportOnly)
			for range 2 {
				_, err := reconcileDP()
				Expect(err).NotTo(HaveOccurred())
			}
			ExpectWithOffset(1, condition(conditionMapped).Status).To(Equal(metav1.ConditionTrue))
			setMode(backflowv1alpha1.ModeMergeRequest)
		}

		It("renders the files of the new head and builds the commit on it", func() {
			mapOnly()
			head := repo.commit(map[string]string{"helm/demo/README.md": "changed\n", "helm/demo/values-prod.yaml": "global:\n  region: us-east-2\n"}, "elsewhere")
			callsBefore := renderer.calls

			settle(backflowv1alpha1.PhaseProposed)
			Expect(renderer.calls-callsBefore).To(Equal(2+1), "the head is rendered again, twice for the baseline and once for the edit")
			tip := repo.branch(latest().Status.MergeRequest.Branch)
			Expect(repo.commitAt(tip).ParentHashes[0].String()).To(Equal(head))
			Expect(repo.file(tip, "helm/demo/README.md")).To(Equal("changed\n"))
			Expect(repo.file(tip, "helm/demo/values-dev.yaml")).To(ContainSubstring("region: us-west-2"))
		})

		It("is superseded when the head no longer renders the old value", func() {
			mapOnly()
			repo.commit(map[string]string{"helm/demo/values-dev.yaml": strings.Replace(helmValuesDev, "eu-central-1", "eu-north-1", 1)}, "someone changed the region")

			_, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			got := latest()
			Expect(got.Status.Phase).To(Equal(backflowv1alpha1.PhaseSuperseded))
			Expect(condition(conditionProposed).Reason).To(Equal(reasonSourceChanged))
			Expect(got.Status.Message).To(ContainSubstring(sourceChangedMessage))
			Expect(got.Status.Message).To(ContainSubstring(helmmap.ReasonRenderMismatch))
			Expect(provider.created).To(BeEmpty())
		})

		It("is not superseded by Argo CD being unreachable at the new head", func() {
			mapOnly()
			repo.commit(map[string]string{"helm/demo/README.md": "changed\n"}, "elsewhere")
			renderer.failures = []error{errors.New("boom")}

			res, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(BeNumerically(">", 0))
			Expect(latest().Status.Phase).To(Equal(backflowv1alpha1.PhaseMapping))
			Expect(condition(conditionProposed).Reason).To(Equal(reasonArgoCDUnreachable))

			settle(backflowv1alpha1.PhaseProposed)
		})
	})
})
