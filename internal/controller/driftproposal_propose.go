package controller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
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
	"github.com/sserkanml/backflow/internal/mapping"
	"github.com/sserkanml/backflow/internal/scm"
)

const (
	conditionProposed = "Proposed"

	defaultBranchPrefix = "backflow/"
	// directCommitAttempts is the first try plus one retry from the new head.
	directCommitAttempts = 2
)

// Reasons of the Proposed condition. A False condition on a proposal that is
// still in Mapping is retried; the reasons that end in Failed or Superseded
// are final.
const (
	reasonReportOnly           = "ReportOnly"
	reasonMissingScmConnection = "MissingScmConnection"
	reasonPolicyNotFound       = "PolicyNotFound"
	reasonScmAuthFailed        = "ScmAuthFailed"
	reasonScmForbidden         = "ScmForbidden"
	reasonScmProjectNotFound   = "ScmProjectNotFound"
	reasonScmUnavailable       = "ScmUnavailable"
	reasonMergeRequestRejected = "MergeRequestRejected"
	reasonMergeRequestOpened   = "MergeRequestOpened"
	reasonCommitted            = "Committed"
	reasonTargetNotABranch     = "TargetNotABranch"
	reasonBranchConflict       = "BranchConflict"
	reasonPushRejected         = "PushRejected"
	reasonNonFastForward       = "NonFastForward"
	reasonSourceChanged        = "SourceChanged"
	reasonUnsupportedProvider  = "UnsupportedProvider"
	reasonInvalidRepositoryURL = "InvalidRepositoryURL"
)

const sourceChangedMessage = "The source changed in Git after the drift was detected."

// RepositoryWriter is the Git side of proposing a change. *gitrepo.Cache
// implements it.
type RepositoryWriter interface {
	BranchTip(ctx context.Context, repoURL, branch string, auth *gitrepo.Auth) (string, error)
	DefaultBranch(ctx context.Context, repoURL string, auth *gitrepo.Auth) (string, error)
	CommitInfo(ctx context.Context, repoURL, sha string, auth *gitrepo.Auth) (*gitrepo.CommitInfo, error)
	CommitFile(ctx context.Context, repoURL, baseSHA string, auth *gitrepo.Auth, filePath string, content []byte, meta gitrepo.CommitMeta) (string, error)
	CreateBranch(ctx context.Context, repoURL, branch, sha string, auth *gitrepo.Auth) error
	IsAncestor(ctx context.Context, repoURL, ancestor, descendant string, auth *gitrepo.Auth) (bool, error)
	ChangedPaths(ctx context.Context, repoURL, from, to string, auth *gitrepo.Auth) ([]string, error)
	UpdateBranch(ctx context.Context, repoURL, branch, sha string, auth *gitrepo.Auth) error
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
	result *mapping.Result
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

	res, err := r.mapAt(ctx, dp, access, head)
	if err != nil {
		var held *heldError
		if errors.As(err, &held) {
			return stop(r.holdProposal(ctx, dp, held.reason, held.cause, 0))
		}
		var verdict *verdictError
		if errors.As(err, &verdict) {
			if head == dp.Spec.Source.Revision {
				// Same tree that mapped before: nothing changed in Git, so this
				// is not "the source moved on".
				reason, _ := mappingReason(verdict.err)
				return stop(ctrl.Result{}, r.unmapped(ctx, dp, reason, verdict.err.Error()))
			}
			return stop(r.supersededByGit(ctx, dp, sourceChangedMessage+" "+verdict.err.Error()))
		}
		if isGitError(err) {
			return stop(r.gitFailure(ctx, dp, err))
		}
		return stop(ctrl.Result{}, err)
	}

	sha, err := r.Writer.CommitFile(ctx, dp.Spec.Source.RepoURL, head, access.auth, res.File, res.Edited, r.commitMeta(dp, access))
	if err != nil {
		if errors.Is(err, gitrepo.ErrNoChange) {
			return stop(r.supersededByGit(ctx, dp, sourceChangedMessage+" Git already contains the live value."))
		}
		if errors.Is(err, gitrepo.ErrFileNotFound) || errors.Is(err, gitrepo.ErrNotRegularFile) {
			return stop(r.supersededByGit(ctx, dp, sourceChangedMessage+" "+err.Error()))
		}
		return stop(r.gitFailure(ctx, dp, err))
	}
	return &preparedChange{head: head, sha: sha, result: res}, nil
}

// verdictError is a mapping error that is a verdict about the source (not
// found, ambiguous, does not verify) rather than a failure to read it.
type verdictError struct{ err error }

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

