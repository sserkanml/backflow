// Package scratch renders candidate edits of a multi-source Application. Argo
// CD renders such an Application from commits (each source at a revision), so
// an edit of a value file is committed to a temporary branch of the repository
// that holds it, rendered by its commit SHA, and the branches are deleted
// afterwards. No branch of the source of truth is touched.
package scratch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/sserkanml/backflow/internal/argocd"
	"github.com/sserkanml/backflow/internal/gitrepo"
	"github.com/sserkanml/backflow/internal/metrics"
	"github.com/sserkanml/backflow/internal/rendermap"
)

// BranchPrefix is the prefix of every scratch branch. The sweeper deletes
// branches with this prefix and no others.
const BranchPrefix = "backflow/scratch/"

// DefaultMaxBranches bounds the scratch branches one Renderer creates.
const DefaultMaxBranches = 40

// ErrTooManyBranches means the renders would need more scratch branches than
// allowed.
var ErrTooManyBranches = errors.New("scratch: too many scratch branches")

// Git is the part of gitrepo.Cache the renderer uses.
type Git interface {
	CommitFiles(ctx context.Context, repoURL, baseSHA string, auth *gitrepo.Auth, files map[string][]byte, meta gitrepo.CommitMeta) (string, error)
	PushBranches(ctx context.Context, repoURL string, branches map[string]string, options map[string]string, auth *gitrepo.Auth) error
	DeleteBranches(ctx context.Context, repoURL string, names []string, auth *gitrepo.Auth) error
	ListBranches(ctx context.Context, repoURL, prefix string, auth *gitrepo.Auth) ([]gitrepo.RemoteBranch, error)
}

// Argo is the part of the Argo CD client the renderer uses.
type Argo interface {
	GetManifestsByRevisions(ctx context.Context, q argocd.RevisionsQuery) (*argocd.ManifestsResponse, error)
}

// Config describes one multi-source Application and the repository to write
// scratch commits to.
type Config struct {
	Git  Git
	Argo Argo
	// Repo, Auth: the repository of the ref source that holds the value files.
	Repo string
	Auth *gitrepo.Auth
	// Parent is the commit every scratch commit is made on: the revision of that
	// source Argo CD compared against.
	Parent string
	// Base is the unmodified file set the engine renders (the value files and
	// whatever else it hands to Render). A file set that equals it is rendered at
	// Parent without a commit.
	Base map[string][]byte
	// AppName and AppNamespace name the Application.
	AppName, AppNamespace string
	// Revisions are the resolved revisions of every source, in order: a commit
	// SHA, or the chart version. Source ValuesPosition (from 1) is rendered at
	// the scratch commit instead.
	Revisions      []string
	ValuesPosition int
	// BranchPrefix is the prefix of the branches this renderer creates, within
	// BranchPrefix, such as "backflow/scratch/ab12cd34-".
	BranchPrefix string
	// PushOptions go with every push (GitLab: ci.skip); MessageSuffix ends the
	// commit message ("[skip ci]").
	PushOptions   map[string]string
	MessageSuffix string
	MaxBranches   int
	Committer     gitrepo.Signature
	Now           func() time.Time
}

// Renderer implements rendermap.Renderer and rendermap.Preparer.
type Renderer struct {
	cfg Config

	mu       sync.Mutex
	commits  map[string]string // file set key -> commit
	branches []string          // created, to delete
	pushes   int
	renders  int
}

var (
	_ rendermap.Renderer      = (*Renderer)(nil)
	_ rendermap.Preparer      = (*Renderer)(nil)
	_ rendermap.FreshRenderer = (*Renderer)(nil)
)

