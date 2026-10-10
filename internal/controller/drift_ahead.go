package controller

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	backflowv1alpha1 "github.com/sserkanml/backflow/api/v1alpha1"
	"github.com/sserkanml/backflow/internal/gitrepo"
)

const (
	// aheadErrorTTL is how long a failed look at the repository is remembered
	// before it is tried again; successes are kept, because commits do not change.
	aheadErrorTTL = 30 * time.Second
	// aheadCacheMax bounds the number of remembered looks.
	aheadCacheMax = 256
)

// recheck collects the earliest time a reconcile wants to look again.
type recheck struct{ after time.Duration }

type recheckKey struct{}

// ask requests another look after d, or sooner than already asked.
func (c *recheck) ask(d time.Duration) {
	if c != nil && d > 0 && (c.after == 0 || d < c.after) {
		c.after = d
	}
}

// within returns the earlier of the requested time and limit.
func (c *recheck) within(limit time.Duration) time.Duration {
	if c.after > 0 && c.after < limit {
		return c.after
	}
	return limit
}

// GitDiffer lists the files that differ between two commits of a repository.
// *gitrepo.Cache implements it.
type GitDiffer interface {
	ChangedPaths(ctx context.Context, repoURL, from, to string, auth *gitrepo.Auth) ([]string, error)
	// HasSymlink reports whether anything at or below dir at the commit is a symbolic link.
	HasSymlink(ctx context.Context, repoURL, sha, dir string, auth *gitrepo.Auth) (bool, error)
	// HasFileWithSuffix reports whether a file at or below dir at the commit has one of the suffixes.
	HasFileWithSuffix(ctx context.Context, repoURL, sha, dir string, auth *gitrepo.Auth, suffixes ...string) (bool, error)
}

// jsonnetSuffixes name the files Argo CD evaluates as Jsonnet in a Directory source.
var jsonnetSuffixes = []string{".jsonnet", ".libsonnet"}

// aheadState describes how an Application's comparison relates to its last sync.
type aheadState struct {
	// settling is true while a sync runs, or finished after the last
	// comparison: the resource statuses then still describe the state before
	// the sync and prove nothing.
	settling bool
	// paused is true when the revision Argo CD compares against differs from
	// the revision of the last sync, or there was none.
	paused           bool
	compared, synced string // revisions, joined with commas for several sources
	// unfinished is set when the last sync cannot be taken as "Git was applied
	// up to the synced revision": it failed, or it touched only some resources.
	// The comparison of paths under the directory proves nothing then, so the
	// pause cannot be lifted by it.
	unfinished string
	// sourceChanged describes how the Application's source differs from the
	// source of its last sync. A new source can change what is rendered without
	// a new revision, and neither the revisions nor the paths say so.
	sourceChanged string
}

// hold is the reason a pause cannot be lifted by looking at the files under
// the Application's directory, or "".
func (s aheadState) hold() string {
	if s.unfinished != "" {
		return s.unfinished
	}
	return s.sourceChanged
}

// revisions reads the single revision at the path, or the list of revisions of
// a multi-source Application, joined with commas.
func revisions(app *unstructured.Unstructured, single, list []string) string {
	if rev, _, _ := unstructured.NestedString(app.Object, single...); rev != "" {
		return rev
	}
	if revs, ok, _ := unstructured.NestedStringSlice(app.Object, list...); ok && len(revs) > 0 {
		return strings.Join(revs, ",")
	}
	return ""
}

