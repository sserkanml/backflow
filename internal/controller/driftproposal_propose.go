package controller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	backflowv1alpha1 "github.com/sserkanml/backflow/api/v1alpha1"
	"github.com/sserkanml/backflow/internal/gitrepo"
	"github.com/sserkanml/backflow/internal/helmmap"
	"github.com/sserkanml/backflow/internal/scm"
)

const (
	conditionProposed = "Proposed"

	defaultBranchPrefix = "backflow/"
	// directCommitAttempts is the first try plus one retry from the new head.
	directCommitAttempts = 2
	// defaultCommitScanLimit bounds the first-parent history searched for a
	// commit of the proposal's own, from the head of the target branch back to
	// the revision the proposal was made at.
	defaultCommitScanLimit = 500
)

// Reasons of the Proposed condition. A False condition on a proposal that is
// still in Mapping is retried; the reasons that end in Failed or Superseded
// are final.
const (
	reasonReportOnly = "ReportOnly"
	// reasonProposing is the Proposed condition while the change is mapped again
	// at the target branch, committed and pushed. These steps talk to Argo CD
	// and the Git host and can take minutes; the condition says what the
	// proposal is doing until it has a merge request or a reason to wait.
	reasonProposing            = "Proposing"
	reasonMissingScmConnection = "MissingScmConnection"
	reasonPolicyNotFound       = "PolicyNotFound"
	reasonScmAuthFailed        = "ScmAuthFailed"
	reasonScmForbidden         = "ScmForbidden"
	reasonScmProjectNotFound   = "ScmProjectNotFound"
	reasonScmUnavailable       = "ScmUnavailable"
	reasonMergeRequestRejected = "MergeRequestRejected"
	reasonMergeRequestOpened   = "MergeRequestOpened"
	reasonMergeRequestClosed   = "MergeRequestClosed"
	reasonMergeRequestMerged   = "MergeRequestMerged"
	reasonLiveReverted         = "LiveReverted"
	reasonSuperseded           = "SupersededByNewerDrift"
	reasonCommitted            = "Committed"
	reasonTargetNotABranch     = "TargetNotABranch"
	reasonBranchConflict       = "BranchConflict"
	reasonPushRejected         = "PushRejected"
	reasonNonFastForward       = "NonFastForward"
	reasonSourceChanged        = "SourceChanged"
	reasonHistoryTooLong       = "HistoryTooLong"
	reasonUnsupportedProvider  = "UnsupportedProvider"
	reasonInvalidRepositoryURL = "InvalidRepositoryURL"
)

const (
	// maxPushRejections is how many times the Git server may decline the push
	// of a proposal before it fails. The decline of a pre-receive hook is the
	// same error for a protected branch, which no retry fixes, and for a Git
	// host that is having an outage, which one does. Go-git does not pass on the
	// remote's explanation, so the two cannot be told apart, and every decline is
	// treated alike: tried again after pushRetryDelays, in all about ten minutes.
	maxPushRejections = 3
)

// pushRetryDelays are the waits after the first and the second decline.
var pushRetryDelays = []time.Duration{3 * time.Minute, 7 * time.Minute}

const sourceChangedMessage = "The source changed in Git after the drift was detected."

// RepositoryWriter is the Git side of proposing a change. *gitrepo.Cache
// implements it.
type RepositoryWriter interface {
	BranchTip(ctx context.Context, repoURL, branch string, auth *gitrepo.Auth) (string, error)
	DefaultBranch(ctx context.Context, repoURL string, auth *gitrepo.Auth) (string, error)
	CommitInfo(ctx context.Context, repoURL, sha string, auth *gitrepo.Auth) (*gitrepo.CommitInfo, error)
	CommitFiles(ctx context.Context, repoURL, baseSHA string, auth *gitrepo.Auth, files map[string][]byte, commit gitrepo.CommitMeta) (string, error)
	CreateBranch(ctx context.Context, repoURL, branch, sha string, auth *gitrepo.Auth) error
	IsAncestor(ctx context.Context, repoURL, ancestor, descendant string, auth *gitrepo.Auth) (bool, error)
	ChangedPaths(ctx context.Context, repoURL, from, to string, auth *gitrepo.Auth) ([]string, error)
	UpdateBranch(ctx context.Context, repoURL, branch, sha string, auth *gitrepo.Auth) error
	FirstParentLog(ctx context.Context, repoURL, tip, stop string, limit int, auth *gitrepo.Auth) ([]gitrepo.LogEntry, bool, error)
}

// scmAccess is everything needed to talk to the Git host of a repository.
type scmAccess struct {
	conn  *backflowv1alpha1.ScmConnection
	token string
	ca    []byte
	// auth is the HTTPS credential for Git operations.
	auth *gitrepo.Auth
}

func (r *DriftProposalReconciler) newProvider(cfg scm.Config) (scm.Provider, error) {
	if r.NewProvider != nil {
		return r.NewProvider(cfg)
	}
	return scm.New(cfg)
}

// proposalOutcome is how one attempt to write to Git ended.
type proposalOutcome struct {
	result ctrl.Result
	err    error
}

