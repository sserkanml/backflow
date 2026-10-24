package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
)

// ErrPushOptionsNotSupported means the remote does not take push options.
var ErrPushOptionsNotSupported = errors.New("gitrepo: the remote does not support push options")

// PushBranches creates several branches on the remote in one push: each name
// maps to a commit that is in the cache (see CommitFiles). options are push
// options (GitLab: ci.skip); a remote that does not accept them fails with
// ErrPushOptionsNotSupported. A branch that exists on the remote is refused and
// nothing is created: it never moves an existing branch. Errors: ErrAuth,
// ErrPushRejected, ErrRevisionNotFound, ErrBranchExists.
func (c *Cache) PushBranches(ctx context.Context, repoURL string, branches map[string]string, options map[string]string, auth *Auth) error {
	if len(branches) == 0 {
		return nil
	}
	unlock, err := c.lock(ctx, c.Dir(repoURL))
	if err != nil {
		return err
	}
	defer unlock()

	var repo *git.Repository
	specs := make([]config.RefSpec, 0, len(branches))
	for _, name := range slices.Sorted(maps.Keys(branches)) {
		sha := branches[name]
		if repo, err = c.requireCommit(repoURL, sha); err != nil {
			return err
		}
		specs = append(specs, config.RefSpec(fmt.Sprintf("%s:%s", sha, plumbing.NewBranchReferenceName(name))))
	}
	refs, err := listRemote(ctx, repo, auth)
	if err != nil {
		return err
	}
	for _, ref := range refs {
		for name, sha := range branches {
			if ref.Name() == plumbing.NewBranchReferenceName(name) {
				return &BranchExistsError{Branch: name, SHA: ref.Hash().String() + " (wanted " + sha + ")"}
			}
		}
	}
	return pushSpecs(ctx, repo, specs, options, auth)
}

// DeleteBranches deletes branches of the remote in one push. A branch that is
// not there is skipped. Errors: ErrAuth, ErrPushRejected.
func (c *Cache) DeleteBranches(ctx context.Context, repoURL string, names []string, auth *Auth) error {
	if len(names) == 0 {
		return nil
	}
	unlock, err := c.lock(ctx, c.Dir(repoURL))
	if err != nil {
		return err
	}
	defer unlock()

	repo, err := openOrInit(c.Dir(repoURL), repoURL)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	refs, err := listRemote(ctx, repo, auth)
	if err != nil {
		return err
	}
	var specs []config.RefSpec
	for _, ref := range refs {
		for _, name := range names {
			if ref.Name() == plumbing.NewBranchReferenceName(name) {
				specs = append(specs, config.RefSpec(":"+ref.Name().String()))
			}
		}
	}
	if len(specs) == 0 {
		return nil
	}
	return pushSpecs(ctx, repo, specs, nil, auth)
}

// RemoteBranch is a branch of the remote with the time of its tip commit.
type RemoteBranch struct {
	Name string
	SHA  string
	When time.Time
}

// ListBranches returns the branches of the remote whose name starts with
// prefix, with the committer time of each tip. Only that prefix is looked at.
func (c *Cache) ListBranches(ctx context.Context, repoURL, prefix string, auth *Auth) ([]RemoteBranch, error) {
	if prefix == "" {
		return nil, errors.New("gitrepo: a prefix is required")
	}
	unlock, err := c.lock(ctx, c.Dir(repoURL))
	if err != nil {
		return nil, err
	}
	defer unlock()

	repo, err := openOrInit(c.Dir(repoURL), repoURL)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	refs, err := listRemote(ctx, repo, auth)
	if err != nil {
		return nil, err
	}
	var out []RemoteBranch
	for _, ref := range refs {
		if !ref.Name().IsBranch() || !strings.HasPrefix(ref.Name().Short(), prefix) {
			continue
		}
		sha := ref.Hash().String()
		if _, err := repo.CommitObject(ref.Hash()); err != nil {
			if err := fetch(ctx, repo, sha, auth); err != nil {
				return nil, err
			}
		}
		commit, err := repo.CommitObject(ref.Hash())
		if err != nil {
			return nil, fmt.Errorf("%w: %s", ErrRevisionNotFound, sha)
		}
		out = append(out, RemoteBranch{Name: ref.Name().Short(), SHA: sha, When: commit.Committer.When})
	}
	slices.SortFunc(out, func(a, b RemoteBranch) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

func pushSpecs(ctx context.Context, repo *git.Repository, specs []config.RefSpec, options map[string]string, auth *Auth) error {
	opts := &git.PushOptions{RemoteName: remoteName, RefSpecs: specs, Options: options}
	if fo := fetchOptions(auth, nil); fo != nil {
		opts.Auth, opts.CABundle = fo.Auth, fo.CABundle
	}
	err := repo.PushContext(ctx, opts)
	if err == nil || errors.Is(err, git.NoErrAlreadyUpToDate) {
		return nil
	}
	if strings.Contains(strings.ToLower(err.Error()), "push-options") || strings.Contains(strings.ToLower(err.Error()), "push options") {
		return fmt.Errorf("%w: %v", ErrPushOptionsNotSupported, err)
	}
	return classifyPush(err)
}
