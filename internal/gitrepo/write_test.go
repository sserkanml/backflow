package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

var (
	committer = Signature{Name: "Backflow", Email: "backflow@noreply.invalid"}
	detected  = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	meta      = CommitMeta{Committer: committer, Message: "backflow: sync ConfigMap demo/demo-config from cluster\n", When: detected}
)

const (
	cmBefore = "# managed by Argo CD\ndata:\n  LOG_LEVEL: info\n"
	cmAfter  = "# managed by Argo CD\ndata:\n  LOG_LEVEL: debug\n"
)

func seed(t *testing.T) (*sourceRepo, string) {
	t.Helper()
	src := newSourceRepo(t)
	base := src.commit(map[string]string{
		"apps/demo/configmap.yaml": cmBefore,
		"apps/demo/deploy.yaml":    "spec:\n  replicas: 1\n",
		"README.md":                "hello\n",
	})
	return src, base
}

// branchSHA returns where a branch of the source repository points, or "".
func (s *sourceRepo) branchSHA(name string) string {
	ref, err := s.repo.Reference(plumbing.NewBranchReferenceName(name), true)
	if err != nil {
		return ""
	}
	return ref.Hash().String()
}

func (s *sourceRepo) fileAt(sha, name string) string {
	s.t.Helper()
	// Reopen: objects pushed by the cache arrive in packs this handle has not seen.
	repo, err := git.PlainOpen(s.dir)
	if err != nil {
		s.t.Fatal(err)
	}
	c, err := repo.CommitObject(plumbing.NewHash(sha))
	if err != nil {
		s.t.Fatal(err)
	}
	f, err := c.File(name)
	if err != nil {
		s.t.Fatalf("%s at %s: %v", name, sha, err)
	}
	out, err := f.Contents()
	if err != nil {
		s.t.Fatal(err)
	}
	return out
}

func TestCommitFileReplacesOneFileOnTopOfBase(t *testing.T) {
	src, base := seed(t)
	cache := NewCache(t.TempDir())
	when := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	author := Signature{Name: "alice", Email: "alice@example.com"}

	sha, err := cache.CommitFile(t.Context(), src.url(), base, nil, "apps/demo/configmap.yaml", []byte(cmAfter),
		CommitMeta{Author: &author, Committer: committer, Message: "msg\n\nChanged-by: alice\n", When: when})
	if err != nil {
		t.Fatal(err)
	}

	view, err := cache.Open(t.Context(), src.url(), sha, nil)
	if err != nil {
		t.Fatalf("new commit must be readable through the cache: %v", err)
	}
	defer func() { _ = view.Close() }()
	if got, _ := view.ReadFile("apps/demo/configmap.yaml"); string(got) != cmAfter {
		t.Errorf("configmap = %q", got)
	}
	if got, _ := view.ReadFile("apps/demo/deploy.yaml"); string(got) != "spec:\n  replicas: 1\n" {
		t.Errorf("sibling changed: %q", got)
	}
	if got, _ := view.ReadFile("README.md"); string(got) != "hello\n" {
		t.Errorf("README changed: %q", got)
	}

	commit, err := cache.mustCommit(t, src.url(), sha)
	if err != nil {
		t.Fatal(err)
	}
	if len(commit.ParentHashes) != 1 || commit.ParentHashes[0].String() != base {
		t.Errorf("parents = %v, want [%s]", commit.ParentHashes, base)
	}
	if commit.Author.Name != "alice" || commit.Author.Email != "alice@example.com" {
		t.Errorf("author = %+v", commit.Author)
	}
	if commit.Committer.Name != "Backflow" || commit.Committer.Email != "backflow@noreply.invalid" {
		t.Errorf("committer = %+v", commit.Committer)
	}
	if !commit.Author.When.Equal(when) || !strings.Contains(commit.Message, "Changed-by: alice") {
		t.Errorf("when/message = %v %q", commit.Author.When, commit.Message)
	}

	// Writing the commit moves nothing and pushes nothing.
	if got := src.branchSHA("master"); got != base {
		t.Errorf("master = %s, want %s", got, base)
	}
	if got := src.fileAt(base, "apps/demo/configmap.yaml"); got != cmBefore {
		t.Errorf("base commit changed: %q", got)
	}
}

