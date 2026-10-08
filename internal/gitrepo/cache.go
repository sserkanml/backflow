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

// Auth is HTTPS basic authentication with an access token.
type Auth struct {
	Username string
	Token    string
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

	repo, err := openOrInit(dir, repoURL)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	hash := plumbing.NewHash(sha)
	if _, err := repo.CommitObject(hash); err != nil {
		if err := fetch(ctx, repo, sha, auth); err != nil {
			return nil, err
		}
	}
	commit, err := repo.CommitObject(hash)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrRevisionNotFound, sha)
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return &View{sha: sha, tree: tree, repo: repo}, nil
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
	opts := &git.FetchOptions{
		RemoteName: "origin",
		RefSpecs: []config.RefSpec{
			"+refs/heads/*:refs/heads/*",
			"+refs/tags/*:refs/tags/*",
		},
		Tags: git.NoTags,
	}
	if auth != nil {
		opts.Auth = &http.BasicAuth{Username: auth.Username, Password: auth.Token}
	}
	err := repo.FetchContext(ctx, opts)
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return classify(err)
	}
	if _, err := repo.CommitObject(plumbing.NewHash(sha)); err == nil {
		return nil
	}
	// Not reachable from a branch or tag, for example a force-pushed-away
	// commit. Servers that allow fetching by SHA still serve it.
	opts.RefSpecs = []config.RefSpec{config.RefSpec(fmt.Sprintf("%s:refs/backflow/%s", sha, sha))}
	if err := repo.FetchContext(ctx, opts); err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		if errors.Is(classify(err), ErrUnavailable) && ctx.Err() == nil {
			return fmt.Errorf("%w: %s", ErrRevisionNotFound, sha)
		}
		return classify(err)
	}
	return nil
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
