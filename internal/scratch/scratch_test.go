package scratch

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sserkanml/backflow/internal/argocd"
	"github.com/sserkanml/backflow/internal/gitrepo"
	"github.com/sserkanml/backflow/internal/metrics/metricstest"
	"github.com/sserkanml/backflow/internal/rendermap"
)

const parent = "1111111111111111111111111111111111111111"

type fakeGit struct {
	commits  map[string]map[string][]byte // sha -> files
	messages []string
	pushes   []map[string]string
	options  []map[string]string
	deleted  [][]string
	branches []gitrepo.RemoteBranch
	pushErr  error
	// optionsErr is returned by a push that has options.
	optionsErr error
}

func (g *fakeGit) CommitFiles(_ context.Context, _, base string, _ *gitrepo.Auth, files map[string][]byte, meta gitrepo.CommitMeta) (string, error) {
	if base != parent {
		return "", fmt.Errorf("the parent is %s", base)
	}
	if g.commits == nil {
		g.commits = map[string]map[string][]byte{}
	}
	sha := fmt.Sprintf("%040d", len(g.commits)+2)
	g.commits[sha] = files
	g.messages = append(g.messages, meta.Message)
	return sha, nil
}

func (g *fakeGit) PushBranches(_ context.Context, _ string, branches map[string]string, options map[string]string, _ *gitrepo.Auth) error {
	if g.pushErr != nil {
		return g.pushErr
	}
	if len(options) > 0 && g.optionsErr != nil {
		return g.optionsErr
	}
	g.pushes = append(g.pushes, maps.Clone(branches))
	g.options = append(g.options, options)
	return nil
}

func (g *fakeGit) DeleteBranches(_ context.Context, _ string, names []string, _ *gitrepo.Auth) error {
	g.deleted = append(g.deleted, slices.Clone(names))
	return nil
}

func (g *fakeGit) ListBranches(context.Context, string, string, *gitrepo.Auth) ([]gitrepo.RemoteBranch, error) {
	return g.branches, nil
}

type fakeArgo struct {
	queries   []argocd.RevisionsQuery
	err       error
	manifests []string
}

func (a *fakeArgo) GetManifestsByRevisions(_ context.Context, q argocd.RevisionsQuery) (*argocd.ManifestsResponse, error) {
	a.queries = append(a.queries, q)
	if a.err != nil {
		return nil, a.err
	}
	m := a.manifests
	if m == nil {
		m = []string{`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cm"},"data":{"k":"v"}}`}
	}
	return &argocd.ManifestsResponse{Manifests: m}, nil
}

var base = map[string][]byte{"env/values.yaml": []byte("a: 1\n"), "env/other.yaml": []byte("b: 1\n"), "chart:x/values.yaml": []byte("c: 1\n")}

func newRenderer(g *fakeGit, a *fakeArgo, mutate func(*Config)) *Renderer {
	cfg := Config{
		Git: g, Argo: a, Repo: "https://gitlab.com/x/values.git", Parent: parent, Base: base,
		AppName: "ms", AppNamespace: "argocd", Revisions: []string{"6.7.1", parent}, ValuesPosition: 2,
		BranchPrefix: BranchPrefix + "ab12cd34-", PushOptions: map[string]string{"ci.skip": ""}, MessageSuffix: "[skip ci]",
		Committer: gitrepo.Signature{Name: "Backflow", Email: "b@x"}, Now: func() time.Time { return time.Unix(1_700_000_000, 0) },
	}
	if mutate != nil {
		mutate(&cfg)
	}
	return New(cfg)
}

func edited(extra map[string]string) map[string][]byte {
	out := maps.Clone(base)
	for k, v := range extra {
		out[k] = []byte(v)
	}
	return out
}

