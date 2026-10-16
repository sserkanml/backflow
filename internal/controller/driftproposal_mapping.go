package controller

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"
	"unicode/utf8"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	backflowv1alpha1 "github.com/sserkanml/backflow/api/v1alpha1"
	"github.com/sserkanml/backflow/internal/gitrepo"
	"github.com/sserkanml/backflow/internal/mapping"
)

const (
	conditionMapped = "Mapped"

	// Retries of a source that cannot be read start after retryMin and back
	// off to at most retryMax, indefinitely. "We could not look" is not
	// "we looked and could not map", so it never turns into Unmapped.
	retryMin = 5 * time.Second
	retryMax = 10 * time.Minute

	// maxDiffBytes is the largest diff stored in a proposal's status.
	maxDiffBytes  = 32 << 10
	diffTruncated = "\n... diff truncated ...\n"
)

// Reasons of the Mapped condition.
const (
	reasonMapped                = "Mapped"
	reasonNotFound              = "NotFound"
	reasonAmbiguous             = "Ambiguous"
	reasonVerificationFailed    = "VerificationFailed"
	reasonCannotApply           = "CannotApply"
	reasonUnsupportedSourceType = "UnsupportedSourceType"
	reasonUnsupportedFileFormat = "UnsupportedFileFormat"
	reasonInvalidSourceOptions  = "InvalidSourceOptions"
	reasonRepositoryUnavailable = "RepositoryUnavailable"
	reasonRepositoryAuthFailed  = "RepositoryAuthFailed"
	reasonRepositoryNotFound    = "RepositoryNotFound"
	reasonRevisionNotFound      = "RevisionNotFound"
	reasonApplicationNotFound   = "ApplicationNotFound"
	reasonRepositoryURLHasCreds = "RepositoryURLHasCredentials"
	// The Application or the BackflowPolicy could not be read: not a verdict, so retried.
	reasonApplicationUnavailable = "ApplicationUnavailable"
)

// SourceTree is a read-only view of a repository at one commit.
type SourceTree interface {
	fs.FS
	Close() error
}

// RepositoryReader opens a repository at an exact commit. auth is nil for
// anonymous access.
type RepositoryReader interface {
	Open(ctx context.Context, repoURL, sha string, auth *gitrepo.Auth) (SourceTree, error)
}

// CacheReader reads repositories through a gitrepo.Cache.
type CacheReader struct{ Cache *gitrepo.Cache }

func (c CacheReader) Open(ctx context.Context, repoURL, sha string, auth *gitrepo.Auth) (SourceTree, error) {
	v, err := c.Cache.Open(ctx, repoURL, sha, auth)
	if err != nil {
		return nil, err
	}
	return v, nil
}

// mappingSettled reports whether mapping has reached a result that is not
// retried: it succeeded, or the proposal is Unmapped.
func mappingSettled(dp *backflowv1alpha1.DriftProposal) bool {
	if dp.Status.Phase == backflowv1alpha1.PhaseUnmapped {
		return true
	}
	return meta.IsStatusConditionTrue(dp.Status.Conditions, conditionMapped)
}

