// Package gitrepo reads Git repositories at an exact commit without a git
// binary. Repositories are kept as bare clones in a local cache directory and
// exposed as read-only file systems, so concurrent readers of different
// commits never disturb each other.
package gitrepo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
)

var (
	// ErrAuth means the repository requires credentials that were missing or rejected.
	ErrAuth = errors.New("gitrepo: authentication failed")
	// ErrRepositoryNotFound means the repository does not exist or is not visible.
	ErrRepositoryNotFound = errors.New("gitrepo: repository not found")
	// ErrRevisionNotFound means the commit does not exist in the repository.
	ErrRevisionNotFound = errors.New("gitrepo: revision not found")
	// ErrUnavailable means the repository could not be reached or read; it may be transient.
	ErrUnavailable = errors.New("gitrepo: repository unavailable")
)

var fullSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Auth holds how to reach a repository over HTTPS: basic authentication with
// an access token and, for servers with a private certificate authority, the
// PEM bundle that verifies them. Either part may be empty.
type Auth struct {
	Username string
	Token    string
	// CABundle is a PEM bundle of extra certificate authorities to trust.
	CABundle []byte
}

// BasicAuth returns the credentials a provider expects for a token:
// username "oauth2" for GitLab and "x-access-token" for GitHub. Other
// providers get no credentials.
func BasicAuth(provider, token string) *Auth {
	if token == "" {
		return nil
	}
	switch strings.ToLower(provider) {
	case "gitlab":
		return &Auth{Username: "oauth2", Token: token}
	case "github":
		return &Auth{Username: "x-access-token", Token: token}
	}
	return nil
}

// DefaultRoot is the cache directory used when none is configured.
func DefaultRoot() string { return filepath.Join(os.TempDir(), "backflow-repos") }

// Cache keeps one bare clone per repository URL under a root directory.
type Cache struct {
	root string

	mu    sync.Mutex
	locks map[string]chan struct{}
}