func TestUnmodifiedFilesRenderAtTheParentWithoutACommit(t *testing.T) {
	g, a := &fakeGit{}, &fakeArgo{}
	r := newRenderer(g, a, nil)
	objs, err := r.Render(t.Context(), rendermap.App{DestNamespace: "demo"}, maps.Clone(base))
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 1 || objs[0].GetNamespace() != "demo" {
		t.Errorf("objects = %v", objs)
	}
	if len(g.commits) != 0 || len(g.pushes) != 0 {
		t.Errorf("a commit or a push was made: %d, %d", len(g.commits), len(g.pushes))
	}
	q := a.queries[0]
	if !slices.Equal(q.Revisions, []string{"6.7.1", parent}) || !slices.Equal(q.SourcePositions, []int{1, 2}) ||
		q.AppName != "ms" || q.AppNamespace != "argocd" {
		t.Errorf("query = %+v: every source must be pinned", q)
	}
}

func TestPrepareCreatesAllBranchesInOnePush(t *testing.T) {
	g, a := &fakeGit{}, &fakeArgo{}
	r := newRenderer(g, a, nil)
	sets := []map[string][]byte{
		edited(map[string]string{"env/values.yaml": "a: 2\n"}),
		edited(map[string]string{"env/values.yaml": "a: 3\n"}),
		edited(map[string]string{"env/other.yaml": "b: 2\n"}),
		maps.Clone(base), // nothing to commit
		edited(map[string]string{"env/values.yaml": "a: 2\n"}), // the same as the first
	}
	if err := r.Prepare(t.Context(), sets); err != nil {
		t.Fatal(err)
	}
	if len(g.pushes) != 1 || len(g.pushes[0]) != 3 {
		t.Fatalf("pushes = %v, want one push of three branches", g.pushes)
	}
	for name := range g.pushes[0] {
		if !strings.HasPrefix(name, BranchPrefix+"ab12cd34-") {
			t.Errorf("branch %q is outside the prefix", name)
		}
	}
	if g.options[0]["ci.skip"] != "" || len(g.options[0]) != 1 {
		t.Errorf("push options = %v", g.options[0])
	}
	for _, m := range g.messages {
		if !strings.Contains(m, "[skip ci]") {
			t.Errorf("commit message %q lacks [skip ci]", m)
		}
	}
	if len(g.commits) != 3 {
		t.Errorf("commits = %d, want 3 (the unmodified set and the repeat make none)", len(g.commits))
	}
	for sha, files := range g.commits {
		for name := range files {
			if name == "chart:x/values.yaml" {
				t.Errorf("commit %s has the file %q that is not a file of the repository", sha, name)
			}
		}
		if len(files) != 1 {
			t.Errorf("commit %s holds %d files, want only the edited one", sha, len(files))
		}
	}

	// Rendering the prepared sets pushes nothing more, and renders the commit.
	for _, s := range sets[:3] {
		if _, err := r.Render(t.Context(), rendermap.App{}, s); err != nil {
			t.Fatal(err)
		}
	}
	if len(g.pushes) != 1 {
		t.Errorf("pushes = %d after the renders", len(g.pushes))
	}
	seen := map[string]bool{}
	for _, q := range a.queries {
		if q.Revisions[0] != "6.7.1" || q.Revisions[1] == parent {
			t.Errorf("revisions = %v", q.Revisions)
		}
		seen[q.Revisions[1]] = true
	}
	if len(seen) != 3 {
		t.Errorf("the renders used %d commits, want 3", len(seen))
	}
	if b, p, n := r.Stats(); b != 3 || p != 1 || n != 3 {
		t.Errorf("stats = %d, %d, %d", b, p, n)
	}
}

func TestRenderPushesAFileSetThatWasNotPrepared(t *testing.T) {
	g, a := &fakeGit{}, &fakeArgo{}
	r := newRenderer(g, a, nil)
	set := edited(map[string]string{"env/values.yaml": "a: 9\n"})
	for range 2 {
		if _, err := r.Render(t.Context(), rendermap.App{}, set); err != nil {
			t.Fatal(err)
		}
	}
	if len(g.pushes) != 1 || len(g.pushes[0]) != 1 || len(g.commits) != 1 {
		t.Errorf("pushes = %v, commits = %d: the same set is committed once", g.pushes, len(g.commits))
	}
}

