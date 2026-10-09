package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/plumbing/transport"
	gitclient "github.com/go-git/go-git/v5/plumbing/transport/client"
	gitserver "github.com/go-git/go-git/v5/plumbing/transport/server"
	"github.com/go-git/go-git/v5/storage/filesystem"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	backflowv1alpha1 "github.com/sserkanml/backflow/api/v1alpha1"
	"github.com/sserkanml/backflow/internal/gitrepo"
	"github.com/sserkanml/backflow/internal/scm"
)

// --- local Git hosting -----------------------------------------------------

// testRepos maps the path of a repository URL (https://git.test/<path>) to a
// directory with a real repository. The Git client of the package is pointed
// at it, so the operator's full read and write path runs without a network.
var testRepos sync.Map

type testLoader struct{}

func (testLoader) Load(ep *transport.Endpoint) (storer.Storer, error) {
	dir, ok := testRepos.Load(ep.Path)
	if !ok {
		return nil, transport.ErrRepositoryNotFound
	}
	// A fresh storage per request: objects pushed meanwhile are visible.
	return filesystem.NewStorage(osfs.New(filepath.Join(dir.(string), ".git")), cache.NewObjectLRUDefault()), nil
}

func init() {
	gitclient.InstallProtocol("https", gitserver.NewClient(testLoader{}))
}

const testRepoPath = "/group/proj.git"

// testRepoURL is the URL the proposals under test use.
const testRepoURL = "https://git.test" + testRepoPath

// gitFixture is a local repository whose default branch is main.
type gitFixture struct {
	dir  string
	repo *git.Repository
}

func newGitFixture(files map[string]string) *gitFixture {
	dir, err := os.MkdirTemp("", "backflow-propose-*")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	repo, err := git.PlainInitWithOptions(dir, &git.PlainInitOptions{
		InitOptions: git.InitOptions{DefaultBranch: plumbing.NewBranchReferenceName("main")},
	})
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	g := &gitFixture{dir: dir, repo: repo}
	g.commit(files, "initial")
	testRepos.Store(testRepoPath, dir)
	return g
}

func (g *gitFixture) close() {
	testRepos.Delete(testRepoPath)
	_ = os.RemoveAll(g.dir)
}

// commit writes the files (an empty content deletes the file) and commits on the current branch.
func (g *gitFixture) commit(files map[string]string, msg string) string {
	wt, err := g.repo.Worktree()
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	for name, content := range files {
		full := filepath.Join(g.dir, name)
		if content == "" {
			_ = os.Remove(full)
			continue
		}
		ExpectWithOffset(1, os.MkdirAll(filepath.Dir(full), 0o750)).To(Succeed())
		ExpectWithOffset(1, os.WriteFile(full, []byte(content), 0o600)).To(Succeed())
	}
	_, err = wt.Add(".")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	ExpectWithOffset(1, wt.AddWithOptions(&git.AddOptions{All: true})).To(Succeed())
	h, err := wt.Commit(msg, &git.CommitOptions{Author: &object.Signature{Name: "dev", Email: "dev@example.com", When: time.Now()}})
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return h.String()
}

// open reopens the repository: pushes land in packs an old handle has not seen.
func (g *gitFixture) open() *git.Repository {
	r, err := git.PlainOpen(g.dir)
	ExpectWithOffset(2, err).NotTo(HaveOccurred())
	return r
}

func (g *gitFixture) branch(name string) string {
	ref, err := g.open().Reference(plumbing.NewBranchReferenceName(name), true)
	if err != nil {
		return ""
	}
	return ref.Hash().String()
}

func (g *gitFixture) commitAt(sha string) *object.Commit {
	c, err := g.open().CommitObject(plumbing.NewHash(sha))
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return c
}

func (g *gitFixture) file(sha, path string) string {
	f, err := g.commitAt(sha).File(path)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	s, err := f.Contents()
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return s
}

// createBranch makes a branch at sha with one extra commit of other content.
func (g *gitFixture) createBranchAt(name, sha string) {
	ExpectWithOffset(1, g.open().Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(name), plumbing.NewHash(sha)))).To(Succeed())
}

// --- fake merge request provider --------------------------------------------

type fakeProvider struct {
	mu      sync.Mutex
	open    map[string]*scm.MergeRequest // by source branch
	nextNum int64
	created []scm.CreateRequest
	calls   []string

	createErr error // returned once from CreateMergeRequest when set
	findErr   error
}

func newFakeProvider() *fakeProvider {
	return &fakeProvider{open: map[string]*scm.MergeRequest{}, nextNum: 7}
}