// propose writes a mapped proposal to Git according to the policy's mode.
// Reconcile calls it for proposals that are in Mapping with Mapped=True.
func (r *DriftProposalReconciler) propose(ctx context.Context, dp *backflowv1alpha1.DriftProposal) (ctrl.Result, error) {
	var policy backflowv1alpha1.BackflowPolicy
	if err := r.Get(ctx, types.NamespacedName{Namespace: dp.Namespace, Name: dp.Spec.PolicyName}, &policy); err != nil {
		if apierrors.IsNotFound(err) {
			return r.holdProposal(ctx, dp, reasonPolicyNotFound, fmt.Errorf("BackflowPolicy %q not found", dp.Spec.PolicyName), 0)
		}
		return ctrl.Result{}, err
	}
	mode := policy.Spec.Mode
	if mode == "" {
		mode = backflowv1alpha1.ModeMergeRequest
	}
	if mode == backflowv1alpha1.ModeReportOnly {
		return ctrl.Result{}, r.setProposedCondition(ctx, dp, metav1.ConditionFalse, reasonReportOnly,
			"The policy mode is ReportOnly; nothing is written to Git.")
	}

	if wait := r.pushBackoff(dp); wait > 0 {
		logf.FromContext(ctx).V(1).Info("The Git server declined the last push; waiting before the next one",
			"proposal", dp.Name, "declines", dp.Status.PushRejections, "retryIn", wait.String())
		return ctrl.Result{RequeueAfter: wait}, nil
	}

	access, err := r.accessFor(ctx, dp, &policy)
	if err != nil {
		if errors.Is(err, gitrepo.ErrAuth) {
			return r.holdProposal(ctx, dp, reasonScmAuthFailed, err, 0)
		}
		return ctrl.Result{}, err
	}
	if access == nil {
		host := "the repository host"
		if h, _, perr := scm.ParseRepoURL(dp.Spec.Source.RepoURL); perr == nil {
			host = h
		}
		return r.holdProposal(ctx, dp, reasonMissingScmConnection,
			fmt.Errorf("no ScmConnection matches %s; create one so Backflow can write to Git", host), 0)
	}

	_, project, err := scm.ParseRepoURL(dp.Spec.Source.RepoURL)
	if err != nil {
		return r.fail(ctx, dp, reasonInvalidRepositoryURL, err.Error())
	}

	target, out := r.targetBranch(ctx, dp, &policy, access)
	if out != nil {
		return out.result, out.err
	}

	if meta.FindStatusCondition(dp.Status.Conditions, conditionProposed) == nil {
		if err := r.setProposedCondition(ctx, dp, metav1.ConditionFalse, reasonProposing,
			"Mapping the change again at the head of "+target+", committing it and pushing a branch."); err != nil {
			return ctrl.Result{}, err
		}
		logf.FromContext(ctx).Info("Proposing the change", "proposal", dp.Name, "mode", mode, "targetBranch", target)
	}

	switch mode {
	case backflowv1alpha1.ModeDirectCommit:
		return r.directCommit(ctx, dp, access, target)
	default:
		return r.openMergeRequest(ctx, dp, &policy, access, project, target)
	}
}

// targetBranch resolves the branch the change is proposed against: the
// policy's targetBranch, else the Application's targetRevision when it is a
// branch. A tag or commit SHA cannot be pushed to, so it fails the proposal.
func (r *DriftProposalReconciler) targetBranch(ctx context.Context, dp *backflowv1alpha1.DriftProposal,
	policy *backflowv1alpha1.BackflowPolicy, access *scmAccess) (string, *proposalOutcome) {
	explicit := policy.Spec.MergeRequest != nil && policy.Spec.MergeRequest.TargetBranch != ""
	branch := dp.Spec.Source.TargetRevision
	if explicit {
		branch = policy.Spec.MergeRequest.TargetBranch
	}
	if !explicit && (branch == "" || branch == "HEAD") {
		def, err := r.Writer.DefaultBranch(ctx, dp.Spec.Source.RepoURL, access.auth)
		if err != nil {
			if errors.Is(err, gitrepo.ErrBranchNotFound) {
				res, ferr := r.fail(ctx, dp, reasonTargetNotABranch,
					"The Application tracks HEAD and the repository does not report its default branch; set spec.mergeRequest.targetBranch on the policy.")
				return "", &proposalOutcome{result: res, err: ferr}
			}
			res, rerr := r.gitFailure(ctx, dp, err)
			return "", &proposalOutcome{result: res, err: rerr}
		}
		branch = def
	}
	if _, err := r.Writer.BranchTip(ctx, dp.Spec.Source.RepoURL, branch, access.auth); err != nil {
		if errors.Is(err, gitrepo.ErrBranchNotFound) {
			origin := "the Application's targetRevision"
			if explicit {
				origin = "spec.mergeRequest.targetBranch"
			}
			res, ferr := r.fail(ctx, dp, reasonTargetNotABranch, fmt.Sprintf(
				"%s %q is not a branch of the repository (it may be a tag, a commit SHA, or a branch that does not exist). "+
					"Set spec.mergeRequest.targetBranch on the policy.", origin, branch))
			return "", &proposalOutcome{result: res, err: ferr}
		}
		res, rerr := r.gitFailure(ctx, dp, err)
		return "", &proposalOutcome{result: res, err: rerr}
	}
	return branch, nil
}

// preparedChange is a proposal re-mapped onto the head of the target branch
// and committed on top of it.
type preparedChange struct {
	head   string // tip of the target branch the commit is built on
	sha    string // the commit
	result *mappedChange
}

