package controller

import (
	"context"
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
	// Fresh handle: objects pushed by the operator arrive in packs an old one has not seen.
	g.repo = g.open()
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

// commitOnBranch commits on another branch and returns to main.
func (g *gitFixture) commitOnBranch(branch string, files map[string]string, msg string) string {
	g.repo = g.open()
	wt, err := g.repo.Worktree()
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	ExpectWithOffset(1, wt.Checkout(&git.CheckoutOptions{Branch: plumbing.NewBranchReferenceName(branch), Force: true})).To(Succeed())
	sha := g.commit(files, msg)
	ExpectWithOffset(1, wt.Checkout(&git.CheckoutOptions{Branch: plumbing.NewBranchReferenceName("main"), Force: true})).To(Succeed())
	return sha
}

// createBranchAt makes a branch point at sha.
func (g *gitFixture) createBranchAt(name, sha string) {
	ExpectWithOffset(1, g.open().Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(name), plumbing.NewHash(sha)))).To(Succeed())
}

// --- fake merge request provider --------------------------------------------

type fakeProvider struct {
	mu       sync.Mutex
	open     map[string]*scm.MergeRequest // open merge requests by source branch
	byNumber map[int64]*scm.MergeRequest
	nextNum  int64
	created  []scm.CreateRequest
	calls    []string

	// Injected failures. A non-nil error is returned (once, when the name
	// ends in Once) by the call it is named after.
	createErr, findErr, getErr            error
	commentErr, closeErr, deleteErr       error
	commentErrOnce, closeErrOnce, delOnce bool

	comments []string
	closed   []int64
	deleted  []string
}

func newFakeProvider() *fakeProvider {
	return &fakeProvider{open: map[string]*scm.MergeRequest{}, byNumber: map[int64]*scm.MergeRequest{}, nextNum: 7}
}

func (f *fakeProvider) record(c string) { f.calls = append(f.calls, c) }

// setState changes a merge request behind the operator's back, as a person would.
func (f *fakeProvider) setState(number int64, state scm.State, mergeSHA string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	mr := f.byNumber[number]
	mr.State, mr.MergeCommitSHA = state, mergeSHA
	if state != scm.StateOpen {
		delete(f.open, mr.SourceBranch)
	}
}

func (f *fakeProvider) FindOpenMergeRequest(_ context.Context, project, branch string) (*scm.MergeRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("find " + project + " " + branch)
	if f.findErr != nil {
		return nil, f.findErr
	}
	return f.open[branch], nil
}

// FindMergeRequest returns the merge request of the branch in any state: the
// open one, else the newest.
func (f *fakeProvider) FindMergeRequest(_ context.Context, project, branch string) (*scm.MergeRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("find-any " + project + " " + branch)
	if f.findErr != nil {
		return nil, f.findErr
	}
	if mr := f.open[branch]; mr != nil {
		return mr, nil
	}
	var newest *scm.MergeRequest
	for _, mr := range f.byNumber {
		// A merge request still marked open but missing from f.open was dropped by the test.
		if mr.SourceBranch == branch && mr.State != scm.StateOpen && (newest == nil || mr.Number > newest.Number) {
			newest = mr
		}
	}
	return newest, nil
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
	f.byNumber[mr.Number] = mr
	return mr, nil
}

func (f *fakeProvider) GetMergeRequest(_ context.Context, _ string, number int64) (*scm.MergeRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(fmt.Sprintf("get %d", number))
	if f.getErr != nil {
		return nil, f.getErr
	}
	mr, ok := f.byNumber[number]
	if !ok {
		return nil, fmt.Errorf("%w: merge request %d", scm.ErrNotFound, number)
	}
	cp := *mr
	return &cp, nil
}

// once returns err and clears it when once is set.
func once(err *error, once *bool) error {
	e := *err
	if *once {
		*err, *once = nil, false
	}
	return e
}

func (f *fakeProvider) CloseMergeRequest(_ context.Context, _ string, number int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(fmt.Sprintf("close %d", number))
	if err := once(&f.closeErr, &f.closeErrOnce); err != nil {
		return err
	}
	f.closed = append(f.closed, number)
	if mr := f.byNumber[number]; mr != nil {
		mr.State = scm.StateClosed
		delete(f.open, mr.SourceBranch)
	}
	return nil
}

func (f *fakeProvider) CommentOnMergeRequest(_ context.Context, _ string, number int64, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(fmt.Sprintf("comment %d", number))
	if err := once(&f.commentErr, &f.commentErrOnce); err != nil {
		return err
	}
	f.comments = append(f.comments, body)
	return nil
}

func (f *fakeProvider) DeleteBranch(_ context.Context, _ string, branch string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("delete " + branch)
	if err := once(&f.deleteErr, &f.delOnce); err != nil {
		return err
	}
	f.deleted = append(f.deleted, branch)
	return nil
}

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

// branchWriter wraps the real cache and runs hook before a branch is created.
type branchWriter struct {
	RepositoryWriter
	hook func()
}

func (w *branchWriter) CreateBranch(ctx context.Context, url, branch, sha string, auth *gitrepo.Auth) error {
	if w.hook != nil {
		w.hook()
	}
	return w.RepositoryWriter.CreateBranch(ctx, url, branch, sha, auth)
}

// tipWriter wraps the real cache and fails BranchTip for one branch while err is set.
type tipWriter struct {
	RepositoryWriter
	branch string
	err    error
}

