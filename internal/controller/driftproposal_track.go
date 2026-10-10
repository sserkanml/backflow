package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	backflowv1alpha1 "github.com/sserkanml/backflow/api/v1alpha1"
	"github.com/sserkanml/backflow/internal/gitrepo"
	"github.com/sserkanml/backflow/internal/scm"
)

const (
	// trackInterval is how often the state of an open merge request is polled.
	trackInterval = 2 * time.Minute

	// conditionLiveReverted is True while Argo CD has reset the cluster to Git
	// but the proposal's merge request is still open: the change is not lost,
	// it waits in the merge request.
	conditionLiveReverted = "LiveReverted"

	// conditionCleanedUp is True once the merge request of a Superseded or
	// Reverted proposal is closed and its branch deleted.
	conditionCleanedUp = "CleanedUp"
)

// Reasons of the LiveReverted condition.
const (
	reasonSyncedNewRevision  = "SyncedNewRevision"
	reasonLiveChangeReturned = "LiveChangeReturned"
)

// Reasons of the CleanedUp condition.
const (
	reasonCleanedUp     = "Closed"
	reasonAlreadyClosed = "AlreadyClosed"
	reasonAlreadyMerged = "AlreadyMerged"
	reasonCommented     = "Commented"
	reasonBranchKept    = "BranchKept"
	reasonCleanupFailed = "CleanupPending"
)

// mrClient is the provider client and project of a proposal's repository.
type mrClient struct {
	provider scm.Provider
	project  string
	access   *scmAccess
}

// clientFor builds the provider client for the proposal's repository. A
// problem the user can fix (missing policy, ScmConnection, Secret) is
// returned as a *heldError.
func (r *DriftProposalReconciler) clientFor(ctx context.Context, dp *backflowv1alpha1.DriftProposal) (*mrClient, error) {
	var policy backflowv1alpha1.BackflowPolicy
	if err := r.Get(ctx, types.NamespacedName{Namespace: dp.Namespace, Name: dp.Spec.PolicyName}, &policy); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, &heldError{reason: reasonPolicyNotFound, cause: fmt.Errorf("BackflowPolicy %q not found", dp.Spec.PolicyName)}
		}
		return nil, err
	}
	access, err := r.accessFor(ctx, dp, &policy)
	if err != nil {
		if errors.Is(err, gitrepo.ErrAuth) {
			return nil, &heldError{reason: reasonScmAuthFailed, cause: err}
		}
		return nil, err
	}
	if access == nil {
		return nil, &heldError{reason: reasonMissingScmConnection, cause: errors.New("no ScmConnection matches the repository")}
	}
	_, project, err := scm.ParseRepoURL(dp.Spec.Source.RepoURL)
	if err != nil {
		return nil, &heldError{reason: reasonInvalidRepositoryURL, cause: err}
	}
	provider, err := r.newProvider(scm.Config{
		Provider: string(access.conn.Spec.Provider), BaseURL: access.conn.Spec.URL, Token: access.token, CACert: access.ca,
	})
	if err != nil {
		return nil, &heldError{reason: reasonUnsupportedProvider, cause: err}
	}
	return &mrClient{provider: provider, project: project, access: access}, nil
}

// retiring reports whether a superseded or reverted annotation waits to be
// applied.
func retiring(dp *backflowv1alpha1.DriftProposal) bool {
	return dp.Annotations[annotationSupersededBy] != "" || dp.Annotations[annotationReverted] != ""
}

// applyLifecycle sets the phase a superseded or reverted annotation asks for.
// It reports whether one was applied.
func applyLifecycle(dp *backflowv1alpha1.DriftProposal) bool {
	switch {
	case dp.Annotations[annotationSupersededBy] != "":
		dp.Status.Phase = backflowv1alpha1.PhaseSuperseded
		dp.Status.SupersededBy = dp.Annotations[annotationSupersededBy]
		dp.Status.Message = "A newer drift on the same resource replaced this proposal."
		return true
	case dp.Annotations[annotationReverted] != "":
		dp.Status.Phase = backflowv1alpha1.PhaseReverted
		dp.Status.Message = dp.Annotations[annotationReverted]
		return true
	case dp.Annotations[annotationLiveReverted] != "":
		// Only reached when there is no open merge request to keep the change
		// in (a proposal with one is handled by trackProposed): a plain revert.
		dp.Status.Phase = backflowv1alpha1.PhaseReverted
		dp.Status.Message = "The live resource was reset to Git by an Argo CD sync of " + dp.Annotations[annotationLiveReverted] + "."
		return true
	}
	return false
}

