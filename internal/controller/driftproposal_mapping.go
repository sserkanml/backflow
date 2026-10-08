package controller

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
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
	c := meta.FindStatusCondition(dp.Status.Conditions, conditionMapped)
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

	if dp.Spec.Source.Type != backflowv1alpha1.SourceDirectory {
		return ctrl.Result{}, r.unmapped(ctx, dp, reasonUnsupportedSourceType,
			fmt.Sprintf("Mapping %s sources is not supported yet; only Directory sources are mapped.", dp.Spec.Source.Type))
	}

	src, err := r.directorySource(ctx, dp)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return r.retry(ctx, dp, reasonApplicationNotFound,
				fmt.Errorf("Argo CD Application %s/%s not found", dp.Spec.Application.Namespace, dp.Spec.Application.Name))
		}
		return ctrl.Result{}, err
	}

	auth, err := r.repositoryAuth(ctx, dp)
	if err != nil {
		if errors.Is(err, gitrepo.ErrAuth) {
			return r.retry(ctx, dp, reasonRepositoryAuthFailed, err)
		}
		return ctrl.Result{}, err
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

	res, err := mapping.Map(tree, dp.Spec.Source.Path, src.options, src.destinationNamespace,
		mapping.ResourceID{
			Group: dp.Spec.Resource.Group, Kind: dp.Spec.Resource.Kind,
			Namespace: dp.Spec.Resource.Namespace, Name: dp.Spec.Resource.Name,
		}, dp.Spec.Changes)
	if err != nil {
		reason, ok := mappingReason(err)
		if !ok {
			return ctrl.Result{}, err // reading the repository failed; retried with backoff
		}
		return ctrl.Result{}, r.unmapped(ctx, dp, reason, err.Error())
	}

	orig := dp.DeepCopy()
	edits := make([]backflowv1alpha1.SourceEdit, len(res.Edits))
	for i, e := range res.Edits {
		edits[i] = backflowv1alpha1.SourceEdit{File: e.File, Location: e.Location, Value: e.Value, ChangeIndex: int32(e.ChangeIndex)}
	}
	dp.Status.Mapping = &backflowv1alpha1.Mapping{
		Strategy: backflowv1alpha1.StrategyDirect,
		Edits:    edits,
		Verified: true,
		Diff:     truncateDiff(res.Diff),
	}
	dp.Status.Message = fmt.Sprintf("Mapped to %s; the edit reproduces the live state and is verified.", res.File)
	meta.SetStatusCondition(&dp.Status.Conditions, metav1.Condition{
		Type: conditionMapped, Status: metav1.ConditionTrue, Reason: reasonMapped,
		Message: dp.Status.Message, ObservedGeneration: dp.Generation,
	})
	if err := r.patchStatus(ctx, dp, orig); err != nil {
		return ctrl.Result{}, err
	}
	log.Info("Proposal mapped", "proposal", dp.Name, "file", res.File, "edits", len(edits))
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

// authFailedProposals enqueues the proposals in the namespace of a changed
// Secret or ScmConnection that are waiting for access to be fixed: the
// credentials were rejected, or the repository is not visible to them.
func (r *DriftProposalReconciler) authFailedProposals(ctx context.Context, obj client.Object) []reconcile.Request {
	var list backflowv1alpha1.DriftProposalList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var requests []reconcile.Request
	for i := range list.Items {
		p := &list.Items[i]
		if c := meta.FindStatusCondition(p.Status.Conditions, conditionMapped); c != nil &&
			p.Status.Phase == backflowv1alpha1.PhaseMapping && c.Status == metav1.ConditionFalse &&
			(c.Reason == reasonRepositoryAuthFailed || c.Reason == reasonRepositoryNotFound) {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(p)})
		}
	}
	return requests
}

// directorySourceInfo is what mapping needs from the Argo CD Application.
type directorySourceInfo struct {
	options              mapping.DirectoryOptions
	destinationNamespace string
}

// directorySource reads the Directory options and destination namespace from
// the Application. Like the policy controller, it uses the first source of a
// multi-source Application.
func (r *DriftProposalReconciler) directorySource(ctx context.Context, dp *backflowv1alpha1.DriftProposal) (directorySourceInfo, error) {
	app := &unstructured.Unstructured{}
	app.SetGroupVersionKind(applicationGVK)
	key := types.NamespacedName{Namespace: dp.Spec.Application.Namespace, Name: dp.Spec.Application.Name}
	if err := r.Get(ctx, key, app); err != nil {
		if meta.IsNoMatchError(err) {
			return directorySourceInfo{}, apierrors.NewNotFound(applicationGVK.GroupVersion().WithResource("applications").GroupResource(), key.Name)
		}
		return directorySourceInfo{}, err
	}

	src, found, _ := unstructured.NestedMap(app.Object, "spec", "source")
	if !found {
		if sources, ok, _ := unstructured.NestedSlice(app.Object, "spec", "sources"); ok && len(sources) > 0 {
			src, _ = sources[0].(map[string]interface{})
		}
	}
	var info directorySourceInfo
	info.options.Recurse, _, _ = unstructured.NestedBool(src, "directory", "recurse")
	info.options.Include, _, _ = unstructured.NestedString(src, "directory", "include")
	info.options.Exclude, _, _ = unstructured.NestedString(src, "directory", "exclude")
	info.destinationNamespace, _, _ = unstructured.NestedString(app.Object, "spec", "destination", "namespace")
	return info, nil
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
	var connName string
	for _, a := range policy.Status.Applications {
		if a.Name == dp.Spec.Application.Name {
			connName = a.ScmConnection
			break
		}
	}
	if connName == "" {
		return nil, nil
	}

	var conn backflowv1alpha1.ScmConnection
	if err := r.Get(ctx, types.NamespacedName{Namespace: dp.Namespace, Name: connName}, &conn); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("%w: ScmConnection %q not found", gitrepo.ErrAuth, connName)
		}
		return nil, err
	}
	ref := conn.Spec.TokenSecretRef
	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: conn.Namespace, Name: ref.Name}, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("%w: token Secret %q of ScmConnection %q not found", gitrepo.ErrAuth, ref.Name, connName)
		}
		return nil, err
	}
	token := strings.TrimSpace(string(secret.Data[ref.Key]))
	if token == "" {
		return nil, fmt.Errorf("%w: Secret %q has no key %q", gitrepo.ErrAuth, ref.Name, ref.Key)
	}
	auth := gitrepo.BasicAuth(string(conn.Spec.Provider), token)

	if ca := conn.Spec.CASecretRef; ca != nil {
		var caSecret corev1.Secret
		if err := r.Get(ctx, types.NamespacedName{Namespace: conn.Namespace, Name: ca.Name}, &caSecret); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, fmt.Errorf("%w: CA Secret %q of ScmConnection %q not found", gitrepo.ErrAuth, ca.Name, connName)
			}
			return nil, err
		}
		bundle := caSecret.Data[ca.Key]
		if len(bundle) == 0 {
			return nil, fmt.Errorf("%w: Secret %q has no key %q", gitrepo.ErrAuth, ca.Name, ca.Key)
		}
		if auth == nil {
			auth = &gitrepo.Auth{}
		}
		auth.CABundle = bundle
	}
	return auth, nil
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
