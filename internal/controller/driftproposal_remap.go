package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	backflowv1alpha1 "github.com/sserkanml/backflow/api/v1alpha1"
	"github.com/sserkanml/backflow/internal/helmmap"
)

// A proposal is mapped at the revision Argo CD synced, and written to Git on
// top of the head of the target branch. When nothing the render depends on
// changed in between, the mapping is still right and rendering again (seconds
// of Argo CD's time for every candidate) would only repeat it.
//
// The result of the mapping is kept in memory, since the edited files are not
// in the status. An operator that restarted has none and maps again, which is
// always correct.

const (
	memoMax = 256
	memoTTL = time.Hour
)

// mappingMemo is a mapping result and what it was made from.
type mappingMemo struct {
	change *mappedChange
	// revision is the commit it was mapped at.
	revision string
	// fingerprint is what the mapping depends on besides the files: the
	// Application's source and the options of the policy.
	fingerprint string
	at          time.Time
}

type mappingMemos struct {
	mu sync.Mutex
	m  map[types.UID]mappingMemo
}

func (c *mappingMemos) put(uid types.UID, memo mappingMemo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[types.UID]mappingMemo{}
	}
	for k, v := range c.m {
		if time.Since(v.at) > memoTTL {
			delete(c.m, k)
		}
	}
	if len(c.m) >= memoMax {
		c.m = map[types.UID]mappingMemo{}
	}
	c.m[uid] = memo
}

func (c *mappingMemos) get(uid types.UID) (mappingMemo, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	memo, ok := c.m[uid]
	return memo, ok
}

// mappingFingerprint covers what a mapping depends on that is not a file of
// the repository: the source of the Application and the policy options that
// steer the mapping.
func mappingFingerprint(app *unstructured.Unstructured, policy *backflowv1alpha1.BackflowPolicy) string {
	source, _, _ := unstructured.NestedMap(app.Object, "spec", "source")
	delete(source, "targetRevision")
	sources, _, _ := unstructured.NestedSlice(app.Object, "spec", "sources")
	dest, _, _ := unstructured.NestedString(app.Object, "spec", "destination", "namespace")
	b, _ := json.Marshal([]any{source, sources, dest, app.GetName(), policy.Spec.Helm, policy.Spec.Mapping})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// Ways the mapping of a proposal reaches the head of the target branch.
const (
	remapReused  = "reused"
	remapMapped  = "mapped again"
	remapReasonH = "the target branch is still at the revision the proposal was mapped at"
)

// mapForHead returns the mapping of a proposal for the head of the target
// branch: the one made earlier when nothing it depends on changed, a new one
// otherwise. The log says which, and why.
func (r *DriftProposalReconciler) mapForHead(ctx context.Context, dp *backflowv1alpha1.DriftProposal,
	access *scmAccess, head string) (*mappedChange, error) {
	log := logf.FromContext(ctx)
	why, reusable := r.reusableMapping(ctx, dp, access, head)
	if reusable != nil {
		log.Info("Reusing the mapping at the head of the target branch", "proposal", dp.Name, "head", head, "why", why)
		return reusable, nil
	}
	log.Info("Mapping again at the head of the target branch", "proposal", dp.Name, "head", head, "why", why)
	return r.mapAt(ctx, dp, access, head)
}

// reusableMapping returns the kept mapping when it is still valid at head, or
// the reason it cannot be used.
func (r *DriftProposalReconciler) reusableMapping(ctx context.Context, dp *backflowv1alpha1.DriftProposal,
	access *scmAccess, head string) (string, *mappedChange) {
	memo, ok := r.memos.get(dp.UID)
	if !ok || memo.revision != dp.Spec.Source.Revision {
		return "there is no earlier result in memory (the operator restarted, or the proposal was not mapped here)", nil
	}
	app, err := r.application(ctx, dp)
	if err != nil {
		return "the Application cannot be read", nil
	}
	policy, err := r.policyOf(ctx, dp)
	if err != nil {
		return "the policy cannot be read", nil
	}
	if mappingFingerprint(app, policy) != memo.fingerprint {
		return "the Application's source or the policy's mapping options changed since the mapping", nil
	}
	if head == memo.revision {
		return remapReasonH, memo.change
	}
	url := dp.Spec.Source.RepoURL
	paths, err := r.Writer.ChangedPaths(ctx, url, memo.revision, head, access.auth)
	if err != nil {
		return "the changes between the revisions cannot be listed: " + redactText(err.Error()), nil
	}
	if len(paths) == 0 {
		return "the target branch moved, but its tree is the same", memo.change
	}
	changed, known := r.changedRelevant(ctx, dp, app, access, memo.revision, head, paths)
	switch {
	case !known:
		return "cannot tell which files the mapping depends on", nil
	case changed != "":
		return "the target branch changed " + changed + ", which the mapping depends on", nil
	}
	return "the target branch moved, but none of the " + itoa(len(paths)) + " changed file(s) is one the mapping depends on", memo.change
}

// changedRelevant returns the first changed path the mapping of the proposal
// depends on. known is false when that cannot be told. A Helm source depends on
// the files its render reads (helmmap.RelevantFiles at either revision); a
// Directory source on everything below its directory.
func (r *DriftProposalReconciler) changedRelevant(ctx context.Context, dp *backflowv1alpha1.DriftProposal,
	app *unstructured.Unstructured, access *scmAccess, from, to string, paths []string) (changed string, known bool) {
	switch dp.Spec.Source.Type {
	case backflowv1alpha1.SourceDirectory:
		dir := strings.Trim(strings.TrimPrefix(strings.TrimSpace(dp.Spec.Source.Path), "./"), "/")
		if dir == "." {
			dir = ""
		}
		for _, p := range paths {
			if dir == "" || p == dir || strings.HasPrefix(p, dir+"/") {
				return p, true
			}
		}
		return "", true
	case backflowv1alpha1.SourceHelm:
		var rel *helmmap.Relevance
		for _, sha := range []string{from, to} {
			tree, err := r.Repos.Open(ctx, dp.Spec.Source.RepoURL, sha, access.auth)
			if err != nil {
				return "", false
			}
			one, err := helmmap.RelevantFiles(tree, helmSpecAt(app, dp.Spec.Source.RepoURL, dp.Spec.Source.Path, sha))
			_ = tree.Close()
			if err != nil {
				return "", false
			}
			if rel == nil {
				rel = one
			} else {
				rel.Merge(one)
			}
		}
		for _, p := range paths {
			if rel.Matches(p) {
				return p, true
			}
		}
		return "", true
	}
	return "", false
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