// New returns a Renderer. Call Finish when done, whatever happened.
func New(cfg Config) *Renderer {
	if cfg.MaxBranches <= 0 {
		cfg.MaxBranches = DefaultMaxBranches
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Renderer{cfg: cfg, commits: map[string]string{}}
}

// changed returns the files of a set that differ from the base.
func (r *Renderer) changed(files map[string][]byte) map[string][]byte {
	out := map[string][]byte{}
	for name, data := range files {
		if base, ok := r.cfg.Base[name]; !ok || string(base) != string(data) {
			out[name] = data
		}
	}
	return out
}

func setKey(changed map[string][]byte) string {
	h := sha256.New()
	for _, n := range slices.Sorted(maps.Keys(changed)) {
		_, _ = fmt.Fprintf(h, "%d:%s%d:", len(n), n, len(changed[n]))
		_, _ = h.Write(changed[n])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Prepare commits the file sets and creates their branches in one push. A set is
// remembered only once its branch is pushed, since a commit that is not on the
// remote cannot be rendered.
func (r *Renderer) Prepare(ctx context.Context, sets []map[string][]byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	type item struct {
		key     string
		changed map[string][]byte
	}
	var todo []item
	seen := map[string]bool{}
	for _, files := range sets {
		changed := r.changed(files)
		key := setKey(changed)
		if _, done := r.commits[key]; done || len(changed) == 0 || seen[key] {
			continue
		}
		seen[key] = true
		todo = append(todo, item{key, changed})
	}
	if len(r.branches)+len(todo) > r.cfg.MaxBranches {
		return fmt.Errorf("%w: %d more would pass %d", ErrTooManyBranches, len(todo), r.cfg.MaxBranches)
	}
	pending := map[string]string{} // branch -> commit
	byKey := map[string]string{}
	for i, it := range todo {
		sha, err := r.commitLocked(ctx, it.changed)
		if err != nil {
			return err
		}
		pending[r.nextBranchLocked(i)] = sha
		byKey[it.key] = sha
	}
	if err := r.pushLocked(ctx, pending); err != nil {
		return err
	}
	maps.Copy(r.commits, byKey)
	return nil
}

// nextBranchLocked names the branch of the n-th commit of a push.
func (r *Renderer) nextBranchLocked(n int) string {
	return fmt.Sprintf("%s%d", r.cfg.BranchPrefix, len(r.branches)+n+1)
}

func (r *Renderer) commitLocked(ctx context.Context, changed map[string][]byte) (string, error) {
	sha, err := r.cfg.Git.CommitFiles(ctx, r.cfg.Repo, r.cfg.Parent, r.cfg.Auth, changed, gitrepo.CommitMeta{
		Committer: r.cfg.Committer, Message: "backflow: scratch commit to verify an edit " + r.cfg.MessageSuffix + "\n",
		When: r.cfg.Now(), Create: slices.Sorted(maps.Keys(changed)),
	})
	if err != nil {
		return "", fmt.Errorf("committing the edit: %w", err)
	}
	return sha, nil
}

func (r *Renderer) pushLocked(ctx context.Context, pending map[string]string) error {
	if len(pending) == 0 {
		return nil
	}
	err := r.cfg.Git.PushBranches(ctx, r.cfg.Repo, pending, r.cfg.PushOptions, r.cfg.Auth)
	if errors.Is(err, gitrepo.ErrPushOptionsNotSupported) {
		// The commit message carries [skip ci] as well.
		err = r.cfg.Git.PushBranches(ctx, r.cfg.Repo, pending, nil, r.cfg.Auth)
	}
	if err != nil {
		metrics.ScratchBranches(metrics.ScratchPushFailed, len(pending))
		// A push can fail after it created some of the branches; the sweeper finds
		// what this does not.
		_ = r.cfg.Git.DeleteBranches(context.WithoutCancel(ctx), r.cfg.Repo, slices.Sorted(maps.Keys(pending)), r.cfg.Auth)
		return fmt.Errorf("pushing the scratch branches: %w", err)
	}
	metrics.ScratchBranches(metrics.ScratchPushed, len(pending))
	r.pushes++
	r.branches = append(r.branches, slices.Sorted(maps.Keys(pending))...)
	return nil
}

// Render implements rendermap.Renderer: the Application rendered with the
// source of the value files at a commit that holds files.
func (r *Renderer) Render(ctx context.Context, app rendermap.App, files map[string][]byte) ([]*unstructured.Unstructured, error) {
	sha, err := r.commitFor(ctx, files)
	if err != nil {
		return nil, err
	}
	return r.renderAt(ctx, app, sha, false)
}

// RenderFresh implements rendermap.FreshRenderer. Argo CD answers a render of a
// revision it rendered before from its manifest cache, also for the unmodified
// files (rendered at Parent): two renders would be one, and a resource the chart
// names at random would look stable. RenderFresh asks for a render that skips the
// cache (noCache), so no commit and no branch are needed.
func (r *Renderer) RenderFresh(ctx context.Context, app rendermap.App, files map[string][]byte) ([]*unstructured.Unstructured, error) {
	sha, err := r.commitFor(ctx, files)
	if err != nil {
		return nil, err
	}
	return r.renderAt(ctx, app, sha, true)
}

// renderAt renders the Application with the source of the value files at sha.
func (r *Renderer) renderAt(ctx context.Context, app rendermap.App, sha string, noCache bool) ([]*unstructured.Unstructured, error) {
	revisions := slices.Clone(r.cfg.Revisions)
	revisions[r.cfg.ValuesPosition-1] = sha
	positions := make([]int, len(revisions))
	for i := range positions {
		positions[i] = i + 1
	}
	start := time.Now()
	resp, err := r.cfg.Argo.GetManifestsByRevisions(ctx, argocd.RevisionsQuery{
		AppName: r.cfg.AppName, AppNamespace: r.cfg.AppNamespace, Revisions: revisions, SourcePositions: positions,
		NoCache: noCache,
	})
	metrics.Render(metrics.RendererRevisions, err, errors.Is(err, argocd.ErrRenderFailed), time.Since(start))
	r.mu.Lock()
	r.renders++
	r.mu.Unlock()
	switch {
	case errors.Is(err, argocd.ErrRenderFailed):
		return nil, fmt.Errorf("%w: %w", rendermap.ErrRenderFailed, err)
	case err != nil:
		return nil, err
	}
	objs, err := rendermap.ParseManifests(app, resp.Manifests)
	if err != nil {
		return nil, err
	}
	return objs, nil
}

// commitFor returns the commit to render for a file set: Parent for the
// unmodified files, otherwise a scratch commit, pushed now when Prepare did not.
func (r *Renderer) commitFor(ctx context.Context, files map[string][]byte) (string, error) {
	changed := r.changed(files)
	if len(changed) == 0 {
		return r.cfg.Parent, nil
	}
	key := setKey(changed)
	r.mu.Lock()
	defer r.mu.Unlock()
	if sha, ok := r.commits[key]; ok {
		return sha, nil
	}
	if len(r.branches)+1 > r.cfg.MaxBranches {
		return "", fmt.Errorf("%w: more than %d", ErrTooManyBranches, r.cfg.MaxBranches)
	}
	sha, err := r.commitLocked(ctx, changed)
	if err != nil {
		return "", err
	}
	if err := r.pushLocked(ctx, map[string]string{r.nextBranchLocked(0): sha}); err != nil {
		return "", err
	}
	r.commits[key] = sha
	return sha, nil
}

// Stats says how many scratch branches were pushed, in how many pushes, and how
// many renders were asked of Argo CD.
func (r *Renderer) Stats() (branches, pushes, renders int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.branches), r.pushes, r.renders
}

// Finish deletes the scratch branches, in one push. It is safe to call more
// than once, and must be called whatever happened, with a context that outlives
// the one of the verification.
func (r *Renderer) Finish(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.branches) == 0 {
		return nil
	}
	if err := r.cfg.Git.DeleteBranches(ctx, r.cfg.Repo, r.branches, r.cfg.Auth); err != nil {
		return fmt.Errorf("deleting the scratch branches: %w", err)
	}
	metrics.ScratchBranches(metrics.ScratchDeleted, len(r.branches))
	r.branches = nil
	return nil
}

// Sweep deletes the scratch branches of the repository whose tip is older than
// maxAge, for the ones a crashed verification left behind. Only branches with
// BranchPrefix are looked at.
func Sweep(ctx context.Context, git Git, repo string, auth *gitrepo.Auth, maxAge time.Duration, now time.Time) (deleted []string, err error) {
	branches, err := git.ListBranches(ctx, repo, BranchPrefix, auth)
	if err != nil {
		return nil, err
	}
	for _, b := range branches {
		if now.Sub(b.When) > maxAge {
			deleted = append(deleted, b.Name)
		}
	}
	if err := git.DeleteBranches(ctx, repo, deleted, auth); err != nil {
		return nil, err
	}
	metrics.ScratchBranches(metrics.ScratchSwept, len(deleted))
	return deleted, nil
}

// SweepPrefix deletes every branch of the repository that starts with prefix,
// which must be within BranchPrefix: the branches of one proposal that an
// earlier attempt left behind.
func SweepPrefix(ctx context.Context, git Git, repo string, auth *gitrepo.Auth, prefix string) (deleted []string, err error) {
	if !strings.HasPrefix(prefix, BranchPrefix) || len(prefix) <= len(BranchPrefix) {
		return nil, fmt.Errorf("scratch: %q is not a prefix within %s", prefix, BranchPrefix)
	}
	branches, err := git.ListBranches(ctx, repo, prefix, auth)
	if err != nil {
		return nil, err
	}
	for _, b := range branches {
		deleted = append(deleted, b.Name)
	}
	if err := git.DeleteBranches(ctx, repo, deleted, auth); err != nil {
		return nil, err
	}
	metrics.ScratchBranches(metrics.ScratchSwept, len(deleted))
	return deleted, nil
}