// mapAt re-runs Locate/Apply/Verify on the repository at commit sha. A
// verdict about the source is returned as a *verdictError; other errors are
// failures to read.
func (r *DriftProposalReconciler) mapAt(ctx context.Context, dp *backflowv1alpha1.DriftProposal,
	access *scmAccess, sha string) (*mapping.Result, error) {
	src, err := r.directorySource(ctx, dp)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, &heldError{reason: reasonApplicationNotFound, cause: fmt.Errorf(
				"Argo CD Application %s/%s not found", dp.Spec.Application.Namespace, dp.Spec.Application.Name)}
		}
		return nil, err
	}
	tree, err := r.Repos.Open(ctx, dp.Spec.Source.RepoURL, sha, access.auth)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tree.Close() }()

	res, err := mapping.Map(tree, dp.Spec.Source.Path, src.options, src.destinationNamespace,
		mapping.ResourceID{
			Group: dp.Spec.Resource.Group, Kind: dp.Spec.Resource.Kind,
			Namespace: dp.Spec.Resource.Namespace, Name: dp.Spec.Resource.Name,
		}, dp.Spec.Changes)
	if err != nil {
		if _, verdict := mappingReason(err); verdict {
			return nil, &verdictError{err: err}
		}
		return nil, err // reading the repository failed; retried with backoff
	}
	return res, nil
}

// ownBranch reports whether an existing branch is this proposal's own work
// that an earlier run of the operator pushed, so it can be reused instead of
// failing with BranchConflict. All of these must hold:
//
//  1. the tip carries this proposal's "Proposal:" trailer;
//  2. the tip has exactly one parent, and that parent is an ancestor of the
//     current head of the target branch;
//  3. relative to its parent the tip changes only the mapped file, and to
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
	if !hasProposalTrailer(info.Message, dp.Name) {
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
		var verdict *verdictError
		if errors.As(err, &verdict) {
			return false, "the change cannot be reproduced from its base: " + verdict.err.Error(), nil
		}
		return false, "", err
	}
	changed, err := r.Writer.ChangedPaths(ctx, url, parent, tip, access.auth)
	if err != nil {
		return false, "", err
	}
	if len(changed) != 1 || changed[0] != res.File {
		return false, fmt.Sprintf("it changes %v, not only %s", changed, res.File), nil
	}
	tree, err := r.Repos.Open(ctx, url, tip, access.auth)
	if err != nil {
		return false, "", err
	}
	defer func() { _ = tree.Close() }()
	got, err := fs.ReadFile(tree, res.File)
	if err != nil {
		return false, "", fmt.Errorf("%w: reading %s at %s: %v", gitrepo.ErrUnavailable, res.File, tip, err)
	}
	if !bytes.Equal(got, res.Edited) {
		return false, fmt.Sprintf("%s differs from the content this proposal produces", res.File), nil
	}
	return true, "", nil
}

// commitMeta builds the commit identity. The commit time is the time the
// drift was detected, never the clock: retries then rebuild the same commit.
// The Kubernetes actor has no Git identity, so the commit is authored by the
// committer and the actor is named in a trailer.
func (r *DriftProposalReconciler) commitMeta(dp *backflowv1alpha1.DriftProposal, access *scmAccess) gitrepo.CommitMeta {
	committer := gitrepo.Signature{Name: defaultCommitterName, Email: defaultCommitterEmail}
	if c := access.conn.Spec.Committer; c != nil && c.Name != "" && c.Email != "" {
		committer = gitrepo.Signature{Name: c.Name, Email: c.Email}
	}
	return gitrepo.CommitMeta{Committer: committer, Message: commitMessage(dp), When: dp.Spec.DetectedAt.Time.UTC()}
}