func statusTime(app *unstructured.Unstructured, path ...string) time.Time {
	s, _, _ := unstructured.NestedString(app.Object, path...)
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// gitAhead compares the revision Argo CD compares the Application against
// (status.sync.revision) with the revision of its last sync
// (status.operationState.syncResult.revision). They differ when a commit has
// not been applied yet: the sync is manual, still to come, or failing. An
// Application that was never synced counts as behind. When Argo CD reports no
// compared revision there is nothing to judge, and nothing is paused.
//
// Argo CD writes the result of a sync and the new resource statuses in
// separate updates. The Application is settling while a sync runs and until a
// comparison after it is recorded (reconciledAt reaches the sync's finishedAt).
// Its comparison can still rest on a cache of the cluster that lags behind the
// sync; the stability rule (see stable) covers that without guessing how long.
func gitAhead(app *unstructured.Unstructured) aheadState {
	st := aheadState{
		compared: revisions(app, []string{"status", "sync", "revision"}, []string{"status", "sync", "revisions"}),
		synced: revisions(app, []string{"status", "operationState", "syncResult", "revision"},
			[]string{"status", "operationState", "syncResult", "revisions"}),
	}
	st.paused = st.compared != "" && st.compared != st.synced

	st.sourceChanged = sourceChange(app)

	phase, _, _ := unstructured.NestedString(app.Object, "status", "operationState", "phase")
	if st.compared != "" && st.synced != "" { // never synced: Git being ahead is the reason given
		selective, _, _ := unstructured.NestedSlice(app.Object, "status", "operationState", "operation", "sync", "resources")
		switch {
		case phase == "Running" || phase == "Terminating":
			// Settling covers a sync that is still running.
		case phase != "Succeeded":
			if phase == "" {
				phase = "none"
			}
			st.unfinished = fmt.Sprintf("the last sync did not succeed (phase %s)", phase)
		case len(selective) > 0:
			st.unfinished = "the last sync was selective and applied only some resources"
		}
	}
	_, requested, _ := unstructured.NestedMap(app.Object, "operation")
	if requested || phase == "Running" || phase == "Terminating" {
		st.settling = true
	}
	finished := statusTime(app, "status", "operationState", "finishedAt")
	reconciled := statusTime(app, "status", "reconciledAt")
	if !finished.IsZero() && !reconciled.IsZero() && reconciled.Before(finished) {
		st.settling = true
	}
	return st
}

// sourceFields are the parts of an Application source that decide what is
// rendered from a revision.
var sourceFields = []string{"repoURL", "path", "targetRevision", "chart", "directory", "helm", "kustomize", "plugin"}

// sourcesOf reads the source of an Application at the path (a map), or its
// sources (a list). ok is false when neither is there.
func sourcesOf(app *unstructured.Unstructured, single, list []string) (out []map[string]interface{}, ok bool) {
	if m, found, _ := unstructured.NestedMap(app.Object, single...); found {
		return []map[string]interface{}{m}, true
	}
	items, found, _ := unstructured.NestedSlice(app.Object, list...)
	if !found {
		return nil, false
	}
	for _, it := range items {
		m, _ := it.(map[string]interface{})
		out = append(out, m)
	}
	return out, true
}

// normalizeSource keeps the fields that matter and drops what carries no
// meaning, so a source Argo CD wrote without a default compares equal to one
// that spells the default out: empty values, false, and a path with "./" or "/".
func normalizeSource(src map[string]interface{}) map[string]interface{} {
	out := map[string]interface{}{}
	for _, f := range sourceFields {
		v := pruneEmpty(src[f])
		if v == nil {
			continue
		}
		if s, ok := v.(string); ok && f == "path" {
			if s = strings.Trim(strings.TrimPrefix(s, "./"), "/"); s == "" || s == "." {
				continue
			}
			v = s
		}
		out[f] = v
	}
	return out
}

// pruneEmpty returns v without nil, false, empty strings, and empty maps and
// slices, or nil when nothing is left.
func pruneEmpty(v interface{}) interface{} {
	switch t := v.(type) {
	case nil:
		return nil
	case bool:
		if !t {
			return nil
		}
	case string:
		if t == "" {
			return nil
		}
	case map[string]interface{}:
		out := map[string]interface{}{}
		for k, e := range t {
			if p := pruneEmpty(e); p != nil {
				out[k] = p
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	case []interface{}:
		var out []interface{}
		for _, e := range t {
			out = append(out, pruneEmpty(e))
		}
		allNil := true
		for _, e := range out {
			allNil = allNil && e == nil
		}
		if allNil {
			return nil
		}
		return out
	}
	return v
}

// sourceChange says how the current source (spec.source or spec.sources)
// differs from the source of the last sync, or returns "". With no recorded
// sync there is nothing to compare, and the revisions speak for themselves.
func sourceChange(app *unstructured.Unstructured) string {
	spec, specOK := sourcesOf(app, []string{"spec", "source"}, []string{"spec", "sources"})
	synced, syncedOK := sourcesOf(app, []string{"status", "operationState", "syncResult", "source"},
		[]string{"status", "operationState", "syncResult", "sources"})
	if !specOK || !syncedOK {
		return ""
	}
	if len(spec) != len(synced) {
		return fmt.Sprintf("the number of sources changed since the last sync (%d now, %d then)", len(spec), len(synced))
	}
	for i := range spec {
		a, b := normalizeSource(spec[i]), normalizeSource(synced[i])
		var fields []string
		for _, f := range sourceFields {
			if !reflect.DeepEqual(a[f], b[f]) {
				fields = append(fields, f)
			}
		}
		if len(fields) > 0 {
			return fmt.Sprintf("the Application's source changed since the last sync (%s)", strings.Join(fields, ", "))
		}
	}
	return ""
}

// shortRevision abbreviates commit SHAs for messages; other revisions are kept.
func shortRevision(rev string) string {
	parts := strings.Split(rev, ",")
	for i, p := range parts {
		if len(p) == 40 {
			parts[i] = p[:7]
		}
	}
	return strings.Join(parts, ",")
}

func isFullSHA(rev string) bool {
	if len(rev) != 40 {
		return false
	}
	for _, c := range rev {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// countUnderPath counts the changed files that can change an Application
// whose Directory source is path. A path matches on directory boundaries:
// apps/demo2/x is not under apps/demo. Without recurse only files directly in
// the directory count. Anything that could matter counts; the include and
// exclude patterns of the source are ignored, since ignoring them can only
// count more.
func countUnderPath(changed []string, path string, recurse bool) int {
	dir := strings.Trim(strings.TrimPrefix(strings.TrimSpace(path), "./"), "/")
	if dir == "." {
		dir = ""
	}
	n := 0
	for _, c := range changed {
		rest := c
		if dir != "" {
			switch {
			case c == dir:
				n++ // the directory itself became a file, or the reverse
				continue
			case !strings.HasPrefix(c, dir+"/"):
				continue
			}
			rest = c[len(dir)+1:]
		}
		if !recurse && strings.Contains(rest, "/") {
			continue
		}
		n++
	}
	return n
}

// aheadKey identifies one look at the repository.
type aheadKey struct {
	repo, from, to, path string
	recurse              bool
}

type aheadCheck struct {
	changed int
	// err is a failed look, tried again after aheadErrorTTL. unchecked is the
	// reason for a look that gives no answer for good (a symbolic link or a
	// Jsonnet file under the path).
	err       error
	unchecked string
	at        time.Time
}

func (r *DriftReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// changedUnderPath tells how many files under the Application's directory
// changed between the last synced revision and the revision Argo CD compares
// against. checked is false when that cannot be told, and the pause then
// applies: only a Directory source of a single-source Application is looked
// at (Kustomize and Helm can depend on files elsewhere), both revisions must
// be commits, and the repository must be readable.
func (r *DriftReconciler) changedUnderPath(ctx context.Context, policy *backflowv1alpha1.BackflowPolicy,
	summary backflowv1alpha1.ApplicationSummary, app *unstructured.Unstructured, st aheadState) (changed int, checked bool) {
	if r.Git == nil || summary.SourceType != string(backflowv1alpha1.SourceDirectory) || summary.RepoURL == "" ||
		!isFullSHA(st.compared) || !isFullSHA(st.synced) {
		return 0, false
	}
	if sources, ok, _ := unstructured.NestedSlice(app.Object, "spec", "sources"); ok && len(sources) > 0 {
		return 0, false
	}
	// Jsonnet can read any file, so which files the Application is made of
	// cannot be told from its directory.
	if jsonnet, _, _ := unstructured.NestedMap(app.Object, "spec", "source", "directory", "jsonnet"); len(jsonnet) > 0 {
		logf.FromContext(ctx).Info("The Application's directory source uses Jsonnet; pausing drift detection while Git is ahead",
			"application", summary.Name)
		return 0, false
	}
	recurse, _, _ := unstructured.NestedBool(app.Object, "spec", "source", "directory", "recurse")
	key := aheadKey{repo: summary.RepoURL, from: st.synced, to: st.compared, path: summary.Path, recurse: recurse}

	r.aheadMu.Lock()
	hit, ok := r.aheadChecks[key]
	r.aheadMu.Unlock()
	if !ok || (hit.err != nil && r.now().Sub(hit.at) >= aheadErrorTTL) {
		var auth *gitrepo.Auth
		access, err := scmAccessFor(ctx, r.Client, policy.Namespace, policy, summary.Name)
		if err == nil && access != nil {
			auth = access.auth
		}
		if err == nil {
			hit, err = r.lookAtRepository(ctx, summary, st, recurse, auth)
		}
		if err != nil {
			hit = aheadCheck{err: err, at: r.now()}
		}
		r.aheadMu.Lock()
		if r.aheadChecks == nil || len(r.aheadChecks) >= aheadCacheMax {
			r.aheadChecks = map[aheadKey]aheadCheck{}
		}
		r.aheadChecks[key] = hit
		r.aheadMu.Unlock()
	}
	if hit.err != nil || hit.unchecked != "" {
		cause := hit.unchecked
		if hit.err != nil {
			cause = hit.err.Error()
		}
		logf.FromContext(ctx).Info("Cannot tell whether Git changed the Application's directory; pausing drift detection",
			"application", summary.Name, "cause", cause)
		return 0, false
	}
	return hit.changed, true
}

// lookAtRepository counts the changed files under the Application's directory.
// A symbolic link or a Jsonnet file below the directory at either revision
// means the directory can depend on files the comparison of paths does not
// see, so no answer is given.
func (r *DriftReconciler) lookAtRepository(ctx context.Context, summary backflowv1alpha1.ApplicationSummary,
	st aheadState, recurse bool, auth *gitrepo.Auth) (aheadCheck, error) {
	for _, sha := range []string{st.synced, st.compared} {
		linked, err := r.Git.HasSymlink(ctx, summary.RepoURL, sha, summary.Path, auth)
		if err != nil {
			return aheadCheck{}, err
		}
		if linked {
			return aheadCheck{unchecked: "a symbolic link below the directory"}, nil
		}
		jsonnet, err := r.Git.HasFileWithSuffix(ctx, summary.RepoURL, sha, summary.Path, auth, jsonnetSuffixes...)
		if err != nil {
			return aheadCheck{}, err
		}
		if jsonnet {
			return aheadCheck{unchecked: "a Jsonnet file below the directory"}, nil
		}
	}
	paths, err := r.Git.ChangedPaths(ctx, summary.RepoURL, st.synced, st.compared, auth)
	if err != nil {
		return aheadCheck{}, err
	}
	return aheadCheck{changed: countUnderPath(paths, summary.Path, recurse)}, nil
}

// detectionPaused reports whether no new proposal may be made for the
// Application now, and says why in an Event when that is Git being ahead.
func (r *DriftReconciler) detectionPaused(ctx context.Context, policy *backflowv1alpha1.BackflowPolicy,
	summary backflowv1alpha1.ApplicationSummary, app *unstructured.Unstructured) bool {
	st := gitAhead(app)
	if st.settling {
		logf.FromContext(ctx).V(1).Info("A sync is running or not yet compared; no new proposals", "application", summary.Name)
		return true
	}
	if st.hold() != "" {
		r.reportHeld(policy, summary, st)
		return true
	}
	if !st.paused {
		return false
	}
	changed, checked := r.changedUnderPath(ctx, policy, summary, app, st)
	if checked && changed == 0 {
		// A commit elsewhere in the repository: nothing the Application is made of changed.
		return false
	}
	r.reportGitAhead(policy, summary, st, changed, checked)
	return true
}

// reportHeld emits one Event per Application, revision and reason when drift
// detection is paused because the last sync did not apply the Application as it
// is now: it failed or was partial, or the source changed since.
func (r *DriftReconciler) reportHeld(policy *backflowv1alpha1.BackflowPolicy,
	summary backflowv1alpha1.ApplicationSummary, st aheadState) {
	if !r.reportOnce(policy, summary.Name, "ahead", "", "held/"+st.compared+"/"+st.hold()) {
		return
	}
	r.event(policy, corev1.EventTypeNormal, "GitAhead", fmt.Sprintf(
		"Drift detection for %s is paused: %s (revision %s). It resumes after a complete, successful sync of the current source.",
		summary.Name, st.hold(), shortRevision(st.compared)))
}

// reportKey identifies what an Event was last emitted for.
type reportKey struct {
	policy, app, kind, resource string
}

// reportOnce records the value (usually a revision) an Event of the kind was
// emitted for, per policy and Application, and per resource when given. It
// says whether the value is new, so the same Event is not repeated on every
// reconcile.
func (r *DriftReconciler) reportOnce(policy *backflowv1alpha1.BackflowPolicy, app, kind, resource, value string) bool {
	key := reportKey{policy: policy.Namespace + "/" + policy.Name, app: app, kind: kind, resource: resource}
	r.aheadMu.Lock()
	defer r.aheadMu.Unlock()
	if r.aheadReported == nil {
		r.aheadReported = map[reportKey]string{}
	}
	last, seen := r.aheadReported[key]
	r.aheadReported[key] = value
	return !seen || last != value
}

// reportGitAhead emits one Event per Application and combination of compared
// and synced revision, so a long wait does not repeat itself.
func (r *DriftReconciler) reportGitAhead(policy *backflowv1alpha1.BackflowPolicy,
	summary backflowv1alpha1.ApplicationSummary, st aheadState, changed int, checked bool) {
	if !r.reportOnce(policy, summary.Name, "ahead", "", st.compared+">"+st.synced) {
		return
	}
	synced := "never synced"
	if st.synced != "" {
		synced = shortRevision(st.synced)
	}
	revs := fmt.Sprintf("%s vs %s", shortRevision(st.compared), synced)
	if checked {
		where := summary.Path
		if strings.Trim(where, "./") == "" {
			where = "the repository root"
		}
		r.event(policy, corev1.EventTypeNormal, "GitAhead", fmt.Sprintf(
			"Git is ahead of the last sync of %s and %d file(s) under %s changed (%s); drift detection for it is paused until Argo CD syncs.",
			summary.Name, changed, where, revs))
		return
	}
	r.event(policy, corev1.EventTypeNormal, "GitAhead", fmt.Sprintf(
		"Git is ahead of the last sync of %s (%s); drift detection for it is paused until Argo CD syncs.", summary.Name, revs))
}