// trackProposed follows a proposal that has an open merge request. The state
// of the merge request is read first, before any annotation is applied: when a
// merge request was merged or closed, the cluster usually drifts back in sync
// right away, and the drift controller then marks the proposal reverted. The
// merge request is the truth about what happened.
func (r *DriftProposalReconciler) trackProposed(ctx context.Context, dp *backflowv1alpha1.DriftProposal) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	ref := dp.Status.MergeRequest

	c, err := r.clientFor(ctx, dp)
	if err != nil {
		var held *heldError
		if errors.As(err, &held) {
			return r.trackingFailure(ctx, dp, held.cause, 0)
		}
		return ctrl.Result{}, err
	}
	cur, err := c.provider.GetMergeRequest(ctx, c.project, ref.Number)
	if err != nil {
		return r.trackingFailure(ctx, dp, err, scm.RetryAfter(err))
	}

	orig := dp.DeepCopy()
	switch cur.State {
	case scm.StateMerged:
		dp.Status.Phase = backflowv1alpha1.PhaseMerged
		dp.Status.CommitSHA = cur.MergeCommitSHA
		ref.State = string(scm.StateMerged)
		dp.Status.Message = mergedMessage(ref.URL)
		log.Info("Merge request merged", "proposal", dp.Name, "url", ref.URL, "commit", cur.MergeCommitSHA)
		return ctrl.Result{}, r.patchStatus(ctx, dp, orig)
	case scm.StateClosed:
		dp.Status.Phase = backflowv1alpha1.PhaseRejected
		ref.State = string(scm.StateClosed)
		dp.Status.Message = closedMessage(ref.URL)
		log.Info("Merge request closed without merging", "proposal", dp.Name, "url", ref.URL)
		return ctrl.Result{}, r.patchStatus(ctx, dp, orig)
	}

	// Still open.
	if retiring(dp) && applyLifecycle(dp) {
		if err := r.patchStatus(ctx, dp, orig); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("Proposal phase set", "proposal", dp.Name, "phase", dp.Status.Phase)
		return r.cleanup(ctx, dp)
	}
	ref.State = string(scm.StateOpen)
	if strings.HasPrefix(dp.Status.Message, trackingFailurePrefix) {
		// The poll works again: the message of the failure is stale.
		dp.Status.Message = openMessage(dp, cur)
	}
	if err := r.patchStatus(ctx, dp, orig); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.syncLiveReverted(ctx, c, dp); err != nil {
		return r.trackingFailure(ctx, dp, err, scm.RetryAfter(err))
	}
	return ctrl.Result{RequeueAfter: r.trackEvery()}, nil
}

// trackingFailurePrefix starts the message of a proposal whose merge request
// could not be read.
const trackingFailurePrefix = "Cannot read the state of merge request"

// openMessage is the message of a proposal whose merge request is open.
func openMessage(dp *backflowv1alpha1.DriftProposal, cur *scm.MergeRequest) string {
	ref := dp.Status.MergeRequest
	if rev := dp.Annotations[annotationLiveReverted]; rev != "" &&
		meta.IsStatusConditionTrue(dp.Status.Conditions, conditionLiveReverted) {
		return fmt.Sprintf("The cluster was reset to Git by an Argo CD sync of %s; the change waits in merge request %s.", rev, ref.URL)
	}
	target := cur.TargetBranch
	if target == "" {
		target = "its target branch"
	}
	return fmt.Sprintf("Merge request %s proposes the change against %s.", ref.URL, target)
}