func (r *DriftProposalReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// retryDelay is how long to wait before trying again. It grows with the time
// the source has been unreadable (half of it), between retryMin and retryMax.
func (r *DriftProposalReconciler) retryDelay(dp *backflowv1alpha1.DriftProposal) time.Duration {
	return r.retryDelayFor(dp, conditionMapped)
}

// retryDelayFor is retryDelay for the given condition type.
func (r *DriftProposalReconciler) retryDelayFor(dp *backflowv1alpha1.DriftProposal, conditionType string) time.Duration {
	c := meta.FindStatusCondition(dp.Status.Conditions, conditionType)
	if c == nil || c.Status != metav1.ConditionFalse {
		return retryMin
	}
	d := r.now().Sub(c.LastTransitionTime.Time) / 2
	return min(max(d, retryMin), retryMax)
}

// mapProposal maps a proposal onto the file in Git that defines the resource
// and records the verified result. It never writes to Git.
func (r *DriftProposalReconciler) mapProposal(ctx context.Context, dp *backflowv1alpha1.DriftProposal) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if t := dp.Spec.Source.Type; t != backflowv1alpha1.SourceDirectory && t != backflowv1alpha1.SourceHelm {
		return ctrl.Result{}, r.unmapped(ctx, dp, reasonUnsupportedSourceType,
			fmt.Sprintf("Mapping %s sources is not supported yet; only Directory and Helm sources are mapped.", t))
	}

	if hasCredentials(dp.Spec.Source.RepoURL) {
		// The URL would put a credential into every log line, status and
		// request that mentions it. Access comes from an ScmConnection.
		return ctrl.Result{}, r.unmapped(ctx, dp, reasonRepositoryURLHasCreds, fmt.Sprintf(
			"The repository URL %s of the Argo CD Application contains credentials, which Backflow does not use. "+
				"Remove them from the Application and give Backflow access with an ScmConnection.", redactText(dp.Spec.Source.RepoURL)))
	}

	app, err := r.application(ctx, dp)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return r.retry(ctx, dp, reasonApplicationNotFound,
				fmt.Errorf("the Argo CD Application %s/%s was not found", dp.Spec.Application.Namespace, dp.Spec.Application.Name))
		}
		return r.retry(ctx, dp, reasonApplicationUnavailable, fmt.Errorf("cannot read the Argo CD Application: %w", err))
	}

	if dp.Spec.Source.Type == backflowv1alpha1.SourceHelm {
		// A chart that is not in the Git repository has nothing to read there.
		if err := helmSourceProblem(app); err != nil {
			return ctrl.Result{}, r.unmapped(ctx, dp, reasonUnsupportedSourceType, err.Error())
		}
	}

	auth, err := r.repositoryAuth(ctx, dp)
	if err != nil {
		switch {
		case errors.Is(err, gitrepo.ErrAuth):
			return r.retry(ctx, dp, reasonRepositoryAuthFailed, err)
		case apierrors.IsNotFound(err):
			return r.retry(ctx, dp, reasonPolicyNotFound, fmt.Errorf("BackflowPolicy %q not found", dp.Spec.PolicyName))
		}
		return r.retry(ctx, dp, reasonRepositoryUnavailable, fmt.Errorf("cannot read the access to the repository: %w", err))
	}

	tree, err := r.Repos.Open(ctx, dp.Spec.Source.RepoURL, dp.Spec.Source.Revision, auth)
	if err != nil {
		switch {
		case errors.Is(err, gitrepo.ErrRevisionNotFound):
			return ctrl.Result{}, r.unmapped(ctx, dp, reasonRevisionNotFound, err.Error())
		case errors.Is(err, gitrepo.ErrRepositoryNotFound):
			// GitLab and GitHub answer 404 for a private repository the
			// caller cannot see, so this usually means "no access": a missing
			// ScmConnection or a token without permission, which the user can
			// fix. Only a missing commit is permanent.
			return r.retry(ctx, dp, reasonRepositoryNotFound, err)
		case errors.Is(err, gitrepo.ErrAuth):
			return r.retry(ctx, dp, reasonRepositoryAuthFailed, err)
		}
		return r.retry(ctx, dp, reasonRepositoryUnavailable, err)
	}
	defer func() { _ = tree.Close() }()

	change, err := r.mapTree(ctx, dp, app, tree, dp.Spec.Source.Revision)
	if err != nil {
		if verdict, ok := errors.AsType[*verdictError](err); ok {
			return ctrl.Result{}, r.unmapped(ctx, dp, verdict.reason, verdict.err.Error())
		}
		if held, ok := errors.AsType[*heldError](err); ok {
			return r.retry(ctx, dp, held.reason, held.cause)
		}
		if ctx.Err() != nil {
			return ctrl.Result{}, ctx.Err()
		}
		// Reading the repository failed: not a verdict, so it is retried
		// with the same backoff and cap as an unavailable repository.
		return r.retry(ctx, dp, reasonRepositoryUnavailable, err)
	}

	orig := dp.DeepCopy()
	edits := change.edits()
	dp.Status.Mapping = &backflowv1alpha1.Mapping{
		Strategy: change.strategy,
		Edits:    edits,
		Verified: true,
		Diff:     truncateDiff(change.diff()),
	}
	dp.Status.Message = change.message()
	meta.SetStatusCondition(&dp.Status.Conditions, metav1.Condition{
		Type: conditionMapped, Status: metav1.ConditionTrue, Reason: reasonMapped,
		Message: dp.Status.Message, ObservedGeneration: dp.Generation,
	})
	if err := r.patchStatus(ctx, dp, orig); err != nil {
		return ctrl.Result{}, err
	}
	log.Info("Proposal mapped", "proposal", dp.Name, "strategy", change.strategy, "files", change.paths(), "edits", len(edits))
	return ctrl.Result{}, nil
}