// mustCommit reads a commit object out of the cache repository.
func (c *Cache) mustCommit(t *testing.T, repoURL, sha string) (*object.Commit, error) {
	t.Helper()
	repo, err := git.PlainOpen(c.Dir(repoURL))
	if err != nil {
		return nil, err
	}
	return repo.CommitObject(plumbing.NewHash(sha))
}

func TestCommitFileAuthorDefaultsToCommitter(t *testing.T) {
	src, base := seed(t)
	cache := NewCache(t.TempDir())
	sha, err := cache.CommitFile(t.Context(), src.url(), base, nil, "README.md", []byte("bye\n"), meta)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := cache.mustCommit(t, src.url(), sha)
	if c.Author.Name != "Backflow" || c.Author.Email != committer.Email || c.Author.When.IsZero() {
		t.Errorf("author = %+v", c.Author)
	}
}

func TestCommitFileKeepsFileMode(t *testing.T) {
	src := newSourceRepo(t)
	full := filepath.Join(src.dir, "run.sh")
	if err := os.WriteFile(full, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	base := src.commit(nil)
	cache := NewCache(t.TempDir())
	sha, err := cache.CommitFile(t.Context(), src.url(), base, nil, "run.sh", []byte("#!/bin/sh\necho hi\n"), meta)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := cache.mustCommit(t, src.url(), sha)
	tree, _ := c.Tree()
	e, err := tree.FindEntry("run.sh")
	if err != nil || e.Mode != filemode.Executable {
		t.Errorf("entry = %+v, err %v; want executable", e, err)
	}
}

func TestCommitFileErrors(t *testing.T) {
	src := newSourceRepo(t)
	if err := os.Symlink("README.md", filepath.Join(src.dir, "link")); err != nil {
		t.Fatal(err)
	}
	base := src.commit(map[string]string{"README.md": "hello\n", "apps/a.yaml": "a: 1\n"})
	cache := NewCache(t.TempDir())

	cases := []struct {
		name, sha, file string
		content         string
		want            error
	}{
		{"missing file", base, "nope.yaml", "x", ErrFileNotFound},
		{"missing directory", base, "nodir/a.yaml", "x", ErrFileNotFound},
		{"directory", base, "apps", "x", ErrNotRegularFile},
		{"file used as directory", base, "README.md/x", "x", ErrFileNotFound},
		{"symlink", base, "link", "x", ErrNotRegularFile},
		{"unchanged", base, "README.md", "hello\n", ErrNoChange},
		{"parent escape", base, "../README.md", "x", ErrFileNotFound},
		{"absolute path", base, "/README.md", "x", ErrFileNotFound},
		{"short sha", "abc", "README.md", "x", ErrRevisionNotFound},
		{"unknown sha", strings.Repeat("0", 40), "README.md", "x", ErrRevisionNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := cache.CommitFile(t.Context(), src.url(), tc.sha, nil, tc.file, []byte(tc.content), meta)
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestBranchTip(t *testing.T) {
	src, base := seed(t)
	cache := NewCache(t.TempDir())
	ctx := t.Context()

	tip, err := cache.BranchTip(ctx, src.url(), "master", nil)
	if err != nil || tip != base {
		t.Fatalf("tip = %q, %v; want %s", tip, err, base)
	}
	next := src.commit(map[string]string{"README.md": "v2\n"})
	if tip, err = cache.BranchTip(ctx, src.url(), "master", nil); err != nil || tip != next {
		t.Fatalf("tip after new commit = %q, %v; want %s", tip, err, next)
	}
	// The tip commit is readable without another fetch.
	view, err := cache.Open(ctx, src.url(), tip, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = view.Close()

	if _, err := cache.BranchTip(ctx, src.url(), "nope", nil); !errors.Is(err, ErrBranchNotFound) {
		t.Errorf("missing branch: err = %v", err)
	}
	if _, err := cache.BranchTip(ctx, "file:///does/not/exist", "master", nil); err == nil || errors.Is(err, ErrBranchNotFound) {
		t.Errorf("unreachable repository: err = %v", err)
	}
}

func TestBranchTipSeesDeletedBranch(t *testing.T) {
	src, base := seed(t)
	cache := NewCache(t.TempDir())
	ctx := t.Context()
	sha, err := cache.CommitFile(ctx, src.url(), base, nil, "README.md", []byte("x\n"), meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.CreateBranch(ctx, src.url(), "backflow/dp-1", sha, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.BranchTip(ctx, src.url(), "backflow/dp-1", nil); err != nil {
		t.Fatal(err)
	}
	// Somebody deletes the branch (closing the merge request, for example).
	if err := src.repo.Storer.RemoveReference(plumbing.NewBranchReferenceName("backflow/dp-1")); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.BranchTip(ctx, src.url(), "backflow/dp-1", nil); !errors.Is(err, ErrBranchNotFound) {
		t.Fatalf("deleted branch: err = %v, want ErrBranchNotFound", err)
	}
	// ... and it can be created again.
	if err := cache.CreateBranch(ctx, src.url(), "backflow/dp-1", sha, nil); err != nil {
		t.Fatalf("re-create: %v", err)
	}
	if got := src.branchSHA("backflow/dp-1"); got != sha {
		t.Errorf("branch = %s, want %s", got, sha)
	}
}

func TestCreateBranch(t *testing.T) {
	src, base := seed(t)
	cache := NewCache(t.TempDir())
	ctx := t.Context()
	sha, err := cache.CommitFile(ctx, src.url(), base, nil, "apps/demo/configmap.yaml", []byte(cmAfter), meta)
	if err != nil {
		t.Fatal(err)
	}

	if err := cache.CreateBranch(ctx, src.url(), "backflow/dp-1", sha, nil); err != nil {
		t.Fatal(err)
	}
	if got := src.branchSHA("backflow/dp-1"); got != sha {
		t.Fatalf("branch = %s, want %s", got, sha)
	}
	if got := src.fileAt(sha, "apps/demo/configmap.yaml"); got != cmAfter {
		t.Errorf("pushed content = %q", got)
	}
	if got := src.branchSHA("master"); got != base {
		t.Errorf("target branch moved to %s", got)
	}

	// Idempotent when the branch already has our commit.
	if err := cache.CreateBranch(ctx, src.url(), "backflow/dp-1", sha, nil); err != nil {
		t.Errorf("second create: %v", err)
	}

	// A different commit on an existing branch is reported, not overwritten.
	other, err := cache.CommitFile(ctx, src.url(), base, nil, "README.md", []byte("other\n"), meta)
	if err != nil {
		t.Fatal(err)
	}
	err = cache.CreateBranch(ctx, src.url(), "backflow/dp-1", other, nil)
	var exists *BranchExistsError
	if !errors.Is(err, ErrBranchExists) || !errors.As(err, &exists) || exists.SHA != sha || exists.Branch != "backflow/dp-1" {
		t.Fatalf("err = %v, want BranchExistsError at %s", err, sha)
	}
	if got := src.branchSHA("backflow/dp-1"); got != sha {
		t.Errorf("existing branch was moved to %s", got)
	}
}

func TestCreateBranchRequiresKnownCommit(t *testing.T) {
	src, _ := seed(t)
	cache := NewCache(t.TempDir())
	if err := cache.CreateBranch(t.Context(), src.url(), "b", strings.Repeat("a", 40), nil); !errors.Is(err, ErrRevisionNotFound) {
		t.Errorf("unknown commit: err = %v", err)
	}
	if err := cache.CreateBranch(t.Context(), src.url(), "b", "short", nil); !errors.Is(err, ErrRevisionNotFound) {
		t.Errorf("short sha: err = %v", err)
	}
}

func TestUpdateBranchFastForward(t *testing.T) {
	src, base := seed(t)
	cache := NewCache(t.TempDir())
	ctx := t.Context()
	sha, err := cache.CommitFile(ctx, src.url(), base, nil, "apps/demo/configmap.yaml", []byte(cmAfter), meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.UpdateBranch(ctx, src.url(), "master", sha, nil); err != nil {
		t.Fatal(err)
	}
	if got := src.branchSHA("master"); got != sha {
		t.Fatalf("master = %s, want %s", got, sha)
	}
	if err := cache.UpdateBranch(ctx, src.url(), "master", sha, nil); err != nil {
		t.Errorf("repeat: %v", err)
	}
	if err := cache.UpdateBranch(ctx, src.url(), "nope", sha, nil); !errors.Is(err, ErrBranchNotFound) {
		t.Errorf("missing branch: err = %v", err)
	}
}

func TestUpdateBranchNonFastForward(t *testing.T) {
	src, base := seed(t)
	cache := NewCache(t.TempDir())
	ctx := t.Context()
	sha, err := cache.CommitFile(ctx, src.url(), base, nil, "apps/demo/configmap.yaml", []byte(cmAfter), meta)
	if err != nil {
		t.Fatal(err)
	}
	// The branch moves on after our commit was built.
	head := src.commit(map[string]string{"README.md": "moved on\n"})

	err = cache.UpdateBranch(ctx, src.url(), "master", sha, nil)
	if !errors.Is(err, ErrNonFastForward) {
		t.Fatalf("err = %v, want ErrNonFastForward", err)
	}
	if got := src.branchSHA("master"); got != head {
		t.Errorf("master was overwritten: %s", got)
	}

	// Retry from the new head, as the controller does once.
	tip, err := cache.BranchTip(ctx, src.url(), "master", nil)
	if err != nil || tip != head {
		t.Fatalf("tip = %q, %v", tip, err)
	}
	retry, err := cache.CommitFile(ctx, src.url(), tip, nil, "apps/demo/configmap.yaml", []byte(cmAfter), meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.UpdateBranch(ctx, src.url(), "master", retry, nil); err != nil {
		t.Fatal(err)
	}
	if got := src.fileAt(src.branchSHA("master"), "README.md"); got != "moved on\n" {
		t.Errorf("the concurrent change was lost: %q", got)
	}
}

func TestClassifyPush(t *testing.T) {
	cases := []struct {
		err  error
		want error
	}{
		{git.ErrNonFastForwardUpdate, ErrNonFastForward},
		{errors.New("remote: rejected (fetch first)"), ErrNonFastForward},
		{errors.New("command error on refs/heads/main: pre-receive hook declined"), ErrPushRejected},
		{errors.New("remote: GitLab: You are not allowed to push code to protected branches on this project."), ErrPushRejected},
		{errors.New("unpack error: bad object"), ErrPushRejected},
		{errors.New("authentication required"), ErrUnavailable},
		{fmt.Errorf("dial tcp: %w", context.DeadlineExceeded), ErrUnavailable},
	}
	for _, tc := range cases {
		if got := classifyPush(tc.err); !errors.Is(got, tc.want) {
			t.Errorf("classifyPush(%q) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

func TestWritesAreSerialisedPerRepository(t *testing.T) {
	src, base := seed(t)
	cache := NewCache(t.TempDir())
	ctx := t.Context()

	var wg sync.WaitGroup
	errs := make(chan error, 24)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sha, err := cache.CommitFile(ctx, src.url(), base, nil, "README.md", []byte(fmt.Sprintf("v%d\n", i)), meta)
			if err != nil {
				errs <- err
				return
			}
			errs <- cache.CreateBranch(ctx, src.url(), fmt.Sprintf("backflow/dp-%d", i), sha, nil)
			if view, err := cache.Open(ctx, src.url(), base, nil); err != nil {
				errs <- err
			} else {
				_ = view.Close()
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	for i := 0; i < 8; i++ {
		sha := src.branchSHA(fmt.Sprintf("backflow/dp-%d", i))
		if sha == "" || src.fileAt(sha, "README.md") != fmt.Sprintf("v%d\n", i) {
			t.Errorf("branch %d missing or wrong", i)
		}
	}
}

func TestWriteGivesUpWhileWaitingForTheLock(t *testing.T) {
	src, base := seed(t)
	cache := NewCache(t.TempDir())
	unlock, err := cache.lock(t.Context(), cache.Dir(src.url()))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, err := cache.CommitFile(ctx, src.url(), base, nil, "README.md", []byte("x\n"), meta); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("CommitFile: err = %v", err)
	}
	if _, err := cache.BranchTip(ctx, src.url(), "master", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("BranchTip: err = %v", err)
	}
	if err := cache.CreateBranch(ctx, src.url(), "b", base, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("CreateBranch: err = %v", err)
	}
	if err := cache.UpdateBranch(ctx, src.url(), "master", base, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("UpdateBranch: err = %v", err)
	}
}

func TestCommitFileIsDeterministic(t *testing.T) {
	src, base := seed(t)
	cache := NewCache(t.TempDir())
	ctx := t.Context()
	commit := func(m CommitMeta, content string) string {
		t.Helper()
		sha, err := cache.CommitFile(ctx, src.url(), base, nil, "apps/demo/configmap.yaml", []byte(content), m)
		if err != nil {
			t.Fatal(err)
		}
		return sha
	}
	first := commit(meta, cmAfter)
	time.Sleep(1100 * time.Millisecond) // a clock-based time would differ by now
	if again := commit(meta, cmAfter); again != first {
		t.Errorf("same inputs gave %s and %s", first, again)
	}
	// A different cache (a restarted operator) agrees too.
	other, err := NewCache(t.TempDir()).CommitFile(ctx, src.url(), base, nil, "apps/demo/configmap.yaml", []byte(cmAfter), meta)
	if err != nil || other != first {
		t.Errorf("fresh cache: %s, %v; want %s", other, err, first)
	}
	later := meta
	later.When = detected.Add(time.Hour)
	if commit(later, cmAfter) == first {
		t.Error("a different time must give a different SHA")
	}
	if commit(meta, cmAfter+"# x\n") == first {
		t.Error("different content must give a different SHA")
	}
}

func TestCommitFileRequiresATimestamp(t *testing.T) {
	src, base := seed(t)
	cache := NewCache(t.TempDir())
	m := meta
	m.When = time.Time{}
	if _, err := cache.CommitFile(t.Context(), src.url(), base, nil, "README.md", []byte("x\n"), m); !errors.Is(err, ErrNoTimestamp) {
		t.Errorf("err = %v, want ErrNoTimestamp", err)
	}
}

// The operator restarts after pushing the branch but before recording it. The
// retry rebuilds the commit, possibly with another time, and must adopt the branch.
func TestCreateBranchAdoptsTheSameChangeCommittedAtAnotherTime(t *testing.T) {
	src, base := seed(t)
	ctx := t.Context()
	first := NewCache(t.TempDir())
	pushed, err := first.CommitFile(ctx, src.url(), base, nil, "apps/demo/configmap.yaml", []byte(cmAfter), meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.CreateBranch(ctx, src.url(), "backflow/dp-1", pushed, nil); err != nil {
		t.Fatal(err)
	}

	retry := meta
	retry.When = detected.Add(5 * time.Minute)
	retry.Message = "a different message\n"
	restarted := NewCache(t.TempDir()) // empty cache, as after a pod restart
	sha, err := restarted.CommitFile(ctx, src.url(), base, nil, "apps/demo/configmap.yaml", []byte(cmAfter), retry)
	if err != nil {
		t.Fatal(err)
	}
	if sha == pushed {
		t.Fatal("test needs two different commits")
	}
	if err := restarted.CreateBranch(ctx, src.url(), "backflow/dp-1", sha, nil); err != nil {
		t.Fatalf("same parent and tree must be adopted, got %v", err)
	}
	if got := src.branchSHA("backflow/dp-1"); got != pushed {
		t.Errorf("branch = %s; the existing branch must stay at %s", got, pushed)
	}
}

func TestCreateBranchDoesNotAdoptADifferentChange(t *testing.T) {
	src, base := seed(t)
	ctx := t.Context()
	cache := NewCache(t.TempDir())
	commit := func(parent, file, content string) string {
		t.Helper()
		sha, err := cache.CommitFile(ctx, src.url(), parent, nil, file, []byte(content), meta)
		if err != nil {
			t.Fatal(err)
		}
		return sha
	}
	want := commit(base, "apps/demo/configmap.yaml", cmAfter)

	cases := map[string]string{
		"other tree": commit(base, "apps/demo/configmap.yaml", cmAfter+"# more\n"),
		"other file": commit(base, "README.md", "changed\n"),
	}
	// Same tree on a different parent: the branch was built on a newer head.
	newHead := src.commit(map[string]string{"README.md": "moved on\n"})
	cases["other parent"] = commit(newHead, "apps/demo/configmap.yaml", cmAfter)
	for name, foreign := range cases {
		t.Run(name, func(t *testing.T) {
			branch := "backflow/" + strings.ReplaceAll(name, " ", "-")
			if err := cache.CreateBranch(ctx, src.url(), branch, foreign, nil); err != nil {
				t.Fatal(err)
			}
			err := cache.CreateBranch(ctx, src.url(), branch, want, nil)
			var exists *BranchExistsError
			if !errors.As(err, &exists) || exists.SHA != foreign {
				t.Fatalf("err = %v, want BranchExistsError at %s", err, foreign)
			}
			if got := src.branchSHA(branch); got != foreign {
				t.Errorf("branch moved to %s", got)
			}
		})
	}
}

func TestDefaultBranch(t *testing.T) {
	src, _ := seed(t)
	cache := NewCache(t.TempDir())
	got, err := cache.DefaultBranch(t.Context(), src.url(), nil)
	if err != nil || got != "master" {
		t.Fatalf("DefaultBranch = %q, %v; want master", got, err)
	}
	if _, err := cache.DefaultBranch(t.Context(), "file:///does/not/exist", nil); err == nil || errors.Is(err, ErrBranchNotFound) {
		t.Errorf("unreachable repository: err = %v", err)
	}
	empty := newSourceRepo(t)
	if _, err := cache.DefaultBranch(t.Context(), empty.url(), nil); !errors.Is(err, ErrBranchNotFound) {
		t.Errorf("empty repository: err = %v, want ErrBranchNotFound", err)
	}
}

func TestCommitInfo(t *testing.T) {
	src, base := seed(t)
	cache := NewCache(t.TempDir())
	ctx := t.Context()
	sha, err := cache.CommitFile(ctx, src.url(), base, nil, "README.md", []byte("x\n"), meta)
	if err != nil {
		t.Fatal(err)
	}
	info, err := cache.CommitInfo(ctx, src.url(), sha, nil)
	if err != nil {
		t.Fatal(err)
	}
	if info.Message != meta.Message || len(info.Parents) != 1 || info.Parents[0] != base {
		t.Errorf("info = %+v", info)
	}
	if _, err := cache.CommitInfo(ctx, src.url(), "short", nil); !errors.Is(err, ErrRevisionNotFound) {
		t.Errorf("short sha: err = %v", err)
	}
	if _, err := cache.CommitInfo(ctx, src.url(), strings.Repeat("0", 40), nil); !errors.Is(err, ErrRevisionNotFound) {
		t.Errorf("unknown sha: err = %v", err)
	}
	// A commit the cache does not have yet is fetched.
	fresh := NewCache(t.TempDir())
	if info, err := fresh.CommitInfo(ctx, src.url(), base, nil); err != nil || len(info.Parents) != 0 {
		t.Errorf("fresh cache: %+v, %v", info, err)
	}
}

func TestFirstParentLog(t *testing.T) {
	src := newSourceRepo(t)
	c1 := src.commit(map[string]string{"a": "1\n"})
	c2 := src.commit(map[string]string{"a": "2\n"})
	c3 := src.commit(map[string]string{"a": "3\n"})
	c4 := src.commit(map[string]string{"a": "4\n"})
	cache := NewCache(t.TempDir())
	ctx := t.Context()
	shas := func(es []LogEntry) []string {
		var out []string
		for _, e := range es {
			out = append(out, e.SHA)
		}
		return out
	}

	tests := []struct {
		name        string
		tip, stop   string
		limit       int
		want        []string
		wantFound   bool
		wantParents int // of the newest entry
	}{
		{"back to the stop, which is not included", c4, c2, 100, []string{c4, c3}, true, 1},
		{"the stop is the tip itself", c4, c4, 100, nil, true, 0},
		{"no stop walks to the root", c3, "", 100, []string{c3, c2, c1}, false, 1},
		{"a stop that is not in the history", c3, strings.Repeat("0", 40), 100, []string{c3, c2, c1}, false, 1},
		{"the limit cuts the walk", c4, c1, 2, []string{c4, c3}, false, 1},
		{"the stop right at the limit is found", c4, c2, 2, []string{c4, c3}, true, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, found, err := cache.FirstParentLog(ctx, src.url(), tt.tip, tt.stop, tt.limit, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(shas(got), tt.want) || found != tt.wantFound {
				t.Errorf("FirstParentLog = %v, %v; want %v, %v", shas(got), found, tt.want, tt.wantFound)
			}
			if len(got) > 0 && got[0].Message != "test" || len(got) > 0 && got[0].Parents != tt.wantParents {
				t.Errorf("first entry = %+v", got[0])
			}
		})
	}
	if _, _, err := cache.FirstParentLog(ctx, src.url(), "short", "", 10, nil); !errors.Is(err, ErrRevisionNotFound) {
		t.Errorf("short sha: err = %v", err)
	}
}

func TestIsAncestor(t *testing.T) {
	src, base := seed(t)
	cache := NewCache(t.TempDir())
	ctx := t.Context()
	child, err := cache.CommitFile(ctx, src.url(), base, nil, "README.md", []byte("x\n"), meta)
	if err != nil {
		t.Fatal(err)
	}
	sibling, err := cache.CommitFile(ctx, src.url(), base, nil, "README.md", []byte("y\n"), meta)
	if err != nil {
		t.Fatal(err)
	}
	grandchild, err := cache.CommitFile(ctx, src.url(), child, nil, "apps/demo/deploy.yaml", []byte("spec:\n  replicas: 2\n"), meta)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, a, d string
		want       bool
	}{
		{"parent of child", base, child, true},
		{"grandparent", base, grandchild, true},
		{"itself", child, child, true},
		{"child is not the parent's ancestor", child, base, false},
		{"siblings", child, sibling, false},
	}
	for _, tc := range cases {
		got, err := cache.IsAncestor(ctx, src.url(), tc.a, tc.d, nil)
		if err != nil || got != tc.want {
			t.Errorf("%s: got %v, %v; want %v", tc.name, got, err, tc.want)
		}
	}
	if _, err := cache.IsAncestor(ctx, src.url(), "short", child, nil); !errors.Is(err, ErrRevisionNotFound) {
		t.Errorf("short sha: err = %v", err)
	}
	if _, err := cache.IsAncestor(ctx, src.url(), strings.Repeat("0", 40), child, nil); !errors.Is(err, ErrRevisionNotFound) {
		t.Errorf("unknown sha: err = %v", err)
	}
}

func TestChangedPaths(t *testing.T) {
	src, base := seed(t)
	cache := NewCache(t.TempDir())
	ctx := t.Context()
	one, err := cache.CommitFile(ctx, src.url(), base, nil, "apps/demo/configmap.yaml", []byte(cmAfter), meta)
	if err != nil {
		t.Fatal(err)
	}
	two, err := cache.CommitFile(ctx, src.url(), one, nil, "README.md", []byte("x\n"), meta)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		from, to string
		want     []string
	}{
		{base, one, []string{"apps/demo/configmap.yaml"}},
		{base, two, []string{"README.md", "apps/demo/configmap.yaml"}},
		{one, two, []string{"README.md"}},
		{base, base, []string{}},
	} {
		got, err := cache.ChangedPaths(ctx, src.url(), tc.from, tc.to, nil)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("ChangedPaths = %v, %v; want %v", got, err, tc.want)
		}
	}
	if _, err := cache.ChangedPaths(ctx, src.url(), "short", one, nil); !errors.Is(err, ErrRevisionNotFound) {
		t.Errorf("short sha: err = %v", err)
	}
}

func TestHasFileWithSuffix(t *testing.T) {
	src := newSourceRepo(t)
	sha := src.commit(map[string]string{
		"apps/plain/configmap.yaml":  "a: 1\n",
		"apps/jsonnet/main.jsonnet":  "{}\n",
		"apps/lib/deep/x.libsonnet":  "{}\n",
		"apps/other/jsonnet.yaml":    "a: 1\n",
		"apps/dirsuffix.jsonnet/a.y": "a: 1\n",
		"main.jsonnet":               "{}\n",
	})
	cache := NewCache(t.TempDir())
	tests := []struct {
		name, dir string
		want      bool
	}{
		{"a .jsonnet file in the directory", "apps/jsonnet", true},
		{"a .libsonnet file deeper", "apps/lib", true},
		{"only yaml", "apps/plain", false},
		{"a name that merely contains jsonnet", "apps/other", false},
		{"a directory named like a jsonnet file is not a file", "apps/dirsuffix.jsonnet", false},
		{"above the files", "apps", true},
		{"the root", "", true},
		{"the path itself is a jsonnet file", "main.jsonnet", true},
		{"a path that does not exist", "apps/missing", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := cache.HasFileWithSuffix(t.Context(), src.url(), sha, tt.dir, nil, ".jsonnet", ".libsonnet")
			if err != nil || got != tt.want {
				t.Errorf("HasFileWithSuffix(%q) = %v, %v; want %v", tt.dir, got, err, tt.want)
			}
		})
	}
	if _, err := cache.HasFileWithSuffix(t.Context(), src.url(), "short", "apps", nil, ".jsonnet"); !errors.Is(err, ErrRevisionNotFound) {
		t.Errorf("short sha: err = %v", err)
	}
}

func TestHasSymlink(t *testing.T) {
	src := newSourceRepo(t)
	if err := os.MkdirAll(filepath.Join(src.dir, "apps", "linked"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src.dir, "apps", "deep", "er"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../README.md", filepath.Join(src.dir, "apps", "linked", "readme")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../../README.md", filepath.Join(src.dir, "apps", "deep", "er", "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("apps", filepath.Join(src.dir, "dirlink")); err != nil {
		t.Fatal(err)
	}
	sha := src.commit(map[string]string{
		"README.md":                 "hello\n",
		"apps/plain/configmap.yaml": "a: 1\n",
		"apps/plain2/x.yaml":        "a: 1\n",
		"apps/linked2/x.yaml":       "a: 1\n",
		"apps/deep/top.yaml":        "a: 1\n",
	})
	cache := NewCache(t.TempDir())
	ctx := t.Context()

	tests := []struct {
		name, dir string
		want      bool
	}{
		{"directory without links", "apps/plain", false},
		{"link directly in the directory", "apps/linked", true},
		{"link deeper below the directory", "apps/deep", true},
		{"a sibling whose name is a prefix is not affected", "apps/linked2", false},
		{"directory above the links", "apps", true},
		{"the repository root", "", true},
		{"the root as a dot", ".", true},
		{"slashes around the path", "/apps/plain/", false},
		{"the path itself is a link", "dirlink", true},
		{"a path that does not exist", "apps/missing", false},
		{"a path that is a file", "README.md", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := cache.HasSymlink(ctx, src.url(), sha, tt.dir, nil)
			if err != nil || got != tt.want {
				t.Errorf("HasSymlink(%q) = %v, %v; want %v", tt.dir, got, err, tt.want)
			}
		})
	}
	if _, err := cache.HasSymlink(ctx, src.url(), "short", "apps", nil); !errors.Is(err, ErrRevisionNotFound) {
		t.Errorf("short sha: err = %v", err)
	}
	if _, err := cache.HasSymlink(ctx, src.url(), strings.Repeat("0", 40), "apps", nil); !errors.Is(err, ErrRevisionNotFound) {
		t.Errorf("unknown sha: err = %v", err)
	}
}