// prepare re-runs Locate/Apply/Verify against the current head of the target
// branch and builds the commit. The mapping done at spec.source.revision only
// says the change was traceable then; the merge request must be based on what
// Git says now. When the edit no longer verifies, the source changed under
// the proposal and Git wins: the proposal is Superseded.
//
// A non-nil outcome means the proposal was settled or held; the caller stops.
func (r *DriftProposalReconciler) prepare(ctx context.Context, dp *backflowv1alpha1.DriftProposal,
	access *scmAccess, target string) (*preparedChange, *proposalOutcome) {
	stop := func(res ctrl.Result, err error) (*preparedChange, *proposalOutcome) {
		return nil, &proposalOutcome{result: res, err: err}
	}

	head, err := r.Writer.BranchTip(ctx, dp.Spec.Source.RepoURL, target, access.auth)
	if err != nil {
		if errors.Is(err, gitrepo.ErrBranchNotFound) {
			return stop(r.fail(ctx, dp, reasonTargetNotABranch, fmt.Sprintf("Branch %q no longer exists in the repository.", target)))
		}
		return stop(r.gitFailure(ctx, dp, err))
	}

	started := time.Now()
	res, err := r.mapAt(ctx, dp, access, head)
	renders := 0
	if res != nil {
		renders = res.renders
	}
	logf.FromContext(ctx).Info("Mapped again at the head of the target branch",
		"proposal", dp.Name, "head", head, "took", time.Since(started).Round(time.Millisecond).String(), "renders", renders, "error", err)
	if err != nil {
		if held, ok := errors.AsType[*heldError](err); ok {
			return stop(r.holdProposal(ctx, dp, held.reason, held.cause, 0))
		}
		if verdict, ok := errors.AsType[*verdictError](err); ok {
			if head == dp.Spec.Source.Revision {
				// Same tree that mapped before: nothing changed in Git, so this
				// is not "the source moved on".
				return stop(ctrl.Result{}, r.unmapped(ctx, dp, verdict.reason, verdict.err.Error()))
			}
			return stop(ctrl.Result{}, r.supersededByGit(ctx, dp, sourceChangedMessage+" "+verdict.err.Error()))
		}
		if isGitError(err) {
			return stop(r.gitFailure(ctx, dp, err))
		}
		return stop(ctrl.Result{}, err)
	}

	started = time.Now()
	sha, err := r.Writer.CommitFiles(ctx, dp.Spec.Source.RepoURL, head, access.auth, res.files(), r.commitMeta(dp, access, res.leftOut, res.overrides))
	logf.FromContext(ctx).Info("Commit built",
		"proposal", dp.Name, "took", time.Since(started).Round(time.Millisecond).String(), "error", err)
	if err != nil {
		if errors.Is(err, gitrepo.ErrNoChange) {
			return stop(ctrl.Result{}, r.supersededByGit(ctx, dp, sourceChangedMessage+" Git already contains the live value."))
		}
		if errors.Is(err, gitrepo.ErrFileNotFound) || errors.Is(err, gitrepo.ErrNotRegularFile) {
			return stop(ctrl.Result{}, r.supersededByGit(ctx, dp, sourceChangedMessage+" "+err.Error()))
		}
		return stop(r.gitFailure(ctx, dp, err))
	}
	return &preparedChange{head: head, sha: sha, result: res}, nil
}

// verdictError is a mapping error that is a verdict about the source (not
// found, ambiguous, does not verify) rather than a failure to read it.
type verdictError struct {
	err error
	// reason is the machine-readable reason for the Mapped condition.
	reason string
}

func (e *verdictError) Error() string { return e.err.Error() }
func (e *verdictError) Unwrap() error { return e.err }

// heldError asks the caller to hold the proposal with a reason.
type heldError struct {
	reason string
	cause  error
}

func (e *heldError) Error() string { return e.cause.Error() }

// isGitError reports whether err comes from reading or writing the repository.
func isGitError(err error) bool {
	for _, target := range []error{gitrepo.ErrAuth, gitrepo.ErrRepositoryNotFound, gitrepo.ErrRevisionNotFound,
		gitrepo.ErrUnavailable, gitrepo.ErrPushRejected} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

// mapAt re-runs the mapping on the repository at commit sha: for a Directory
// source Locate/Apply/Verify, for a Helm source finding the candidates and
// verifying them by rendering. A verdict about the source is returned as a
// *verdictError; a problem outside the repository as a *heldError; other
// errors are failures to read.
func (r *DriftProposalReconciler) mapAt(ctx context.Context, dp *backflowv1alpha1.DriftProposal,
	access *scmAccess, sha string) (*mappedChange, error) {
	app, err := r.application(ctx, dp)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, &heldError{reason: reasonApplicationNotFound, cause: fmt.Errorf(
				"the Argo CD Application %s/%s was not found", dp.Spec.Application.Namespace, dp.Spec.Application.Name)}
		}
		return nil, err
	}
	tree, err := r.Repos.Open(ctx, dp.Spec.Source.RepoURL, sha, access.auth)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tree.Close() }()
	return r.mapTree(ctx, dp, app, tree, sha)
}