func (w *tipWriter) BranchTip(ctx context.Context, url, branch string, auth *gitrepo.Auth) (string, error) {
	if w.err != nil && branch == w.branch {
		return "", w.err
	}
	return w.RepositoryWriter.BranchTip(ctx, url, branch, auth)
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
			Expect(res.RequeueAfter).To(Equal(trackInterval), "the merge request is polled from now on")

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
			Expect(provider.calls).To(ContainElement("find-any group/proj backflow/prop-dp"))
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

		Context("an existing branch of the same name", func() {
			ourMessage := "backflow: sync ConfigMap demo/demo-config from cluster\n\nProposal: prop-dp\n"
			debugMap := strings.Replace(proposeConfigMap, "LOG_LEVEL: info", "LOG_LEVEL: debug", 1)

			// pushedEarlier: the operator pushed the branch, then lost its status.
			pushedEarlier := func() string {
				_, err := reconcileDP()
				Expect(err).NotTo(HaveOccurred())
				tip := repo.branch("backflow/prop-dp")
				forgetStatus()
				provider.open = map[string]*scm.MergeRequest{}
				return tip
			}
			expectConflict := func(tip, why string) {
				_, err := reconcileDP()
				Expect(err).NotTo(HaveOccurred())
				got := latest()
				Expect(got.Status.Phase).To(Equal(backflowv1alpha1.PhaseFailed))
				Expect(condition(conditionProposed).Reason).To(Equal(reasonBranchConflict))
				Expect(got.Status.Message).To(ContainSubstring(why))
				Expect(repo.branch("backflow/prop-dp")).To(Equal(tip), "someone else's branch is never touched")
				Expect(provider.created).To(HaveLen(1), "no second merge request")
			}

			It("is adopted after the target branch moved on", func() {
				tip := pushedEarlier()
				head := repo.commit(map[string]string{"README.md": "moved on\n"}, "unrelated change")

				_, err := reconcileDP()
				Expect(err).NotTo(HaveOccurred())
				got := latest()
				Expect(got.Status.Phase).To(Equal(backflowv1alpha1.PhaseProposed))
				Expect(got.Status.MergeRequest.Branch).To(Equal("backflow/prop-dp"))
				Expect(repo.branch("backflow/prop-dp")).To(Equal(tip), "the branch is reused as it is")
				Expect(repo.commitAt(tip).ParentHashes[0].String()).To(Equal(initial))
				Expect(repo.branch("main")).To(Equal(head))
				Expect(provider.created).To(HaveLen(2), "a merge request is opened for it again")
			})

			It("is not adopted when a human commit sits on top", func() {
				tip := pushedEarlier()
				human := repo.commitOnBranch("backflow/prop-dp", map[string]string{"README.md": "reviewer fix\n"}, "address review")
				Expect(human).NotTo(Equal(tip))
				repo.commit(map[string]string{"README.md": "moved on\n"}, "unrelated change")

				expectConflict(human, "trailer")
			})

			It("is not adopted when the file content differs", func() {
				repo.createBranchAt("backflow/prop-dp", initial)
				tip := repo.commitOnBranch("backflow/prop-dp",
					map[string]string{"apps/demo/configmap.yaml": strings.Replace(proposeConfigMap, "LOG_LEVEL: info", "LOG_LEVEL: trace", 1)}, ourMessage)
				provider.created = append(provider.created, scm.CreateRequest{}) // keeps expectConflict's count honest
				expectConflict(tip, "differs from the content")
			})

			It("is not adopted when it also changes another file", func() {
				repo.createBranchAt("backflow/prop-dp", initial)
				tip := repo.commitOnBranch("backflow/prop-dp",
					map[string]string{"apps/demo/configmap.yaml": debugMap, "README.md": "sneaked in\n"}, ourMessage)
				provider.created = append(provider.created, scm.CreateRequest{})
				expectConflict(tip, "not only apps/demo/configmap.yaml")
			})

			It("is not adopted when its base is not in the target branch history", func() {
				repo.createBranchAt("side", initial)
				side := repo.commitOnBranch("side", map[string]string{"README.md": "side work\n"}, "side")
				repo.createBranchAt("backflow/prop-dp", side)
				tip := repo.commitOnBranch("backflow/prop-dp", map[string]string{"apps/demo/configmap.yaml": debugMap}, ourMessage)
				provider.created = append(provider.created, scm.CreateRequest{})
				expectConflict(tip, "not part of the target branch history")
			})

			It("without our trailer is only reused while it is exactly the change we would push", func() {
				repo.createBranchAt("backflow/prop-dp", initial)
				tip := repo.commitOnBranch("backflow/prop-dp", map[string]string{"apps/demo/configmap.yaml": debugMap}, "someone's identical edit")

				By("same base and same tree: it is the very change, whoever committed it")
				_, err := reconcileDP()
				Expect(err).NotTo(HaveOccurred())
				Expect(latest().Status.Phase).To(Equal(backflowv1alpha1.PhaseProposed))
				Expect(repo.branch("backflow/prop-dp")).To(Equal(tip))

				By("once the target moved, the trailer is required")
				forgetStatus()
				provider.open = map[string]*scm.MergeRequest{}
				repo.commit(map[string]string{"README.md": "moved on\n"}, "unrelated change")
				_, err = reconcileDP()
				Expect(err).NotTo(HaveOccurred())
				Expect(latest().Status.Phase).To(Equal(backflowv1alpha1.PhaseFailed))
				Expect(latest().Status.Message).To(ContainSubstring("trailer"))
				Expect(repo.branch("backflow/prop-dp")).To(Equal(tip))
			})
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

	Context("tracking and cleanup", func() {
		branch := "backflow/prop-dp"
		annotate := func(key, value string) {
			ExpectWithOffset(1, k8sClient.Patch(ctx, latest(), client.RawPatch(types.MergePatchType,
				[]byte(fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, key, value))))).To(Succeed())
		}
		cleaned := func() *metav1.Condition { return condition(conditionCleanedUp) }

		BeforeEach(func() {
			_, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(latest().Status.Phase).To(Equal(backflowv1alpha1.PhaseProposed))
			provider.calls = nil
		})

		It("keeps polling while the merge request is open", func() {
			before := latest()
			res, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(Equal(2 * time.Minute))
			got := latest()
			Expect(got.Status.Phase).To(Equal(backflowv1alpha1.PhaseProposed))
			Expect(got.Status.MergeRequest.State).To(Equal("open"))
			Expect(got.ResourceVersion).To(Equal(before.ResourceVersion), "an unchanged state writes nothing")
			Expect(provider.calls).To(Equal([]string{"get 7"}))
		})

		It("records a merge as Merged with the merge commit", func() {
			provider.setState(7, scm.StateMerged, "abc123def")
			res, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(res.IsZero()).To(BeTrue())
			got := latest()
			Expect(got.Status.Phase).To(Equal(backflowv1alpha1.PhaseMerged))
			Expect(got.Status.CommitSHA).To(Equal("abc123def"))
			Expect(got.Status.MergeRequest.State).To(Equal("merged"))
			Expect(got.Status.Message).To(ContainSubstring("merged"))
			Expect(provider.calls).To(Equal([]string{"get 7"}), "nothing is closed or deleted after a merge")

			_, err = reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(provider.calls).To(HaveLen(1), "terminal: no more polling")
		})

		It("records a close without merge as Rejected and leaves the branch alone", func() {
			provider.setState(7, scm.StateClosed, "")
			_, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			got := latest()
			Expect(got.Status.Phase).To(Equal(backflowv1alpha1.PhaseRejected))
			Expect(got.Status.MergeRequest.State).To(Equal("closed"))
			Expect(got.Status.Message).To(ContainSubstring("Argo CD can revert"))
			Expect(provider.deleted).To(BeEmpty())
			Expect(repo.branch(branch)).NotTo(BeEmpty())
		})

		DescribeTable("closes the merge request with a comment and deletes the branch",
			func(key, value, phase, wantInComment string) {
				annotate(key, value)
				res, err := reconcileDP()
				Expect(err).NotTo(HaveOccurred())
				Expect(res.IsZero()).To(BeTrue())

				got := latest()
				Expect(string(got.Status.Phase)).To(Equal(phase))
				Expect(provider.comments).To(HaveLen(1))
				Expect(provider.comments[0]).To(ContainSubstring(wantInComment))
				Expect(provider.comments[0]).To(ContainSubstring("default/prop-dp"))
				Expect(provider.closed).To(Equal([]int64{7}))
				Expect(provider.deleted).To(Equal([]string{branch}))
				Expect(provider.calls).To(Equal([]string{"get 7", "get 7", "comment 7", "close 7", "delete " + branch}),
					"the reason is posted before the merge request is closed")
				Expect(cleaned().Status).To(Equal(metav1.ConditionTrue))
				Expect(cleaned().Reason).To(Equal(reasonCleanedUp))
				Expect(got.Status.MergeRequest.State).To(Equal("closed"))
			},
			Entry("superseded by a newer drift", annotationSupersededBy, "newer-dp", "Superseded", "A newer drift on the same resource replaced this proposal."),
			Entry("reverted: live went back to Git", annotationReverted, "The live resource is back in sync with Git.", "Reverted", "The live resource is back in sync with Git."),
		)

		It("records the newer proposal on a superseded one", func() {
			annotate(annotationSupersededBy, "newer-dp")
			_, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(latest().Status.SupersededBy).To(Equal("newer-dp"))
		})

		It("prefers what happened to the merge request over a pending revert", func() {
			By("merged: Argo CD saw the merged Git state, the drift is gone, the drift controller says reverted")
			provider.setState(7, scm.StateMerged, "abc123")
			annotate(annotationReverted, "The live resource is back in sync with Git.")
			_, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			got := latest()
			Expect(got.Status.Phase).To(Equal(backflowv1alpha1.PhaseMerged))
			Expect(got.Status.CommitSHA).To(Equal("abc123"))
			Expect(provider.comments).To(BeEmpty())
			Expect(provider.closed).To(BeEmpty())
			Expect(provider.deleted).To(BeEmpty())
		})

		It("calls a closed merge request Rejected even when the cluster already reverted", func() {
			provider.setState(7, scm.StateClosed, "")
			annotate(annotationReverted, "The live resource is back in sync with Git.")
			_, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(latest().Status.Phase).To(Equal(backflowv1alpha1.PhaseRejected))
			Expect(provider.comments).To(BeEmpty())
		})

		It("waits instead of guessing when the merge request cannot be read", func() {
			annotate(annotationReverted, "The live resource is back in sync with Git.")
			provider.getErr = fmt.Errorf("%w: boom", scm.ErrUnavailable)

			res, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(BeNumerically(">=", trackInterval))
			got := latest()
			Expect(got.Status.Phase).To(Equal(backflowv1alpha1.PhaseProposed), "no verdict without looking")
			Expect(got.Status.Message).To(ContainSubstring("Cannot read the state of merge request"))
			Expect(provider.closed).To(BeEmpty())

			provider.getErr = nil
			_, err = reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(latest().Status.Phase).To(Equal(backflowv1alpha1.PhaseReverted))
			Expect(provider.closed).To(Equal([]int64{7}))
		})

		It("restores the message once the merge request can be read again", func() {
			before := latest().Status.Message
			Expect(before).To(ContainSubstring("proposes the change against main"))
			provider.getErr = fmt.Errorf("%w: boom", scm.ErrUnavailable)
			_, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(latest().Status.Message).To(HavePrefix(trackingFailurePrefix))

			provider.getErr = nil
			_, err = reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			got := latest()
			Expect(got.Status.Phase).To(Equal(backflowv1alpha1.PhaseProposed))
			Expect(got.Status.Message).To(Equal(before))
		})

		It("restores the live-reverted message, not the plain one, after a failed poll", func() {
			annotate(annotationLiveReverted, "0123456789abcdef")
			_, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			reverted := latest().Status.Message
			Expect(reverted).To(ContainSubstring("reset to Git"))

			provider.getErr = fmt.Errorf("%w: boom", scm.ErrUnavailable)
			_, err = reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(latest().Status.Message).To(HavePrefix(trackingFailurePrefix))
			provider.getErr = nil
			_, err = reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(latest().Status.Message).To(Equal(reverted))
		})

		It("keeps polling through a provider outage without changing the phase", func() {
			provider.getErr = fmt.Errorf("%w: boom", scm.ErrForbidden)
			res, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(Equal(trackInterval))
			Expect(latest().Status.Phase).To(Equal(backflowv1alpha1.PhaseProposed))
		})

		It("does not repeat the comment when closing failed the first time", func() {
			annotate(annotationSupersededBy, "newer-dp")
			provider.closeErr, provider.closeErrOnce = fmt.Errorf("%w: boom", scm.ErrUnavailable), true

			res, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(BeNumerically(">=", retryMin))
			Expect(latest().Status.Phase).To(Equal(backflowv1alpha1.PhaseSuperseded), "the phase is already final")
			Expect(cleaned().Status).To(Equal(metav1.ConditionFalse))
			Expect(cleaned().Reason).To(Equal(reasonCommented))
			Expect(provider.comments).To(HaveLen(1))
			Expect(provider.deleted).To(BeEmpty(), "the branch stays while the merge request is open")

			_, err = reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(provider.comments).To(HaveLen(1), "the explanation is posted once")
			Expect(provider.closed).To(Equal([]int64{7}))
			Expect(provider.deleted).To(Equal([]string{branch}))
			Expect(cleaned().Status).To(Equal(metav1.ConditionTrue))
		})

		It("retries the comment when it could not be posted, and does not close without it", func() {
			annotate(annotationReverted, "back in sync")
			provider.commentErr, provider.commentErrOnce = fmt.Errorf("%w: boom", scm.ErrUnavailable), true

			_, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(provider.closed).To(BeEmpty())
			Expect(cleaned().Reason).To(Equal(reasonCleanupFailed))

			_, err = reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(provider.comments).To(HaveLen(1))
			Expect(provider.closed).To(Equal([]int64{7}))
			Expect(cleaned().Status).To(Equal(metav1.ConditionTrue))
		})

		It("finishes the cleanup when only deleting the branch failed", func() {
			annotate(annotationSupersededBy, "newer-dp")
			provider.deleteErr, provider.delOnce = fmt.Errorf("%w: boom", scm.ErrUnavailable), true

			_, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(provider.closed).To(Equal([]int64{7}))
			Expect(cleaned().Status).To(Equal(metav1.ConditionFalse))

			_, err = reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(provider.closed).To(Equal([]int64{7}), "closed once")
			Expect(provider.comments).To(HaveLen(1))
			Expect(provider.deleted).To(Equal([]string{branch}))
			Expect(cleaned().Status).To(Equal(metav1.ConditionTrue))
		})

		It("treats an already deleted branch as done", func() {
			annotate(annotationReverted, "back in sync")
			provider.deleteErr = fmt.Errorf("%w: branch", scm.ErrNotFound)
			_, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(cleaned().Status).To(Equal(metav1.ConditionTrue))
		})

		It("does not guess when tracking cannot find the merge request", func() {
			delete(provider.byNumber, 7)
			delete(provider.open, branch)
			res, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(BeNumerically(">=", trackInterval))
			Expect(latest().Status.Phase).To(Equal(backflowv1alpha1.PhaseProposed), "a 404 can also mean the token lost access")
		})

		It("only deletes the branch when the merge request of a retired proposal is gone", func() {
			got := latest()
			got.Status.Phase = backflowv1alpha1.PhaseReverted
			got.Status.Message = "back in sync"
			Expect(k8sClient.Status().Update(ctx, got)).To(Succeed())
			delete(provider.byNumber, 7)
			delete(provider.open, branch)

			_, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(provider.comments).To(BeEmpty())
			Expect(provider.closed).To(BeEmpty())
			Expect(provider.deleted).To(Equal([]string{branch}))
			Expect(cleaned().Status).To(Equal(metav1.ConditionTrue))
		})

		Context("when Argo CD reset the cluster to Git while the merge request is open", func() {
			const rev = "0123456789abcdef0123456789abcdef01234567"
			liveReverted := func() *metav1.Condition { return condition(conditionLiveReverted) }

			It("keeps the proposal Proposed, comments once and keeps tracking", func() {
				annotate(annotationLiveReverted, rev)
				res, err := reconcileDP()
				Expect(err).NotTo(HaveOccurred())
				Expect(res.RequeueAfter).To(Equal(trackInterval), "tracking goes on")

				got := latest()
				Expect(got.Status.Phase).To(Equal(backflowv1alpha1.PhaseProposed))
				Expect(got.Status.MergeRequest.State).To(Equal("open"))
				Expect(liveReverted().Status).To(Equal(metav1.ConditionTrue))
				Expect(liveReverted().Reason).To(Equal(reasonSyncedNewRevision))
				Expect(liveReverted().ObservedGeneration).To(Equal(got.Generation))
				Expect(got.Status.Message).To(ContainSubstring(rev))
				Expect(provider.comments).To(HaveLen(1))
				Expect(provider.comments[0]).To(Equal(
					"The cluster was reset to Git by an Argo CD sync of " + rev + ". " +
						"Merging this merge request makes the change permanent again.\n"))
				Expect(provider.closed).To(BeEmpty(), "the merge request stays open")
				Expect(provider.deleted).To(BeEmpty(), "and so does its branch")
				Expect(repo.branch(branch)).NotTo(BeEmpty())
				Expect(condition(conditionCleanedUp)).To(BeNil())

				By("one comment, however often it is polled")
				for i := 0; i < 3; i++ {
					_, err = reconcileDP()
					Expect(err).NotTo(HaveOccurred())
				}
				Expect(provider.comments).To(HaveLen(1))
				Expect(latest().Status.Phase).To(Equal(backflowv1alpha1.PhaseProposed))
			})

			It("is merged later as usual", func() {
				annotate(annotationLiveReverted, rev)
				_, err := reconcileDP()
				Expect(err).NotTo(HaveOccurred())

				provider.setState(7, scm.StateMerged, "abc123")
				_, err = reconcileDP()
				Expect(err).NotTo(HaveOccurred())
				got := latest()
				Expect(got.Status.Phase).To(Equal(backflowv1alpha1.PhaseMerged))
				Expect(got.Status.CommitSHA).To(Equal("abc123"))
				Expect(provider.closed).To(BeEmpty())
			})

			It("is rejected later as usual when the merge request is closed", func() {
				annotate(annotationLiveReverted, rev)
				_, err := reconcileDP()
				Expect(err).NotTo(HaveOccurred())

				provider.setState(7, scm.StateClosed, "")
				_, err = reconcileDP()
				Expect(err).NotTo(HaveOccurred())
				Expect(latest().Status.Phase).To(Equal(backflowv1alpha1.PhaseRejected))
				Expect(provider.deleted).To(BeEmpty())
			})

			It("can still be superseded: the merge request is then closed", func() {
				annotate(annotationLiveReverted, rev)
				_, err := reconcileDP()
				Expect(err).NotTo(HaveOccurred())

				annotate(annotationSupersededBy, "newer-dp")
				_, err = reconcileDP()
				Expect(err).NotTo(HaveOccurred())
				Expect(latest().Status.Phase).To(Equal(backflowv1alpha1.PhaseSuperseded))
				Expect(provider.closed).To(Equal([]int64{7}))
				Expect(provider.deleted).To(Equal([]string{branch}))
			})

			It("goes False when the change is live again, and comments again for a second reset", func() {
				annotate(annotationLiveReverted, rev)
				_, err := reconcileDP()
				Expect(err).NotTo(HaveOccurred())

				By("the drift controller removes the annotation when the change returns")
				got := latest()
				delete(got.Annotations, annotationLiveReverted)
				Expect(k8sClient.Update(ctx, got)).To(Succeed())
				_, err = reconcileDP()
				Expect(err).NotTo(HaveOccurred())
				Expect(liveReverted().Status).To(Equal(metav1.ConditionFalse))
				Expect(liveReverted().Reason).To(Equal(reasonLiveChangeReturned))
				Expect(latest().Status.Phase).To(Equal(backflowv1alpha1.PhaseProposed))
				Expect(provider.comments).To(HaveLen(1), "nothing is said when the change returns")

				By("a later reset is a new event")
				annotate(annotationLiveReverted, "fedcba9876543210fedcba9876543210fedcba98")
				_, err = reconcileDP()
				Expect(err).NotTo(HaveOccurred())
				Expect(liveReverted().Status).To(Equal(metav1.ConditionTrue))
				Expect(provider.comments).To(HaveLen(2))
				Expect(provider.comments[1]).To(ContainSubstring("fedcba9876543210"))
			})

			It("retries the comment when it could not be posted, without recording the reset", func() {
				annotate(annotationLiveReverted, rev)
				provider.commentErr, provider.commentErrOnce = fmt.Errorf("%w: boom", scm.ErrUnavailable), true

				res, err := reconcileDP()
				Expect(err).NotTo(HaveOccurred())
				Expect(res.RequeueAfter).To(BeNumerically(">=", trackInterval))
				Expect(liveReverted()).To(BeNil())
				Expect(latest().Status.Phase).To(Equal(backflowv1alpha1.PhaseProposed))

				_, err = reconcileDP()
				Expect(err).NotTo(HaveOccurred())
				Expect(liveReverted().Status).To(Equal(metav1.ConditionTrue))
				Expect(provider.comments).To(HaveLen(1))
			})
		})

		It("does nothing for a proposal that is already cleaned up", func() {
			annotate(annotationSupersededBy, "newer-dp")
			_, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			provider.calls = nil
			res, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(res.IsZero()).To(BeTrue())
			Expect(provider.calls).To(BeEmpty())
		})

		It("cleans up a retired proposal a restarted operator finds", func() {
			By("the phase is final but the merge request was never closed")
			got := latest()
			got.Status.Phase = backflowv1alpha1.PhaseReverted
			got.Status.Message = "back in sync"
			Expect(k8sClient.Status().Update(ctx, got)).To(Succeed())

			_, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(provider.closed).To(Equal([]int64{7}))
			Expect(provider.deleted).To(Equal([]string{branch}))
			Expect(provider.comments[0]).To(ContainSubstring("back in sync"))
		})

		Context("branches that are not only ours", func() {
			retire := func() {
				annotate(annotationSupersededBy, "newer")
				_, err := reconcileDP()
				ExpectWithOffset(1, err).NotTo(HaveOccurred())
				ExpectWithOffset(1, latest().Status.Phase).To(Equal(backflowv1alpha1.PhaseSuperseded))
			}

			It("deletes a branch that holds only the proposal's commits", func() {
				retire()
				Expect(provider.closed).To(Equal([]int64{7}))
				Expect(provider.deleted).To(Equal([]string{branch}))
				Expect(provider.comments[0]).NotTo(ContainSubstring("was not deleted"))
				Expect(cleaned().Reason).To(Equal(reasonCleanedUp))
			})

			It("closes the merge request but leaves a branch a person pushed to, and says so", func() {
				human := repo.commitOnBranch(branch, map[string]string{"README.md": "reviewer fix\n"}, "address review")
				retire()
				Expect(provider.closed).To(Equal([]int64{7}))
				Expect(provider.deleted).To(BeEmpty())
				Expect(repo.branch(branch)).To(Equal(human), "the branch is untouched")
				Expect(provider.comments).To(HaveLen(1))
				Expect(provider.comments[0]).To(ContainSubstring("The branch `backflow/prop-dp` was not deleted"))
				Expect(provider.comments[0]).To(ContainSubstring("someone else pushed to it"))
				Expect(cleaned().Status).To(Equal(metav1.ConditionTrue))
				Expect(cleaned().Reason).To(Equal(reasonBranchKept))
				Expect(cleaned().Message).To(ContainSubstring("left in place"))
			})

			It("leaves a branch that is the target branch's own history", func() {
				// A branch with the same name that never held our commit: it points at main.
				repo.createBranchAt(branch, repo.branch("main"))
				retire()
				Expect(provider.closed).To(Equal([]int64{7}))
				Expect(provider.deleted).To(BeEmpty())
				Expect(cleaned().Reason).To(Equal(reasonBranchKept))
			})

			It("does not call the provider for a branch that is already gone", func() {
				Expect(repo.open().Storer.RemoveReference(plumbing.NewBranchReferenceName(branch))).To(Succeed())
				retire()
				Expect(provider.closed).To(Equal([]int64{7}))
				Expect(provider.deleted).To(BeEmpty())
				Expect(cleaned().Status).To(Equal(metav1.ConditionTrue))
			})

			It("waits, without closing or deleting, while it cannot read the branch", func() {
				w := &tipWriter{RepositoryWriter: writer, branch: branch, err: fmt.Errorf("%w: boom", gitrepo.ErrUnavailable)}
				r.Writer = w
				annotate(annotationSupersededBy, "newer")
				res, err := reconcileDP()
				Expect(err).NotTo(HaveOccurred())
				Expect(res.RequeueAfter).To(BeNumerically(">", 0))
				Expect(provider.comments).To(BeEmpty())
				Expect(provider.closed).To(BeEmpty())
				Expect(provider.deleted).To(BeEmpty())
				Expect(cleaned().Status).To(Equal(metav1.ConditionFalse))

				w.err = nil
				_, err = reconcileDP()
				Expect(err).NotTo(HaveOccurred())
				Expect(provider.closed).To(Equal([]int64{7}))
				Expect(provider.deleted).To(Equal([]string{branch}))
			})
		})

		It("leaves a merge request that was merged during the cleanup alone", func() {
			got := latest()
			got.Status.Phase = backflowv1alpha1.PhaseSuperseded
			got.Status.Message = "replaced"
			Expect(k8sClient.Status().Update(ctx, got)).To(Succeed())
			provider.setState(7, scm.StateMerged, "abc")

			_, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(provider.closed).To(BeEmpty())
			Expect(provider.deleted).To(BeEmpty())
			Expect(provider.comments).To(BeEmpty())
			Expect(cleaned().Reason).To(Equal(reasonAlreadyMerged))
			Expect(latest().Status.MergeRequest.State).To(Equal("merged"))
		})

		It("holds the cleanup while the ScmConnection is gone", func() {
			annotate(annotationSupersededBy, "newer-dp")
			setConnection("")
			// Tracking cannot read the merge request without a connection.
			res, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(BeNumerically(">", 0))
			Expect(latest().Status.Phase).To(Equal(backflowv1alpha1.PhaseProposed))
			Expect(provider.calls).To(BeEmpty())
		})
	})

	Context("an operator that stopped half way through proposing", func() {
		const branch = "backflow/prop-dp"
		// loseStatus forgets everything the operator recorded after it pushed
		// the branch, as if it stopped before the status patch.
		loseStatus := func() {
			got := latest()
			recorded := got.Status.Branch
			got.Status = backflowv1alpha1.DriftProposalStatus{Branch: recorded}
			ExpectWithOffset(1, k8sClient.Status().Update(ctx, got)).To(Succeed())
		}
		annotate := func(key, value string) {
			ExpectWithOffset(1, k8sClient.Patch(ctx, latest(), client.RawPatch(types.MergePatchType,
				[]byte(fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, key, value))))).To(Succeed())
		}
		restarted := func() {
			r.Repos = reader()
			r.Writer = gitrepo.NewCache(cacheDir + "-restarted")
			DeferCleanup(os.RemoveAll, cacheDir+"-restarted")
		}

		It("records the branch before it pushes it", func() {
			var seen string
			r.Writer = &branchWriter{RepositoryWriter: writer, hook: func() { seen = latest().Status.Branch }}
			_, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(seen).To(Equal(branch))
			Expect(latest().Status.Branch).To(Equal(branch))
		})

		Context("after the branch was pushed and before the merge request was opened", func() {
			BeforeEach(func() {
				provider.createErr = fmt.Errorf("%w: boom", scm.ErrUnavailable)
				_, err := reconcileDP()
				Expect(err).NotTo(HaveOccurred())
				Expect(repo.branch(branch)).NotTo(BeEmpty())
				Expect(latest().Status.MergeRequest).To(BeNil())
				Expect(latest().Status.Branch).To(Equal(branch))
			})

			It("deletes the orphaned branch when the proposal is reverted", func() {
				annotate(annotationReverted, "back in sync")
				_, err := reconcileDP()
				Expect(err).NotTo(HaveOccurred())
				Expect(latest().Status.Phase).To(Equal(backflowv1alpha1.PhaseReverted))
				Expect(provider.deleted).To(Equal([]string{branch}))
				Expect(provider.closed).To(BeEmpty())
				Expect(condition(conditionCleanedUp).Status).To(Equal(metav1.ConditionTrue))
			})

			It("deletes the orphaned branch when the proposal is superseded", func() {
				annotate(annotationSupersededBy, "newer")
				_, err := reconcileDP()
				Expect(err).NotTo(HaveOccurred())
				Expect(latest().Status.Phase).To(Equal(backflowv1alpha1.PhaseSuperseded))
				Expect(provider.deleted).To(Equal([]string{branch}))
			})
		})

		Context("after the merge request was opened and before it was recorded", func() {
			BeforeEach(func() {
				_, err := reconcileDP()
				Expect(err).NotTo(HaveOccurred())
				Expect(provider.created).To(HaveLen(1))
				loseStatus()
				restarted()
			})

			It("adopts the open merge request", func() {
				_, err := reconcileDP()
				Expect(err).NotTo(HaveOccurred())
				got := latest()
				Expect(got.Status.Phase).To(Equal(backflowv1alpha1.PhaseProposed))
				Expect(got.Status.MergeRequest.Number).To(Equal(int64(7)))
				Expect(provider.created).To(HaveLen(1))
			})

			It("records a merge request closed meanwhile as Rejected, and opens no second one", func() {
				provider.setState(7, scm.StateClosed, "")
				res, err := reconcileDP()
				Expect(err).NotTo(HaveOccurred())
				Expect(res.IsZero()).To(BeTrue())
				got := latest()
				Expect(got.Status.Phase).To(Equal(backflowv1alpha1.PhaseRejected))
				Expect(got.Status.MergeRequest).To(Equal(&backflowv1alpha1.MergeRequestRef{
					URL: "https://git.test/group/proj/-/merge_requests/7", Number: 7, Branch: branch, State: "closed",
				}))
				Expect(provider.created).To(HaveLen(1))
			})

			It("records a merge request merged meanwhile as Merged, and opens no second one", func() {
				merged := repo.commit(map[string]string{"README.md": "merged\n"}, "merge")
				provider.setState(7, scm.StateMerged, merged)
				_, err := reconcileDP()
				Expect(err).NotTo(HaveOccurred())
				got := latest()
				Expect(got.Status.Phase).To(Equal(backflowv1alpha1.PhaseMerged))
				Expect(got.Status.CommitSHA).To(Equal(merged))
				Expect(got.Status.MergeRequest.State).To(Equal("merged"))
				Expect(provider.created).To(HaveLen(1))
			})

			It("looks the merge request up in any state even if only the pushed branch is known", func() {
				provider.setState(7, scm.StateClosed, "")
				// The status is gone entirely; the branch is found by its tip.
				forgetStatus()
				_, err := reconcileDP()
				Expect(err).NotTo(HaveOccurred())
				Expect(latest().Status.Phase).To(Equal(backflowv1alpha1.PhaseRejected))
				Expect(provider.created).To(HaveLen(1))
			})

			It("closes the merge request found by its branch when the proposal is retired", func() {
				annotate(annotationSupersededBy, "newer")
				_, err := reconcileDP()
				Expect(err).NotTo(HaveOccurred())
				got := latest()
				Expect(got.Status.Phase).To(Equal(backflowv1alpha1.PhaseSuperseded))
				Expect(provider.closed).To(Equal([]int64{7}))
				Expect(provider.deleted).To(Equal([]string{branch}))
				Expect(provider.comments).To(HaveLen(1))
				Expect(got.Status.MergeRequest).NotTo(BeNil(), "the merge request that was found is recorded")
				Expect(got.Status.MergeRequest.Number).To(Equal(int64(7)))
				Expect(got.Status.MergeRequest.State).To(Equal("closed"))
				Expect(condition(conditionCleanedUp).Status).To(Equal(metav1.ConditionTrue))
			})

			It("leaves a merge request merged meanwhile alone when the proposal is retired", func() {
				provider.setState(7, scm.StateMerged, repo.commit(map[string]string{"README.md": "merged\n"}, "merge"))
				annotate(annotationReverted, "back in sync")
				_, err := reconcileDP()
				Expect(err).NotTo(HaveOccurred())
				Expect(provider.closed).To(BeEmpty())
				Expect(provider.deleted).To(BeEmpty())
				Expect(condition(conditionCleanedUp).Reason).To(Equal(reasonAlreadyMerged))
			})
		})

		It("deletes the branch left behind when Git changes under the proposal before the retry", func() {
			provider.createErr = fmt.Errorf("%w: boom", scm.ErrUnavailable)
			_, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(repo.branch(branch)).NotTo(BeEmpty())

			repo.commit(map[string]string{"apps/demo/configmap.yaml": ""}, "remove the ConfigMap")
			_, err = reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(latest().Status.Phase).To(Equal(backflowv1alpha1.PhaseSuperseded))
			Expect(provider.deleted).To(Equal([]string{branch}))
			Expect(condition(conditionCleanedUp).Status).To(Equal(metav1.ConditionTrue))
		})
	})

	Context("retiring a proposal that has no merge request yet", func() {
		It("treats a reset by a sync as a plain revert", func() {
			setMode(backflowv1alpha1.ModeReportOnly, nil)
			_, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(latest().Status.Phase).To(Equal(backflowv1alpha1.PhaseMapping))
			Expect(k8sClient.Patch(ctx, latest(), client.RawPatch(types.MergePatchType,
				[]byte(`{"metadata":{"annotations":{"`+annotationLiveReverted+`":"0123456789abcdef"}}}`)))).To(Succeed())

			_, err = reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			got := latest()
			Expect(got.Status.Phase).To(Equal(backflowv1alpha1.PhaseReverted))
			Expect(got.Status.Message).To(ContainSubstring("0123456789abcdef"))
			Expect(provider.calls).To(BeEmpty())
			Expect(condition(conditionLiveReverted)).To(BeNil())
		})

		It("does not touch the provider", func() {
			setMode(backflowv1alpha1.ModeReportOnly, nil)
			_, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(latest().Status.Phase).To(Equal(backflowv1alpha1.PhaseMapping))
			Expect(k8sClient.Patch(ctx, latest(), client.RawPatch(types.MergePatchType,
				[]byte(`{"metadata":{"annotations":{"`+annotationSupersededBy+`":"newer"}}}`)))).To(Succeed())

			_, err = reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			Expect(latest().Status.Phase).To(Equal(backflowv1alpha1.PhaseSuperseded))
			Expect(provider.calls).To(BeEmpty())
			Expect(condition(conditionCleanedUp)).To(BeNil())
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

		It("recognises its own commit when other commits were pushed on top, after a restart", func() {
			_, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			ours := repo.branch("main")
			Expect(repo.commitAt(ours).Message).To(ContainSubstring("Proposal: prop-dp\n"))
			var top string
			for i := 0; i < 3; i++ {
				top = repo.commit(map[string]string{"README.md": fmt.Sprintf("change %d\n", i)}, fmt.Sprintf("later change %d", i))
			}

			forgetStatus()
			r.Writer = gitrepo.NewCache(cacheDir + "-restarted")
			DeferCleanup(os.RemoveAll, cacheDir+"-restarted")
			_, err = reconcileDP()
			Expect(err).NotTo(HaveOccurred())

			got := latest()
			Expect(got.Status.Phase).To(Equal(backflowv1alpha1.PhaseMerged))
			Expect(got.Status.CommitSHA).To(Equal(ours))
			Expect(repo.branch("main")).To(Equal(top), "no second commit")
		})

		It("does not mistake another proposal's commit for its own", func() {
			other := repo.commit(map[string]string{"README.md": "x\n"}, "backflow: other\n\nProposal: someone-else\n")
			_, err := reconcileDP()
			Expect(err).NotTo(HaveOccurred())
			got := latest()
			Expect(got.Status.Phase).To(Equal(backflowv1alpha1.PhaseMerged))
			Expect(got.Status.CommitSHA).NotTo(Equal(other))
			Expect(repo.branch("main")).To(Equal(got.Status.CommitSHA))
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
