package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	"github.com/go-git/go-git/v5/plumbing/transport/server"
)

func init() {
	// Serve file:// URLs in-process, so the tests need no git binary.
	client.InstallProtocol("file", server.NewClient(server.NewFilesystemLoader(osfs.New("/"))))
}

// sourceRepo is a local repository the tests commit to.
type sourceRepo struct {
	t    *testing.T
	dir  string
	repo *git.Repository
}

func newSourceRepo(t *testing.T) *sourceRepo {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	return &sourceRepo{t: t, dir: dir, repo: repo}
}

// url is the file:// URL of the repository's .git directory.
func (s *sourceRepo) url() string { return "file://" + filepath.Join(s.dir, ".git") }

// commit writes files (path -> content), stages everything and returns the commit SHA.
func (s *sourceRepo) commit(files map[string]string) string {
	s.t.Helper()
	wt, err := s.repo.Worktree()
	if err != nil {
		s.t.Fatal(err)
	}
	for name, content := range files {
		full := filepath.Join(s.dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			s.t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			s.t.Fatal(err)
		}
	}
	if _, err := wt.Add("."); err != nil {
		s.t.Fatal(err)
	}
	hash, err := wt.Commit("test", &git.CommitOptions{
		Author: &object.Signature{Name: "t", Email: "t@example.com", When: time.Now()},
	})
	if err != nil {
		s.t.Fatal(err)
	}
	return hash.String()
}

func TestOpenReadsTheExactCommit(t *testing.T) {
	src := newSourceRepo(t)
	first := src.commit(map[string]string{
		"apps/demo/configmap.yaml": "data:\n  LOG_LEVEL: info\n",
		"apps/demo/deploy.yaml":    "spec:\n  replicas: 1\n",
		"README.md":                "hello\n",
	})
	second := src.commit(map[string]string{"apps/demo/configmap.yaml": "data:\n  LOG_LEVEL: debug\n"})

	cache := NewCache(t.TempDir())
	ctx := context.Background()

	v1, err := cache.Open(ctx, src.url(), first, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = v1.Close() }()
	v2, err := cache.Open(ctx, src.url(), second, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = v2.Close() }()

	for _, tt := range []struct {
		name string
		view *View
		sha  string
		want string
	}{
		{"first commit", v1, first, "data:\n  LOG_LEVEL: info\n"},
		{"second commit", v2, second, "data:\n  LOG_LEVEL: debug\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.view.SHA() != tt.sha {
				t.Errorf("SHA = %s", tt.view.SHA())
			}
			got, err := fs.ReadFile(tt.view, "apps/demo/configmap.yaml")
			if err != nil || string(got) != tt.want {
				t.Errorf("content = %q, err = %v", got, err)
			}
		})
	}

	t.Run("is a valid fs.FS", func(t *testing.T) {
		if err := fstest.TestFS(v2, "README.md", "apps/demo/configmap.yaml", "apps/demo/deploy.yaml"); err != nil {
			t.Error(err)
		}
	})

	t.Run("lists directories sorted", func(t *testing.T) {
		entries, err := fs.ReadDir(v2, "apps/demo")
		if err != nil || len(entries) != 2 || entries[0].Name() != "configmap.yaml" || entries[1].Name() != "deploy.yaml" {
			t.Errorf("entries = %v, err = %v", entries, err)
		}
	})

	t.Run("missing paths", func(t *testing.T) {
		if _, err := fs.ReadFile(v2, "nope.yaml"); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("err = %v", err)
		}
		if _, err := fs.ReadDir(v2, "nope"); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("err = %v", err)
		}
		if _, err := v2.Open("../etc/passwd"); !errors.Is(err, fs.ErrInvalid) {
			t.Errorf("err = %v", err)
		}
	})
}