// syncLiveReverted keeps the LiveReverted condition in line with the
// annotation the drift controller sets. When Argo CD reset the cluster to Git
// by syncing a new revision, the proposal stays Proposed: one comment on the
// merge request says why the cluster no longer shows the change and that
// merging makes it permanent. When the change is live again, the condition
// goes False.
func (r *DriftProposalReconciler) syncLiveReverted(ctx context.Context, c *mrClient, dp *backflowv1alpha1.DriftProposal) error {
	rev := dp.Annotations[annotationLiveReverted]
	cond := meta.FindStatusCondition(dp.Status.Conditions, conditionLiveReverted)
	switch {
	case rev != "" && (cond == nil || cond.Status != metav1.ConditionTrue):
		if err := c.provider.CommentOnMergeRequest(ctx, c.project, dp.Status.MergeRequest.Number, liveRevertedComment(rev)); err != nil {
			return err
		}
		orig := dp.DeepCopy()
		dp.Status.Message = fmt.Sprintf("The cluster was reset to Git by an Argo CD sync of %s; the change waits in merge request %s.",
			rev, dp.Status.MergeRequest.URL)
		meta.SetStatusCondition(&dp.Status.Conditions, metav1.Condition{
			Type: conditionLiveReverted, Status: metav1.ConditionTrue, Reason: reasonSyncedNewRevision,
			Message: dp.Status.Message, ObservedGeneration: dp.Generation,
		})
		logf.FromContext(ctx).Info("Live change reset by a sync; kept in its merge request", "proposal", dp.Name, "revision", rev)
		return r.patchStatus(ctx, dp, orig)
	case rev == "" && cond != nil && cond.Status == metav1.ConditionTrue:
		orig := dp.DeepCopy()
		dp.Status.Message = fmt.Sprintf("The change is live again; merge request %s proposes it.", dp.Status.MergeRequest.URL)
		meta.SetStatusCondition(&dp.Status.Conditions, metav1.Condition{
			Type: conditionLiveReverted, Status: metav1.ConditionFalse, Reason: reasonLiveChangeReturned,
			Message: dp.Status.Message, ObservedGeneration: dp.Generation,
		})
		return r.patchStatus(ctx, dp, orig)
	}
	return nil
}

// trackingFailure records that the merge request could not be read and tries
// again later. The phase does not change: not being able to look is not a
// verdict. A pending superseded or reverted annotation waits too, because the
// merge request may have been merged meanwhile.
func (r *DriftProposalReconciler) trackingFailure(ctx context.Context, dp *backflowv1alpha1.DriftProposal,
	cause error, advised time.Duration) (ctrl.Result, error) {
	orig := dp.DeepCopy()
	dp.Status.Message = fmt.Sprintf("%s %s: %v", trackingFailurePrefix, dp.Status.MergeRequest.URL, cause)
	if err := r.patchStatus(ctx, dp, orig); err != nil {
		return ctrl.Result{}, err
	}
	delay := r.trackEvery()
	if advised > delay {
		delay = min(advised, retryMax)
	}
	logf.FromContext(ctx).Info("Cannot read the merge request state; will retry",
		"proposal", dp.Name, "cause", cause.Error(), "retryAfter", delay.String())
	return ctrl.Result{RequeueAfter: delay}, nil
}

// needsCleanup reports whether a Superseded or Reverted proposal still has a
// merge request or a branch to clean up. A recorded branch is enough: the
// operator may have stopped after pushing it and before recording the merge
// request.
func needsCleanup(dp *backflowv1alpha1.DriftProposal) bool {
	if dp.Status.MergeRequest == nil && dp.Status.Branch == "" {
		return false
	}
	if dp.Status.Phase != backflowv1alpha1.PhaseSuperseded && dp.Status.Phase != backflowv1alpha1.PhaseReverted {
		return false
	}
	return !meta.IsStatusConditionTrue(dp.Status.Conditions, conditionCleanedUp)
}

