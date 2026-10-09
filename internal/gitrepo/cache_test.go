package gitrepo

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
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
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("BasicAuth(%q, %q) = %v, want %v", tt.provider, tt.token, got, tt.want)
		}
	}
}

func TestFetchOptions(t *testing.T) {
	refs := []config.RefSpec{"+refs/heads/*:refs/heads/*"}

	anonymous := fetchOptions(nil, refs)
	if anonymous.Auth != nil || anonymous.CABundle != nil || anonymous.RemoteName != "origin" || len(anonymous.RefSpecs) != 1 {
		t.Errorf("anonymous options = %+v", anonymous)
	}

	withToken := fetchOptions(&Auth{Username: "oauth2", Token: "tok"}, refs)
	basic, ok := withToken.Auth.(*githttp.BasicAuth)
	if !ok || basic.Username != "oauth2" || basic.Password != "tok" || withToken.CABundle != nil {
		t.Errorf("token options = %+v", withToken)
	}

	// A private CA without a token must not turn into empty credentials.
	caOnly := fetchOptions(&Auth{CABundle: []byte("pem")}, refs)
	if caOnly.Auth != nil || string(caOnly.CABundle) != "pem" {
		t.Errorf("CA-only options = %+v", caOnly)
	}

	both := fetchOptions(&Auth{Username: "u", Token: "t", CABundle: []byte("pem")}, refs)
	if both.Auth == nil || string(both.CABundle) != "pem" {
		t.Errorf("combined options = %+v", both)
	}
}

// A server whose certificate comes from a private CA is reachable only with
// that CA bundle. The test server answers 404 to everything, which go-git
// reports as "repository not found" once TLS works.
func TestOpenTrustsTheCABundle(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.NotFoundHandler())
	srv.Config.ErrorLog = log.New(io.Discard, "", 0) // rejected handshakes are expected
	srv.StartTLS()
	defer srv.Close()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	repoURL := srv.URL + "/group/project.git"
	sha := "0123456789abcdef0123456789abcdef01234567"
	ctx := context.Background()

	t.Run("without the CA the certificate is rejected", func(t *testing.T) {
		_, err := NewCache(t.TempDir()).Open(ctx, repoURL, sha, nil)
		if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "certificate") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("a token alone does not help", func(t *testing.T) {
		_, err := NewCache(t.TempDir()).Open(ctx, repoURL, sha, &Auth{Username: "oauth2", Token: "tok"})
		if !errors.Is(err, ErrUnavailable) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("with the CA the connection works", func(t *testing.T) {
		_, err := NewCache(t.TempDir()).Open(ctx, repoURL, sha, &Auth{CABundle: ca})
		if !errors.Is(err, ErrRepositoryNotFound) {
			t.Errorf("err = %v, want the server's 404 to come through as %v", err, ErrRepositoryNotFound)
		}
	})
	t.Run("with the CA and a token", func(t *testing.T) {
		_, err := NewCache(t.TempDir()).Open(ctx, repoURL, sha, &Auth{Username: "oauth2", Token: "tok", CABundle: ca})
		if !errors.Is(err, ErrRepositoryNotFound) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("a CA bundle that does not match is rejected", func(t *testing.T) {
		wrong := selfSignedCA(t)
		_, err := NewCache(t.TempDir()).Open(ctx, repoURL, sha, &Auth{CABundle: wrong})
		if !errors.Is(err, ErrUnavailable) {
			t.Errorf("err = %v", err)
		}
	})
}

// selfSignedCA returns a PEM certificate that is not the test server's.
func selfSignedCA(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "other CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
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

func TestIsRefusal(t *testing.T) {
	tests := []struct {
		err  error
		want bool
	}{
		{git.ErrExactSHA1NotSupported, true},
		{&pktline.ErrorLine{Text: "upload-pack: not our ref abc"}, true},
		{fmt.Errorf("wrapped: %w", &pktline.ErrorLine{Text: "denied"}), true},
		{errors.New("remote error: upload-pack: not our ref abc"), true},
		{errors.New("dial tcp 10.0.0.1:443: connect: connection refused"), false},
		{errors.New("unexpected EOF"), false},
		{context.DeadlineExceeded, false},
	}
	for _, tt := range tests {
		if got := isRefusal(tt.err); got != tt.want {
			t.Errorf("isRefusal(%v) = %v, want %v", tt.err, got, tt.want)
		}
	}
}

// flakyTransport serves the first upload-pack session like a healthy server
// and breaks the connection of every later one.
type flakyTransport struct {
	transport.Transport
	mu       sync.Mutex
	sessions int
}

func (f *flakyTransport) NewUploadPackSession(ep *transport.Endpoint, auth transport.AuthMethod) (transport.UploadPackSession, error) {
	f.mu.Lock()
	f.sessions++
	n := f.sessions
	f.mu.Unlock()
	if n > 1 {
		return nil, errors.New("read tcp 10.0.0.1:443: connection reset by peer")
	}
	return f.Transport.NewUploadPackSession(ep, auth)
}

// A network failure while fetching a commit by SHA is not "the commit is gone".
func TestOpenNetworkErrorIsUnavailableNotRevisionNotFound(t *testing.T) {
	src := newSourceRepo(t)
	src.commit(map[string]string{"a.yaml": "a: 1\n"})
	flaky := &flakyTransport{Transport: server.NewClient(server.NewFilesystemLoader(osfs.New("/")))}
	client.InstallProtocol("flaky", flaky)

	cache := NewCache(t.TempDir())
	// The branch fetch succeeds, the fetch of the unknown SHA loses the connection.
	_, err := cache.Open(t.Context(), "flaky://"+filepath.Join(src.dir, ".git"), strings.Repeat("a", 40), nil)
	if !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrRevisionNotFound) {
		t.Fatalf("err = %v, want ErrUnavailable and not ErrRevisionNotFound", err)
	}
	if flaky.sessions < 2 {
		t.Errorf("sessions = %d; the SHA fetch was never attempted", flaky.sessions)
	}
}

func TestEnsureWritable(t *testing.T) {
	root := filepath.Join(t.TempDir(), "a", "b")
	if err := EnsureWritable(root); err != nil {
		t.Fatalf("creatable directory: %v", err)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Errorf("probe file left behind: %v", entries)
	}

	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureWritable(filepath.Join(file, "sub")); err == nil {
		t.Error("a path below a regular file must fail")
	}

	if os.Geteuid() != 0 {
		ro := t.TempDir()
		if err := os.Chmod(ro, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(ro, 0o700) })
		err := EnsureWritable(ro)
		if err == nil || !strings.Contains(err.Error(), "not writable") {
			t.Errorf("read-only directory: err = %v", err)
		}
	}
}