// mappingReason maps a mapping error to a Mapped condition reason. ok is
// false for errors that are not a verdict about the source, such as a failed
// read, which are retried.
func mappingReason(err error) (reason string, ok bool) {
	switch {
	case errors.Is(err, mapping.ErrNotFound):
		return reasonNotFound, true
	case errors.Is(err, mapping.ErrAmbiguous):
		return reasonAmbiguous, true
	case errors.Is(err, mapping.ErrVerificationFailed):
		return reasonVerificationFailed, true
	case errors.Is(err, mapping.ErrCannotApply):
		return reasonCannotApply, true
	case errors.Is(err, mapping.ErrUnsupportedFileFormat):
		return reasonUnsupportedFileFormat, true
	case errors.Is(err, mapping.ErrInvalidGlob):
		return reasonInvalidSourceOptions, true
	}
	return "", false
}

// unmapped records that the change cannot be traced back to Git with
// certainty. A human decides what to do with it.
func (r *DriftProposalReconciler) unmapped(ctx context.Context, dp *backflowv1alpha1.DriftProposal, reason, message string) error {
	message = redactText(message)
	orig := dp.DeepCopy()
	dp.Status.Phase = backflowv1alpha1.PhaseUnmapped
	dp.Status.Message = message
	dp.Status.Mapping = &backflowv1alpha1.Mapping{Strategy: backflowv1alpha1.StrategyUnmapped, Reason: message}
	meta.SetStatusCondition(&dp.Status.Conditions, metav1.Condition{
		Type: conditionMapped, Status: metav1.ConditionFalse, Reason: reason,
		Message: message, ObservedGeneration: dp.Generation,
	})
	logf.FromContext(ctx).Info("Proposal unmapped", "proposal", dp.Name, "reason", reason, "message", message)
	return r.patchStatus(ctx, dp, orig)
}

// retry records that the source cannot be read right now and asks for
// another attempt later. It never gives up: the Mapped condition stays False
// with the reason, the phase stays Mapping, and the proposal is mapped as
// soon as the problem is gone.
func (r *DriftProposalReconciler) retry(ctx context.Context, dp *backflowv1alpha1.DriftProposal, reason string, cause error) (ctrl.Result, error) {
	cause = errors.New(redactText(cause.Error()))
	delay := r.retryDelay(dp)
	orig := dp.DeepCopy()
	meta.SetStatusCondition(&dp.Status.Conditions, metav1.Condition{
		Type: conditionMapped, Status: metav1.ConditionFalse, Reason: reason,
		Message: cause.Error(), ObservedGeneration: dp.Generation,
	})
	if err := r.patchStatus(ctx, dp, orig); err != nil {
		return ctrl.Result{}, err
	}
	logf.FromContext(ctx).Error(cause, "Cannot map the proposal yet; will retry",
		"proposal", dp.Name, "reason", reason, "retryAfter", delay.String())
	return ctrl.Result{RequeueAfter: delay}, nil
}

// heldReasons are the reasons of a False Mapped or Proposed condition that the
// user fixes by changing a Secret, an ScmConnection or a BackflowPolicy: access
// that is missing or refused, or a policy in ReportOnly mode that is switched to
// one that writes to Git.
var heldReasons = map[string]bool{
	reasonRepositoryAuthFailed: true, reasonRepositoryNotFound: true,
	reasonMissingScmConnection: true, reasonPolicyNotFound: true,
	reasonScmAuthFailed: true, reasonScmForbidden: true, reasonScmProjectNotFound: true,
	reasonReportOnly: true,
}