// cleanup closes the merge request of a Superseded or Reverted proposal with
// a comment that says why, then deletes its branch. The merge request is the
// recorded one, or else the one found by the recorded branch in any state: it
// may have been opened just before the operator stopped. Every step can be
// repeated, and progress is kept in the CleanedUp condition, so a failure
// part-way is picked up again.
func (r *DriftProposalReconciler) cleanup(ctx context.Context, dp *backflowv1alpha1.DriftProposal) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	ref := dp.Status.MergeRequest
	branch := dp.Status.Branch
	if ref != nil && ref.Branch != "" {
		branch = ref.Branch
	}

	c, err := r.clientFor(ctx, dp)
	if err != nil {
		var held *heldError
		if errors.As(err, &held) {
			return r.cleanupPending(ctx, dp, held.cause, 0)
		}
		return ctrl.Result{}, err
	}

	var cur *scm.MergeRequest
	done := reasonCleanedUp
	if ref != nil {
		cur, err = c.provider.GetMergeRequest(ctx, c.project, ref.Number)
		if errors.Is(err, scm.ErrNotFound) {
			cur, err, done = nil, nil, reasonAlreadyClosed // gone: only the branch is left
		}
	} else {
		cur, err = c.provider.FindMergeRequest(ctx, c.project, branch)
	}
	if err != nil {
		return r.cleanupPending(ctx, dp, err, scm.RetryAfter(err))
	}
	where := "branch " + branch
	if cur != nil {
		where = fmt.Sprintf("Merge request %s", cur.URL)
	} else if ref != nil {
		where = fmt.Sprintf("Merge request %s", ref.URL)
	}
	switch {
	case cur != nil && cur.State == scm.StateMerged:
		// Merged while the proposal was being retired: the change is in Git,
		// so there is nothing to close and the branch is left to the host.
		return ctrl.Result{}, r.finishCleanup(ctx, dp, cur, string(scm.StateMerged), reasonAlreadyMerged,
			fmt.Sprintf("Merge request %s was merged.", cur.URL))
	case cur != nil && cur.State == scm.StateClosed:
		done = reasonAlreadyClosed
	}

	// Only a branch that holds nothing but this proposal's own commits is
	// deleted. What cannot be told is retried; it is never guessed.
	keptWhy := ""
	deleteBranch := branch != ""
	if deleteBranch {
		exists, why, err := r.branchIsOurs(ctx, dp, c.access, branch)
		if err != nil {
			return r.cleanupPending(ctx, dp, err, scm.RetryAfter(err))
		}
		switch {
		case !exists:
			deleteBranch = false // nothing left to delete
		case why != "":
			deleteBranch, keptWhy = false, why
		}
	}

	if cur != nil && cur.State == scm.StateOpen {
		// Comment first so the reason is on the page when it closes, and
		// remember it so a retry of the close does not comment again.
		if prog := meta.FindStatusCondition(dp.Status.Conditions, conditionCleanedUp); prog == nil || prog.Reason != reasonCommented {
			if err := c.provider.CommentOnMergeRequest(ctx, c.project, cur.Number, closingComment(dp, branchIf(keptWhy != "", branch), keptWhy)); err != nil {
				return r.cleanupPending(ctx, dp, err, scm.RetryAfter(err))
			}
			if err := r.setCleanedUp(ctx, dp, metav1.ConditionFalse, reasonCommented, "Commented on the merge request; closing it."); err != nil {
				return ctrl.Result{}, err
			}
		}
		if err := c.provider.CloseMergeRequest(ctx, c.project, cur.Number); err != nil {
			return r.cleanupPending(ctx, dp, err, scm.RetryAfter(err))
		}
	}

	state := string(scm.StateClosed)
	if keptWhy != "" {
		log.Info("Merge request closed; branch left in place", "proposal", dp.Name, "merge request", where, "branch", branch, "why", keptWhy)
		return ctrl.Result{}, r.finishCleanup(ctx, dp, cur, state, reasonBranchKept,
			fmt.Sprintf("%s closed. Branch %s was left in place: %s.", where, branch, keptWhy))
	}
	if deleteBranch {
		if err := c.provider.DeleteBranch(ctx, c.project, branch); err != nil && !errors.Is(err, scm.ErrNotFound) {
			return r.cleanupPending(ctx, dp, err, scm.RetryAfter(err))
		}
	}
	log.Info("Merge request closed and branch deleted", "proposal", dp.Name, "merge request", where, "branch", branch)
	message := fmt.Sprintf("%s closed and branch %s deleted.", where, branch)
	if cur == nil && ref == nil {
		message = fmt.Sprintf("Branch %s deleted.", branch)
	}
	return ctrl.Result{}, r.finishCleanup(ctx, dp, cur, state, done, message)
}

func branchIf(cond bool, branch string) string {
	if cond {
		return branch
	}
	return ""
}

// branchScanLimit bounds how many of the proposal's own commits are looked at
// on its branch. A branch with more is not something Backflow built.
const branchScanLimit = 50

