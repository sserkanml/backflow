package controller

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	backflowv1 "github.com/sserkanml/backflow/api/v1"
	"github.com/sserkanml/backflow/internal/metrics"
	"github.com/sserkanml/backflow/internal/scm"
)

// applicationGVK identifies Argo CD Applications. We read them as
// unstructured objects so Backflow does not depend on the Argo CD Go module.
var applicationGVK = schema.GroupVersionKind{
	Group:   "argoproj.io",
	Version: "v1alpha1",
	Kind:    "Application",
}

// Fallback resync, in case an Application event was missed or the
// Argo CD CRD was installed after Backflow started.
const policyResyncInterval = 5 * time.Minute

// BackflowPolicyReconciler resolves the Argo CD Applications a policy
// selects and records what it found in status.
type BackflowPolicyReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
	// ArgoCD is the connection policies without spec.argoCD use. Optional.
	ArgoCD *ArgoCDDefaults
	// Visibility tells which Applications the token of a policy with its own
	// spec.argoCD may get. Nil: not asked, every selected Application is visible.
	Visibility *ApplicationVisibility

	mu       sync.Mutex
	reported map[string]string // policy/application -> redacted repoURL an Event was emitted for
}

// reasonCredentialsInRepoURL is the reason of the Event for an Application that is not tracked
// because a repository URL of it contains credentials.
const reasonCredentialsInRepoURL = string(backflowv1.UntrackedCredentialsInRepoURL)

// maxStatusApplications is how many Applications status.applications lists; the policy covers all.
const maxStatusApplications = 100

// +kubebuilder:rbac:groups=backflow.io,resources=backflowpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=backflow.io,resources=backflowpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=backflow.io,resources=backflowpolicies/finalizers,verbs=update
// +kubebuilder:rbac:groups=backflow.io,resources=scmconnections,verbs=get;list;watch
// +kubebuilder:rbac:groups=backflow.io,resources=driftproposals,verbs=get;list;watch
// +kubebuilder:rbac:groups=backflow.io,resources=driftproposals/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=argoproj.io,resources=applications,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

