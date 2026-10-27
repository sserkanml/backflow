package controller

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/yaml"

	backflowv1alpha1 "github.com/sserkanml/backflow/api/v1alpha1"
	"github.com/sserkanml/backflow/internal/argocd"
	"github.com/sserkanml/backflow/internal/gitrepo"
	"github.com/sserkanml/backflow/internal/rendermap"
	"github.com/sserkanml/backflow/internal/scm"
)

// fakeKustomizeRenderer stands in for Argo CD's Kustomize: it builds the dev
// overlay of a tiny repository (one Deployment from the base, the images and
// patches of the overlay) from the files it is given. With buildMetadata in
// the overlay it adds the origin annotation, as kustomize does.
type fakeKustomizeRenderer struct {
	mu    sync.Mutex
	calls int
}

func (f *fakeKustomizeRenderer) Render(_ context.Context, app rendermap.App, files map[string][]byte) ([]*unstructured.Unstructured, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	var overlay map[string]any
	if err := yaml.Unmarshal(files["kust/overlays/dev/kustomization.yaml"], &overlay); err != nil {
		return nil, fmt.Errorf("%w: %v", rendermap.ErrRenderFailed, err)
	}
	var dep map[string]any
	if err := yaml.Unmarshal(files["kust/base/deployment.yaml"], &dep); err != nil {
		return nil, fmt.Errorf("%w: %v", rendermap.ErrRenderFailed, err)
	}
	patches, _ := overlay["patches"].([]any)
	for _, p := range patches {
		pm, _ := p.(map[string]any)
		file, _ := pm["path"].(string)
		data, ok := files["kust/overlays/dev/"+file]
		if !ok {
			return nil, fmt.Errorf("%w: patch %s is not in the files", rendermap.ErrRenderFailed, file)
		}
		var patch map[string]any
		if err := yaml.Unmarshal(data, &patch); err != nil {
			return nil, fmt.Errorf("%w: %v", rendermap.ErrRenderFailed, err)
		}
		mergeSMP(dep, patch)
	}
	images, _ := overlay["images"].([]any)
	for _, i := range images {
		im, _ := i.(map[string]any)
		for _, c := range containersOf(dep) {
			if img, _ := c["image"].(string); strings.HasPrefix(img, fmt.Sprint(im["name"])+":") {
				c["image"] = fmt.Sprint(im["name"]) + ":" + fmt.Sprint(im["newTag"])
			}
		}
	}
	obj := &unstructured.Unstructured{Object: dep}
	obj.SetNamespace(app.DestNamespace)
	if _, ok := overlay["buildMetadata"]; ok {
		obj.SetAnnotations(map[string]string{"config.kubernetes.io/origin": "path: ../../base/deployment.yaml\n"})
	}
	return []*unstructured.Unstructured{obj}, nil
}