func TestFinishDeletesEveryBranchOnce(t *testing.T) {
	g, a := &fakeGit{}, &fakeArgo{}
	r := newRenderer(g, a, nil)
	if err := r.Finish(t.Context()); err != nil || len(g.deleted) != 0 {
		t.Fatalf("nothing was pushed, nothing to delete: %v, %v", err, g.deleted)
	}
	if err := r.Prepare(t.Context(), []map[string][]byte{
		edited(map[string]string{"env/values.yaml": "a: 2\n"}), edited(map[string]string{"env/values.yaml": "a: 3\n"}),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Render(t.Context(), rendermap.App{}, edited(map[string]string{"env/other.yaml": "b: 7\n"})); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(g.deleted) != 1 || len(g.deleted[0]) != 3 {
		t.Fatalf("deleted = %v, want one push deleting three branches", g.deleted)
	}
	if err := r.Finish(t.Context()); err != nil || len(g.deleted) != 1 {
		t.Errorf("a second Finish deleted again: %v", g.deleted)
	}
}

func TestBranchBound(t *testing.T) {
	g, a := &fakeGit{}, &fakeArgo{}
	r := newRenderer(g, a, func(c *Config) { c.MaxBranches = 2 })
	sets := make([]map[string][]byte, 0, 3)
	for i := range 3 {
		sets = append(sets, edited(map[string]string{"env/values.yaml": fmt.Sprintf("a: %d\n", i+10)}))
	}
	if err := r.Prepare(t.Context(), sets); !errors.Is(err, ErrTooManyBranches) {
		t.Fatalf("err = %v", err)
	}
	if len(g.pushes) != 0 {
		t.Errorf("a push was made past the bound")
	}
	if err := r.Prepare(t.Context(), sets[:2]); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Render(t.Context(), rendermap.App{}, sets[2]); !errors.Is(err, ErrTooManyBranches) {
		t.Errorf("render past the bound: %v", err)
	}
}

func TestRenderErrors(t *testing.T) {
	g := &fakeGit{}
	set := edited(map[string]string{"env/values.yaml": "a: 2\n"})
	a := &fakeArgo{err: fmt.Errorf("%w: gRPC status 2: template failed", argocd.ErrRenderFailed)}
	if _, err := newRenderer(g, a, nil).Render(t.Context(), rendermap.App{}, set); !errors.Is(err, rendermap.ErrRenderFailed) {
		t.Errorf("a failed render: %v", err)
	}
	a = &fakeArgo{err: argocd.ErrUnsupported}
	if _, err := newRenderer(g, a, nil).Render(t.Context(), rendermap.App{}, set); !errors.Is(err, argocd.ErrUnsupported) || errors.Is(err, rendermap.ErrRenderFailed) {
		t.Errorf("an Argo CD without revisions: %v", err)
	}
	g = &fakeGit{pushErr: gitrepo.ErrPushRejected}
	if _, err := newRenderer(g, &fakeArgo{}, nil).Render(t.Context(), rendermap.App{}, set); !errors.Is(err, gitrepo.ErrPushRejected) {
		t.Errorf("a rejected push: %v", err)
	}
	if len(g.deleted) != 1 {
		t.Errorf("a failed push leaves nothing behind: deleted = %v", g.deleted)
	}
}

func TestSweepDeletesOnlyOldScratchBranches(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	g := &fakeGit{branches: []gitrepo.RemoteBranch{
		{Name: BranchPrefix + "old-1", When: now.Add(-2 * time.Hour)},
		{Name: BranchPrefix + "new-1", When: now.Add(-10 * time.Minute)},
		{Name: BranchPrefix + "old-2", When: now.Add(-61 * time.Minute)},
	}}
	deleted, err := Sweep(t.Context(), g, "https://x/y.git", nil, time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{BranchPrefix + "old-1", BranchPrefix + "old-2"}
	if !slices.Equal(deleted, want) || len(g.deleted) != 1 || !slices.Equal(g.deleted[0], want) {
		t.Errorf("deleted = %v (%v)", deleted, g.deleted)
	}
}

func TestRenderFreshAsksArgoCDNotToUseItsCache(t *testing.T) {
	g, a := &fakeGit{}, &fakeArgo{}
	r := newRenderer(g, a, nil)
	if _, err := r.Render(t.Context(), rendermap.App{DestNamespace: "demo"}, maps.Clone(base)); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := r.RenderFresh(t.Context(), rendermap.App{DestNamespace: "demo"}, maps.Clone(base)); err != nil {
			t.Fatal(err)
		}
	}
	if len(a.queries) != 3 {
		t.Fatalf("queries = %d, want 3", len(a.queries))
	}
	if a.queries[0].NoCache {
		t.Error("an ordinary render must use the cache")
	}
	for _, q := range a.queries[1:] {
		if !q.NoCache || !slices.Equal(q.Revisions, []string{"6.7.1", parent}) {
			t.Errorf("query = %+v: the unmodified files are rendered at the parent, with noCache", q)
		}
	}
	if len(g.commits) != 0 || len(g.pushes) != 0 {
		t.Errorf("commits = %d, pushes = %d: a fresh render of the unmodified files needs no scratch branch", len(g.commits), len(g.pushes))
	}
	if branches, pushes, renders := r.Stats(); branches != 0 || pushes != 0 || renders != 3 {
		t.Errorf("branches = %d, pushes = %d, renders = %d", branches, pushes, renders)
	}
	// Edited files still go to a scratch commit, with noCache as well.
	if _, err := r.RenderFresh(t.Context(), rendermap.App{}, edited(map[string]string{"env/values.yaml": "a: 2\n"})); err != nil {
		t.Fatal(err)
	}
	last := a.queries[len(a.queries)-1]
	if !last.NoCache || last.Revisions[1] == parent || len(g.commits) != 1 {
		t.Errorf("query = %+v, commits = %d", last, len(g.commits))
	}
}

func TestScratchMetrics(t *testing.T) {
	branches := func(action string) float64 {
		return metricstest.Value("backflow_scratch_branches_total", map[string]string{"action": action})
	}
	renders := func(result string) float64 {
		return metricstest.Value("backflow_renders_total", map[string]string{"renderer": "revisions", "result": result})
	}
	pushed, deleted, swept, pushFailed := branches("pushed"), branches("deleted"), branches("swept"), branches("push_failed")
	ok, failed := renders("ok"), renders("failed")

	g, a := &fakeGit{}, &fakeArgo{}
	r := newRenderer(g, a, nil)
	if err := r.Prepare(t.Context(), []map[string][]byte{
		edited(map[string]string{"env/values.yaml": "a: 2\n"}), edited(map[string]string{"env/values.yaml": "a: 3\n"}),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Render(t.Context(), rendermap.App{}, edited(map[string]string{"env/values.yaml": "a: 2\n"})); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := branches("pushed") - pushed; got != 2 {
		t.Errorf("pushed = %v, want 2", got)
	}
	if got := branches("deleted") - deleted; got != 2 {
		t.Errorf("deleted = %v, want 2", got)
	}
	if got := renders("ok") - ok; got != 1 {
		t.Errorf("renders ok = %v, want 1", got)
	}

	set := edited(map[string]string{"env/values.yaml": "a: 9\n"})
	failing := newRenderer(&fakeGit{}, &fakeArgo{err: fmt.Errorf("%w: x", argocd.ErrRenderFailed)}, nil)
	_, _ = failing.Render(t.Context(), rendermap.App{}, set)
	if got := renders("failed") - failed; got != 1 {
		t.Errorf("renders failed = %v, want 1", got)
	}

	_, _ = newRenderer(&fakeGit{pushErr: gitrepo.ErrPushRejected}, &fakeArgo{}, nil).Render(t.Context(), rendermap.App{}, set)
	if got := branches("push_failed") - pushFailed; got != 1 {
		t.Errorf("push_failed = %v, want 1", got)
	}

	now := time.Unix(1_700_000_000, 0)
	old := &fakeGit{branches: []gitrepo.RemoteBranch{{Name: BranchPrefix + "old-1", When: now.Add(-2 * time.Hour)}, {Name: BranchPrefix + "new-1", When: now}}}
	if _, err := Sweep(t.Context(), old, "https://x/y.git", nil, time.Hour, now); err != nil {
		t.Fatal(err)
	}
	if got := branches("swept") - swept; got != 1 {
		t.Errorf("swept = %v, want 1", got)
	}
}