// ownBranch reports whether an existing branch is this proposal's own work
// that an earlier run of the operator pushed, so it can be reused instead of
// failing with BranchConflict. All of these must hold:
//
//  1. the tip carries this proposal's "Proposal:" trailer;
//  2. the tip has exactly one parent, and that parent is an ancestor of the
//     current head of the target branch;
//  3. relative to its parent the tip changes only the mapped files, and to
//     exactly the content the mapping produces from that parent.
//
// Anything else is someone else's work, which is never overwritten. The
// string says why a branch is not ours.
func (r *DriftProposalReconciler) ownBranch(ctx context.Context, dp *backflowv1alpha1.DriftProposal,
	access *scmAccess, tip, head string) (bool, string, error) {
	url := dp.Spec.Source.RepoURL
	info, err := r.Writer.CommitInfo(ctx, url, tip, access.auth)
	if err != nil {
		return false, "", err
	}
	if !hasProposalTrailer(info.Message, dp) {
		return false, "its tip does not carry this proposal's trailer", nil
	}
	if len(info.Parents) != 1 {
		return false, fmt.Sprintf("its tip has %d parents, not one", len(info.Parents)), nil
	}
	parent := info.Parents[0]
	ancestor, err := r.Writer.IsAncestor(ctx, url, parent, head, access.auth)
	if err != nil {
		return false, "", err
	}
	if !ancestor {
		return false, fmt.Sprintf("its base %s is not part of the target branch history", parent), nil
	}

	res, err := r.mapAt(ctx, dp, access, parent)
	if err != nil {
		if verdict, ok := errors.AsType[*verdictError](err); ok {
			return false, "the change cannot be reproduced from its base: " + verdict.err.Error(), nil
		}
		return false, "", err
	}
	changed, err := r.Writer.ChangedPaths(ctx, url, parent, tip, access.auth)
	if err != nil {
		return false, "", err
	}
	if want := res.paths(); !slices.Equal(changed, want) {
		return false, fmt.Sprintf("it changes %v, not only %s", changed, strings.Join(want, ", ")), nil
	}
	tree, err := r.Repos.Open(ctx, url, tip, access.auth)
	if err != nil {
		return false, "", err
	}
	defer func() { _ = tree.Close() }()
	for _, file := range res.paths() {
		got, err := fs.ReadFile(tree, file)
		if err != nil {
			return false, "", fmt.Errorf("%w: reading %s at %s: %v", gitrepo.ErrUnavailable, file, tip, err)
		}
		if !bytes.Equal(got, res.files()[file]) {
			return false, fmt.Sprintf("%s differs from the content this proposal produces", file), nil
		}
	}
	return true, "", nil
}

// commitMeta builds the commit identity. The commit time is the time the
// drift was detected, never the clock: retries then rebuild the same commit.
// The Kubernetes actor has no Git identity, so the commit is authored by the
// committer and the actor is named in a trailer.
func (r *DriftProposalReconciler) commitMeta(dp *backflowv1alpha1.DriftProposal, access *scmAccess, left []leftOutField, overrides []helmmap.AppliedOverride) gitrepo.CommitMeta {
	committer := gitrepo.Signature{Name: defaultCommitterName, Email: defaultCommitterEmail}
	if c := access.conn.Spec.Committer; c != nil && c.Name != "" && c.Email != "" {
		committer = gitrepo.Signature{Name: c.Name, Email: c.Email}
	}
	return gitrepo.CommitMeta{Committer: committer, Message: commitMessage(dp, left, overrides), When: dp.Spec.DetectedAt.UTC()}
}

func (r *DriftProposalReconciler) openMergeRequest(ctx context.Context, dp *backflowv1alpha1.DriftProposal,
	policy *backflowv1alpha1.BackflowPolicy, access *scmAccess, project, target string) (ctrl.Result, error) {
	opts := mergeRequestOptions(policy)
	branch := recordedOrNewBranch(opts.BranchPrefix, dp)
	if branch == target {
		// Never push to the target branch in MergeRequest mode.
		return r.fail(ctx, dp, reasonBranchConflict, fmt.Sprintf("The proposal branch %q is the target branch.", branch))
	}

	provider, err := r.newProvider(scm.Config{
		Provider: string(access.conn.Spec.Provider), BaseURL: access.conn.Spec.URL, Token: access.token, CACert: access.ca,
	})
	if err != nil {
		return r.fail(ctx, dp, reasonUnsupportedProvider, err.Error())
	}

	// An earlier run got as far as pushing, and may have opened a merge request
	// that was not recorded before the operator stopped. Whatever happened to it
	// since (merged, closed) is the truth: look before building anything new.
	if dp.Status.Branch != "" {
		mr, err := provider.FindMergeRequest(ctx, project, branch)
		if err != nil {
			return r.scmFailure(ctx, dp, err)
		}
		if mr != nil && r.carriesOurWork(ctx, dp, access, mr, branch) {
			return r.adoptMergeRequest(ctx, dp, mr, branch, target)
		}
	}

	change, out := r.prepare(ctx, dp, access, target)
	if out != nil {
		return out.result, out.err
	}

	// Record the branch before pushing it: a crash between the push and the
	// merge request would otherwise leave a branch nobody knows about.
	if dp.Status.Branch != branch {
		orig := dp.DeepCopy()
		dp.Status.Branch = branch
		if err := r.patchStatus(ctx, dp, orig); err != nil {
			return ctrl.Result{}, err
		}
	}

	pushStarted := time.Now()
	err = r.Writer.CreateBranch(ctx, dp.Spec.Source.RepoURL, branch, change.sha, access.auth)
	logf.FromContext(ctx).Info("Pushed the branch",
		"proposal", dp.Name, "branch", branch, "took", time.Since(pushStarted).Round(time.Millisecond).String(), "error", err)
	if err != nil {
		var exists *gitrepo.BranchExistsError
		if !errors.As(err, &exists) {
			return r.gitFailure(ctx, dp, err)
		}
		// An earlier run of ours may have pushed it before the target branch moved.
		ours, why, oerr := r.ownBranch(ctx, dp, access, exists.SHA, change.head)
		if oerr != nil {
			return r.gitFailure(ctx, dp, oerr)
		}
		if !ours {
			return r.fail(ctx, dp, reasonBranchConflict, fmt.Sprintf(
				"Branch %q already exists at %s and is not this proposal's work (%s); it was left untouched.", branch, exists.SHA, why))
		}
		logf.FromContext(ctx).Info("Reusing the branch pushed earlier", "proposal", dp.Name, "branch", branch, "tip", exists.SHA)
	}

	// In any state: a merge request that was merged or closed meanwhile is not
	// opened a second time.
	mr, err := provider.FindMergeRequest(ctx, project, branch)
	if err != nil {
		return r.scmFailure(ctx, dp, err)
	}
	if mr != nil && r.carriesOurWork(ctx, dp, access, mr, branch, change.sha) {
		return r.adoptMergeRequest(ctx, dp, mr, branch, target)
	}
	assignee := ""
	if opts.AssignActor && dp.Spec.Actor != nil && !strings.HasPrefix(dp.Spec.Actor.Username, "system:") {
		assignee = singleLine(dp.Spec.Actor.Username)
	}
	mr, err = provider.CreateMergeRequest(ctx, project, scm.CreateRequest{
		SourceBranch: branch, TargetBranch: target,
		Title: commitSubject(dp, change.result.leftOut), Body: mergeRequestBody(dp, change.result.leftOut, change.result.overrides, change.result.diff()),
		Labels: opts.Labels, Reviewers: opts.Reviewers, Assignee: assignee,
	})
	if err != nil {
		if errors.Is(err, scm.ErrConflict) {
			// Somebody opened it between our lookup and create; adopt it next time.
			return r.holdProposal(ctx, dp, reasonScmUnavailable, err, retryMin)
		}
		return r.scmFailure(ctx, dp, err)
	}
	for _, w := range mr.Warnings {
		logf.FromContext(ctx).Info("Merge request created with a warning", "proposal", dp.Name, "warning", w)
	}
	return r.adoptMergeRequest(ctx, dp, mr, branch, target)
}

