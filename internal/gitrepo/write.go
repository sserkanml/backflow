package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

var (
	// ErrFileNotFound means the file to replace does not exist at the base commit.
	ErrFileNotFound = errors.New("gitrepo: file not found")
	// ErrNotRegularFile means the path is a directory, symbolic link or submodule.
	ErrNotRegularFile = errors.New("gitrepo: path is not a regular file")
	// ErrNoTimestamp means CommitMeta.When was not set. The commit time is part
	// of the commit SHA, so the caller must fix it for retries to be idempotent.
	ErrNoTimestamp = errors.New("gitrepo: commit time is required")
	// ErrNoChange means the new content equals the current content.
	ErrNoChange = errors.New("gitrepo: content is unchanged")
	// ErrBranchNotFound means the branch does not exist on the remote.
	ErrBranchNotFound = errors.New("gitrepo: branch not found")
	// ErrBranchExists means the branch to create already exists at another
	// commit. Use errors.As with *BranchExistsError to read that commit.
	ErrBranchExists = errors.New("gitrepo: branch already exists")
	// ErrNonFastForward means the remote branch moved on; fetch the new head
	// and try again from there.
	ErrNonFastForward = errors.New("gitrepo: push is not a fast-forward")
	// ErrPushRejected means the server refused the push, e.g. a protected
	// branch or a pre-receive hook.
	ErrPushRejected = errors.New("gitrepo: push rejected")
)

// BranchExistsError reports the commit an already existing branch points to.
type BranchExistsError struct {
	Branch string
	SHA    string
}

func (e *BranchExistsError) Error() string {
	return fmt.Sprintf("%v: %s is at %s", ErrBranchExists, e.Branch, e.SHA)
}
func (e *BranchExistsError) Unwrap() error { return ErrBranchExists }

// Signature identifies a person in a commit.
type Signature struct {
	Name  string
	Email string
}

// CommitMeta is who and what to record in a commit.
type CommitMeta struct {
	// Author is optional; the committer is used when nil.
	Author    *Signature
	Committer Signature
	Message   string
	// When is the author and committer time. It is required and should be
	// derived from the change (e.g. the time the drift was detected), never
	// from the clock: the same inputs then always produce the same commit
	// SHA, which makes retries after a restart find their own branch again.
	When time.Time
}

// BranchTip returns the commit a branch points to on the remote. It asks the
// remote directly, so a branch deleted there is reported as missing even if
// an earlier fetch saw it. The commit is fetched into the cache when needed.
// It returns ErrBranchNotFound when the branch does not exist.
func (c *Cache) BranchTip(ctx context.Context, repoURL, branch string, auth *Auth) (string, error) {
	unlock, err := c.lock(ctx, c.Dir(repoURL))
	if err != nil {
		return "", err
	}
	defer unlock()
	return c.branchTipLocked(ctx, repoURL, branch, auth)
}

func (c *Cache) branchTipLocked(ctx context.Context, repoURL, branch string, auth *Auth) (string, error) {
	repo, err := openOrInit(c.Dir(repoURL), repoURL)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	sha, err := remoteBranch(ctx, repo, branch, auth)
	if err != nil {
		return "", err
	}
	if _, _, err := c.openCommit(ctx, repoURL, sha, auth); err != nil {
		return "", err
	}
	return sha, nil
}

// remoteBranch lists the remote's refs and returns the tip of branch.
func remoteBranch(ctx context.Context, repo *git.Repository, branch string, auth *Auth) (string, error) {
	remote, err := repo.Remote("origin")
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	opts := &git.ListOptions{}
	if fo := fetchOptions(auth, nil); fo != nil {
		opts.Auth, opts.CABundle = fo.Auth, fo.CABundle
	}
	refs, err := remote.ListContext(ctx, opts)
	if err != nil {
		if errors.Is(classify(err), ErrRevisionNotFound) { // empty repository
			return "", fmt.Errorf("%w: %s", ErrBranchNotFound, branch)
		}
		return "", classify(err)
	}
	want := plumbing.NewBranchReferenceName(branch)
	for _, ref := range refs {
		if ref.Name() == want {
			return ref.Hash().String(), nil
		}
	}
	return "", fmt.Errorf("%w: %s", ErrBranchNotFound, branch)
}

