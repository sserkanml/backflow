package controller

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.yaml.in/yaml/v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	backflowv1alpha1 "github.com/sserkanml/backflow/api/v1alpha1"
	"github.com/sserkanml/backflow/internal/argocd"
	"github.com/sserkanml/backflow/internal/gitrepo"
	"github.com/sserkanml/backflow/internal/rendermap"
	"github.com/sserkanml/backflow/internal/scm"
)

// childApplicationYAML is the Application manifest a parent defines in Git: the
// Helm Application of the demo chart, with the region set by a parameter.
func childApplicationYAML(region string) string {
	return fmt.Sprintf(`apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: child-app
  namespace: default
spec:
  project: default
  source:
    repoURL: %s
    path: helm/demo
    targetRevision: main
    helm:
      valueFiles:
        - values-dev.yaml
      parameters:
        - name: global.region
          value: %s
  destination:
    server: https://kubernetes.default.svc
    namespace: demo-helm
`, testRepoURL, region)
}

// fakeParentRenderer stands in for Argo CD: it renders the parents of the child
// Application (a Helm chart whose template emits it, a Kustomize base that holds
// it) and, for anything else, the demo chart of the child.
type fakeParentRenderer struct{ child *fakeHelmRenderer }

func (f *fakeParentRenderer) Render(ctx context.Context, app rendermap.App, files map[string][]byte) ([]*unstructured.Unstructured, error) {
	switch {
	case files["charts/parent/values.yaml"] != nil:
		var vals map[string]any
		if err := yaml.Unmarshal(files["charts/parent/values.yaml"], &vals); err != nil {
			return nil, fmt.Errorf("%w: %v", rendermap.ErrRenderFailed, err)
		}
		return parseApplication(childApplicationYAML(fmt.Sprint(vals["feature"])), "")
	case files["kust/base/child.yaml"] != nil:
		origin := ""
		if strings.Contains(string(files["kust/overlays/dev/kustomization.yaml"]), "buildMetadata") {
			origin = "path: ../../base/child.yaml\n"
		}
		return parseApplication(string(files["kust/base/child.yaml"]), origin)
	}
	return f.child.Render(ctx, app, files)
}

func parseApplication(text, origin string) ([]*unstructured.Unstructured, error) {
	var obj map[string]any
	if err := yaml.Unmarshal([]byte(text), &obj); err != nil {
		return nil, fmt.Errorf("%w: %v", rendermap.ErrRenderFailed, err)
	}
	u := &unstructured.Unstructured{Object: obj}
	if origin != "" {
		u.SetAnnotations(map[string]string{"config.kubernetes.io/origin": origin})
	}
	return []*unstructured.Unstructured{u}, nil
}

