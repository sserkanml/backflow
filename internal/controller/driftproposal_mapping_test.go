package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	backflowv1alpha1 "github.com/sserkanml/backflow/api/v1alpha1"
	"github.com/sserkanml/backflow/internal/gitrepo"
)

const mappingConfigMap = `apiVersion: v1
kind: ConfigMap
metadata:
  name: demo-config
data:
  # Log level of the demo app.
  LOG_LEVEL: info
`

type fakeTree struct{ fstest.MapFS }

func (fakeTree) Close() error { return nil }

// fakeRepos stands in for the repository cache.
type fakeRepos struct {
	files    fstest.MapFS
	err      error
	calls    int
	gotURL   string
	gotSHA   string
	gotAuth  *gitrepo.Auth
	authSeen bool
}

func (f *fakeRepos) Open(_ context.Context, url, sha string, auth *gitrepo.Auth) (SourceTree, error) {
	f.calls++
	f.gotURL, f.gotSHA, f.gotAuth, f.authSeen = url, sha, auth, true
	if f.err != nil {
		return nil, f.err
	}
	return fakeTree{f.files}, nil
}

func tree(files map[string]string) fstest.MapFS {
	m := fstest.MapFS{}
	for name, content := range files {
		m[name] = &fstest.MapFile{Data: []byte(content)}
	}
	return m
}

func TestTruncateDiff(t *testing.T) {
	if got := truncateDiff("short\n"); got != "short\n" {
		t.Errorf("short diff changed: %q", got)
	}
	long := strings.Repeat("+line with é\n", 5000)
	got := truncateDiff(long)
	if len(got) > maxDiffBytes {
		t.Errorf("len = %d, want at most %d", len(got), maxDiffBytes)
	}
	if !strings.HasSuffix(got, diffTruncated) {
		t.Error("no truncation marker")
	}
	body := strings.TrimSuffix(got, diffTruncated)
	if !strings.HasSuffix(body, "\n") || strings.Contains(body, "�") {
		t.Errorf("not cut at a line boundary: %q", body[len(body)-20:])
	}
}

