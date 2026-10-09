package controller

import (
	"context"
	"errors"
	"fmt"
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
	reasonCleanupFailed = "CleanupPending"
)

// mrClient is the provider client and project of a proposal's repository.
type mrClient struct {
	provider scm.Provider
	project  string
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
	return &mrClient{provider: provider, project: project}, nil
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
		dp.Status.Message = fmt.Sprintf("Merge request %s was merged; Git now holds the live change.", ref.URL)
		log.Info("Merge request merged", "proposal", dp.Name, "url", ref.URL, "commit", cur.MergeCommitSHA)
		return ctrl.Result{}, r.patchStatus(ctx, dp, orig)
	case scm.StateClosed:
		dp.Status.Phase = backflowv1alpha1.PhaseRejected
		ref.State = string(scm.StateClosed)
		dp.Status.Message = fmt.Sprintf("Merge request %s was closed without merging; Argo CD can revert the cluster to what Git says.", ref.URL)
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
	if err := r.patchStatus(ctx, dp, orig); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.syncLiveReverted(ctx, c, dp); err != nil {
		return r.trackingFailure(ctx, dp, err, scm.RetryAfter(err))
	}
	return ctrl.Result{RequeueAfter: r.trackEvery()}, nil
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
	dp.Status.Message = fmt.Sprintf("Cannot read the state of merge request %s: %v", dp.Status.MergeRequest.URL, cause)
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
// merge request to close.
func needsCleanup(dp *backflowv1alpha1.DriftProposal) bool {
	if dp.Status.MergeRequest == nil {
		return false
	}
	if dp.Status.Phase != backflowv1alpha1.PhaseSuperseded && dp.Status.Phase != backflowv1alpha1.PhaseReverted {
		return false
	}
	return !meta.IsStatusConditionTrue(dp.Status.Conditions, conditionCleanedUp)
}

// cleanup closes the merge request of a Superseded or Reverted proposal with
// a comment that says why, then deletes its branch. Every step can be
// repeated, and progress is kept in the CleanedUp condition, so a failure
// part-way is picked up again.
func (r *DriftProposalReconciler) cleanup(ctx context.Context, dp *backflowv1alpha1.DriftProposal) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	ref := dp.Status.MergeRequest

	c, err := r.clientFor(ctx, dp)
	if err != nil {
		var held *heldError
		if errors.As(err, &held) {
			return r.cleanupPending(ctx, dp, held.cause, 0)
		}
		return ctrl.Result{}, err
	}

	cur, err := c.provider.GetMergeRequest(ctx, c.project, ref.Number)
	done := reasonCleanedUp
	switch {
	case errors.Is(err, scm.ErrNotFound):
		cur, done = nil, reasonAlreadyClosed // gone: only the branch is left
	case err != nil:
		return r.cleanupPending(ctx, dp, err, scm.RetryAfter(err))
	case cur.State == scm.StateMerged:
		// Merged while the proposal was being retired: the change is in Git,
		// so there is nothing to close and the branch is left to the host.
		return ctrl.Result{}, r.finishCleanup(ctx, dp, string(scm.StateMerged), reasonAlreadyMerged,
			fmt.Sprintf("Merge request %s was merged.", ref.URL))
	case cur.State == scm.StateClosed:
		done = reasonAlreadyClosed
	}

	if cur != nil && cur.State == scm.StateOpen {
		// Comment first so the reason is on the page when it closes, and
		// remember it so a retry of the close does not comment again.
		if prog := meta.FindStatusCondition(dp.Status.Conditions, conditionCleanedUp); prog == nil || prog.Reason != reasonCommented {
			if err := c.provider.CommentOnMergeRequest(ctx, c.project, ref.Number, closingComment(dp)); err != nil {
				return r.cleanupPending(ctx, dp, err, scm.RetryAfter(err))
			}
			if err := r.setCleanedUp(ctx, dp, metav1.ConditionFalse, reasonCommented, "Commented on the merge request; closing it."); err != nil {
				return ctrl.Result{}, err
			}
		}
		if err := c.provider.CloseMergeRequest(ctx, c.project, ref.Number); err != nil {
			return r.cleanupPending(ctx, dp, err, scm.RetryAfter(err))
		}
	}

	if err := c.provider.DeleteBranch(ctx, c.project, ref.Branch); err != nil && !errors.Is(err, scm.ErrNotFound) {
		return r.cleanupPending(ctx, dp, err, scm.RetryAfter(err))
	}
	log.Info("Merge request closed and branch deleted", "proposal", dp.Name, "url", ref.URL, "branch", ref.Branch)
	return ctrl.Result{}, r.finishCleanup(ctx, dp, string(scm.StateClosed), done,
		fmt.Sprintf("Merge request %s closed and branch %s deleted.", ref.URL, ref.Branch))
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
	mrState, reason, message string) error {
	orig := dp.DeepCopy()
	dp.Status.MergeRequest.State = mrState
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