var _ = Describe("DriftProposal whose value an option of the Application sets", func() {
	const ns = "default"
	var (
		ctx      = context.Background()
		repo     *gitFixture
		initial  string
		provider *fakeProvider
		cacheDir string
		renderer *fakeParentRenderer
		recorder *events.FakeRecorder
		r        *DriftProposalReconciler
		policy   *backflowv1alpha1.BackflowPolicy
		child    *unstructured.Unstructured
		parents  []*unstructured.Unstructured
		dp       *backflowv1alpha1.DriftProposal
	)

	files := func() map[string]string {
		f := helmFiles()
		f["apps/children/child-app.yaml"] = childApplicationYAML("eu-central-1")
		f["apps/values-children/child-app.yaml"] = strings.Replace(childApplicationYAML("eu-central-1"),
			"      parameters:\n        - name: global.region\n          value: eu-central-1\n",
			"      values: |\n        global:\n          region: eu-central-1\n", 1)
		f["charts/parent/Chart.yaml"] = "apiVersion: v2\nname: parent\nversion: 0.1.0\n"
		f["charts/parent/values.yaml"] = "# parent values\nfeature: eu-central-1\n"
		f["charts/parent/templates/child.yaml"] = "kind: Application\n"
		f["kust/base/kustomization.yaml"] = "resources:\n- child.yaml\n"
		f["kust/base/child.yaml"] = childApplicationYAML("eu-central-1")
		f["kust/overlays/dev/kustomization.yaml"] = "resources:\n- ../../base\n"
		return f
	}
	newApp := func(name string, spec map[string]any, status map[string]any) *unstructured.Unstructured {
		a := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
		a.SetGroupVersionKind(applicationGVK)
		a.SetName(name)
		a.SetNamespace(ns)
		if status != nil {
			a.Object["status"] = status
		}
		ExpectWithOffset(1, k8sClient.Create(ctx, a)).To(Succeed())
		return a
	}
	// parent creates a parent Application of the child that is Synced at the
	// initial commit, with the child among its resources.
	parent := func(name, sourceType, path string, helm map[string]any) *unstructured.Unstructured {
		source := map[string]any{"repoURL": testRepoURL, "path": path, "targetRevision": "main"}
		if helm != nil {
			source["helm"] = helm
		}
		p := newApp(name, map[string]any{"source": source, "destination": map[string]any{"namespace": ns}}, map[string]any{
			"sourceType": sourceType,
			"sync":       map[string]any{"status": "Synced", "revision": initial},
			"resources":  []any{map[string]any{"group": "argoproj.io", "kind": "Application", "namespace": ns, "name": "child-app", "version": "v1alpha1"}},
		})
		parents = append(parents, p)
		return p
	}
	latest := func() *backflowv1alpha1.DriftProposal {
		got := &backflowv1alpha1.DriftProposal{}
		ExpectWithOffset(1, k8sClient.Get(ctx, client.ObjectKeyFromObject(dp), got)).To(Succeed())
		return got
	}
	mapped := func() *metav1.Condition { return meta.FindStatusCondition(latest().Status.Conditions, conditionMapped) }
	settle := func() *backflowv1alpha1.ApplicationValue {
		for range 5 {
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(dp)})
			ExpectWithOffset(1, err).NotTo(HaveOccurred())
			if latest().Status.Phase == backflowv1alpha1.PhaseUnmapped {
				ExpectWithOffset(1, mapped().Reason).To(Equal(rendermap.ReasonSetByApplication), mapped().Message)
				val := latest().Status.Mapping.Application
				ExpectWithOffset(1, val).NotTo(BeNil())
				return val
			}
		}
		Fail(fmt.Sprintf("the proposal did not become Unmapped: %s (%v)", latest().Status.Phase, mapped()))
		return nil
	}
	setChildHelm := func(helm map[string]any) {
		ExpectWithOffset(1, unstructured.SetNestedField(child.Object, helm, "spec", "source", "helm")).To(Succeed())
		ExpectWithOffset(1, k8sClient.Update(ctx, child)).To(Succeed())
	}

	BeforeEach(func() {
		repo = newGitFixture(files())
		initial = repo.branch("main")
		provider = newFakeProvider()
		provider.tip = repo.branch
		renderer = &fakeParentRenderer{child: &fakeHelmRenderer{}}
		recorder = events.NewFakeRecorder(20)
		var err error
		cacheDir, err = os.MkdirTemp("", "backflow-appvalues-cache-*")
		Expect(err).NotTo(HaveOccurred())
		cache := gitrepo.NewCache(cacheDir)
		r = &DriftProposalReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Repos: CacheReader{Cache: cache}, Writer: cache, Recorder: recorder,
			NewProvider: func(scm.Config) (scm.Provider, error) { return provider, nil },
			NewRenderer: func(argocd.Config) (rendermap.Renderer, error) { return renderer, nil },
		}
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "av-argo-token", Namespace: ns}, StringData: map[string]string{"token": "tok"},
		})).To(Succeed())
		policy = &backflowv1alpha1.BackflowPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "av-policy", Namespace: ns},
			Spec: backflowv1alpha1.BackflowPolicySpec{
				ArgoCDNamespace: ns,
				Applications:    backflowv1alpha1.ApplicationSelector{Names: []string{"child-app"}},
				Mode:            backflowv1alpha1.ModeMergeRequest,
				ArgoCD: &backflowv1alpha1.ArgoCDServer{
					TokenSecretRef: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "av-argo-token"}, Key: "token"},
				},
			},
		}
		Expect(k8sClient.Create(ctx, policy)).To(Succeed())
		policy.Status.Applications = []backflowv1alpha1.ApplicationSummary{{Name: "child-app", RepoURL: testRepoURL}}
		Expect(k8sClient.Status().Update(ctx, policy)).To(Succeed())

		// The child: the demo chart with the region set by a parameter that equals
		// the value file's, so only the Application decides it.
		child = newApp("child-app", map[string]any{
			"project": "default",
			"source": map[string]any{
				"repoURL": testRepoURL, "path": "helm/demo", "targetRevision": "main",
				"helm": map[string]any{
					"valueFiles": []any{"values-dev.yaml"},
					"parameters": []any{map[string]any{"name": "global.region", "value": "eu-central-1"}},
				},
			},
			"destination": map[string]any{"namespace": "demo-helm"},
		}, nil)
		parents = nil

		dp = &backflowv1alpha1.DriftProposal{
			ObjectMeta: metav1.ObjectMeta{Name: "av-dp", Namespace: ns},
			Spec: backflowv1alpha1.DriftProposalSpec{
				PolicyName:  "av-policy",
				Application: backflowv1alpha1.ObjectRef{Name: "child-app", Namespace: ns},
				Resource:    backflowv1alpha1.ResourceRef{Version: "v1", Kind: "ConfigMap", Namespace: "demo-helm", Name: "demo-helm-config"},
				Source: backflowv1alpha1.SourceRef{
					RepoURL: testRepoURL, Revision: initial, TargetRevision: "main", Path: "helm/demo", Type: backflowv1alpha1.SourceHelm,
				},
				Changes: []backflowv1alpha1.FieldChange{{
					Path: "/data/REGION", Op: backflowv1alpha1.OpReplace, Desired: `"eu-central-1"`, Live: `"us-west-2"`,
				}},
				DetectedAt: metav1.NewTime(time.Now().Add(-time.Hour)),
			},
		}
		Expect(k8sClient.Create(ctx, dp)).To(Succeed())
	})

	AfterEach(func() {
		for _, o := range append([]*unstructured.Unstructured{child}, parents...) {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, o))).To(Succeed())
		}
		for _, o := range []client.Object{dp, policy, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "av-argo-token", Namespace: ns}}} {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, o))).To(Succeed())
		}
		repo.close()
		_ = os.RemoveAll(cacheDir)
	})

	expectNothingWritten := func() {
		ExpectWithOffset(1, provider.created).To(BeEmpty(), "no merge request")
		ExpectWithOffset(1, repo.branch("main")).To(Equal(initial))
		ExpectWithOffset(1, latest().Status.MergeRequest).To(BeNil())
		ExpectWithOffset(1, latest().Status.Mapping.Edits).To(BeEmpty())
	}

	Context("the parent defines the Application in Git", func() {
		It("finds the manifest of a Directory parent, with the diff verified", func() {
			parent("parent-dir", "Directory", "apps/children", nil)
			val := settle()
			Expect(val.Application).To(Equal(ns + "/child-app"))
			Expect(val.Option).To(Equal("spec.source.helm.parameters[name=global.region]"))
			Expect(val.OptionPath).To(Equal("/spec/source/helm/parameters/0/value"))
			Expect(val.Current).To(Equal(`"eu-central-1"`))
			Expect(val.Suggested).To(Equal(`"us-west-2"`))
			Expect(val.Parent.State).To(Equal(backflowv1alpha1.ParentLocated))
			Expect(val.Parent.Name).To(Equal(ns + "/parent-dir"))
			Expect(val.Parent.File).To(Equal("apps/children/child-app.yaml"))
			Expect(val.Parent.Location).To(Equal("/spec/source/helm/parameters/0/value"))
			Expect(val.Parent.Revision).To(Equal(initial))
			Expect(val.Parent.Diff).To(ContainSubstring("-          value: eu-central-1\n+          value: us-west-2\n"))
			msg := mapped().Message
			Expect(msg).To(ContainSubstring("Set by spec.source.helm.parameters[name=global.region] of the Application " + ns + "/child-app, which is defined in apps/children/child-app.yaml of "))
			Expect(msg).To(ContainSubstring("(Application " + ns + "/parent-dir, at /spec/source/helm/parameters/0/value); set it to \"us-west-2\" there"))
			Expect(msg).To(ContainSubstring("no Argo CD API renders an Application with a changed option"))
			expectNothingWritten()
			Expect(recorder.Events).To(Receive(And(ContainSubstring(reasonApplicationValue), ContainSubstring("Set by spec.source.helm.parameters"))))
			Expect(recorder.Events).NotTo(Receive(), "one Event per proposal")
		})

		It("finds the template value of a Helm parent, verified by rendering the parent", func() {
			parent("parent-helm", "Helm", "charts/parent", map[string]any{"valueFiles": []any{"values.yaml"}})
			val := settle()
			Expect(val.Parent.State).To(Equal(backflowv1alpha1.ParentLocated), val.Parent.Message)
			Expect(val.Parent.File).To(Equal("charts/parent/values.yaml"))
			Expect(val.Parent.Location).To(Equal("feature"))
			Expect(val.Parent.Diff).To(ContainSubstring("-feature: eu-central-1\n+feature: us-west-2\n"))
			Expect(renderer.child.calls).To(BeNumerically(">", 0))
			expectNothingWritten()
		})

		It("finds the resource file of a Kustomize parent, the file that holds the value and not a new patch", func() {
			parent("parent-kust", "Kustomize", "kust/overlays/dev", nil)
			val := settle()
			Expect(val.Parent.State).To(Equal(backflowv1alpha1.ParentLocated), val.Parent.Message)
			Expect(val.Parent.File).To(Equal("kust/base/child.yaml"))
			Expect(val.Parent.Location).To(Equal("/spec/source/helm/parameters/0/value"))
			Expect(val.Parent.Diff).To(ContainSubstring("+          value: us-west-2"))
			expectNothingWritten()
		})

		It("uses the tracking-id annotation as the fast path, and falls back to status.resources without it", func() {
			p := parent("parent-dir", "Directory", "apps/children", nil)
			child.SetAnnotations(map[string]string{"argocd.argoproj.io/tracking-id": "parent-dir:argoproj.io/Application:" + ns + "/child-app"})
			Expect(k8sClient.Update(ctx, child)).To(Succeed())
			Expect(settle().Parent.Name).To(Equal(ns + "/" + p.GetName()))
		})

		It("is not fooled by a tracking-id of an Application that does not list the child", func() {
			parent("parent-dir", "Directory", "apps/children", nil)
			stale := newApp("stale-owner", map[string]any{"source": map[string]any{"repoURL": testRepoURL, "path": "x"}}, map[string]any{"resources": []any{}})
			parents = append(parents, stale)
			child.SetAnnotations(map[string]string{"argocd.argoproj.io/tracking-id": "stale-owner:argoproj.io/Application:" + ns + "/child-app"})
			Expect(k8sClient.Update(ctx, child)).To(Succeed())
			Expect(settle().Parent.Name).To(Equal(ns + "/parent-dir"))
		})

		It("locates, without a value, an option that is a string holding YAML", func() {
			setChildHelm(map[string]any{
				"valueFiles": []any{"values-dev.yaml"},
				"values":     "global:\n  region: eu-central-1\n",
			})
			parent("parent-dir", "Directory", "apps/values-children", nil)
			val := settle()
			Expect(val.Option).To(Equal("spec.source.helm.values (key global.region)"))
			Expect(val.OptionPath).To(Equal("/spec/source/helm/values"))
			Expect(val.Suggested).To(BeEmpty())
			Expect(val.Parent.State).To(Equal(backflowv1alpha1.ParentLocated), val.Parent.Message)
			Expect(val.Parent.File).To(Equal("apps/values-children/child-app.yaml"))
			Expect(val.Parent.Location).To(Equal("/spec/source/helm/values"))
			Expect(val.Parent.Diff).To(BeEmpty(), "there is no value to propose, so the probe edit is not shown")
			Expect(mapped().Message).NotTo(ContainSubstring("set it to"))
		})

		It("does not call a file the place of an option it does not hold", func() {
			setChildHelm(map[string]any{"valueFiles": []any{"values-dev.yaml"}, "values": "global:\n  region: eu-central-1\n"})
			parent("parent-dir", "Directory", "apps/children", nil) // its file sets a parameter, not helm.values
			val := settle()
			Expect(val.Parent.State).To(Equal(backflowv1alpha1.ParentNotVerifiable))
			Expect(val.Parent.Message).To(ContainSubstring("does not hold the current value"))
		})
	})

	Context("there is no Git file to point to", func() {
		It("says the Application is not managed from Git", func() {
			val := settle()
			Expect(val.Parent.State).To(Equal(backflowv1alpha1.ParentNotManagedFromGit))
			Expect(mapped().Message).To(ContainSubstring("which is not managed from Git"))
			Expect(val.Suggested).To(Equal(`"us-west-2"`), "the value to set is still known")
			expectNothingWritten()
		})

		It("names the ApplicationSet that generated it", func() {
			child.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "argoproj.io/v1alpha1", Kind: "ApplicationSet", Name: "regions", UID: "11111111-1111-1111-1111-111111111111"}})
			Expect(k8sClient.Update(ctx, child)).To(Succeed())
			parent("parent-dir", "Directory", "apps/children", nil) // does not matter: the ApplicationSet owns it
			val := settle()
			Expect(val.Parent.State).To(Equal(backflowv1alpha1.ParentGeneratedByApplicationSet))
			Expect(val.Parent.Name).To(Equal(ns + "/regions"))
			Expect(mapped().Message).To(ContainSubstring("ApplicationSet regions"))
			expectNothingWritten()
		})

		It("does not guess between two parents", func() {
			parent("parent-a", "Directory", "apps/children", nil)
			parent("parent-b", "Directory", "apps/children", nil)
			val := settle()
			Expect(val.Parent.State).To(Equal(backflowv1alpha1.ParentAmbiguous))
			Expect(val.Parent.Message).To(And(ContainSubstring("parent-a"), ContainSubstring("parent-b")))
		})

		It("does not trust a parent that is not synced", func() {
			p := parent("parent-dir", "Directory", "apps/children", nil)
			Expect(unstructured.SetNestedField(p.Object, "OutOfSync", "status", "sync", "status")).To(Succeed())
			Expect(k8sClient.Update(ctx, p)).To(Succeed())
			val := settle()
			Expect(val.Parent.State).To(Equal(backflowv1alpha1.ParentOutOfSync))
			Expect(val.Parent.File).To(BeEmpty())
		})

		It("is NotVerifiable for a parent with several sources, with a source that is not in Git, or of another type", func() {
			p := parent("parent-dir", "Directory", "apps/children", nil)
			Expect(unstructured.SetNestedSlice(p.Object, []any{map[string]any{"repoURL": testRepoURL}, map[string]any{"repoURL": testRepoURL}}, "spec", "sources")).To(Succeed())
			Expect(k8sClient.Update(ctx, p)).To(Succeed())
			val := settle()
			Expect(val.Parent.State).To(Equal(backflowv1alpha1.ParentNotVerifiable))
			Expect(val.Parent.Message).To(ContainSubstring("several sources"))

			By("a chart from a Helm repository")
			unstructured.RemoveNestedField(p.Object, "spec", "sources")
			Expect(unstructured.SetNestedField(p.Object, "podinfo", "spec", "source", "chart")).To(Succeed())
			Expect(k8sClient.Update(ctx, p)).To(Succeed())
			recreateDP(ctx, &dp)
			Expect(settle().Parent.Message).To(ContainSubstring("does not have a Git source"))

			By("a plugin source")
			unstructured.RemoveNestedField(p.Object, "spec", "source", "chart")
			Expect(unstructured.SetNestedField(p.Object, "Plugin", "status", "sourceType")).To(Succeed())
			Expect(k8sClient.Update(ctx, p)).To(Succeed())
			recreateDP(ctx, &dp)
			Expect(settle().Parent.Message).To(ContainSubstring(`"Plugin" source`))
		})
	})

	It("keeps the engine's reason when some unmapped field is not set by the Application", func() {
		dp.Spec.Changes = append(dp.Spec.Changes, backflowv1alpha1.FieldChange{Path: "/data/LOG_LEVEL", Op: backflowv1alpha1.OpReplace, Desired: `"debug"`, Live: `"warn"`})
		recreateDP(ctx, &dp)
		for range 5 {
			_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(dp)})
			if latest().Status.Phase == backflowv1alpha1.PhaseUnmapped {
				break
			}
		}
		Expect(latest().Status.Phase).To(Equal(backflowv1alpha1.PhaseUnmapped))
		Expect(mapped().Reason).NotTo(Equal(rendermap.ReasonSetByApplication))
		Expect(mapped().Message).To(ContainSubstring("Set by spec.source.helm.parameters[name=global.region]"))
	})
})

// recreateDP replaces the proposal with a copy of its spec, so it is mapped again.
func recreateDP(ctx context.Context, dp **backflowv1alpha1.DriftProposal) {
	old := *dp
	ExpectWithOffset(1, k8sClient.Delete(ctx, old)).To(Succeed())
	fresh := &backflowv1alpha1.DriftProposal{ObjectMeta: metav1.ObjectMeta{Name: old.Name, Namespace: old.Namespace}, Spec: *old.Spec.DeepCopy()}
	ExpectWithOffset(1, k8sClient.Create(ctx, fresh)).To(Succeed())
	*dp = fresh
}