func (f *fakeProvider) record(c string) { f.calls = append(f.calls, c) }

func (f *fakeProvider) FindOpenMergeRequest(_ context.Context, project, branch string) (*scm.MergeRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("find " + project + " " + branch)
	if f.findErr != nil {
		return nil, f.findErr
	}
	return f.open[branch], nil
}

func (f *fakeProvider) CreateMergeRequest(_ context.Context, project string, req scm.CreateRequest) (*scm.MergeRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("create " + project + " " + req.SourceBranch)
	if f.createErr != nil {
		err := f.createErr
		f.createErr = nil
		return nil, err
	}
	f.created = append(f.created, req)
	mr := &scm.MergeRequest{
		Number: f.nextNum, URL: fmt.Sprintf("https://git.test/group/proj/-/merge_requests/%d", f.nextNum),
		State: scm.StateOpen, SourceBranch: req.SourceBranch, TargetBranch: req.TargetBranch,
	}
	f.nextNum++
	f.open[req.SourceBranch] = mr
	return mr, nil
}

func (f *fakeProvider) GetMergeRequest(context.Context, string, int64) (*scm.MergeRequest, error) {
	return nil, errors.New("not used in this phase")
}
func (f *fakeProvider) CloseMergeRequest(context.Context, string, int64) error { return nil }
func (f *fakeProvider) CommentOnMergeRequest(context.Context, string, int64, string) error {
	return nil
}
func (f *fakeProvider) DeleteBranch(context.Context, string, string) error { return nil }
func (f *fakeProvider) LookupUser(context.Context, string) (*scm.User, error) {
	return nil, scm.ErrNotFound
}

// --- writer that interferes with pushes ---------------------------------------

// racingWriter wraps the real cache and runs hook before UpdateBranch, to
// move the target branch under the operator or to refuse the push.
type racingWriter struct {
	RepositoryWriter
	updates int
	hook    func(n int) error
}

func (w *racingWriter) UpdateBranch(ctx context.Context, url, branch, sha string, auth *gitrepo.Auth) error {
	w.updates++
	if w.hook != nil {
		if err := w.hook(w.updates); err != nil {
			return err
		}
	}
	return w.RepositoryWriter.UpdateBranch(ctx, url, branch, sha, auth)
}

// --- specs ------------------------------------------------------------------

const proposeConfigMap = `# Managed by Argo CD.
apiVersion: v1
kind: ConfigMap
metadata:
  name: demo-config
data:
  # Log level of the demo app.
  LOG_LEVEL: info
`