func TestOpenReusesAndFetchesIncrementally(t *testing.T) {
	src := newSourceRepo(t)
	first := src.commit(map[string]string{"a.yaml": "a: 1\n"})
	cache := NewCache(t.TempDir())
	ctx := context.Background()

	v, err := cache.Open(ctx, src.url(), first, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = v.Close()

	// A commit made after the first Open is fetched on demand.
	second := src.commit(map[string]string{"a.yaml": "a: 2\n"})
	v, err = cache.Open(ctx, src.url(), second, nil)
	if err != nil {
		t.Fatalf("new commit: %v", err)
	}
	_ = v.Close()

	// Both commits are served from the cache once the source is gone.
	if err := os.RemoveAll(src.dir); err != nil {
		t.Fatal(err)
	}
	for _, sha := range []string{first, second} {
		v, err := cache.Open(ctx, src.url(), sha, nil)
		if err != nil {
			t.Fatalf("cached %s: %v", sha, err)
		}
		_ = v.Close()
	}
}

func TestOpenErrors(t *testing.T) {
	src := newSourceRepo(t)
	sha := src.commit(map[string]string{"a.yaml": "a: 1\n"})
	cache := NewCache(t.TempDir())
	ctx := context.Background()

	tests := []struct {
		name string
		url  string
		sha  string
		want error
	}{
		{"unknown commit", src.url(), "0123456789abcdef0123456789abcdef01234567", ErrRevisionNotFound},
		{"branch name instead of SHA", src.url(), "main", ErrRevisionNotFound},
		{"short SHA", src.url(), sha[:7], ErrRevisionNotFound},
		{"missing repository", "file://" + filepath.Join(t.TempDir(), "nope"), sha, ErrRepositoryNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, err := cache.Open(ctx, tt.url, tt.sha, nil)
			if err == nil {
				_ = v.Close()
			}
			if !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestOpenConcurrently(t *testing.T) {
	src := newSourceRepo(t)
	sha := src.commit(map[string]string{"a.yaml": "a: 1\n"})
	cache := NewCache(t.TempDir())

	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := 0; i < cap(errs); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := cache.Open(context.Background(), src.url(), sha, nil)
			if err != nil {
				errs <- err
				return
			}
			defer func() { _ = v.Close() }()
			if _, err := fs.ReadFile(v, "a.yaml"); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// A View is read after Open returned and released the repository lock. Each
// Open uses its own go-git repository handle and Git objects are immutable
// (a fetch only adds files), so a fetch into the same cache must not disturb
// readers. Run with -race.
func TestViewIsReadableWhileAnotherOpenFetches(t *testing.T) {
	src := newSourceRepo(t)
	first := src.commit(map[string]string{
		"apps/a.yaml": "a: 1\n",
		"apps/b.yaml": "b: 1\n",
	})
	cache := NewCache(t.TempDir())
	ctx := context.Background()

	view, err := cache.Open(ctx, src.url(), first, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = view.Close() }()

	stop := make(chan struct{})
	readerErr := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if got, err := fs.ReadFile(view, "apps/a.yaml"); err != nil || string(got) != "a: 1\n" {
				readerErr <- fmt.Errorf("read a.yaml = %q, %v", got, err)
				return
			}
			if entries, err := fs.ReadDir(view, "apps"); err != nil || len(entries) != 2 {
				readerErr <- fmt.Errorf("readdir = %v, %v", entries, err)
				return
			}
		}
	}()

	for i := 0; i < 15; i++ {
		sha := src.commit(map[string]string{"apps/c.yaml": fmt.Sprintf("c: %d\n", i)})
		v, err := cache.Open(ctx, src.url(), sha, nil)
		if err != nil {
			t.Fatalf("fetch %d: %v", i, err)
		}
		if got, err := fs.ReadFile(v, "apps/c.yaml"); err != nil || string(got) != fmt.Sprintf("c: %d\n", i) {
			t.Fatalf("new view %d = %q, %v", i, got, err)
		}
		_ = v.Close()
	}
	close(stop)
	wg.Wait()
	select {
	case err := <-readerErr:
		t.Fatal(err)
	default:
	}
}

func TestOpenGivesUpWhileWaitingForTheLock(t *testing.T) {
	cache := NewCache(t.TempDir())
	dir := cache.Dir("file:///x")
	unlock, err := cache.lock(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = cache.Open(ctx, "file:///x", "0123456789abcdef0123456789abcdef01234567", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v", err)
	}
}

func TestDir(t *testing.T) {
	c := NewCache("/cache")
	a := c.Dir("https://gitlab.com/x/y.git")
	if a != c.Dir("https://gitlab.com/x/y.git/") {
		t.Error("a trailing slash changed the directory")
	}
	if a == c.Dir("https://gitlab.com/x/z.git") {
		t.Error("different repositories share a directory")
	}
	if filepath.Dir(a) != "/cache" {
		t.Errorf("dir = %s", a)
	}
	if NewCache("").root != DefaultRoot() {
		t.Error("empty root should use the default")
	}
}

func TestBasicAuth(t *testing.T) {
	tests := []struct {
		provider, token string
		want            *Auth
	}{
		{"gitlab", "tok", &Auth{Username: "oauth2", Token: "tok"}},
		{"GitHub", "tok", &Auth{Username: "x-access-token", Token: "tok"}},
		{"gitlab", "", nil},
		{"bitbucket", "tok", nil},
	}
	for _, tt := range tests {
		got := BasicAuth(tt.provider, tt.token)
		if (got == nil) != (tt.want == nil) || (got != nil && *got != *tt.want) {
			t.Errorf("BasicAuth(%q, %q) = %v, want %v", tt.provider, tt.token, got, tt.want)
		}
	}
}

func TestClassify(t *testing.T) {
	tests := []struct {
		err  error
		want error
	}{
		{transport.ErrAuthenticationRequired, ErrAuth},
		{transport.ErrAuthorizationFailed, ErrAuth},
		{transport.ErrRepositoryNotFound, ErrRepositoryNotFound},
		{transport.ErrEmptyRemoteRepository, ErrRevisionNotFound},
		{errors.New("connection refused"), ErrUnavailable},
	}
	for _, tt := range tests {
		if got := classify(tt.err); !errors.Is(got, tt.want) {
			t.Errorf("classify(%v) = %v, want %v", tt.err, got, tt.want)
		}
	}
}