func (r *BackflowPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var policy backflowv1.BackflowPolicy
	if err := r.Get(ctx, req.NamespacedName, &policy); err != nil {
		if apierrors.IsNotFound(err) {
			r.forgetPolicy(req.Namespace, req.Name)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	res, matchErr := r.resolveApplications(ctx, &policy)
	apps := res.apps
	if _, isCheck := errors.AsType[*checkError](matchErr); matchErr != nil && !isCheck {
		// Transient API error: retry with backoff, keep the old status.
		return ctrl.Result{}, matchErr
	}

	var conflict *applicationConflict
	if matchErr == nil && len(apps) > 0 {
		var err error
		if conflict, err = r.findConflict(ctx, &policy, apps); err != nil {
			return ctrl.Result{}, err
		}
	}

	var conns backflowv1.ScmConnectionList
	if err := r.List(ctx, &conns, client.InNamespace(policy.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	slices.SortFunc(conns.Items, func(a, b backflowv1.ScmConnection) int { return cmp.Compare(a.Name, b.Name) })

	names := make([]string, 0, len(apps))
	summaries := make([]backflowv1.ApplicationSummary, 0, len(apps))
	var missingScm, withCredentials []string
	for i := range apps {
		names = append(names, apps[i].GetName())
		// A credential in a repository URL is checked before anything of the
		// Application is stored: only the redacted URL is kept, and the
		// Application is not tracked.
		if redacted, found := credentialedRepoURL(&apps[i]); found {
			summaries = append(summaries, backflowv1.ApplicationSummary{
				Name: apps[i].GetName(), UntrackedReason: backflowv1.UntrackedCredentialsInRepoURL,
				Sources: []backflowv1.SourceSummary{{Position: 1, RepoURL: redacted}},
			})
			withCredentials = append(withCredentials, apps[i].GetName())
			r.reportCredentials(&policy, apps[i].GetName(), redacted)
			continue
		}
		s := summarizeApplication(&apps[i])
		// Each Git repository has its own connection, matched by host. A chart
		// from a Helm repository or a registry needs none.
		matchSourceConnections(s.Sources, conns.Items)
		if missingConnection(s) {
			missingScm = append(missingScm, s.Name)
		}
		summaries = append(summaries, s)
	}
	r.forgetCredentialReports(&policy, withCredentials)

	var proposals backflowv1.DriftProposalList
	if err := r.List(ctx, &proposals, client.InNamespace(policy.Namespace),
		client.MatchingLabels{labelPolicy: safeLabel(policy.Name)}); err != nil {
		return ctrl.Result{}, err
	}
	var open int32
	for i := range proposals.Items {
		if isOpenPhase(proposals.Items[i].Status.Phase) {
			open++
		}
	}

	secretErr := r.argoCDSecretForbidden(ctx, &policy)

	cond := metav1.Condition{Type: "Ready", ObservedGeneration: policy.Generation}
	switch {
	case matchErr != nil:
		cond.Status = metav1.ConditionFalse
		cond.Reason = reasonOf(matchErr)
		cond.Message = matchErr.Error()
	case secretErr != nil:
		cond.Status = metav1.ConditionFalse
		cond.Reason = reasonOf(secretErr)
		cond.Message = secretErr.Error()
	case len(apps) == 0 && res.unknown > 0:
		cond.Status = metav1.ConditionFalse
		cond.Reason = "ApplicationVisibilityUnknown"
		cond.Message = fmt.Sprintf("Argo CD could not be asked which of the selected applications the policy's token may see; "+
			"none is shown until it can. Looking again in %s", visibilityRetry)
	case len(apps) == 0:
		cond.Status = metav1.ConditionFalse
		cond.Reason = "NoApplicationsMatched"
		cond.Message = fmt.Sprintf("No Argo CD Application in namespace %q matches this policy", policy.Spec.ApplicationNamespace())
	case conflict != nil:
		cond.Status = metav1.ConditionFalse
		cond.Reason = "ApplicationConflict"
		cond.Message = fmt.Sprintf("Application %q is already managed by BackflowPolicy %q, which is older; "+
			"an Application can be managed by one policy only", conflict.application, conflict.policy)
	case len(missingScm) > 0 && policy.Spec.Mode != backflowv1.ModeReportOnly:
		cond.Status = metav1.ConditionFalse
		cond.Reason = "MissingScmConnection"
		cond.Message = fmt.Sprintf("No ScmConnection in namespace %q matches the repository of: %s",
			policy.Namespace, strings.Join(missingScm, ", "))
	default:
		cond.Status = metav1.ConditionTrue
		cond.Reason = "Resolved"
		cond.Message = fmt.Sprintf("Watching %d application(s)", len(apps)-len(withCredentials))
		if d := describeArgoCD(argoCDStatus(&policy, r.ArgoCD)); d != "" {
			cond.Message += "; " + d
		}
		if len(withCredentials) > 0 {
			cond.Message += fmt.Sprintf("; not tracked because a repository URL contains credentials: %s",
				strings.Join(withCredentials, ", "))
		}
		if n := len(res.excluded) + res.unknown; n > 0 {
			cond.Message += fmt.Sprintf("; %d selected application(s) excluded: %s", n, excludedExplanation(res.excludedReason))
		}
	}

	policy.Status.ObservedGeneration = policy.Generation
	policy.Status.ApplicationCount = int32(len(summaries))
	policy.Status.ApplicationsTruncated = len(summaries) > maxStatusApplications
	if policy.Status.ApplicationsTruncated {
		// The policy covers every Application it selects; only the list in the status is cut.
		summaries = summaries[:maxStatusApplications]
	}
	policy.Status.Applications = summaries
	policy.Status.OpenProposals = open
	policy.Status.ArgoCD = argoCDStatus(&policy, r.ArgoCD)
	res.recordIn(&policy.Status)
	meta.SetStatusCondition(&policy.Status.Conditions, cond)

	if err := r.Status().Update(ctx, &policy); err != nil {
		if apierrors.IsConflict(err) {
			// The policy changed while its status was computed. Nothing was
			// written, so the status never describes a spec it was not computed
			// from; look again soon.
			log.V(1).Info("The policy changed while its status was computed; reconciling again")
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		return ctrl.Result{}, err
	}

	notAllowed := reasonOf(matchErr) == reasonArgoCDDefaultNotAllowed
	if err := r.retireExcluded(ctx, proposals.Items, res.excluded, notAllowed); err != nil {
		return ctrl.Result{}, err
	}
	metrics.SetPolicy(metrics.Policy{Namespace: policy.Namespace, Name: policy.Name}, cond.Status == metav1.ConditionTrue, len(apps)-len(withCredentials))
	log.Info("Policy resolved", "applications", names, "ready", cond.Status, "reason", cond.Reason)
	return ctrl.Result{RequeueAfter: nextResync(secretErr != nil, res.unknown > 0)}, nil
}

// forgetPolicy drops what is kept for a policy that is gone.
func (r *BackflowPolicyReconciler) forgetPolicy(namespace, name string) {
	metrics.ForgetPolicy(metrics.Policy{Namespace: namespace, Name: name})
	if r.Visibility != nil {
		r.Visibility.forget(namespace + "/" + name)
	}
}

// nextResync is when a policy is looked at again: soon when a Secret could not be read
// (Secrets are not watched, access may have been granted) or when Argo CD could not be
// asked what the policy's token may see, else at the periodic resync.
func nextResync(secretForbidden, visibilityUnknown bool) time.Duration {
	switch {
	case secretForbidden:
		return min(scmRetryInterval, policyResyncInterval)
	case visibilityUnknown:
		return visibilityRetry
	}
	return policyResyncInterval
}

// argoCDSecretForbidden returns a SecretForbidden error when the operator may
// not read the Argo CD token or CA Secret of the policy. A Secret that is
// missing, or any other error, is not reported here.
func (r *BackflowPolicyReconciler) argoCDSecretForbidden(ctx context.Context, policy *backflowv1.BackflowPolicy) error {
	spec := policy.Spec.ArgoCD.Server
	if spec == nil {
		// The token of the operator's default connection, in the namespace the installer named.
		if !r.ArgoCD.Configured() {
			return nil
		}
		ref := r.ArgoCD.tokenRef()
		var secret corev1.Secret
		err := r.Get(ctx, types.NamespacedName{Namespace: r.ArgoCD.TokenSecretNamespace, Name: ref.Name}, &secret)
		if err = secretReadError(err, r.ArgoCD.TokenSecretNamespace, ref.Name); isSecretForbidden(err) {
			return err
		}
		return nil
	}
	refs := []backflowv1.SecretKeyReference{spec.TokenSecretRef}
	if spec.CASecretRef != nil {
		refs = append(refs, *spec.CASecretRef)
	}
	for _, ref := range refs {
		var secret corev1.Secret
		err := r.Get(ctx, types.NamespacedName{Namespace: policy.Namespace, Name: ref.Name}, &secret)
		if err = secretReadError(err, policy.Namespace, ref.Name); isSecretForbidden(err) {
			return err
		}
	}
	return nil
}

// retireExcluded marks the open proposals of the Applications the policy may no
// longer see (status.signals.accessRevoked), so that the proposal controller ends them
// without a call to Argo CD. A proposal is the policy's own resource and shows nothing
// about the Application that its policy cannot see. The signal is written with a merge
// patch of that one object, so it does not overwrite what the proposal controller writes.
func (r *BackflowPolicyReconciler) retireExcluded(ctx context.Context,
	proposals []backflowv1.DriftProposal, excluded []string, all bool) error {
	if len(excluded) == 0 && !all {
		return nil
	}
	gone := make(map[string]bool, len(excluded))
	for _, n := range excluded {
		gone[n] = true
	}
	for i := range proposals {
		p := &proposals[i]
		if (!all && !gone[p.Spec.ApplicationRef.Name]) || !isOpenPhase(p.Status.Phase) || p.Status.Signal().AccessRevoked {
			continue
		}
		orig := p.DeepCopy()
		p.Status.EnsureSignals().AccessRevoked = true
		if err := r.Status().Patch(ctx, p, client.MergeFrom(orig)); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return nil
}

// credentialedRepoURL returns the redacted repoURL of the first source of the
// Application (spec.source or any of spec.sources) whose URL contains a
// credential.
func credentialedRepoURL(app *unstructured.Unstructured) (redacted string, found bool) {
	var sources []map[string]any
	if src, ok, _ := unstructured.NestedMap(app.Object, "spec", "source"); ok {
		sources = append(sources, src)
	}
	if list, ok, _ := unstructured.NestedSlice(app.Object, "spec", "sources"); ok {
		for _, it := range list {
			if m, isMap := it.(map[string]any); isMap {
				sources = append(sources, m)
			}
		}
	}
	for _, src := range sources {
		if raw, _, _ := unstructured.NestedString(src, "repoURL"); scm.URLHasCredentials(raw) {
			return scm.RedactURL(raw), true
		}
	}
	return "", false
}

// reportCredentials emits one Event per Application (and URL) saying that it is
// not tracked.
func (r *BackflowPolicyReconciler) reportCredentials(policy *backflowv1.BackflowPolicy, app, redacted string) {
	key := policy.Namespace + "/" + policy.Name + "/" + app
	r.mu.Lock()
	last, known := r.reported[key]
	seen := known && last == redacted
	if r.reported == nil {
		r.reported = map[string]string{}
	}
	r.reported[key] = redacted
	r.mu.Unlock()
	if seen || r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(policy, nil, corev1.EventTypeWarning, reasonCredentialsInRepoURL, "Tracking",
		"Application %s is not tracked: its repository URL %s contains credentials. Remove them from the Application "+
			"and give Backflow access with an ScmConnection", app, redacted)
}

// forgetCredentialReports drops the reports of Applications of the policy that
// are no longer blocked, so a returning credential is reported again.
func (r *BackflowPolicyReconciler) forgetCredentialReports(policy *backflowv1.BackflowPolicy, blocked []string) {
	prefix := policy.Namespace + "/" + policy.Name + "/"
	keep := map[string]bool{}
	for _, app := range blocked {
		keep[prefix+app] = true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for key := range r.reported {
		if strings.HasPrefix(key, prefix) && !keep[key] {
			delete(r.reported, key)
		}
	}
}

// resolution is the Applications a policy selects and may see. excluded are the
// names of the ones it selects but may not see: they are used to retire proposals
// and are never written to status, Events or logs; only their number is.
type resolution struct {
	apps []unstructured.Unstructured
	// excluded are the ones a definite answer refused: only these retire proposals.
	excluded []string
	// unknown counts the ones that could not be checked (Argo CD unreachable and no
	// answer ever confirmed). They are not shown and not retired.
	unknown        int
	excludedReason string
}

// Reasons Applications are excluded (status.argoCD.hiddenApplications[].reason).
const (
	excludedProjectNotAllowed = string(backflowv1.HiddenProjectNotAllowed)
	excludedNotVisibleToToken = string(backflowv1.HiddenNotVisibleToToken)
	excludedVisibilityUnknown = string(backflowv1.HiddenVisibilityUnknown)
)

// visibilityRetry is how soon a policy whose Applications could not be checked looks again.
const visibilityRetry = 30 * time.Second

// recordIn writes the counts of the hidden Applications by reason, and nothing that names one,
// to the status.
func (res resolution) recordIn(st *backflowv1.BackflowPolicyStatus) {
	n := int32(len(res.excluded) + res.unknown)
	if st.ArgoCD == nil {
		return
	}
	st.ArgoCD.HiddenApplications = nil
	if n > 0 {
		st.ArgoCD.HiddenApplications = []backflowv1.HiddenApplications{{Reason: backflowv1.HiddenReason(res.excludedReason), Count: n}}
	}
}

func excludedExplanation(reason string) string {
	switch reason {
	case excludedNotVisibleToToken:
		return "the Argo CD token of the policy cannot get them"
	case excludedVisibilityUnknown:
		return fmt.Sprintf("Argo CD could not be asked whether the policy's token may see them; looking again in %s", visibilityRetry)
	}
	return "their Argo CD project is not allowed for this namespace (argocd.defaultAccess)"
}

// resolveApplications is the Applications a policy selects and may see (see appResolver).
func (r *BackflowPolicyReconciler) resolveApplications(ctx context.Context, policy *backflowv1.BackflowPolicy) (resolution, error) {
	return r.resolver().resolve(ctx, policy)
}

// matchApplications is the Applications a policy selects and may see.
func (r *BackflowPolicyReconciler) matchApplications(ctx context.Context, policy *backflowv1.BackflowPolicy) ([]unstructured.Unstructured, error) {
	res, err := r.resolveApplications(ctx, policy)
	return res.apps, err
}

func (r *BackflowPolicyReconciler) resolver() appResolver {
	return appResolver{Client: r.Client, ArgoCD: r.ArgoCD, Visibility: r.Visibility}
}

// applicationConflict names an Application that an older policy already manages.
type applicationConflict struct {
	application string
	policy      string // namespace/name
}

// policyOlder orders policies by creationTimestamp, then name, then namespace.
func policyOlder(a, b *backflowv1.BackflowPolicy) bool {
	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.CreationTimestamp.Before(&b.CreationTimestamp)
	}
	if a.Name != b.Name {
		return a.Name < b.Name
	}
	return a.Namespace < b.Namespace
}

// findConflict reports whether an older policy, in any namespace, that reads
// the same Argo CD namespace already manages one of apps. Older policies claim
// their Applications in age order, and a policy that is itself blocked claims
// nothing, so the oldest policy always wins and a blocked one cannot block a
// younger one. Policies whose own selection is invalid are ignored.
func (r *BackflowPolicyReconciler) findConflict(ctx context.Context, policy *backflowv1.BackflowPolicy,
	apps []unstructured.Unstructured) (*applicationConflict, error) {
	var all backflowv1.BackflowPolicyList
	if err := r.List(ctx, &all); err != nil {
		return nil, err
	}
	var older []*backflowv1.BackflowPolicy
	for i := range all.Items {
		p := &all.Items[i]
		if p.Spec.ApplicationNamespace() != policy.Spec.ApplicationNamespace() || p.DeletionTimestamp != nil ||
			(p.Namespace == policy.Namespace && p.Name == policy.Name) || !policyOlder(p, policy) {
			continue
		}
		older = append(older, p)
	}
	slices.SortFunc(older, func(a, b *backflowv1.BackflowPolicy) int {
		switch {
		case policyOlder(a, b):
			return -1
		case policyOlder(b, a):
			return 1
		}
		return 0
	})

	owner := map[string]string{} // Application name -> namespace/name of the policy that manages it
	for _, p := range older {
		theirs, err := r.matchApplications(ctx, p)
		if err != nil {
			if _, ok := errors.AsType[*checkError](err); ok {
				continue
			}
			return nil, err
		}
		blocked := false
		for i := range theirs {
			if _, taken := owner[theirs[i].GetName()]; taken {
				blocked = true
				break
			}
		}
		if blocked {
			continue
		}
		for i := range theirs {
			owner[theirs[i].GetName()] = p.Namespace + "/" + p.Name
		}
	}
	for i := range apps {
		if by, taken := owner[apps[i].GetName()]; taken {
			return &applicationConflict{application: apps[i].GetName(), policy: by}, nil
		}
	}
	return nil, nil
}

// policiesSharingArgoCDNamespace enqueues the other policies, in any
// namespace, that read Applications from the same Argo CD namespace, because
// which of them wins an Application depends on each other. A policy whose
// argoCDNamespace changed leaves its old peers to the periodic resync.
func (r *BackflowPolicyReconciler) policiesSharingArgoCDNamespace(ctx context.Context, obj client.Object) []reconcile.Request {
	changed, ok := obj.(*backflowv1.BackflowPolicy)
	if !ok {
		return nil
	}
	var list backflowv1.BackflowPolicyList
	if err := r.List(ctx, &list); err != nil {
		return nil
	}
	var requests []reconcile.Request
	for _, p := range list.Items {
		if p.Spec.ApplicationNamespace() != changed.Spec.ApplicationNamespace() ||
			(p.Namespace == changed.Namespace && p.Name == changed.Name) {
			continue
		}
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{Namespace: p.Namespace, Name: p.Name},
		})
	}
	return requests
}

// summarizeApplication extracts what Backflow needs from an Application: its sources, always
// at least one (an Application with a single source has one entry), each with what Argo CD
// resolved for it.
func summarizeApplication(app *unstructured.Unstructured) backflowv1.ApplicationSummary {
	s := backflowv1.ApplicationSummary{Name: app.GetName()}
	if resources, ok, _ := unstructured.NestedSlice(app.Object, fieldStatus, "resources"); ok {
		s.ManagedResources = int32(len(resources))
	}
	if s.Sources = summarizeSources(app); s.Sources != nil {
		return s
	}

	src, found, _ := unstructured.NestedMap(app.Object, "spec", "source")
	if !found {
		src = map[string]any{}
	}
	one := backflowv1.SourceSummary{
		Position: 1, RepoURL: strField(src, "repoURL"), TargetRevision: strField(src, "targetRevision"),
		Path: strField(src, "path"), Chart: strField(src, "chart"),
	}
	// The revision Argo CD compared against is also what the last sync put in place: a single-source
	// Application reports no other.
	one.Revision, _, _ = unstructured.NestedString(app.Object, fieldStatus, "sync", "revision")
	if one.Revision == "" {
		if revs, ok, _ := unstructured.NestedStringSlice(app.Object, fieldStatus, "sync", "revisions"); ok && len(revs) > 0 {
			one.Revision = revs[0]
		}
	}
	one.SyncedRevision = one.Revision

	// Argo CD reports the detected type in status.sourceType. Fall back to
	// the spec when the Application has not been reconciled yet.
	detected, _, _ := unstructured.NestedString(app.Object, fieldStatus, "sourceType")
	one.Type = backflowv1.SourceType(detected)
	if one.Type == "" {
		switch {
		case one.Chart != "", src["helm"] != nil:
			one.Type = sourceTypeHelm
		case src["kustomize"] != nil:
			one.Type = backflowv1.SourceKustomize
		default:
			one.Type = backflowv1.SourceDirectory
		}
	}
	s.Sources = []backflowv1.SourceSummary{one}
	return s
}

// matchSourceConnections sets the ScmConnection of every Git source.
func matchSourceConnections(sources []backflowv1.SourceSummary, conns []backflowv1.ScmConnection) {
	for i := range sources {
		if isGitSource(sources[i]) {
			sources[i].ScmConnectionName = connectionFor(sources[i].RepoURL, conns)
		}
	}
}

// missingConnection reports whether a repository Backflow reads or writes has
// no ScmConnection: the repository of a single-source Application, or any Git
// source of a multi-source one.
func missingConnection(s backflowv1.ApplicationSummary) bool {
	for _, src := range s.Sources {
		if isGitSource(src) && src.ScmConnectionName == "" {
			return true
		}
	}
	return false
}

// repoHost returns the canonical (lower-cased, www.github.com as github.com) host of a Git URL. It understands
// https://host/..., ssh://git@host/... and scp-like git@host:group/repo.git.
func repoHost(repoURL string) string {
	if strings.Contains(repoURL, "://") {
		u, err := url.Parse(repoURL)
		if err != nil {
			return ""
		}
		return scm.CanonicalHost(u.Hostname())
	}
	if at := strings.Index(repoURL, "@"); at >= 0 {
		repoURL = repoURL[at+1:]
	}
	if colon := strings.Index(repoURL, ":"); colon >= 0 {
		return scm.CanonicalHost(repoURL[:colon])
	}
	return ""
}

// connectionFor returns the name of the first ScmConnection whose hosts
// include the repository's host. Connections without spec.hosts match the
// host of their spec.url.
func connectionFor(repoURL string, conns []backflowv1.ScmConnection) string {
	host := repoHost(repoURL)
	if host == "" {
		return ""
	}
	for _, c := range conns {
		hosts := c.Spec.Hosts
		if len(hosts) == 0 {
			hosts = []string{repoHost(c.Spec.URL)}
		}
		for _, h := range hosts {
			if scm.CanonicalHost(h) == host {
				return c.Name
			}
		}
	}
	return ""
}

// policiesForApplication enqueues every policy that reads Applications from
// the namespace of the changed Application.
func (r *BackflowPolicyReconciler) policiesForApplication(ctx context.Context, obj client.Object) []reconcile.Request {
	return policiesForApplication(ctx, r.Client, obj)
}

// policiesInNamespace enqueues every policy in the namespace of the changed
// ScmConnection.
func (r *BackflowPolicyReconciler) policiesInNamespace(ctx context.Context, obj client.Object) []reconcile.Request {
	var list backflowv1.BackflowPolicyList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0, len(list.Items))
	for _, p := range list.Items {
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{Namespace: p.Namespace, Name: p.Name},
		})
	}
	return requests
}

// applicationChanged ignores the frequent Application status updates that
// do not affect what a policy reports (health, operation state, timestamps).
func applicationChanged() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldApp, ok1 := e.ObjectOld.(*unstructured.Unstructured)
			newApp, ok2 := e.ObjectNew.(*unstructured.Unstructured)
			if !ok1 || !ok2 {
				return true
			}
			if oldApp.GetGeneration() != newApp.GetGeneration() ||
				!reflect.DeepEqual(oldApp.GetLabels(), newApp.GetLabels()) {
				return true
			}
			// The revisions of every source: status.sync.revision is empty for several.
			oldRev, newRev := syncedRevision(oldApp), syncedRevision(newApp)
			oldRes, _, _ := unstructured.NestedSlice(oldApp.Object, fieldStatus, "resources")
			newRes, _, _ := unstructured.NestedSlice(newApp.Object, fieldStatus, "resources")
			oldType, _, _ := unstructured.NestedString(oldApp.Object, fieldStatus, "sourceType")
			newType, _, _ := unstructured.NestedString(newApp.Object, fieldStatus, "sourceType")
			return oldRev != newRev || len(oldRes) != len(newRes) || oldType != newType
		},
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *BackflowPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		For(&backflowv1.BackflowPolicy{},
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&backflowv1.BackflowPolicy{},
			handler.EnqueueRequestsFromMapFunc(r.policiesSharingArgoCDNamespace),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&backflowv1.ScmConnection{},
			handler.EnqueueRequestsFromMapFunc(r.policiesInNamespace),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&backflowv1.DriftProposal{},
			handler.EnqueueRequestsFromMapFunc(policyOfProposal),
			builder.WithPredicates(proposalPhaseChanged())).
		Named("backflowpolicy")

	// Only watch Applications when Argo CD is installed; otherwise the
	// manager would fail to start. The periodic resync covers late installs.
	if _, err := mgr.GetRESTMapper().RESTMapping(applicationGVK.GroupKind(), applicationGVK.Version); err == nil {
		app := &unstructured.Unstructured{}
		app.SetGroupVersionKind(applicationGVK)
		b = b.Watches(app,
			handler.EnqueueRequestsFromMapFunc(r.policiesForApplication),
			builder.WithPredicates(applicationChanged()))
	} else {
		mgr.GetLogger().Info("Argo CD Application CRD not found; Application changes will be picked up by periodic resync",
			"interval", policyResyncInterval.String())
	}

	return b.Complete(r)
}

// policyOfProposal enqueues the policy that owns a DriftProposal.
func policyOfProposal(_ context.Context, obj client.Object) []reconcile.Request {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.Kind == "BackflowPolicy" && strings.HasPrefix(ref.APIVersion, backflowv1.GroupVersion.Group+"/") {
			return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: ref.Name}}}
		}
	}
	return nil
}

// proposalPhaseChanged fires on create, delete and phase changes, which is
// what status.openProposals depends on. Spec-only updates are ignored.
func proposalPhaseChanged() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldP, ok1 := e.ObjectOld.(*backflowv1.DriftProposal)
			newP, ok2 := e.ObjectNew.(*backflowv1.DriftProposal)
			return !ok1 || !ok2 || isOpenPhase(oldP.Status.Phase) != isOpenPhase(newP.Status.Phase)
		},
	}
}