var _ = Describe("DriftProposal proposing", func() {
	const ns = "default"
	var (
		ctx      = context.Background()
		repo     *gitFixture
		initial  string
		provider *fakeProvider
		cacheDir string
		writer   RepositoryWriter
		r        *DriftProposalReconciler
		policy   *backflowv1alpha1.BackflowPolicy
		app      *unstructured.Unstructured
		dp       *backflowv1alpha1.DriftProposal
		conn     *backflowv1alpha1.ScmConnection
	)

	reconcileDP := func() (reconcile.Result, error) {
		return r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(dp)})
	}
	latest := func() *backflowv1alpha1.DriftProposal {
		got := &backflowv1alpha1.DriftProposal{}
		ExpectWithOffset(1, k8sClient.Get(ctx, client.ObjectKeyFromObject(dp), got)).To(Succeed())
		return got
	}
	condition := func(t string) *metav1.Condition { return meta.FindStatusCondition(latest().Status.Conditions, t) }
	// forgetStatus simulates an operator that restarted before it recorded its work.
	forgetStatus := func() {
		got := latest()
		got.Status = backflowv1alpha1.DriftProposalStatus{}
		ExpectWithOffset(1, k8sClient.Status().Update(ctx, got)).To(Succeed())
	}
	setMode := func(mode backflowv1alpha1.BackflowMode, mr *backflowv1alpha1.MergeRequestOptions) {
		ExpectWithOffset(1, k8sClient.Get(ctx, client.ObjectKeyFromObject(policy), policy)).To(Succeed())
		policy.Spec.Mode = mode
		policy.Spec.MergeRequest = mr
		ExpectWithOffset(1, k8sClient.Update(ctx, policy)).To(Succeed())
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
	setConnection := func(name string) {
		ExpectWithOffset(1, k8sClient.Get(ctx, client.ObjectKeyFromObject(policy), policy)).To(Succeed())
		policy.Status.Applications[0].ScmConnection = name
		ExpectWithOffset(1, k8sClient.Status().Update(ctx, policy)).To(Succeed())
	}
	reader := func() RepositoryReader { return CacheReader{Cache: gitrepo.NewCache(cacheDir)} }

	BeforeEach(func() {
		repo = newGitFixture(map[string]string{
			"apps/demo/configmap.yaml": proposeConfigMap,
			"README.md":                "demo\n",
		})
		initial = repo.branch("main")
		provider = newFakeProvider()

		var err error
		cacheDir, err = os.MkdirTemp("", "backflow-cache-*")
		Expect(err).NotTo(HaveOccurred())
		cache := gitrepo.NewCache(cacheDir)
		writer = cache
		r = &DriftProposalReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Repos: CacheReader{Cache: cache}, Writer: writer,
			NewProvider: func(scm.Config) (scm.Provider, error) { return provider, nil },
		}

		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "prop-token", Namespace: ns},
			StringData: map[string]string{"token": "glpat-test"},
		})).To(Succeed())
		conn = &backflowv1alpha1.ScmConnection{
			ObjectMeta: metav1.ObjectMeta{Name: "prop-scm", Namespace: ns},
			Spec: backflowv1alpha1.ScmConnectionSpec{
				Provider: backflowv1alpha1.ScmProviderGitLab, URL: "https://git.test",
				TokenSecretRef: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "prop-token"}, Key: "token"},
			},
		}
		Expect(k8sClient.Create(ctx, conn)).To(Succeed())

		policy = &backflowv1alpha1.BackflowPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "prop-policy", Namespace: ns},
			Spec: backflowv1alpha1.BackflowPolicySpec{
				ArgoCDNamespace: ns,
				Applications:    backflowv1alpha1.ApplicationSelector{Names: []string{"prop-app"}},
				Mode:            backflowv1alpha1.ModeMergeRequest,
				MergeRequest: &backflowv1alpha1.MergeRequestOptions{
					BranchPrefix: "backflow/", Labels: []string{"backflow"}, Reviewers: []string{"bob"}, AssignActor: true,
				},
			},
		}
		Expect(k8sClient.Create(ctx, policy)).To(Succeed())
		policy.Status.Applications = []backflowv1alpha1.ApplicationSummary{{Name: "prop-app", RepoURL: testRepoURL, ScmConnection: "prop-scm"}}
		Expect(k8sClient.Status().Update(ctx, policy)).To(Succeed())

		app = &unstructured.Unstructured{}
		app.SetGroupVersionKind(applicationGVK)
		app.SetName("prop-app")
		app.SetNamespace(ns)
		Expect(unstructured.SetNestedField(app.Object, "demo", "spec", "destination", "namespace")).To(Succeed())
		Expect(k8sClient.Create(ctx, app)).To(Succeed())

		dp = &backflowv1alpha1.DriftProposal{
			ObjectMeta: metav1.ObjectMeta{Name: "prop-dp", Namespace: ns},
			Spec: backflowv1alpha1.DriftProposalSpec{
				PolicyName:  "prop-policy",
				Application: backflowv1alpha1.ObjectRef{Name: "prop-app", Namespace: ns},
				Resource:    backflowv1alpha1.ResourceRef{Version: "v1", Kind: "ConfigMap", Namespace: "demo", Name: "demo-config"},
				Source: backflowv1alpha1.SourceRef{
					RepoURL: testRepoURL, Revision: initial, TargetRevision: "main",
					Path: "apps/demo", Type: backflowv1alpha1.SourceDirectory,
				},
				Changes: []backflowv1alpha1.FieldChange{{
					Path: "/data/LOG_LEVEL", Op: backflowv1alpha1.OpReplace, Desired: `"info"`, Live: `"debug"`,
				}},
				Actor:      &backflowv1alpha1.Actor{Username: "alice", UserAgent: "kubectl"},
				DetectedAt: metav1.NewTime(time.Now().Add(-time.Hour)),
			},
		}
		Expect(k8sClient.Create(ctx, dp)).To(Succeed())
	})

	AfterEach(func() {
		for _, o := range []client.Object{dp, policy, app, conn,
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "prop-token", Namespace: ns}},
		} {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, o))).To(Succeed())
		}
		repo.close()
		_ = os.RemoveAll(cacheDir)
	})

	Context("MergeRequest mode", func() {
		It("pushes a branch, opens a merge request and records it", func() {
			res, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(res.IsZero()).To(BeTrue())

			got := latest()
			Expect(got.Status.Phase).To(Equal(backflowv1alpha1.PhaseProposed))
			Expect(got.Status.MergeRequest).To(Equal(&backflowv1alpha1.MergeRequestRef{
				URL: "https://git.test/group/proj/-/merge_requests/7", Number: 7, Branch: "backflow/prop-dp", State: "open",
			}))
			Expect(condition(conditionMapped).Status).To(Equal(metav1.ConditionTrue))
			Expect(condition(conditionProposed).Status).To(Equal(metav1.ConditionTrue))
			Expect(condition(conditionProposed).Reason).To(Equal(reasonMergeRequestOpened))
			Expect(condition(conditionProposed).ObservedGeneration).To(Equal(got.Generation))
			Expect(got.Status.Message).To(ContainSubstring("merge_requests/7"))

			By("the branch holds the live value and keeps the comments")
			branch := repo.branch("backflow/prop-dp")
			Expect(branch).NotTo(BeEmpty())
			content := repo.file(branch, "apps/demo/configmap.yaml")
			Expect(content).To(Equal(strings.Replace(proposeConfigMap, "LOG_LEVEL: info", "LOG_LEVEL: debug", 1)))
			Expect(repo.file(branch, "README.md")).To(Equal("demo\n"))

			By("the target branch is never written in MergeRequest mode")
			Expect(repo.branch("main")).To(Equal(initial))

			By("the commit identifies Backflow, the actor and the proposal, with a fixed time")
			c := repo.commitAt(branch)
			Expect(c.ParentHashes).To(HaveLen(1))
			Expect(c.ParentHashes[0].String()).To(Equal(initial))
			Expect(c.Committer.Name).To(Equal("Backflow"))
			Expect(c.Committer.Email).To(Equal("backflow@noreply.invalid"))
			Expect(c.Author.Name).To(Equal("Backflow"))
			Expect(c.Author.Email).To(Equal("backflow@noreply.invalid"))
			Expect(c.Author.When.Equal(got.Spec.DetectedAt.Time)).To(BeTrue())
			Expect(c.Message).To(HavePrefix("backflow: sync ConfigMap demo/demo-config from cluster\n"))
			Expect(c.Message).To(ContainSubstring("- /data/LOG_LEVEL\n"))
			Expect(c.Message).To(ContainSubstring("Proposal: prop-dp\n"))
			Expect(c.Message).To(ContainSubstring("Changed-by: alice\n"))

			By("the merge request carries title, body, labels, reviewers and assignee")
			Expect(provider.created).To(HaveLen(1))
			req := provider.created[0]
			Expect(req.SourceBranch).To(Equal("backflow/prop-dp"))
			Expect(req.TargetBranch).To(Equal("main"))
			Expect(req.Title).To(Equal("backflow: sync ConfigMap demo/demo-config from cluster"))
			Expect(req.Body).To(ContainSubstring("| ` /data/LOG_LEVEL ` | ` \"info\" ` | ` \"debug\" ` |"))
			Expect(req.Body).To(ContainSubstring("-  LOG_LEVEL: info\n+  LOG_LEVEL: debug\n"))
			Expect(req.Body).To(ContainSubstring("**Changed by:** alice (kubectl)"))
			Expect(req.Body).To(ContainSubstring("DriftProposal: `default/prop-dp`"))
			Expect(req.Body).To(ContainSubstring("lets Argo CD revert"))
			Expect(req.Labels).To(Equal([]string{"backflow"}))
			Expect(req.Reviewers).To(Equal([]string{"bob"}))
			Expect(req.Assignee).To(Equal("alice"))
			Expect(provider.calls).To(ContainElement("find group/proj backflow/prop-dp"))
		})

		It("uses the committer of the ScmConnection and a custom branch prefix", func() {
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(conn), conn)).To(Succeed())
			conn.Spec.Committer = &backflowv1alpha1.GitIdentity{Name: "Platform Bot", Email: "bot@example.com"}
			Expect(k8sClient.Update(ctx, conn)).To(Succeed())
			setMode(backflowv1alpha1.ModeMergeRequest, &backflowv1alpha1.MergeRequestOptions{BranchPrefix: "drift/"})

			_, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(latest().Status.MergeRequest.Branch).To(Equal("drift/prop-dp"))
			c := repo.commitAt(repo.branch("drift/prop-dp"))
			Expect(c.Committer.Name).To(Equal("Platform Bot"))
			Expect(c.Committer.Email).To(Equal("bot@example.com"))
			Expect(repo.branch("backflow/prop-dp")).To(BeEmpty())
		})

		It("applies the defaults of a policy without merge request options", func() {
			setMode(backflowv1alpha1.ModeMergeRequest, nil)
			_, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(latest().Status.MergeRequest.Branch).To(Equal("backflow/prop-dp"))
			Expect(provider.created[0].Assignee).To(Equal("alice"))
			Expect(provider.created[0].Labels).To(BeEmpty())
		})

		It("does not assign service accounts or when assignActor is off", func() {
			// The field is omitempty and defaults to true, so false needs an explicit patch.
			Expect(k8sClient.Patch(ctx, policy, client.RawPatch(types.MergePatchType,
				[]byte(`{"spec":{"mergeRequest":{"assignActor":false}}}`)))).To(Succeed())
			_, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(provider.created[0].Assignee).To(BeEmpty())

			setMode(backflowv1alpha1.ModeMergeRequest, &backflowv1alpha1.MergeRequestOptions{AssignActor: true})
			recreate(func(s *backflowv1alpha1.DriftProposalSpec) {
				s.Actor = &backflowv1alpha1.Actor{Username: "system:serviceaccount:ci:deployer"}
			})
			provider.open = map[string]*scm.MergeRequest{}
			_, err = reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(provider.created).To(HaveLen(2))
			Expect(provider.created[1].Assignee).To(BeEmpty())
		})

		It("rebases onto the head of the target branch", func() {
			head := repo.commit(map[string]string{"README.md": "moved on\n"}, "unrelated change")

			_, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(latest().Status.Phase).To(Equal(backflowv1alpha1.PhaseProposed))

			branch := repo.branch("backflow/prop-dp")
			c := repo.commitAt(branch)
			Expect(c.ParentHashes[0].String()).To(Equal(head), "based on the head, not on the synced revision")
			Expect(repo.file(branch, "README.md")).To(Equal("moved on\n"))
			Expect(repo.file(branch, "apps/demo/configmap.yaml")).To(ContainSubstring("LOG_LEVEL: debug"))
		})

		It("supersedes the proposal when the source no longer verifies at the head", func() {
			repo.commit(map[string]string{"apps/demo/configmap.yaml": ""}, "remove the ConfigMap")

			_, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			got := latest()
			Expect(got.Status.Phase).To(Equal(backflowv1alpha1.PhaseSuperseded))
			Expect(got.Status.Message).To(HavePrefix("The source changed in Git after the drift was detected."))
			Expect(condition(conditionProposed).Status).To(Equal(metav1.ConditionFalse))
			Expect(condition(conditionProposed).Reason).To(Equal(reasonSourceChanged))
			Expect(repo.branch("backflow/prop-dp")).To(BeEmpty(), "nothing is pushed")
			Expect(provider.calls).To(BeEmpty())
		})

		It("supersedes the proposal when Git already holds the live value", func() {
			repo.commit(map[string]string{"apps/demo/configmap.yaml": strings.Replace(proposeConfigMap, "LOG_LEVEL: info", "LOG_LEVEL: debug", 1)}, "adopt debug")

			_, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			got := latest()
			Expect(got.Status.Phase).To(Equal(backflowv1alpha1.PhaseSuperseded))
			Expect(got.Status.Message).To(ContainSubstring("The source changed in Git after the drift was detected."))
			Expect(got.Status.Message).To(ContainSubstring("the edit changes nothing"))
			Expect(repo.branch("backflow/prop-dp")).To(BeEmpty())
		})

		It("is idempotent when the operator restarts after opening the merge request", func() {
			_, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			branch := repo.branch("backflow/prop-dp")
			first := latest().Status.MergeRequest.DeepCopy()

			forgetStatus()
			// A different operator process: empty cache, later clock.
			r.Repos = reader()
			r.Writer = gitrepo.NewCache(cacheDir + "-restarted")
			DeferCleanup(os.RemoveAll, cacheDir+"-restarted")
			_, err = reconcileDP()
			Expect(err).NotTo(HaveOccurred())

			got := latest()
			Expect(got.Status.Phase).To(Equal(backflowv1alpha1.PhaseProposed))
			Expect(got.Status.MergeRequest).To(Equal(first))
			Expect(provider.created).To(HaveLen(1), "the open merge request is adopted, not duplicated")
			Expect(repo.branch("backflow/prop-dp")).To(Equal(branch), "the branch is reused")
		})

		It("reuses its branch when creating the merge request failed the first time", func() {
			provider.createErr = fmt.Errorf("%w: boom", scm.ErrUnavailable)

			res, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(BeNumerically(">=", retryMin))
			got := latest()
			Expect(got.Status.Phase).To(Equal(backflowv1alpha1.PhaseMapping))
			Expect(condition(conditionProposed).Status).To(Equal(metav1.ConditionFalse))
			Expect(condition(conditionProposed).Reason).To(Equal(reasonScmUnavailable))
			branch := repo.branch("backflow/prop-dp")
			Expect(branch).NotTo(BeEmpty(), "the branch was pushed before the merge request failed")

			_, err = reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(latest().Status.Phase).To(Equal(backflowv1alpha1.PhaseProposed))
			Expect(repo.branch("backflow/prop-dp")).To(Equal(branch))
			Expect(condition(conditionProposed).Status).To(Equal(metav1.ConditionTrue))
		})

		It("fails with BranchConflict when the branch holds something else, and leaves it alone", func() {
			other := repo.commit(map[string]string{"README.md": "someone else\n"}, "foreign work")
			repo.createBranchAt("backflow/prop-dp", other)
			// Put main back so the foreign commit is only on the branch.
			Expect(repo.open().Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), plumbing.NewHash(initial)))).To(Succeed())

			_, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			got := latest()
			Expect(got.Status.Phase).To(Equal(backflowv1alpha1.PhaseFailed))
			Expect(condition(conditionProposed).Reason).To(Equal(reasonBranchConflict))
			Expect(got.Status.Message).To(ContainSubstring(other))
			Expect(repo.branch("backflow/prop-dp")).To(Equal(other))
			Expect(provider.created).To(BeEmpty())
		})

		It("fails with TargetNotABranch when the Application tracks a tag", func() {
			recreate(func(s *backflowv1alpha1.DriftProposalSpec) { s.Source.TargetRevision = "v1.0.0" })
			_, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			got := latest()
			Expect(got.Status.Phase).To(Equal(backflowv1alpha1.PhaseFailed))
			Expect(condition(conditionProposed).Reason).To(Equal(reasonTargetNotABranch))
			Expect(got.Status.Message).To(ContainSubstring("v1.0.0"))
			Expect(provider.calls).To(BeEmpty())
		})

		It("fails with TargetNotABranch for a commit SHA", func() {
			recreate(func(s *backflowv1alpha1.DriftProposalSpec) { s.Source.TargetRevision = initial })
			_, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(latest().Status.Phase).To(Equal(backflowv1alpha1.PhaseFailed))
			Expect(condition(conditionProposed).Reason).To(Equal(reasonTargetNotABranch))
		})

		It("takes the target branch from the policy when the Application tracks a tag", func() {
			recreate(func(s *backflowv1alpha1.DriftProposalSpec) { s.Source.TargetRevision = "v1.0.0" })
			setMode(backflowv1alpha1.ModeMergeRequest, &backflowv1alpha1.MergeRequestOptions{TargetBranch: "main", AssignActor: true})
			_, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(latest().Status.Phase).To(Equal(backflowv1alpha1.PhaseProposed))
			Expect(provider.created[0].TargetBranch).To(Equal("main"))
		})

		It("resolves an Application that tracks HEAD to the default branch", func() {
			recreate(func(s *backflowv1alpha1.DriftProposalSpec) { s.Source.TargetRevision = "HEAD" })
			_, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(latest().Status.Phase).To(Equal(backflowv1alpha1.PhaseProposed))
			Expect(provider.created[0].TargetBranch).To(Equal("main"))
		})
	})

	Context("DirectCommit mode", func() {
		BeforeEach(func() { setMode(backflowv1alpha1.ModeDirectCommit, nil) })

		It("commits to the target branch and records the commit", func() {
			_, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())

			got := latest()
			Expect(got.Status.Phase).To(Equal(backflowv1alpha1.PhaseMerged))
			tip := repo.branch("main")
			Expect(tip).NotTo(Equal(initial))
			Expect(got.Status.CommitSHA).To(Equal(tip))
			Expect(condition(conditionProposed).Reason).To(Equal(reasonCommitted))
			Expect(repo.file(tip, "apps/demo/configmap.yaml")).To(ContainSubstring("# Log level of the demo app.\n  LOG_LEVEL: debug"))
			Expect(repo.commitAt(tip).Message).To(ContainSubstring("Proposal: prop-dp\n"))
			Expect(provider.calls).To(BeEmpty(), "no merge request in DirectCommit mode")
			Expect(got.Status.MergeRequest).To(BeNil())
		})

		It("recognises its own commit after a restart", func() {
			_, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			tip := repo.branch("main")

			forgetStatus()
			r.Writer = gitrepo.NewCache(cacheDir + "-restarted")
			DeferCleanup(os.RemoveAll, cacheDir+"-restarted")
			_, err = reconcileDP()
			Expect(err).NotTo(HaveOccurred())

			got := latest()
			Expect(got.Status.Phase).To(Equal(backflowv1alpha1.PhaseMerged))
			Expect(got.Status.CommitSHA).To(Equal(tip))
			Expect(repo.branch("main")).To(Equal(tip), "no second commit")
		})

		It("retries once from the new head when the branch moved during the push", func() {
			w := &racingWriter{RepositoryWriter: writer}
			w.hook = func(n int) error {
				if n == 1 {
					repo.commit(map[string]string{"README.md": "raced\n"}, "concurrent push")
				}
				return nil
			}
			r.Writer = w

			_, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(w.updates).To(Equal(2))
			got := latest()
			Expect(got.Status.Phase).To(Equal(backflowv1alpha1.PhaseMerged))
			tip := repo.branch("main")
			Expect(repo.file(tip, "README.md")).To(Equal("raced\n"), "the concurrent commit is kept")
			Expect(repo.file(tip, "apps/demo/configmap.yaml")).To(ContainSubstring("LOG_LEVEL: debug"))
		})

		It("fails with NonFastForward when the branch keeps moving", func() {
			w := &racingWriter{RepositoryWriter: writer}
			w.hook = func(n int) error {
				repo.commit(map[string]string{"README.md": fmt.Sprintf("raced %d\n", n)}, "concurrent push")
				return nil
			}
			r.Writer = w

			_, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(w.updates).To(Equal(2), "exactly one retry")
			got := latest()
			Expect(got.Status.Phase).To(Equal(backflowv1alpha1.PhaseFailed))
			Expect(condition(conditionProposed).Reason).To(Equal(reasonNonFastForward))
			Expect(got.Status.CommitSHA).To(BeEmpty())
		})

		It("fails with PushRejected when the server refuses the push", func() {
			w := &racingWriter{RepositoryWriter: writer}
			w.hook = func(int) error {
				return fmt.Errorf("%w: pre-receive hook declined: protected branch", gitrepo.ErrPushRejected)
			}
			r.Writer = w

			_, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(w.updates).To(Equal(1), "a refusal is not retried")
			got := latest()
			Expect(got.Status.Phase).To(Equal(backflowv1alpha1.PhaseFailed))
			Expect(condition(conditionProposed).Reason).To(Equal(reasonPushRejected))
			Expect(got.Status.Message).To(ContainSubstring("protected branch"))
		})

		It("supersedes the proposal when Git changed the line meanwhile", func() {
			repo.commit(map[string]string{"apps/demo/configmap.yaml": ""}, "remove the ConfigMap")
			_, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(latest().Status.Phase).To(Equal(backflowv1alpha1.PhaseSuperseded))
		})
	})

	Context("ReportOnly mode", func() {
		It("writes nothing and stays in Mapping", func() {
			setMode(backflowv1alpha1.ModeReportOnly, nil)
			before := repo.branch("main")

			res, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(res.IsZero()).To(BeTrue())
			got := latest()
			Expect(got.Status.Phase).To(Equal(backflowv1alpha1.PhaseMapping))
			Expect(condition(conditionMapped).Status).To(Equal(metav1.ConditionTrue))
			Expect(condition(conditionProposed).Status).To(Equal(metav1.ConditionFalse))
			Expect(condition(conditionProposed).Reason).To(Equal(reasonReportOnly))
			Expect(repo.branch("main")).To(Equal(before))
			Expect(repo.branch("backflow/prop-dp")).To(BeEmpty())
			Expect(provider.calls).To(BeEmpty())
		})
	})

	Context("credentials", func() {
		It("stays in Mapping with MissingScmConnection and recovers when one appears", func() {
			setConnection("")

			res, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(BeNumerically(">", 0))
			got := latest()
			Expect(got.Status.Phase).To(Equal(backflowv1alpha1.PhaseMapping))
			Expect(condition(conditionMapped).Status).To(Equal(metav1.ConditionTrue))
			Expect(condition(conditionProposed).Status).To(Equal(metav1.ConditionFalse))
			Expect(condition(conditionProposed).Reason).To(Equal(reasonMissingScmConnection))
			Expect(condition(conditionProposed).Message).To(ContainSubstring("git.test"))
			Expect(repo.branch("backflow/prop-dp")).To(BeEmpty())

			By("the held proposal is picked up when the policy gains a connection")
			Expect(r.authFailedProposals(ctx, policy)).To(ConsistOf(reconcile.Request{NamespacedName: client.ObjectKeyFromObject(dp)}))
			setConnection("prop-scm")
			_, err = reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(latest().Status.Phase).To(Equal(backflowv1alpha1.PhaseProposed))
			Expect(condition(conditionProposed).Status).To(Equal(metav1.ConditionTrue))
		})

		It("needs an ScmConnection in DirectCommit mode too", func() {
			setMode(backflowv1alpha1.ModeDirectCommit, nil)
			setConnection("")
			before := repo.branch("main")
			_, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(latest().Status.Phase).To(Equal(backflowv1alpha1.PhaseMapping))
			Expect(condition(conditionProposed).Reason).To(Equal(reasonMissingScmConnection))
			Expect(repo.branch("main")).To(Equal(before))
		})

		It("holds the proposal while the token Secret is missing", func() {
			By("mapping succeeds first, then the Secret disappears")
			r.Writer = nil
			_, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(condition(conditionMapped).Status).To(Equal(metav1.ConditionTrue))
			Expect(k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "prop-token", Namespace: ns}})).To(Succeed())
			r.Writer = writer

			res, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(BeNumerically(">", 0))
			Expect(latest().Status.Phase).To(Equal(backflowv1alpha1.PhaseMapping))
			Expect(condition(conditionProposed).Reason).To(Equal(reasonScmAuthFailed))
			Expect(r.authFailedProposals(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns}})).NotTo(BeEmpty())
		})

		It("holds the proposal when the policy is gone", func() {
			Expect(k8sClient.Delete(ctx, policy)).To(Succeed())
			// Mapping already succeeded before the policy disappeared.
			orig := latest()
			orig.Status.Phase = backflowv1alpha1.PhaseMapping
			meta.SetStatusCondition(&orig.Status.Conditions, metav1.Condition{Type: conditionMapped, Status: metav1.ConditionTrue, Reason: reasonMapped, Message: "ok"})
			Expect(k8sClient.Status().Update(ctx, orig)).To(Succeed())

			res, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(BeNumerically(">", 0))
			Expect(condition(conditionProposed).Reason).To(Equal(reasonPolicyNotFound))
		})
	})

	Context("provider errors", func() {
		DescribeTable("hold the proposal with a reason, never fail it",
			func(err error, reason string) {
				provider.findErr = err
				res, rerr := reconcileDP()
				Expect(rerr).NotTo(HaveOccurred())
				Expect(res.RequeueAfter).To(BeNumerically(">", 0))
				Expect(latest().Status.Phase).To(Equal(backflowv1alpha1.PhaseMapping))
				Expect(condition(conditionProposed).Status).To(Equal(metav1.ConditionFalse))
				Expect(condition(conditionProposed).Reason).To(Equal(reason))
				Expect(condition(conditionProposed).Message).To(ContainSubstring("provider says no"))
			},
			Entry("token rejected", fmt.Errorf("%w: provider says no", scm.ErrUnauthorized), reasonScmAuthFailed),
			Entry("token lacks scope", fmt.Errorf("%w: provider says no", scm.ErrForbidden), reasonScmForbidden),
			Entry("project not visible", fmt.Errorf("%w: provider says no", scm.ErrNotFound), reasonScmProjectNotFound),
			Entry("provider down", fmt.Errorf("%w: provider says no", scm.ErrUnavailable), reasonScmUnavailable),
		)

		It("fails the proposal when the provider refuses the merge request as invalid", func() {
			provider.createErr = fmt.Errorf("%w: no commits between main and the branch", scm.ErrInvalid)
			_, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(latest().Status.Phase).To(Equal(backflowv1alpha1.PhaseFailed))
			Expect(condition(conditionProposed).Reason).To(Equal(reasonMergeRequestRejected))
		})

		It("waits as long as a rate-limited provider asks", func() {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", "45")
				http.Error(w, `{"message":"slow down"}`, http.StatusTooManyRequests)
			}))
			defer srv.Close()
			r.NewProvider = func(cfg scm.Config) (scm.Provider, error) {
				cfg.BaseURL = srv.URL
				return scm.New(cfg)
			}

			res, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(Equal(45 * time.Second))
			Expect(condition(conditionProposed).Reason).To(Equal(reasonScmUnavailable))
			Expect(latest().Status.Phase).To(Equal(backflowv1alpha1.PhaseMapping))
		})
	})

	Context("without a writer", func() {
		It("maps but does not propose", func() {
			r.Writer = nil
			_, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(latest().Status.Phase).To(Equal(backflowv1alpha1.PhaseMapping))
			Expect(condition(conditionMapped).Status).To(Equal(metav1.ConditionTrue))
			Expect(condition(conditionProposed)).To(BeNil())
			Expect(repo.branch("backflow/prop-dp")).To(BeEmpty())
		})
	})
})