// recordedOrNewBranch is the branch a proposal is pushed to. A branch that was
// recorded is always used, whatever the name would be now: a proposal of an
// older version has backflow/<name>, and the prefix of the policy may have
// changed since the branch was pushed. The name is computed only for a
// proposal that has not pushed anything yet.
func recordedOrNewBranch(prefix string, dp *backflowv1alpha1.DriftProposal) string {
	if dp.Status.Branch != "" {
		return dp.Status.Branch
	}
	if ref := dp.Status.MergeRequest; ref != nil && ref.Branch != "" {
		return ref.Branch
	}
	return proposalBranch(prefix, dp)
}

// maxBranchLen keeps proposal branches well within what Git hosts accept.
const maxBranchLen = 200

// proposalBranch names the branch of a proposal: <prefix><name>-<uid8>. The UID
// of the proposal object is stable across restarts, so a crashed run finds its
// own branch again, but an object created later (the user deleted a Rejected
// proposal to retry, or a policy was recreated) has the same content hash as
// its name and never shares a branch, or a merge request, with an old one.
func proposalBranch(prefix string, dp *backflowv1alpha1.DriftProposal) string {
	uid := string(dp.UID)
	if len(uid) > 8 {
		uid = uid[:8]
	}
	name := dp.Name
	if room := maxBranchLen - len(prefix) - len(uid) - 1; room < len(name) && room > 0 {
		name = strings.Trim(name[:room], "-")
	}
	return prefix + name + "-" + uid
}

// carriesOurWork reports whether a merge request found by the proposal's
// branch is the proposal's own. An open one is adopted. A closed or merged one
// only when its head is a commit of ours: the tip of the recorded branch, or a
// commit just built (known). A request of an earlier proposal that happened to
// use the same branch name is not ours.
func (r *DriftProposalReconciler) carriesOurWork(ctx context.Context, dp *backflowv1alpha1.DriftProposal,
	access *scmAccess, mr *scm.MergeRequest, branch string, known ...string) bool {
	if mr.State == scm.StateOpen {
		return true
	}
	if mr.HeadSHA == "" {
		return false
	}
	if slices.Contains(known, mr.HeadSHA) {
		return true
	}
	if tip, err := r.Writer.BranchTip(ctx, dp.Spec.Source.RepoURL, branch, access.auth); err == nil && tip == mr.HeadSHA {
		return true
	}
	logf.FromContext(ctx).Info("Ignoring a closed or merged merge request that does not carry this proposal's work",
		"proposal", dp.Name, "url", mr.URL, "head", mr.HeadSHA)
	return false
}