func containersOf(dep map[string]any) []map[string]any {
	var out []map[string]any
	spec, _ := dep["spec"].(map[string]any)
	tpl, _ := spec["template"].(map[string]any)
	ps, _ := tpl["spec"].(map[string]any)
	cs, _ := ps["containers"].([]any)
	for _, c := range cs {
		if m, ok := c.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// mergeSMP is a strategic merge of maps and of lists of maps with a name.
func mergeSMP(dst, patch map[string]any) {
	for k, v := range patch {
		switch pv := v.(type) {
		case map[string]any:
			if dm, ok := dst[k].(map[string]any); ok {
				mergeSMP(dm, pv)
				continue
			}
		case []any:
			if dl, ok := dst[k].([]any); ok {
				for _, item := range pv {
					im, _ := item.(map[string]any)
					found := false
					for _, d := range dl {
						if dm, _ := d.(map[string]any); dm != nil && im != nil && dm["name"] == im["name"] {
							mergeSMP(dm, im)
							found = true
						}
					}
					if !found {
						dl = append(dl, item)
					}
				}
				dst[k] = dl
				continue
			}
		}
		dst[k] = v
	}
}

const (
	kustBaseDeployment = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: demo-kust
spec:
  replicas: 1
  template:
    spec:
      containers:
      - name: web
        image: nginx:1.0
        env:
        - name: MODE
          value: base
`
	kustDevOverlay  = "resources:\n- ../../base\nimages:\n- name: nginx\n  newTag: \"1.1\"\n"
	kustProdOverlay = "resources:\n- ../../base\n"
)

func kustFiles() map[string]string {
	return map[string]string{
		"kust/base/kustomization.yaml":           "resources:\n- deployment.yaml\n",
		"kust/base/deployment.yaml":              kustBaseDeployment,
		"kust/overlays/dev/kustomization.yaml":   kustDevOverlay,
		"kust/overlays/prod/kustomization.yaml":  kustProdOverlay,
		"kust/overlays/dev/README.md":            "not read by the build\n",
		"kust/overlays/prod/unrelated-file.yaml": "kind: Nothing\n",
	}
}

var _ = Describe("DriftProposal of a Kustomize Application", func() {
	const ns = "default"
	var (
		ctx      = context.Background()
		repo     *gitFixture
		initial  string
		provider *fakeProvider
		cacheDir string
		renderer *fakeKustomizeRenderer
		r        *DriftProposalReconciler
		policy   *backflowv1alpha1.BackflowPolicy
		app      *unstructured.Unstructured
		prodApp  *unstructured.Unstructured
		dp       *backflowv1alpha1.DriftProposal
		conn     *backflowv1alpha1.ScmConnection
	)
	noPatches := false
	imageChange := backflowv1alpha1.FieldChange{
		Path: "/spec/template/spec/containers/0/image", Op: backflowv1alpha1.OpReplace, Desired: `"nginx:1.1"`, Live: `"nginx:2.0"`,
	}
	envChange := backflowv1alpha1.FieldChange{
		Path: "/spec/template/spec/containers/0/env/0/value", Op: backflowv1alpha1.OpReplace, Desired: `"base"`, Live: `"live"`,
	}
	latest := func() *backflowv1alpha1.DriftProposal {
		got := &backflowv1alpha1.DriftProposal{}
		ExpectWithOffset(1, k8sClient.Get(ctx, client.ObjectKeyFromObject(dp), got)).To(Succeed())
		return got
	}
	settle := func(phase backflowv1alpha1.ProposalPhase) {
		for range 5 {
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(dp)})
			ExpectWithOffset(1, err).NotTo(HaveOccurred())
			if latest().Status.Phase == phase {
				return
			}
		}
		Fail(fmt.Sprintf("the proposal did not reach %s: %s", phase, latest().Status.Phase))
	}
	recreate := func(changes ...backflowv1alpha1.FieldChange) {
		ExpectWithOffset(1, k8sClient.Delete(ctx, dp)).To(Succeed())
		fresh := &backflowv1alpha1.DriftProposal{ObjectMeta: metav1.ObjectMeta{Name: dp.Name, Namespace: dp.Namespace}, Spec: *dp.Spec.DeepCopy()}
		fresh.Spec.Changes = changes
		ExpectWithOffset(1, k8sClient.Create(ctx, fresh)).To(Succeed())
		dp = fresh
	}
	setKustomize := func(opts *backflowv1alpha1.KustomizeOptions) {
		ExpectWithOffset(1, k8sClient.Get(ctx, client.ObjectKeyFromObject(policy), policy)).To(Succeed())
		policy.Spec.Kustomize = opts
		ExpectWithOffset(1, k8sClient.Update(ctx, policy)).To(Succeed())
	}
	newApp := func(name, path string) *unstructured.Unstructured {
		a := &unstructured.Unstructured{}
		a.SetGroupVersionKind(applicationGVK)
		a.SetName(name)
		a.SetNamespace(ns)
		ExpectWithOffset(1, unstructured.SetNestedField(a.Object, "demo-kust", "spec", "destination", "namespace")).To(Succeed())
		ExpectWithOffset(1, unstructured.SetNestedField(a.Object, path, "spec", "source", "path")).To(Succeed())
		ExpectWithOffset(1, k8sClient.Create(ctx, a)).To(Succeed())
		return a
	}

	BeforeEach(func() {
		repo = newGitFixture(kustFiles())
		initial = repo.branch("main")
		provider = newFakeProvider()
		provider.tip = repo.branch
		renderer = &fakeKustomizeRenderer{}
		var err error
		cacheDir, err = os.MkdirTemp("", "backflow-kust-cache-*")
		Expect(err).NotTo(HaveOccurred())
		cache := gitrepo.NewCache(cacheDir)
		r = &DriftProposalReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Repos: CacheReader{Cache: cache}, Writer: cache,
			NewProvider: func(scm.Config) (scm.Provider, error) { return provider, nil },
			NewRenderer: func(argocd.Config) (rendermap.Renderer, error) { return renderer, nil },
		}
		for _, name := range []string{"kust-token", "kust-argo-token"} {
			Expect(k8sClient.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, StringData: map[string]string{"token": "tok-" + name},
			})).To(Succeed())
		}
		conn = &backflowv1alpha1.ScmConnection{
			ObjectMeta: metav1.ObjectMeta{Name: "kust-scm", Namespace: ns},
			Spec: backflowv1alpha1.ScmConnectionSpec{
				Provider: backflowv1alpha1.ScmProviderGitLab, URL: "https://git.test",
				TokenSecretRef: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "kust-token"}, Key: "token"},
			},
		}
		Expect(k8sClient.Create(ctx, conn)).To(Succeed())
		policy = &backflowv1alpha1.BackflowPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "kust-policy", Namespace: ns},
			Spec: backflowv1alpha1.BackflowPolicySpec{
				ArgoCDNamespace: ns,
				Applications:    backflowv1alpha1.ApplicationSelector{Names: []string{"kust-app", "kust-prod"}},
				Mode:            backflowv1alpha1.ModeMergeRequest,
				ArgoCD: &backflowv1alpha1.ArgoCDServer{
					TokenSecretRef: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "kust-argo-token"}, Key: "token"},
				},
			},
		}
		Expect(k8sClient.Create(ctx, policy)).To(Succeed())
		policy.Status.Applications = []backflowv1alpha1.ApplicationSummary{
			{Name: "kust-app", RepoURL: testRepoURL, Path: "kust/overlays/dev", SourceType: "Kustomize", ScmConnection: "kust-scm"},
			{Name: "kust-prod", RepoURL: testRepoURL, Path: "kust/overlays/prod", SourceType: "Kustomize", ScmConnection: "kust-scm"},
		}
		Expect(k8sClient.Status().Update(ctx, policy)).To(Succeed())
		app = newApp("kust-app", "kust/overlays/dev")
		prodApp = newApp("kust-prod", "kust/overlays/prod")

		dp = &backflowv1alpha1.DriftProposal{
			ObjectMeta: metav1.ObjectMeta{Name: "kust-dp", Namespace: ns},
			Spec: backflowv1alpha1.DriftProposalSpec{
				PolicyName:  "kust-policy",
				Application: backflowv1alpha1.ObjectRef{Name: "kust-app", Namespace: ns},
				Resource:    backflowv1alpha1.ResourceRef{Group: "apps", Version: "v1", Kind: "Deployment", Namespace: "demo-kust", Name: "demo-kust"},
				Source: backflowv1alpha1.SourceRef{
					RepoURL: testRepoURL, Revision: initial, TargetRevision: "main",
					Path: "kust/overlays/dev", Type: backflowv1alpha1.SourceKustomize,
				},
				Changes:    []backflowv1alpha1.FieldChange{imageChange},
				Actor:      &backflowv1alpha1.Actor{Username: "alice", UserAgent: "kubectl"},
				DetectedAt: metav1.NewTime(time.Now().Add(-time.Hour)),
			},
		}
		Expect(k8sClient.Create(ctx, dp)).To(Succeed())
	})

	AfterEach(func() {
		for _, o := range []client.Object{dp, policy, app, prodApp, conn,
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "kust-token", Namespace: ns}},
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "kust-argo-token", Namespace: ns}},
		} {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, o))).To(Succeed())
		}
		repo.close()
		_ = os.RemoveAll(cacheDir)
	})

	It("maps an image drift to the setting of the overlay and opens a merge request", func() {
		settle(backflowv1alpha1.PhaseProposed)
		got := latest()
		Expect(meta.IsStatusConditionTrue(got.Status.Conditions, conditionMapped)).To(BeTrue())
		Expect(got.Status.Mapping.Strategy).To(Equal(backflowv1alpha1.StrategyKustomizePatch))
		Expect(got.Status.Mapping.Verified).To(BeTrue())
		Expect(got.Status.Mapping.Edits).To(HaveLen(1))
		Expect(got.Status.Mapping.Edits[0].File).To(Equal("kust/overlays/dev/kustomization.yaml"))
		Expect(got.Status.Mapping.Edits[0].Location).To(Equal("/images/0/newTag"))
		Expect(got.Status.Mapping.Edits[0].Value).To(Equal(`"2.0"`))

		tip := repo.branch(got.Status.MergeRequest.Branch)
		Expect(repo.file(tip, "kust/overlays/dev/kustomization.yaml")).To(Equal(strings.Replace(kustDevOverlay, `"1.1"`, `"2.0"`, 1)))
		Expect(repo.file(tip, "kust/base/deployment.yaml")).To(Equal(kustBaseDeployment))
		Expect(repo.branch("main")).To(Equal(initial))

		msg := repo.commitAt(tip).Message
		for _, want := range []string{"a setting of the kustomization", "images[nginx].newTag", "Verified by rendering through Argo CD", "(/spec/template/spec/containers/0/image)", "Proposal: kust-dp\n"} {
			Expect(msg).To(ContainSubstring(want))
		}
		body := provider.created[0].Body
		for _, want := range []string{"**Kustomize**", "images[nginx].newTag", "Verified by rendering through Argo CD"} {
			Expect(body).To(ContainSubstring(want))
		}
		Expect(body).NotTo(ContainSubstring("Warning"))
	})

	It("adds a Backflow patch for a value the shared base holds, creating the file in the same commit", func() {
		recreate(envChange)
		settle(backflowv1alpha1.PhaseProposed)
		got := latest()
		Expect(got.Status.Mapping.Strategy).To(Equal(backflowv1alpha1.StrategyKustomizePatch))
		files := make([]string, 0, len(got.Status.Mapping.Edits))
		for _, e := range got.Status.Mapping.Edits {
			files = append(files, e.File)
		}
		Expect(files).To(ConsistOf("kust/overlays/dev/backflow-patches/deployment-demo-kust.yaml", "kust/overlays/dev/kustomization.yaml"))
		Expect(meta.FindStatusCondition(got.Status.Conditions, conditionMapped).Message).To(ContainSubstring("It adds the new file(s) kust/overlays/dev/backflow-patches/deployment-demo-kust.yaml"))

		tip := repo.branch(got.Status.MergeRequest.Branch)
		patch := repo.file(tip, "kust/overlays/dev/backflow-patches/deployment-demo-kust.yaml")
		Expect(patch).To(ContainSubstring("name: MODE"))
		Expect(patch).To(ContainSubstring("value: live"))
		Expect(repo.file(tip, "kust/overlays/dev/kustomization.yaml")).To(ContainSubstring("- path: backflow-patches/deployment-demo-kust.yaml"))
		Expect(repo.file(tip, "kust/base/deployment.yaml")).To(Equal(kustBaseDeployment))
		Expect(repo.commitAt(tip).Message).To(ContainSubstring("A new patch file was added"))
		Expect(provider.created[0].Body).To(ContainSubstring("A new patch file was added"))
	})

	It("edits a base another Application uses only when no patch can be added, and says who is affected", func() {
		recreate(envChange)
		setKustomize(&backflowv1alpha1.KustomizeOptions{EditBases: true, AddPatches: &noPatches})
		settle(backflowv1alpha1.PhaseProposed)
		got := latest()
		Expect(got.Status.Mapping.Strategy).To(Equal(backflowv1alpha1.StrategyDirect))
		Expect(got.Status.Mapping.Edits).To(HaveLen(1))
		Expect(got.Status.Mapping.Edits[0].File).To(Equal("kust/base/deployment.yaml"))
		Expect(meta.FindStatusCondition(got.Status.Conditions, conditionMapped).Message).To(ContainSubstring("default/kust-prod"))
		body := provider.created[0].Body
		Expect(body).To(ContainSubstring("Merging this also changes: default/kust-prod at their next sync."))
		Expect(body).To(ContainSubstring("Other Applications outside Backflow's policies may use this file."))
	})

	It("tries a new patch before a base other Applications use, even with editBases on", func() {
		recreate(envChange)
		setKustomize(&backflowv1alpha1.KustomizeOptions{EditBases: true})
		settle(backflowv1alpha1.PhaseProposed)
		Expect(latest().Status.Mapping.Strategy).To(Equal(backflowv1alpha1.StrategyKustomizePatch))
	})

	It("edits a base no other Application uses before adding a patch", func() {
		recreate(envChange)
		setKustomize(&backflowv1alpha1.KustomizeOptions{EditBases: true})
		Expect(k8sClient.Delete(ctx, prodApp)).To(Succeed())
		settle(backflowv1alpha1.PhaseProposed)
		got := latest()
		Expect(got.Status.Mapping.Strategy).To(Equal(backflowv1alpha1.StrategyDirect))
		Expect(got.Status.Mapping.Edits[0].File).To(Equal("kust/base/deployment.yaml"))
	})

	It("is Unmapped when the Application sets the image itself", func() {
		Expect(unstructured.SetNestedStringSlice(app.Object, []string{"nginx=nginx:1.1"}, "spec", "source", "kustomize", "images")).To(Succeed())
		Expect(k8sClient.Update(ctx, app)).To(Succeed())
		settle(backflowv1alpha1.PhaseUnmapped)
		c := meta.FindStatusCondition(latest().Status.Conditions, conditionMapped)
		Expect(c.Reason).To(Equal(rendermap.ReasonSetByApplication))
		Expect(c.Message).To(ContainSubstring("Set by spec.source.kustomize.images[nginx] of Application " + ns + "/kust-app"))
		val := latest().Status.Mapping.Application
		Expect(val).NotTo(BeNil())
		Expect(val.OptionPath).To(Equal("/spec/source/kustomize/images/0"))
		Expect(val.Current).To(Equal(`"nginx=nginx:1.1"`))
		Expect(val.Suggested).To(Equal(`"nginx=nginx:2.0"`))
	})
})