var _ = Describe("DriftProposal mapping", func() {
	const ns = "default"
	var (
		ctx    = context.Background()
		repos  *fakeRepos
		r      *DriftProposalReconciler
		policy *backflowv1alpha1.BackflowPolicy
		app    *unstructured.Unstructured
		dp     *backflowv1alpha1.DriftProposal
		offset time.Duration
		base   time.Time
	)

	reconcileDP := func() (reconcile.Result, error) {
		return r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(dp)})
	}
	latest := func() *backflowv1alpha1.DriftProposal {
		got := &backflowv1alpha1.DriftProposal{}
		ExpectWithOffset(1, k8sClient.Get(ctx, client.ObjectKeyFromObject(dp), got)).To(Succeed())
		return got
	}
	mapped := func() *metav1.Condition { return meta.FindStatusCondition(latest().Status.Conditions, conditionMapped) }

	logLevelChange := backflowv1alpha1.FieldChange{
		Path: "/data/LOG_LEVEL", Op: backflowv1alpha1.OpReplace, Desired: `"info"`, Live: `"debug"`,
	}
	setDirectory := func(dir map[string]interface{}) {
		ExpectWithOffset(1, unstructured.SetNestedField(app.Object, dir, "spec", "source", "directory")).To(Succeed())
		ExpectWithOffset(1, k8sClient.Update(ctx, app)).To(Succeed())
	}
	// Specs are immutable, so a different proposal means a new object.
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
	setScmConnection := func(name string) {
		ExpectWithOffset(1, k8sClient.Get(ctx, client.ObjectKeyFromObject(policy), policy)).To(Succeed())
		policy.Status.Applications[0].ScmConnection = name
		ExpectWithOffset(1, k8sClient.Status().Update(ctx, policy)).To(Succeed())
	}

	BeforeEach(func() {
		base = time.Now()
		offset = 0
		repos = &fakeRepos{files: tree(map[string]string{"apps/demo/configmap.yaml": mappingConfigMap})}
		r = &DriftProposalReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Repos: repos,
			Now: func() time.Time { return base.Add(offset) },
		}

		policy = &backflowv1alpha1.BackflowPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "map-policy", Namespace: ns},
			Spec: backflowv1alpha1.BackflowPolicySpec{
				ArgoCDNamespace: ns,
				Applications:    backflowv1alpha1.ApplicationSelector{Names: []string{"map-app"}},
				Mode:            backflowv1alpha1.ModeReportOnly,
			},
		}
		Expect(k8sClient.Create(ctx, policy)).To(Succeed())
		policy.Status.Applications = []backflowv1alpha1.ApplicationSummary{{Name: "map-app", RepoURL: "https://gitlab.com/x/y.git"}}
		Expect(k8sClient.Status().Update(ctx, policy)).To(Succeed())

		app = &unstructured.Unstructured{}
		app.SetGroupVersionKind(applicationGVK)
		app.SetName("map-app")
		app.SetNamespace(ns)
		Expect(unstructured.SetNestedField(app.Object, "demo", "spec", "destination", "namespace")).To(Succeed())
		Expect(k8sClient.Create(ctx, app)).To(Succeed())

		dp = &backflowv1alpha1.DriftProposal{
			ObjectMeta: metav1.ObjectMeta{Name: "map-dp", Namespace: ns},
			Spec: backflowv1alpha1.DriftProposalSpec{
				PolicyName:  "map-policy",
				Application: backflowv1alpha1.ObjectRef{Name: "map-app", Namespace: ns},
				Resource:    backflowv1alpha1.ResourceRef{Version: "v1", Kind: "ConfigMap", Namespace: "demo", Name: "demo-config"},
				Source: backflowv1alpha1.SourceRef{
					RepoURL: "https://gitlab.com/x/y.git", Revision: "0123456789abcdef0123456789abcdef01234567",
					TargetRevision: "main", Path: "apps/demo", Type: backflowv1alpha1.SourceDirectory,
				},
				Changes:    []backflowv1alpha1.FieldChange{logLevelChange},
				DetectedAt: metav1.Now(),
			},
		}
		Expect(k8sClient.Create(ctx, dp)).To(Succeed())
	})

	AfterEach(func() {
		for _, o := range []client.Object{dp, policy, app,
			&backflowv1alpha1.ScmConnection{ObjectMeta: metav1.ObjectMeta{Name: "map-scm", Namespace: ns}},
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "map-token", Namespace: ns}},
		} {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, o))).To(Succeed())
		}
	})

	It("maps a Directory proposal, verifies it and stays in Mapping", func() {
		_, err := reconcileDP()
		Expect(err).NotTo(HaveOccurred())

		got := latest()
		Expect(got.Status.Phase).To(Equal(backflowv1alpha1.PhaseMapping))
		Expect(mapped().Status).To(Equal(metav1.ConditionTrue))
		Expect(mapped().Reason).To(Equal(reasonMapped))
		Expect(mapped().ObservedGeneration).To(Equal(got.Generation))
		Expect(got.Status.Mapping).NotTo(BeNil())
		Expect(got.Status.Mapping.Strategy).To(Equal(backflowv1alpha1.StrategyDirect))
		Expect(got.Status.Mapping.Verified).To(BeTrue())
		Expect(got.Status.Mapping.Reason).To(BeEmpty())
		Expect(got.Status.Mapping.Edits).To(Equal([]backflowv1alpha1.SourceEdit{{
			File: "apps/demo/configmap.yaml", Location: "/data/LOG_LEVEL", Value: `"debug"`, ChangeIndex: 0,
		}}))
		Expect(got.Status.Mapping.Diff).To(ContainSubstring("-  LOG_LEVEL: info\n+  LOG_LEVEL: debug\n"))
		Expect(got.Status.Mapping.Diff).NotTo(ContainSubstring("+  # Log level"))
		Expect(got.Status.Mapping.Diff).NotTo(ContainSubstring("-  # Log level"))

		By("reading the synced commit, not the branch, and anonymously without a connection")
		Expect(repos.gotURL).To(Equal("https://gitlab.com/x/y.git"))
		Expect(repos.gotSHA).To(Equal("0123456789abcdef0123456789abcdef01234567"))
		Expect(repos.gotAuth).To(BeNil())

		By("not mapping again")
		_, err = reconcileDP()
		Expect(err).NotTo(HaveOccurred())
		Expect(repos.calls).To(Equal(1))
		Expect(latest().Status.Phase).To(Equal(backflowv1alpha1.PhaseMapping))
	})

	It("never writes to the proposal spec", func() {
		before := latest().Spec
		_, err := reconcileDP()
		Expect(err).NotTo(HaveOccurred())
		Expect(latest().Spec).To(Equal(before))
	})

	It("honours the Directory options of the Application", func() {
		repos.files = tree(map[string]string{
			"apps/demo/sub/configmap.yaml":  mappingConfigMap,
			"apps/demo/skip/configmap.yaml": mappingConfigMap,
		})

		By("not looking into subdirectories by default")
		_, err := reconcileDP()
		Expect(err).NotTo(HaveOccurred())
		Expect(latest().Status.Phase).To(Equal(backflowv1alpha1.PhaseUnmapped))
		Expect(mapped().Reason).To(Equal(reasonNotFound))
	})

	It("maps with recurse and exclude from the Application", func() {
		repos.files = tree(map[string]string{
			"apps/demo/sub/configmap.yaml":  mappingConfigMap,
			"apps/demo/skip/configmap.yaml": mappingConfigMap,
		})
		setDirectory(map[string]interface{}{"recurse": true, "exclude": "{skip/*}"})
		_, err := reconcileDP()
		Expect(err).NotTo(HaveOccurred())
		Expect(mapped().Status).To(Equal(metav1.ConditionTrue))
		Expect(latest().Status.Mapping.Edits[0].File).To(Equal("apps/demo/sub/configmap.yaml"))
	})

	It("uses the destination namespace for documents without one", func() {
		// The manifest has no namespace; the Application deploys to "demo".
		_, err := reconcileDP()
		Expect(err).NotTo(HaveOccurred())
		Expect(mapped().Status).To(Equal(metav1.ConditionTrue))

		By("and does not match another namespace")
		Expect(unstructured.SetNestedField(app.Object, "other", "spec", "destination", "namespace")).To(Succeed())
		Expect(k8sClient.Update(ctx, app)).To(Succeed())
		other := latest()
		other.Status = backflowv1alpha1.DriftProposalStatus{}
		Expect(k8sClient.Status().Update(ctx, other)).To(Succeed())
		_, err = reconcileDP()
		Expect(err).NotTo(HaveOccurred())
		Expect(mapped().Reason).To(Equal(reasonNotFound))
	})

	DescribeTable("marks the proposal Unmapped, with a reason, when the change cannot be traced back with certainty",
		func(files map[string]string, mutate func(), wantReason string) {
			repos.files = tree(files)
			if mutate != nil {
				mutate()
			}
			_, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())

			got := latest()
			Expect(got.Status.Phase).To(Equal(backflowv1alpha1.PhaseUnmapped))
			Expect(mapped().Status).To(Equal(metav1.ConditionFalse))
			Expect(mapped().Reason).To(Equal(wantReason))
			Expect(got.Status.Mapping).NotTo(BeNil())
			Expect(got.Status.Mapping.Strategy).To(Equal(backflowv1alpha1.StrategyUnmapped))
			Expect(got.Status.Mapping.Reason).NotTo(BeEmpty())
			Expect(got.Status.Mapping.Verified).To(BeFalse())
			Expect(got.Status.Mapping.Edits).To(BeEmpty())

			By("not retrying")
			calls := repos.calls
			_, err = reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(repos.calls).To(Equal(calls))
		},
		Entry("the resource is not in the source",
			map[string]string{"apps/demo/other.yaml": "apiVersion: v1\nkind: Service\nmetadata:\n  name: x\n"},
			nil, reasonNotFound),
		Entry("the resource is defined twice",
			map[string]string{"apps/demo/a.yaml": mappingConfigMap, "apps/demo/b.yaml": mappingConfigMap},
			nil, reasonAmbiguous),
		Entry("the file already holds the live value, so the edit cannot reproduce the proposal",
			map[string]string{"apps/demo/configmap.yaml": strings.Replace(mappingConfigMap, "info", "debug", 1)},
			nil, reasonVerificationFailed),
		Entry("the change cannot be applied to the document",
			map[string]string{"apps/demo/configmap.yaml": mappingConfigMap},
			func() {
				recreate(func(spec *backflowv1alpha1.DriftProposalSpec) {
					spec.Changes = []backflowv1alpha1.FieldChange{{
						Path: "/data/LOG_LEVEL/x", Op: backflowv1alpha1.OpReplace, Desired: `"a"`, Live: `"b"`,
					}}
				})
			}, reasonCannotApply),
		Entry("the manifest is JSON",
			map[string]string{"apps/demo/configmap.json": `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"demo-config"},"data":{"LOG_LEVEL":"info"}}`},
			nil, reasonUnsupportedFileFormat),
		Entry("an include pattern does not compile",
			map[string]string{"apps/demo/configmap.yaml": mappingConfigMap},
			func() { setDirectory(map[string]interface{}{"include": "["}) }, reasonInvalidSourceOptions),
	)

	DescribeTable("does not map sources other than Directory",
		func(sourceType backflowv1alpha1.SourceType) {
			recreate(func(spec *backflowv1alpha1.DriftProposalSpec) { spec.Source.Type = sourceType })

			_, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(latest().Status.Phase).To(Equal(backflowv1alpha1.PhaseUnmapped))
			Expect(mapped().Reason).To(Equal(reasonUnsupportedSourceType))
			Expect(repos.calls).To(BeZero())
		},
		Entry("Helm", backflowv1alpha1.SourceHelm),
		Entry("Kustomize", backflowv1alpha1.SourceKustomize),
	)

	Context("when the repository cannot be read", func() {
		It("keeps retrying with a growing delay and never gives up", func() {
			repos.err = gitrepo.ErrUnavailable
			res, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(Equal(retryMin))

			got := latest()
			Expect(got.Status.Phase).To(Equal(backflowv1alpha1.PhaseMapping))
			Expect(mapped().Status).To(Equal(metav1.ConditionFalse))
			Expect(mapped().Reason).To(Equal(reasonRepositoryUnavailable))
			Expect(got.Status.Mapping).To(BeNil())

			By("backing off as the outage lasts")
			var previous time.Duration
			for _, outage := range []time.Duration{time.Minute, 10 * time.Minute, time.Hour} {
				offset = outage
				res, err = reconcileDP()
				Expect(err).NotTo(HaveOccurred())
				Expect(res.RequeueAfter).To(BeNumerically(">", previous))
				previous = res.RequeueAfter
			}

			By("capping the delay at ten minutes, however long it lasts")
			offset = 30 * 24 * time.Hour
			res, err = reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(Equal(retryMax))
			Expect(latest().Status.Phase).To(Equal(backflowv1alpha1.PhaseMapping))
			Expect(mapped().Reason).To(Equal(reasonRepositoryUnavailable))

			By("mapping as soon as the repository is back")
			repos.err = nil
			res, err = reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(BeZero())
			Expect(mapped().Status).To(Equal(metav1.ConditionTrue))
			Expect(latest().Status.Phase).To(Equal(backflowv1alpha1.PhaseMapping))
		})

		It("treats unknown errors like an unavailable repository", func() {
			repos.err = errors.New("connection refused")
			res, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(BeNumerically(">", 0))
			Expect(mapped().Reason).To(Equal(reasonRepositoryUnavailable))
			Expect(latest().Status.Phase).To(Equal(backflowv1alpha1.PhaseMapping))
		})

		It("keeps retrying when the server rejects the token, with its own reason", func() {
			repos.err = gitrepo.ErrAuth
			offset = 24 * time.Hour
			res, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(BeNumerically(">", 0))
			Expect(mapped().Reason).To(Equal(reasonRepositoryAuthFailed))
			Expect(latest().Status.Phase).To(Equal(backflowv1alpha1.PhaseMapping))

			repos.err = nil
			_, err = reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(mapped().Status).To(Equal(metav1.ConditionTrue))
		})

		DescribeTable("gives up only when the repository or the commit is known not to exist",
			func(cause error, wantReason string) {
				repos.err = cause
				res, err := reconcileDP()
				Expect(err).NotTo(HaveOccurred())
				Expect(res.RequeueAfter).To(BeZero())

				got := latest()
				Expect(got.Status.Phase).To(Equal(backflowv1alpha1.PhaseUnmapped))
				Expect(mapped().Status).To(Equal(metav1.ConditionFalse))
				Expect(mapped().Reason).To(Equal(wantReason))
				Expect(got.Status.Mapping.Strategy).To(Equal(backflowv1alpha1.StrategyUnmapped))
				Expect(got.Status.Mapping.Reason).NotTo(BeEmpty())

				calls := repos.calls
				_, err = reconcileDP()
				Expect(err).NotTo(HaveOccurred())
				Expect(repos.calls).To(Equal(calls), "an Unmapped proposal is not retried")
			},
			Entry("the commit does not exist", gitrepo.ErrRevisionNotFound, reasonRevisionNotFound),
			Entry("the repository does not exist", gitrepo.ErrRepositoryNotFound, reasonRepositoryNotFound),
		)
	})

	Context("authentication", func() {
		BeforeEach(func() {
			Expect(k8sClient.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "map-token", Namespace: ns},
				StringData: map[string]string{"token": "s3cr3t\n"},
			})).To(Succeed())
			Expect(k8sClient.Create(ctx, &backflowv1alpha1.ScmConnection{
				ObjectMeta: metav1.ObjectMeta{Name: "map-scm", Namespace: ns},
				Spec: backflowv1alpha1.ScmConnectionSpec{
					Provider: backflowv1alpha1.ScmProviderGitLab, URL: "https://gitlab.com",
					TokenSecretRef: corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "map-token"}, Key: "token",
					},
				},
			})).To(Succeed())
		})

		It("uses the token of the ScmConnection the policy matched", func() {
			setScmConnection("map-scm")
			_, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(repos.gotAuth).To(Equal(&gitrepo.Auth{Username: "oauth2", Token: "s3cr3t"}))
		})

		It("passes the CA bundle of the ScmConnection along with the token", func() {
			Expect(k8sClient.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "map-ca", Namespace: ns},
				StringData: map[string]string{"ca.crt": "-----BEGIN CERTIFICATE-----\nabc\n-----END CERTIFICATE-----\n"},
			})).To(Succeed())
			DeferCleanup(func() {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "map-ca", Namespace: ns}}))).To(Succeed())
			})
			conn := &backflowv1alpha1.ScmConnection{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "map-scm"}, conn)).To(Succeed())
			conn.Spec.CASecretRef = &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "map-ca"}, Key: "ca.crt",
			}
			Expect(k8sClient.Update(ctx, conn)).To(Succeed())
			setScmConnection("map-scm")

			_, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(repos.gotAuth).NotTo(BeNil())
			Expect(repos.gotAuth.Token).To(Equal("s3cr3t"))
			Expect(string(repos.gotAuth.CABundle)).To(ContainSubstring("BEGIN CERTIFICATE"))
		})

		It("retries, as an authentication failure, while the CA Secret is missing", func() {
			conn := &backflowv1alpha1.ScmConnection{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "map-scm"}, conn)).To(Succeed())
			conn.Spec.CASecretRef = &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "no-such-ca"}, Key: "ca.crt",
			}
			Expect(k8sClient.Update(ctx, conn)).To(Succeed())
			setScmConnection("map-scm")

			res, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(BeNumerically(">", 0))
			Expect(repos.calls).To(BeZero())
			Expect(mapped().Reason).To(Equal(reasonRepositoryAuthFailed))
			Expect(mapped().Message).To(ContainSubstring("no-such-ca"))
		})

		It("uses the GitHub username for a GitHub connection", func() {
			conn := &backflowv1alpha1.ScmConnection{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "map-scm"}, conn)).To(Succeed())
			conn.Spec.Provider = backflowv1alpha1.ScmProviderGitHub
			conn.Spec.URL = "https://github.com"
			Expect(k8sClient.Update(ctx, conn)).To(Succeed())
			setScmConnection("map-scm")
			_, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(repos.gotAuth.Username).To(Equal("x-access-token"))
		})

		It("retries, without reading anonymously, while the token Secret is missing, and recovers once it is fixed", func() {
			Expect(k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "map-token", Namespace: ns}})).To(Succeed())
			setScmConnection("map-scm")
			res, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(BeNumerically(">", 0))
			Expect(repos.calls).To(BeZero())
			Expect(mapped().Reason).To(Equal(reasonRepositoryAuthFailed))
			Expect(latest().Status.Phase).To(Equal(backflowv1alpha1.PhaseMapping))

			By("being picked up when a Secret or ScmConnection in the namespace changes")
			reqs := r.authFailedProposals(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "map-token", Namespace: ns}})
			Expect(reqs).To(ConsistOf(reconcile.Request{NamespacedName: client.ObjectKeyFromObject(dp)}))
			Expect(r.authFailedProposals(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "elsewhere"}})).To(BeEmpty())

			By("recovering by itself")
			Expect(k8sClient.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "map-token", Namespace: ns},
				StringData: map[string]string{"token": "s3cr3t"},
			})).To(Succeed())
			_, err = reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(repos.gotAuth).To(Equal(&gitrepo.Auth{Username: "oauth2", Token: "s3cr3t"}))
			Expect(mapped().Status).To(Equal(metav1.ConditionTrue))
			Expect(r.authFailedProposals(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "map-token", Namespace: ns}})).To(BeEmpty())
		})
	})

	It("keeps retrying while the Application is missing and maps when it is back", func() {
		Expect(k8sClient.Delete(ctx, app)).To(Succeed())
		res, err := reconcileDP()
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(BeNumerically(">", 0))
		Expect(mapped().Reason).To(Equal(reasonApplicationNotFound))
		Expect(mapped().Message).To(ContainSubstring("map-app"))

		offset = 365 * 24 * time.Hour
		res, err = reconcileDP()
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(Equal(retryMax))
		Expect(latest().Status.Phase).To(Equal(backflowv1alpha1.PhaseMapping))

		app = &unstructured.Unstructured{}
		app.SetGroupVersionKind(applicationGVK)
		app.SetName("map-app")
		app.SetNamespace(ns)
		Expect(unstructured.SetNestedField(app.Object, "demo", "spec", "destination", "namespace")).To(Succeed())
		Expect(k8sClient.Create(ctx, app)).To(Succeed())
		_, err = reconcileDP()
		Expect(err).NotTo(HaveOccurred())
		Expect(mapped().Status).To(Equal(metav1.ConditionTrue))
	})

	It("does not map when mapping is disabled", func() {
		r.Repos = nil
		_, err := reconcileDP()
		Expect(err).NotTo(HaveOccurred())
		Expect(latest().Status.Phase).To(Equal(backflowv1alpha1.PhaseDetected))
		Expect(latest().Status.Mapping).To(BeNil())
	})

	It("lets a lifecycle annotation win over mapping, and leaves Unmapped proposals open to them", func() {
		_, err := reconcileDP()
		Expect(err).NotTo(HaveOccurred())

		got := latest()
		got.Annotations = map[string]string{annotationReverted: "back in sync"}
		Expect(k8sClient.Update(ctx, got)).To(Succeed())
		_, err = reconcileDP()
		Expect(err).NotTo(HaveOccurred())
		Expect(latest().Status.Phase).To(Equal(backflowv1alpha1.PhaseReverted))
		Expect(repos.calls).To(Equal(1))
	})
})