// adoptMergeRequest records the merge request of the proposal's branch in the
// state it is in: open (Proposed), or already merged or closed by a person
// while the operator was away.
func (r *DriftProposalReconciler) adoptMergeRequest(ctx context.Context, dp *backflowv1alpha1.DriftProposal,
	mr *scm.MergeRequest, branch, target string) (ctrl.Result, error) {
	orig := dp.DeepCopy()
	dp.Status.Branch = branch
	dp.Status.MergeRequest = &backflowv1alpha1.MergeRequestRef{URL: mr.URL, Number: mr.Number, Branch: branch, State: string(mr.State)}
	switch mr.State {
	case scm.StateMerged:
		dp.Status.Phase = backflowv1alpha1.PhaseMerged
		dp.Status.CommitSHA = mr.MergeCommitSHA
		dp.Status.Message = mergedMessage(mr.URL)
		leaveProposed(dp, reasonMergeRequestMerged, mergedConditionMessage(mr.URL, mr.MergeCommitSHA))
	case scm.StateClosed:
		dp.Status.Phase = backflowv1alpha1.PhaseRejected
		dp.Status.Message = closedMessage(mr.URL)
		leaveProposed(dp, reasonMergeRequestClosed, closedConditionMessage(mr.URL))
	default:
		dp.Status.Phase = backflowv1alpha1.PhaseProposed
		dp.Status.Message = fmt.Sprintf("Merge request %s proposes the change against %s.", mr.URL, target)
		meta.SetStatusCondition(&dp.Status.Conditions, metav1.Condition{
			Type: conditionProposed, Status: metav1.ConditionTrue, Reason: reasonMergeRequestOpened,
			Message: dp.Status.Message, ObservedGeneration: dp.Generation,
		})
	}
	if err := r.patchStatus(ctx, dp, orig); err != nil {
		return ctrl.Result{}, err
	}
	logf.FromContext(ctx).Info("Merge request recorded", "proposal", dp.Name, "url", mr.URL, "branch", branch,
		"target", target, "state", string(mr.State))
	if mr.State != scm.StateOpen {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{RequeueAfter: r.trackEvery()}, nil
}

func mergedMessage(url string) string {
	return fmt.Sprintf("Merge request %s was merged; Git now holds the live change.", url)
}

func closedMessage(url string) string {
	return fmt.Sprintf("Merge request %s was closed without merging; Argo CD can revert the cluster to what Git says.", url)
}

func (r *DriftProposalReconciler) directCommit(ctx context.Context, dp *backflowv1alpha1.DriftProposal,
	access *scmAccess, target string) (ctrl.Result, error) {
	for attempt := 1; attempt <= directCommitAttempts; attempt++ {
		// After a restart the commit may already be on the branch.
		if done, res, err := r.alreadyCommitted(ctx, dp, access, target); done {
			return res, err
		}
		change, out := r.prepare(ctx, dp, access, target)
		if out != nil {
			return out.result, out.err
		}
		pushStarted := time.Now()
		err := r.Writer.UpdateBranch(ctx, dp.Spec.Source.RepoURL, target, change.sha, access.auth)
		logf.FromContext(ctx).Info("Pushed the commit to the target branch",
			"proposal", dp.Name, "branch", target, "took", time.Since(pushStarted).Round(time.Millisecond).String(), "error", err)
		switch {
		case err == nil:
			return ctrl.Result{}, r.recordCommitted(ctx, dp, target, change.sha)
		case errors.Is(err, gitrepo.ErrNonFastForward):
			if attempt == directCommitAttempts {
				return r.fail(ctx, dp, reasonNonFastForward, fmt.Sprintf(
					"Branch %q moved again while pushing, even after retrying from its new head: %v", target, err))
			}
			logf.FromContext(ctx).Info("Target branch moved; retrying from its new head", "proposal", dp.Name, "branch", target)
		case errors.Is(err, gitrepo.ErrPushRejected):
			return r.pushDeclined(ctx, dp, fmt.Sprintf("the push to %q", target), err)
		default:
			return r.gitFailure(ctx, dp, err)
		}
	}
	return ctrl.Result{}, nil // unreachable
}

// alreadyCommitted recognises the proposal's own commit on the target branch:
// the operator restarted after pushing, before recording it. Other people may
// have pushed on top since, so the first-parent history is searched from the
// head back to the revision the proposal was made at, not just the tip.
//
// Not being able to look is not "there is none": a failed walk is held and
// retried, and so is a history longer than the scan limit that does not reach
// the revision, because a second commit would then be pushed on a guess.
func (r *DriftProposalReconciler) alreadyCommitted(ctx context.Context, dp *backflowv1alpha1.DriftProposal,
	access *scmAccess, target string) (bool, ctrl.Result, error) {
	tip, err := r.Writer.BranchTip(ctx, dp.Spec.Source.RepoURL, target, access.auth)
	if err != nil {
		return false, ctrl.Result{}, nil // prepare reports it
	}
	limit := r.scanLimit()
	log, reached, err := r.Writer.FirstParentLog(ctx, dp.Spec.Source.RepoURL, tip, dp.Spec.Source.Revision, limit, access.auth)
	if err != nil {
		res, herr := r.gitFailure(ctx, dp, err)
		return true, res, herr
	}
	for _, e := range log {
		if hasProposalTrailer(e.Message, dp) {
			return true, ctrl.Result{}, r.recordCommitted(ctx, dp, target, e.SHA)
		}
	}
	if !reached && len(log) >= limit {
		res, herr := r.holdProposal(ctx, dp, reasonHistoryTooLong, fmt.Errorf(
			"%q has more than %d commits since revision %s, so it cannot be told whether this proposal was already committed",
			target, limit, shortRevision(dp.Spec.Source.Revision)), 0)
		return true, res, herr
	}
	return false, ctrl.Result{}, nil
}

func (r *DriftProposalReconciler) scanLimit() int {
	if r.CommitScanLimit > 0 {
		return r.CommitScanLimit
	}
	return defaultCommitScanLimit
}

func (r *DriftProposalReconciler) recordCommitted(ctx context.Context, dp *backflowv1alpha1.DriftProposal, target, sha string) error {
	orig := dp.DeepCopy()
	dp.Status.Phase = backflowv1alpha1.PhaseMerged
	dp.Status.CommitSHA = sha
	dp.Status.Message = fmt.Sprintf("Committed to %s as %s.", target, sha)
	meta.SetStatusCondition(&dp.Status.Conditions, metav1.Condition{
		Type: conditionProposed, Status: metav1.ConditionTrue, Reason: reasonCommitted,
		Message: dp.Status.Message, ObservedGeneration: dp.Generation,
	})
	if err := r.patchStatus(ctx, dp, orig); err != nil {
		return err
	}
	logf.FromContext(ctx).Info("Change committed", "proposal", dp.Name, "branch", target, "commit", sha)
	return nil
}

// mergeRequestOptions returns the policy's merge request settings with the
// API defaults applied for a policy that sets none.
func mergeRequestOptions(policy *backflowv1alpha1.BackflowPolicy) backflowv1alpha1.MergeRequestOptions {
	if policy.Spec.MergeRequest == nil {
		return backflowv1alpha1.MergeRequestOptions{BranchPrefix: defaultBranchPrefix, AssignActor: true}
	}
	opts := *policy.Spec.MergeRequest
	if opts.BranchPrefix == "" {
		opts.BranchPrefix = defaultBranchPrefix
	}
	return opts
}

// setProposedCondition records the Proposed condition without changing the phase.
func (r *DriftProposalReconciler) setProposedCondition(ctx context.Context, dp *backflowv1alpha1.DriftProposal,
	status metav1.ConditionStatus, reason, message string) error {
	orig := dp.DeepCopy()
	meta.SetStatusCondition(&dp.Status.Conditions, metav1.Condition{
		Type: conditionProposed, Status: status, Reason: reason, Message: message, ObservedGeneration: dp.Generation,
	})
	return r.patchStatus(ctx, dp, orig)
}

// holdProposal records why nothing could be written yet and asks for another
// attempt. The proposal stays in Mapping and recovers when the cause is gone.
// A zero delay uses the backoff.
func (r *DriftProposalReconciler) holdProposal(ctx context.Context, dp *backflowv1alpha1.DriftProposal,
	reason string, cause error, delay time.Duration) (ctrl.Result, error) {
	cause = errors.New(redactText(cause.Error()))
	if delay <= 0 {
		delay = r.retryDelayFor(dp, conditionProposed)
	}
	if err := r.setProposedCondition(ctx, dp, metav1.ConditionFalse, reason, cause.Error()); err != nil {
		return ctrl.Result{}, err
	}
	logf.FromContext(ctx).Info("Cannot propose the change yet; will retry",
		"proposal", dp.Name, "reason", reason, "cause", cause.Error(), "retryAfter", delay.String())
	return ctrl.Result{RequeueAfter: delay}, nil
}

// fail ends the proposal in Failed. The cause is permanent: a retry would
// give the same answer.
func (r *DriftProposalReconciler) fail(ctx context.Context, dp *backflowv1alpha1.DriftProposal, reason, message string) (ctrl.Result, error) {
	message = redactText(message)
	orig := dp.DeepCopy()
	dp.Status.Phase = backflowv1alpha1.PhaseFailed
	dp.Status.Message = message
	meta.SetStatusCondition(&dp.Status.Conditions, metav1.Condition{
		Type: conditionProposed, Status: metav1.ConditionFalse, Reason: reason, Message: message, ObservedGeneration: dp.Generation,
	})
	logf.FromContext(ctx).Info("Proposal failed", "proposal", dp.Name, "reason", reason, "message", message)
	return ctrl.Result{}, r.patchStatus(ctx, dp, orig)
}

// pushBackoff is how much longer to wait before pushing again after a decline,
// or 0.
func (r *DriftProposalReconciler) pushBackoff(dp *backflowv1alpha1.DriftProposal) time.Duration {
	n := int(dp.Status.PushRejections)
	if n < 1 || n > len(pushRetryDelays) || dp.Status.LastPushRejectedAt == nil {
		return 0
	}
	return max(dp.Status.LastPushRejectedAt.Add(pushRetryDelays[n-1]).Sub(r.now()), 0)
}

// crdsOutdatedMessage is said when the API server drops a field of the status.
const crdsOutdatedMessage = "CRDs are outdated: the driftproposals CRD does not keep status.pushRejections. " +
	"Apply the CRDs of this version: helm show crds oci://ghcr.io/sserkanml/charts/backflow --version <version> | kubectl apply --server-side -f -"

// pushDeclined handles a push the Git server declined. The first declines are
// held and tried again later (the count is in the status, so a restart does not
// reset it); the last one fails the proposal with PushRejected.
func (r *DriftProposalReconciler) pushDeclined(ctx context.Context, dp *backflowv1alpha1.DriftProposal,
	what string, err error) (ctrl.Result, error) {
	declines := int(dp.Status.PushRejections) + 1
	if declines >= maxPushRejections {
		return r.fail(ctx, dp, reasonPushRejected, fmt.Sprintf(
			"The server declined %s %d times (a protected branch, a hook, or the Git host having trouble): %v", what, declines, err))
	}
	delay := pushRetryDelays[declines-1]
	message := redactText(fmt.Sprintf("The server declined %s (attempt %d of %d); trying again in %s: %v",
		what, declines, maxPushRejections, delay, err))
	orig := dp.DeepCopy()
	now := metav1.NewTime(r.now())
	dp.Status.PushRejections = int32(declines) //nolint:gosec // at most maxPushRejections
	dp.Status.LastPushRejectedAt = &now
	meta.SetStatusCondition(&dp.Status.Conditions, metav1.Condition{
		Type: conditionProposed, Status: metav1.ConditionFalse, Reason: reasonPushRejected, Message: message, ObservedGeneration: dp.Generation,
	})
	if perr := r.patchStatus(ctx, dp, orig); perr != nil {
		return ctrl.Result{}, perr
	}
	if int(dp.Status.PushRejections) != declines {
		// The API server answered without the count: the CRD is from before the
		// field existed (helm upgrade does not update CRDs) and pruned it. The count
		// would stay at zero and the push would be retried for ever, so stop.
		if r.Recorder != nil {
			r.Recorder.Eventf(dp, nil, corev1.EventTypeWarning, "CRDsOutdated", "PushRejected", "%s", crdsOutdatedMessage)
		}
		return r.fail(ctx, dp, reasonPushRejected, fmt.Sprintf(
			"The server declined %s, and the retries cannot be counted. %s Cause: %v", what, crdsOutdatedMessage, err))
	}
	logf.FromContext(ctx).Info("The Git server declined the push; will try again",
		"proposal", dp.Name, "declines", declines, "retryAfter", delay.String(), "cause", message)
	return ctrl.Result{RequeueAfter: delay}, nil
}

// supersededByGit ends the proposal because Git changed under it. Git always wins.
func (r *DriftProposalReconciler) supersededByGit(ctx context.Context, dp *backflowv1alpha1.DriftProposal, message string) error {
	message = redactText(message)
	orig := dp.DeepCopy()
	dp.Status.Phase = backflowv1alpha1.PhaseSuperseded
	dp.Status.Message = message
	meta.SetStatusCondition(&dp.Status.Conditions, metav1.Condition{
		Type: conditionProposed, Status: metav1.ConditionFalse, Reason: reasonSourceChanged, Message: message, ObservedGeneration: dp.Generation,
	})
	logf.FromContext(ctx).Info("Proposal superseded by a change in Git", "proposal", dp.Name, "message", message)
	return r.patchStatus(ctx, dp, orig)
}

// gitFailure turns an error from the Git side into a held or failed proposal.
func (r *DriftProposalReconciler) gitFailure(ctx context.Context, dp *backflowv1alpha1.DriftProposal, err error) (ctrl.Result, error) {
	switch {
	case errors.Is(err, gitrepo.ErrAuth):
		return r.holdProposal(ctx, dp, reasonRepositoryAuthFailed, err, 0)
	case errors.Is(err, gitrepo.ErrRepositoryNotFound):
		return r.holdProposal(ctx, dp, reasonRepositoryNotFound, err, 0)
	case errors.Is(err, gitrepo.ErrPushRejected):
		return r.pushDeclined(ctx, dp, "the push", err)
	}
	return r.holdProposal(ctx, dp, reasonRepositoryUnavailable, err, 0)
}

// scmFailure turns an error from the provider API into a held or failed proposal.
func (r *DriftProposalReconciler) scmFailure(ctx context.Context, dp *backflowv1alpha1.DriftProposal, err error) (ctrl.Result, error) {
	switch {
	case errors.Is(err, scm.ErrUnauthorized):
		return r.holdProposal(ctx, dp, reasonScmAuthFailed, err, 0)
	case errors.Is(err, scm.ErrForbidden):
		return r.holdProposal(ctx, dp, reasonScmForbidden, err, 0)
	case errors.Is(err, scm.ErrNotFound):
		return r.holdProposal(ctx, dp, reasonScmProjectNotFound, err, 0)
	case errors.Is(err, scm.ErrInvalid):
		return r.fail(ctx, dp, reasonMergeRequestRejected, err.Error())
	}
	// Unavailable, rate limited or unexpected: honour the provider's advice.
	delay := scm.RetryAfter(err)
	if delay < retryMin {
		delay = 0
	}
	return r.holdProposal(ctx, dp, reasonScmUnavailable, err, min(delay, retryMax))
}

// accessFor finds the ScmConnection the policy matched to the proposal's
// Application and reads its token and CA bundle. See scmAccessFor.
func (r *DriftProposalReconciler) accessFor(ctx context.Context, dp *backflowv1alpha1.DriftProposal,
	policy *backflowv1alpha1.BackflowPolicy) (*scmAccess, error) {
	return scmAccessFor(ctx, r.Client, dp.Namespace, policy, dp.Spec.Application.Name)
}

// scmAccessFor finds the ScmConnection the policy matched to an Application's
// repository and reads its token and CA bundle. It returns nil when there is
// no connection. Unreadable credentials are reported as gitrepo.ErrAuth.
func scmAccessFor(ctx context.Context, c client.Reader, namespace string,
	policy *backflowv1alpha1.BackflowPolicy, appName string) (*scmAccess, error) {
	var connName string
	for _, a := range policy.Status.Applications {
		if a.Name == appName {
			connName = a.ScmConnection
			break
		}
	}
	if connName == "" {
		return nil, nil
	}

	var conn backflowv1alpha1.ScmConnection
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: connName}, &conn); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("%w: ScmConnection %q not found", gitrepo.ErrAuth, connName)
		}
		return nil, err
	}
	ref := conn.Spec.TokenSecretRef
	var secret corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: conn.Namespace, Name: ref.Name}, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("%w: token Secret %q of ScmConnection %q not found", gitrepo.ErrAuth, ref.Name, connName)
		}
		return nil, err
	}
	token := strings.TrimSpace(string(secret.Data[ref.Key]))
	if token == "" {
		return nil, fmt.Errorf("%w: Secret %q has no key %q", gitrepo.ErrAuth, ref.Name, ref.Key)
	}
	acc := &scmAccess{conn: &conn, token: token, auth: gitrepo.BasicAuth(string(conn.Spec.Provider), token)}

	if ca := conn.Spec.CASecretRef; ca != nil {
		var caSecret corev1.Secret
		if err := c.Get(ctx, types.NamespacedName{Namespace: conn.Namespace, Name: ca.Name}, &caSecret); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, fmt.Errorf("%w: CA Secret %q of ScmConnection %q not found", gitrepo.ErrAuth, ca.Name, connName)
			}
			return nil, err
		}
		bundle := caSecret.Data[ca.Key]
		if len(bundle) == 0 {
			return nil, fmt.Errorf("%w: Secret %q has no key %q", gitrepo.ErrAuth, ca.Name, ca.Key)
		}
		acc.ca = bundle
		if acc.auth == nil {
			acc.auth = &gitrepo.Auth{}
		}
		acc.auth.CABundle = bundle
	}
	return acc, nil
}