// CommitFile creates a commit on top of baseSHA whose tree equals the base
// tree except that the regular file filePath holds content. File mode is
// kept. It writes the commit object into the cache only: no branch is moved
// and nothing is pushed. It returns the new commit SHA, which depends only on
// the arguments: the same inputs always give the same SHA.
//
// Errors: ErrFileNotFound, ErrNotRegularFile, ErrNoChange, ErrNoTimestamp,
// ErrRevisionNotFound.
func (c *Cache) CommitFile(ctx context.Context, repoURL, baseSHA string, auth *Auth, filePath string, content []byte, meta CommitMeta) (string, error) {
	if !fullSHA.MatchString(baseSHA) {
		return "", fmt.Errorf("%w: %q is not a full commit SHA", ErrRevisionNotFound, baseSHA)
	}
	if meta.When.IsZero() {
		return "", ErrNoTimestamp
	}
	filePath = path.Clean(filePath)
	if filePath == "." || strings.HasPrefix(filePath, "/") || filePath == ".." || strings.HasPrefix(filePath, "../") {
		return "", fmt.Errorf("%w: invalid path %q", ErrFileNotFound, filePath)
	}
	unlock, err := c.lock(ctx, c.Dir(repoURL))
	if err != nil {
		return "", err
	}
	defer unlock()

	repo, base, err := c.openCommit(ctx, repoURL, baseSHA, auth)
	if err != nil {
		return "", err
	}
	rootTree, err := base.Tree()
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	blob, err := storeBlob(repo, content)
	if err != nil {
		return "", err
	}
	newTree, err := replaceFile(repo, rootTree, strings.Split(filePath, "/"), blob)
	if err != nil {
		return "", err
	}
	when := meta.When
	committer := object.Signature{Name: meta.Committer.Name, Email: meta.Committer.Email, When: when}
	author := committer
	if meta.Author != nil {
		author = object.Signature{Name: meta.Author.Name, Email: meta.Author.Email, When: when}
	}
	commit := &object.Commit{
		Author: author, Committer: committer, Message: meta.Message,
		TreeHash: newTree, ParentHashes: []plumbing.Hash{base.Hash},
	}
	obj := repo.Storer.NewEncodedObject()
	if err := commit.Encode(obj); err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	hash, err := repo.Storer.SetEncodedObject(obj)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return hash.String(), nil
}