func (r *DriftProposalReconciler) openMergeRequest(ctx context.Context, dp *backflowv1alpha1.DriftProposal,
	policy *backflowv1alpha1.BackflowPolicy, access *scmAccess, project, target string) (ctrl.Result, error) {
	opts := mergeRequestOptions(policy)
	branch := opts.BranchPrefix + dp.Name
	if branch == target {
		// Never push to the target branch in MergeRequest mode.
		return r.fail(ctx, dp, reasonBranchConflict, fmt.Sprintf("The proposal branch %q is the target branch.", branch))
	}

	change, out := r.prepare(ctx, dp, access, target)
	if out != nil {
		return out.result, out.err
	}

	provider, err := r.newProvider(scm.Config{
		Provider: string(access.conn.Spec.Provider), BaseURL: access.conn.Spec.URL, Token: access.token, CACert: access.ca,
	})
	if err != nil {
		return r.fail(ctx, dp, reasonUnsupportedProvider, err.Error())
	}

	if err := r.Writer.CreateBranch(ctx, dp.Spec.Source.RepoURL, branch, change.sha, access.auth); err != nil {
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

	mr, err := provider.FindOpenMergeRequest(ctx, project, branch)
	if err != nil {
		return r.scmFailure(ctx, dp, err)
	}
	if mr == nil {
		assignee := ""
		if opts.AssignActor && dp.Spec.Actor != nil && !strings.HasPrefix(dp.Spec.Actor.Username, "system:") {
			assignee = singleLine(dp.Spec.Actor.Username)
		}
		mr, err = provider.CreateMergeRequest(ctx, project, scm.CreateRequest{
			SourceBranch: branch, TargetBranch: target,
			Title: commitSubject(dp), Body: mergeRequestBody(dp, change.result.Diff),
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
	}

	orig := dp.DeepCopy()
	dp.Status.Phase = backflowv1alpha1.PhaseProposed
	dp.Status.MergeRequest = &backflowv1alpha1.MergeRequestRef{URL: mr.URL, Number: mr.Number, Branch: branch, State: string(mr.State)}
	dp.Status.Message = fmt.Sprintf("Merge request %s proposes the change against %s.", mr.URL, target)
	meta.SetStatusCondition(&dp.Status.Conditions, metav1.Condition{
		Type: conditionProposed, Status: metav1.ConditionTrue, Reason: reasonMergeRequestOpened,
		Message: dp.Status.Message, ObservedGeneration: dp.Generation,
	})
	if err := r.patchStatus(ctx, dp, orig); err != nil {
		return ctrl.Result{}, err
	}
	logf.FromContext(ctx).Info("Merge request proposed", "proposal", dp.Name, "url", mr.URL, "branch", branch, "target", target)
	return ctrl.Result{RequeueAfter: r.trackEvery()}, nil
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
		err := r.Writer.UpdateBranch(ctx, dp.Spec.Source.RepoURL, target, change.sha, access.auth)
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
			return r.fail(ctx, dp, reasonPushRejected, fmt.Sprintf(
				"The server rejected the push to %q (a protected branch or a hook?): %v", target, err))
		default:
			return r.gitFailure(ctx, dp, err)
		}
	}
	return ctrl.Result{}, nil // unreachable
}

// alreadyCommitted recognises the proposal's own commit at the tip of the
// target branch: the operator restarted after pushing, before recording it.
func (r *DriftProposalReconciler) alreadyCommitted(ctx context.Context, dp *backflowv1alpha1.DriftProposal,
	access *scmAccess, target string) (bool, ctrl.Result, error) {
	tip, err := r.Writer.BranchTip(ctx, dp.Spec.Source.RepoURL, target, access.auth)
	if err != nil {
		return false, ctrl.Result{}, nil // prepare reports it
	}
	info, err := r.Writer.CommitInfo(ctx, dp.Spec.Source.RepoURL, tip, access.auth)
	if err != nil || !hasProposalTrailer(info.Message, dp.Name) {
		return false, ctrl.Result{}, nil
	}
	return true, ctrl.Result{}, r.recordCommitted(ctx, dp, target, tip)
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
	orig := dp.DeepCopy()
	dp.Status.Phase = backflowv1alpha1.PhaseFailed
	dp.Status.Message = message
	meta.SetStatusCondition(&dp.Status.Conditions, metav1.Condition{
		Type: conditionProposed, Status: metav1.ConditionFalse, Reason: reason, Message: message, ObservedGeneration: dp.Generation,
	})
	logf.FromContext(ctx).Info("Proposal failed", "proposal", dp.Name, "reason", reason, "message", message)
	return ctrl.Result{}, r.patchStatus(ctx, dp, orig)
}

// supersededByGit ends the proposal because Git changed under it. Git always wins.
func (r *DriftProposalReconciler) supersededByGit(ctx context.Context, dp *backflowv1alpha1.DriftProposal, message string) (ctrl.Result, error) {
	orig := dp.DeepCopy()
	dp.Status.Phase = backflowv1alpha1.PhaseSuperseded
	dp.Status.Message = message
	meta.SetStatusCondition(&dp.Status.Conditions, metav1.Condition{
		Type: conditionProposed, Status: metav1.ConditionFalse, Reason: reasonSourceChanged, Message: message, ObservedGeneration: dp.Generation,
	})
	logf.FromContext(ctx).Info("Proposal superseded by a change in Git", "proposal", dp.Name, "message", message)
	return ctrl.Result{}, r.patchStatus(ctx, dp, orig)
}

// gitFailure turns an error from the Git side into a held or failed proposal.
func (r *DriftProposalReconciler) gitFailure(ctx context.Context, dp *backflowv1alpha1.DriftProposal, err error) (ctrl.Result, error) {
	switch {
	case errors.Is(err, gitrepo.ErrAuth):
		return r.holdProposal(ctx, dp, reasonRepositoryAuthFailed, err, 0)
	case errors.Is(err, gitrepo.ErrRepositoryNotFound):
		return r.holdProposal(ctx, dp, reasonRepositoryNotFound, err, 0)
	case errors.Is(err, gitrepo.ErrPushRejected):
		return r.fail(ctx, dp, reasonPushRejected, err.Error())
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