// EnsureWritable creates root (or DefaultRoot when empty) if needed and
// checks that files can be created in it. The manager calls it at startup so
// an unusable cache, e.g. a read-only file system, fails loudly instead of
// leaving every proposal waiting for a repository forever.
func EnsureWritable(root string) error {
	if root == "" {
		root = DefaultRoot()
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return fmt.Errorf("repository cache directory %q: %w", root, err)
	}
	f, err := os.CreateTemp(root, ".writable-*")
	if err != nil {
		return fmt.Errorf("repository cache directory %q is not writable: %w", root, err)
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}

// NewCache returns a Cache rooted at root, or at DefaultRoot when root is empty.
func NewCache(root string) *Cache {
	if root == "" {
		root = DefaultRoot()
	}
	return &Cache{root: root, locks: map[string]chan struct{}{}}
}

// Dir returns the cache directory of a repository URL.
func (c *Cache) Dir(repoURL string) string {
	sum := sha256.Sum256([]byte(strings.TrimRight(repoURL, "/")))
	return filepath.Join(c.root, hex.EncodeToString(sum[:])[:16])
}

// lock serialises access to one repository. It gives up when ctx is done.
func (c *Cache) lock(ctx context.Context, dir string) (unlock func(), err error) {
	c.mu.Lock()
	l, ok := c.locks[dir]
	if !ok {
		l = make(chan struct{}, 1)
		c.locks[dir] = l
	}
	c.mu.Unlock()
	select {
	case l <- struct{}{}:
		return func() { <-l }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Open returns a read-only view of the repository at the exact commit sha.
// The commit is fetched only when the cache does not already have it. auth
// may be nil for public repositories. The caller must Close the view.
func (c *Cache) Open(ctx context.Context, repoURL, sha string, auth *Auth) (*View, error) {
	if !fullSHA.MatchString(sha) {
		return nil, fmt.Errorf("%w: %q is not a full commit SHA", ErrRevisionNotFound, sha)
	}
	dir := c.Dir(repoURL)
	unlock, err := c.lock(ctx, dir)
	if err != nil {
		return nil, err
	}
	defer unlock()

	repo, commit, err := c.openCommit(ctx, repoURL, sha, auth)
	if err != nil {
		return nil, err
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return &View{sha: sha, tree: tree, repo: repo}, nil
}

// openCommit returns the cache repository of repoURL with commit sha present,
// fetching it only when missing. The caller must hold the repository lock.
func (c *Cache) openCommit(ctx context.Context, repoURL, sha string, auth *Auth) (*git.Repository, *object.Commit, error) {
	repo, err := openOrInit(c.Dir(repoURL), repoURL)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	hash := plumbing.NewHash(sha)
	if _, err := repo.CommitObject(hash); err != nil {
		if err := fetch(ctx, repo, sha, auth); err != nil {
			return nil, nil, err
		}
	}
	commit, err := repo.CommitObject(hash)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %s", ErrRevisionNotFound, sha)
	}
	return repo, commit, nil
}

func openOrInit(dir, repoURL string) (*git.Repository, error) {
	repo, err := git.PlainOpen(dir)
	if err == nil {
		return repo, nil
	}
	if !errors.Is(err, git.ErrRepositoryNotExists) {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	repo, err = git.PlainInit(dir, true)
	if err != nil {
		return nil, err
	}
	if _, err := repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{repoURL}}); err != nil {
		return nil, err
	}
	return repo, nil
}

// fetch brings branches and tags up to date, then asks for the commit
// itself in case it is not reachable from any of them.
func fetch(ctx context.Context, repo *git.Repository, sha string, auth *Auth) error {
	opts := fetchOptions(auth, []config.RefSpec{
		"+refs/heads/*:refs/heads/*",
		"+refs/tags/*:refs/tags/*",
	})
	err := repo.FetchContext(ctx, opts)
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return classify(err)
	}
	if _, err := repo.CommitObject(plumbing.NewHash(sha)); err == nil {
		return nil
	}
	// Not reachable from a branch or tag, for example a force-pushed-away
	// commit. Servers that allow fetching by SHA still serve it.
	opts = fetchOptions(auth, []config.RefSpec{config.RefSpec(fmt.Sprintf("%s:refs/backflow/%s", sha, sha))})
	if err := repo.FetchContext(ctx, opts); err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		// Only a refusal by the server means the commit is gone. A network
		// error says nothing about the commit and may be transient.
		if isRefusal(err) && ctx.Err() == nil {
			return fmt.Errorf("%w: %s", ErrRevisionNotFound, sha)
		}
		return classify(err)
	}
	return nil
}

// isRefusal reports whether the server answered a fetch by SHA with a
// refusal: it does not advertise the object ("not our ref", an ERR line), or
// it does not allow fetching by SHA at all.
func isRefusal(err error) bool {
	var errLine *pktline.ErrorLine
	var noMatch git.NoMatchingRefSpecError
	switch {
	case errors.Is(err, git.ErrExactSHA1NotSupported), errors.As(err, &errLine), errors.As(err, &noMatch):
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not our ref") || strings.Contains(msg, "unadvertised object") ||
		strings.Contains(msg, "no such remote ref")
}

// fetchOptions builds the go-git fetch options for the given access.
func fetchOptions(auth *Auth, refSpecs []config.RefSpec) *git.FetchOptions {
	opts := &git.FetchOptions{RemoteName: "origin", RefSpecs: refSpecs, Tags: git.NoTags}
	if auth != nil {
		if auth.Token != "" {
			opts.Auth = &http.BasicAuth{Username: auth.Username, Password: auth.Token}
		}
		opts.CABundle = auth.CABundle
	}
	return opts
}

// classify maps go-git and transport errors to this package's sentinels.
func classify(err error) error {
	switch {
	case errors.Is(err, transport.ErrAuthenticationRequired), errors.Is(err, transport.ErrAuthorizationFailed):
		return fmt.Errorf("%w: %v", ErrAuth, err)
	case errors.Is(err, transport.ErrRepositoryNotFound):
		return fmt.Errorf("%w: %v", ErrRepositoryNotFound, err)
	case errors.Is(err, transport.ErrEmptyRemoteRepository):
		return fmt.Errorf("%w: the repository is empty", ErrRevisionNotFound)
	}
	return fmt.Errorf("%w: %v", ErrUnavailable, err)
}