// heldProposals enqueues the proposals in the namespace of a changed Secret,
// ScmConnection or BackflowPolicy that are waiting for the user to change
// something: access to be fixed (the credentials were rejected, the repository
// is not visible to them, no ScmConnection matched yet), or the policy to leave
// ReportOnly mode, which writes nothing to Git. A change of a policy only
// enqueues the proposals of that policy.
func (r *DriftProposalReconciler) heldProposals(ctx context.Context, obj client.Object) []reconcile.Request {
	var list backflowv1alpha1.DriftProposalList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	policy, isPolicy := obj.(*backflowv1alpha1.BackflowPolicy)
	var requests []reconcile.Request
	for i := range list.Items {
		p := &list.Items[i]
		if p.Status.Phase != backflowv1alpha1.PhaseMapping || (isPolicy && p.Spec.PolicyName != policy.Name) {
			continue
		}
		for _, t := range []string{conditionMapped, conditionProposed} {
			if c := meta.FindStatusCondition(p.Status.Conditions, t); c != nil && c.Status == metav1.ConditionFalse && heldReasons[c.Reason] {
				requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(p)})
				break
			}
		}
	}
	return requests
}

// directorySourceInfo is what mapping needs from the Argo CD Application.
type directorySourceInfo struct {
	options              mapping.DirectoryOptions
	destinationNamespace string
}

// directoryInfoOf reads the Directory options and destination namespace from
// the Application. Like the policy controller, it uses the first source of a
// multi-source Application.
func directoryInfoOf(app *unstructured.Unstructured) directorySourceInfo {
	src, found, _ := unstructured.NestedMap(app.Object, "spec", "source")
	if !found {
		if sources, ok, _ := unstructured.NestedSlice(app.Object, "spec", "sources"); ok && len(sources) > 0 {
			src, _ = sources[0].(map[string]any)
		}
	}
	var info directorySourceInfo
	info.options.Recurse, _, _ = unstructured.NestedBool(src, "directory", "recurse")
	info.options.Include, _, _ = unstructured.NestedString(src, "directory", "include")
	info.options.Exclude, _, _ = unstructured.NestedString(src, "directory", "exclude")
	info.destinationNamespace, _, _ = unstructured.NestedString(app.Object, "spec", "destination", "namespace")
	return info
}

// mapTree maps the proposal onto the files of tree, the repository at sha,
// with the strategy of the proposal's source type. Errors are a *verdictError
// (the change cannot be traced back), a *heldError (something outside the
// repository is not usable now), or a failure to read the repository.
func (r *DriftProposalReconciler) mapTree(ctx context.Context, dp *backflowv1alpha1.DriftProposal,
	app *unstructured.Unstructured, tree fs.FS, sha string) (*mappedChange, error) {
	if dp.Spec.Source.Type == backflowv1alpha1.SourceHelm {
		return r.mapHelm(ctx, dp, app, tree, sha)
	}
	src := directoryInfoOf(app)
	res, err := mapping.Map(tree, dp.Spec.Source.Path, src.options, src.destinationNamespace,
		mapping.ResourceID{
			Group: dp.Spec.Resource.Group, Kind: dp.Spec.Resource.Kind,
			Namespace: dp.Spec.Resource.Namespace, Name: dp.Spec.Resource.Name,
		}, dp.Spec.Changes)
	if err != nil {
		if reason, verdict := mappingReason(err); verdict {
			return nil, &verdictError{err: err, reason: reason}
		}
		return nil, err
	}
	return &mappedChange{strategy: backflowv1alpha1.StrategyDirect, results: []mapping.Result{*res}}, nil
}

// repositoryAuth returns the credentials and CA bundle of the ScmConnection
// the policy matched to the Application's repository, or nil (anonymous) when
// there is none. A connection whose token or CA bundle cannot be read is an
// ErrAuth failure.
func (r *DriftProposalReconciler) repositoryAuth(ctx context.Context, dp *backflowv1alpha1.DriftProposal) (*gitrepo.Auth, error) {
	var policy backflowv1alpha1.BackflowPolicy
	if err := r.Get(ctx, types.NamespacedName{Namespace: dp.Namespace, Name: dp.Spec.PolicyName}, &policy); err != nil {
		return nil, err
	}
	access, err := r.accessFor(ctx, dp, &policy)
	if err != nil || access == nil {
		return nil, err
	}
	return access.auth, nil
}

// truncateDiff keeps a diff within maxDiffBytes, cutting at a line boundary.
func truncateDiff(diff string) string {
	if len(diff) <= maxDiffBytes {
		return diff
	}
	cut := maxDiffBytes - len(diffTruncated)
	if i := strings.LastIndexByte(diff[:cut], '\n'); i >= 0 {
		cut = i + 1
	}
	for cut > 0 && !utf8.ValidString(diff[:cut]) {
		cut--
	}
	return diff[:cut] + diffTruncated
}