// branchIsOurs says whether a branch holds only this proposal's work: its tip
// carries the proposal's "Proposal:" trailer, and so does every commit below
// it down to the base (the first commit without it, which belongs to the
// target branch). Each of those commits has exactly one parent. When it does
// not, why says so and the branch is left alone: someone else may have pushed
// to it. exists is false when the branch is already gone.
func (r *DriftProposalReconciler) branchIsOurs(ctx context.Context, dp *backflowv1alpha1.DriftProposal,
	access *scmAccess, branch string) (exists bool, why string, err error) {
	url := dp.Spec.Source.RepoURL
	tip, err := r.Writer.BranchTip(ctx, url, branch, access.auth)
	if err != nil {
		if errors.Is(err, gitrepo.ErrBranchNotFound) {
			return false, "", nil
		}
		return false, "", err
	}
	log, _, err := r.Writer.FirstParentLog(ctx, url, tip, "", branchScanLimit+1, access.auth)
	if err != nil {
		return false, "", err
	}
	ours := 0
	for _, e := range log {
		if !hasProposalTrailer(e.Message, dp.Name) {
			break
		}
		if e.Parents != 1 {
			return true, fmt.Sprintf("commit %s on it is a merge or a root commit, not this proposal's work", shortSHA(e.SHA)), nil
		}
		ours++
	}
	switch {
	case ours == 0:
		return true, fmt.Sprintf("its tip %s does not carry this proposal's trailer, so someone else pushed to it", shortSHA(tip)), nil
	case ours > branchScanLimit:
		return true, fmt.Sprintf("it holds more than %d commits", branchScanLimit), nil
	}
	return true, "", nil
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// setCleanedUp records progress of the cleanup.
func (r *DriftProposalReconciler) setCleanedUp(ctx context.Context, dp *backflowv1alpha1.DriftProposal,
	status metav1.ConditionStatus, reason, message string) error {
	orig := dp.DeepCopy()
	meta.SetStatusCondition(&dp.Status.Conditions, metav1.Condition{
		Type: conditionCleanedUp, Status: status, Reason: reason, Message: message, ObservedGeneration: dp.Generation,
	})
	return r.patchStatus(ctx, dp, orig)
}

// finishCleanup records that nothing is left to do for the merge request.
func (r *DriftProposalReconciler) finishCleanup(ctx context.Context, dp *backflowv1alpha1.DriftProposal,
	found *scm.MergeRequest, mrState, reason, message string) error {
	orig := dp.DeepCopy()
	switch {
	case dp.Status.MergeRequest != nil:
		dp.Status.MergeRequest.State = mrState
	case found != nil:
		// Found by its branch: record it, so the proposal says what it opened.
		dp.Status.MergeRequest = &backflowv1alpha1.MergeRequestRef{
			URL: found.URL, Number: found.Number, Branch: dp.Status.Branch, State: mrState,
		}
	}
	meta.SetStatusCondition(&dp.Status.Conditions, metav1.Condition{
		Type: conditionCleanedUp, Status: metav1.ConditionTrue, Reason: reason, Message: message, ObservedGeneration: dp.Generation,
	})
	return r.patchStatus(ctx, dp, orig)
}

// cleanupPending records why the cleanup could not finish and asks for
// another attempt. Progress already made (the comment) is kept.
func (r *DriftProposalReconciler) cleanupPending(ctx context.Context, dp *backflowv1alpha1.DriftProposal,
	cause error, advised time.Duration) (ctrl.Result, error) {
	reason := reasonCleanupFailed
	if c := meta.FindStatusCondition(dp.Status.Conditions, conditionCleanedUp); c != nil && c.Reason == reasonCommented {
		reason = reasonCommented
	}
	delay := r.retryDelayFor(dp, conditionCleanedUp)
	if advised > delay {
		delay = min(advised, retryMax)
	}
	if err := r.setCleanedUp(ctx, dp, metav1.ConditionFalse, reason, cause.Error()); err != nil {
		return ctrl.Result{}, err
	}
	logf.FromContext(ctx).Info("Cannot finish closing the merge request yet; will retry",
		"proposal", dp.Name, "cause", cause.Error(), "retryAfter", delay.String())
	return ctrl.Result{RequeueAfter: delay}, nil
}