func storeBlob(repo *git.Repository, content []byte) (plumbing.Hash, error) {
	obj := repo.Storer.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	w, err := obj.Writer()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if _, err := w.Write(content); err != nil {
		_ = w.Close()
		return plumbing.ZeroHash, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if err := w.Close(); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	h, err := repo.Storer.SetEncodedObject(obj)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return h, nil
}

// replaceFile returns the hash of a copy of tree in which the file at parts
// points to blob. Siblings are shared with the original.
func replaceFile(repo *git.Repository, tree *object.Tree, parts []string, blob plumbing.Hash) (plumbing.Hash, error) {
	entries := make([]object.TreeEntry, len(tree.Entries))
	copy(entries, tree.Entries)
	idx := -1
	for i, e := range entries {
		if e.Name == parts[0] {
			idx = i
			break
		}
	}
	if idx < 0 {
		return plumbing.ZeroHash, fmt.Errorf("%w: %s", ErrFileNotFound, strings.Join(parts, "/"))
	}
	e := &entries[idx]
	if len(parts) == 1 {
		if e.Mode != filemode.Regular && e.Mode != filemode.Executable {
			return plumbing.ZeroHash, fmt.Errorf("%w: %s", ErrNotRegularFile, e.Name)
		}
		if e.Hash == blob {
			return plumbing.ZeroHash, ErrNoChange
		}
		e.Hash = blob
	} else {
		if e.Mode != filemode.Dir {
			return plumbing.ZeroHash, fmt.Errorf("%w: %s is not a directory", ErrFileNotFound, e.Name)
		}
		subTree, err := object.GetTree(repo.Storer, e.Hash)
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
		h, err := replaceFile(repo, subTree, parts[1:], blob)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		e.Hash = h
	}
	out := &object.Tree{Entries: entries}
	obj := repo.Storer.NewEncodedObject()
	if err := out.Encode(obj); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	h, err := repo.Storer.SetEncodedObject(obj)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return h, nil
}

// CreateBranch creates refs/heads/<branch> on the remote at sha, which
// CommitFile must have produced in this cache. It is idempotent: when the
// branch already holds this change it does nothing. A branch holds this change
// when it points to sha, or to a commit with the same single parent and the
// same tree (the same change committed with a different time or message).
// Any other existing branch is left untouched and reported as *BranchExistsError
// (wrapping ErrBranchExists). It never moves an existing branch.
func (c *Cache) CreateBranch(ctx context.Context, repoURL, branch, sha string, auth *Auth) error {
	unlock, err := c.lock(ctx, c.Dir(repoURL))
	if err != nil {
		return err
	}
	defer unlock()

	repo, err := c.requireCommit(repoURL, sha)
	if err != nil {
		return err
	}
	tip, err := remoteBranch(ctx, repo, branch, auth)
	switch {
	case err == nil && tip == sha:
		return nil
	case err == nil:
		same, err := c.sameChange(ctx, repo, tip, sha, auth)
		if err != nil {
			return err
		}
		if same {
			return nil
		}
		return &BranchExistsError{Branch: branch, SHA: tip}
	case !errors.Is(err, ErrBranchNotFound):
		return err
	}
	// Absent a force flag the server refuses to move a ref that appeared in
	// the meantime, so a race cannot overwrite someone else's branch.
	return push(ctx, repo, sha, branch, auth)
}

// sameChange reports whether commit tip carries the same change as commit
// want: one parent, equal to want's parent, and an equal tree. The caller
// holds the repository lock.
func (c *Cache) sameChange(ctx context.Context, repo *git.Repository, tip, want string, auth *Auth) (bool, error) {
	wantCommit, err := repo.CommitObject(plumbing.NewHash(want))
	if err != nil {
		return false, fmt.Errorf("%w: %s", ErrRevisionNotFound, want)
	}
	tipCommit, err := repo.CommitObject(plumbing.NewHash(tip))
	if err != nil {
		// The foreign tip is not in the cache yet.
		if err := fetch(ctx, repo, tip, auth); err != nil {
			return false, err
		}
		if tipCommit, err = repo.CommitObject(plumbing.NewHash(tip)); err != nil {
			return false, fmt.Errorf("%w: %s", ErrRevisionNotFound, tip)
		}
	}
	return len(tipCommit.ParentHashes) == 1 && len(wantCommit.ParentHashes) == 1 &&
		tipCommit.ParentHashes[0] == wantCommit.ParentHashes[0] &&
		tipCommit.TreeHash == wantCommit.TreeHash, nil
}

// UpdateBranch fast-forwards refs/heads/<branch> on the remote to sha, which
// must descend from the current tip. It never forces. Errors:
// ErrBranchNotFound, ErrNonFastForward (the branch moved; fetch the new head
// and rebuild the commit), ErrPushRejected (protected branch, hook), ErrAuth.
func (c *Cache) UpdateBranch(ctx context.Context, repoURL, branch, sha string, auth *Auth) error {
	unlock, err := c.lock(ctx, c.Dir(repoURL))
	if err != nil {
		return err
	}
	defer unlock()

	repo, err := c.requireCommit(repoURL, sha)
	if err != nil {
		return err
	}
	tip, err := remoteBranch(ctx, repo, branch, auth)
	if err != nil {
		return err
	}
	if tip == sha {
		return nil
	}
	return push(ctx, repo, sha, branch, auth)
}

// requireCommit opens the cache repository and checks the commit is in it.
func (c *Cache) requireCommit(repoURL, sha string) (*git.Repository, error) {
	if !fullSHA.MatchString(sha) {
		return nil, fmt.Errorf("%w: %q is not a full commit SHA", ErrRevisionNotFound, sha)
	}
	repo, err := openOrInit(c.Dir(repoURL), repoURL)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if _, err := repo.CommitObject(plumbing.NewHash(sha)); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrRevisionNotFound, sha)
	}
	return repo, nil
}

func push(ctx context.Context, repo *git.Repository, sha, branch string, auth *Auth) error {
	spec := config.RefSpec(fmt.Sprintf("%s:%s", sha, plumbing.NewBranchReferenceName(branch)))
	opts := &git.PushOptions{RemoteName: "origin", RefSpecs: []config.RefSpec{spec}}
	if fo := fetchOptions(auth, nil); fo != nil {
		opts.Auth, opts.CABundle = fo.Auth, fo.CABundle
	}
	err := repo.PushContext(ctx, opts)
	if err == nil || errors.Is(err, git.NoErrAlreadyUpToDate) {
		return nil
	}
	return classifyPush(err)
}

// classifyPush maps go-git push errors to this package's sentinels.
func classifyPush(err error) error {
	msg := strings.ToLower(err.Error())
	switch {
	case errors.Is(err, git.ErrNonFastForwardUpdate),
		strings.Contains(msg, "non-fast-forward"), strings.Contains(msg, "fetch first"):
		return fmt.Errorf("%w: %v", ErrNonFastForward, err)
	case strings.Contains(msg, "command error on"), strings.Contains(msg, "unpack error"),
		strings.Contains(msg, "protected"), strings.Contains(msg, "hook declined"),
		strings.Contains(msg, "rejected"):
		return fmt.Errorf("%w: %v", ErrPushRejected, err)
	}
	return classify(err)
}
