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

	backflowv1alpha1 "github.com/sserkanml/backflow/api/v1alpha1"
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

	mu       sync.Mutex
	reported map[string]string // policy/application -> redacted repoURL an Event was emitted for
}

// reasonCredentialsInRepoURL marks an Application that is not tracked because
// a repository URL of it contains credentials.
const reasonCredentialsInRepoURL = "CredentialsInRepoURL"

// +kubebuilder:rbac:groups=backflow.io,resources=backflowpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=backflow.io,resources=backflowpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=backflow.io,resources=backflowpolicies/finalizers,verbs=update
// +kubebuilder:rbac:groups=backflow.io,resources=scmconnections,verbs=get;list;watch
// +kubebuilder:rbac:groups=backflow.io,resources=driftproposals,verbs=get;list;watch
// +kubebuilder:rbac:groups=argoproj.io,resources=applications,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

func (r *BackflowPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var policy backflowv1alpha1.BackflowPolicy
	if err := r.Get(ctx, req.NamespacedName, &policy); err != nil {
		if apierrors.IsNotFound(err) {
			metrics.ForgetPolicy(req.Namespace, req.Name)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	apps, matchErr := r.matchApplications(ctx, &policy)
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

	var conns backflowv1alpha1.ScmConnectionList
	if err := r.List(ctx, &conns, client.InNamespace(policy.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	slices.SortFunc(conns.Items, func(a, b backflowv1alpha1.ScmConnection) int { return cmp.Compare(a.Name, b.Name) })

	names := make([]string, 0, len(apps))
	summaries := make([]backflowv1alpha1.ApplicationSummary, 0, len(apps))
	var missingScm, withCredentials []string
	for i := range apps {
		names = append(names, apps[i].GetName())
		// A credential in a repository URL is checked before anything of the
		// Application is stored: only the redacted URL is kept, and the
		// Application is not tracked.
		if redacted, found := credentialedRepoURL(&apps[i]); found {
			summaries = append(summaries, backflowv1alpha1.ApplicationSummary{
				Name: apps[i].GetName(), RepoURL: redacted, Reason: reasonCredentialsInRepoURL,
			})
			withCredentials = append(withCredentials, apps[i].GetName())
			r.reportCredentials(&policy, apps[i].GetName(), redacted)
			continue
		}
		s := summarizeApplication(&apps[i])
		s.ScmConnection = connectionFor(s.RepoURL, conns.Items)
		if s.Sources = summarizeSources(&apps[i]); s.Sources != nil {
			// Each Git repository has its own connection, matched by host. A chart
			// from a Helm repository or a registry needs none.
			s.ScmConnection = matchSourceConnections(s.Sources, conns.Items)
		}
		if missingConnection(s) {
			missingScm = append(missingScm, s.Name)
		}
		summaries = append(summaries, s)
	}
	r.forgetCredentialReports(&policy, withCredentials)

	var proposals backflowv1alpha1.DriftProposalList
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
	case len(apps) == 0:
		cond.Status = metav1.ConditionFalse
		cond.Reason = "NoApplicationsMatched"
		cond.Message = fmt.Sprintf("No Argo CD Application in namespace %q matches this policy", policy.Spec.ArgoCDNamespace)
	case conflict != nil:
		cond.Status = metav1.ConditionFalse
		cond.Reason = "ApplicationConflict"
		cond.Message = fmt.Sprintf("Application %q is already managed by BackflowPolicy %q, which is older; "+
			"an Application can be managed by one policy only", conflict.application, conflict.policy)
	case len(missingScm) > 0 && policy.Spec.Mode != backflowv1alpha1.ModeReportOnly:
		cond.Status = metav1.ConditionFalse
		cond.Reason = "MissingScmConnection"
		cond.Message = fmt.Sprintf("No ScmConnection in namespace %q matches the repository of: %s",
			policy.Namespace, strings.Join(missingScm, ", "))
	default:
		cond.Status = metav1.ConditionTrue
		cond.Reason = "Resolved"
		cond.Message = fmt.Sprintf("Watching %d application(s)", len(apps)-len(withCredentials))
		if len(withCredentials) > 0 {
			cond.Message += fmt.Sprintf("; not tracked because a repository URL contains credentials: %s",
				strings.Join(withCredentials, ", "))
		}
	}

	policy.Status.ObservedGeneration = policy.Generation
	policy.Status.MatchedApplications = names
	policy.Status.Applications = summaries
	policy.Status.OpenProposals = open
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

	metrics.SetPolicy(policy.Namespace, policy.Name, cond.Status == metav1.ConditionTrue, len(apps)-len(withCredentials))
	log.Info("Policy resolved", "applications", names, "ready", cond.Status, "reason", cond.Reason)
	if secretErr != nil {
		// Secrets are not watched: look again soon, in case access was granted.
		return ctrl.Result{RequeueAfter: min(scmRetryInterval, policyResyncInterval)}, nil
	}
	return ctrl.Result{RequeueAfter: policyResyncInterval}, nil
}

// argoCDSecretForbidden returns a SecretForbidden error when the operator may
// not read the Argo CD token or CA Secret of the policy. A Secret that is
// missing, or any other error, is not reported here.
func (r *BackflowPolicyReconciler) argoCDSecretForbidden(ctx context.Context, policy *backflowv1alpha1.BackflowPolicy) error {
	spec := policy.Spec.ArgoCD
	if spec == nil {
		return nil
	}
	refs := []corev1.SecretKeySelector{spec.TokenSecretRef}
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
func (r *BackflowPolicyReconciler) reportCredentials(policy *backflowv1alpha1.BackflowPolicy, app, redacted string) {
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
func (r *BackflowPolicyReconciler) forgetCredentialReports(policy *backflowv1alpha1.BackflowPolicy, blocked []string) {
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

// matchApplications lists Applications in the Argo CD namespace that match
// the policy. When both names and selector are set, an Application must
// match both. Spec problems are returned as *checkError.
func (r *BackflowPolicyReconciler) matchApplications(ctx context.Context, policy *backflowv1alpha1.BackflowPolicy) ([]unstructured.Unstructured, error) {
	sel := policy.Spec.Applications
	if len(sel.Names) == 0 && sel.Selector == nil {
		return nil, &checkError{reason: "InvalidSpec", msg: "spec.applications needs at least one of names or selector"}
	}

	opts := []client.ListOption{client.InNamespace(policy.Spec.ArgoCDNamespace)}
	if sel.Selector != nil {
		ls, err := metav1.LabelSelectorAsSelector(sel.Selector)
		if err != nil {
			return nil, &checkError{reason: "InvalidSpec", msg: fmt.Sprintf("spec.applications.selector: %v", err)}
		}
		opts = append(opts, client.MatchingLabelsSelector{Selector: ls})
	}

	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(applicationGVK.GroupVersion().WithKind("ApplicationList"))
	if err := r.List(ctx, list, opts...); err != nil {
		if meta.IsNoMatchError(err) {
			return nil, &checkError{reason: "ArgoCDNotFound", msg: "the Argo CD Application CRD is not installed in this cluster"}
		}
		return nil, err
	}

	wanted := make(map[string]bool, len(sel.Names))
	for _, n := range sel.Names {
		wanted[n] = true
	}

	var apps []unstructured.Unstructured
	for _, app := range list.Items {
		if len(wanted) > 0 && !wanted[app.GetName()] {
			continue
		}
		apps = append(apps, app)
	}
	slices.SortFunc(apps, func(a, b unstructured.Unstructured) int { return cmp.Compare(a.GetName(), b.GetName()) })
	return apps, nil
}

// applicationConflict names an Application that an older policy already manages.
type applicationConflict struct {
	application string
	policy      string // namespace/name
}

// policyOlder orders policies by creationTimestamp, then name, then namespace.
func policyOlder(a, b *backflowv1alpha1.BackflowPolicy) bool {
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
func (r *BackflowPolicyReconciler) findConflict(ctx context.Context, policy *backflowv1alpha1.BackflowPolicy,
	apps []unstructured.Unstructured) (*applicationConflict, error) {
	var all backflowv1alpha1.BackflowPolicyList
	if err := r.List(ctx, &all); err != nil {
		return nil, err
	}
	var older []*backflowv1alpha1.BackflowPolicy
	for i := range all.Items {
		p := &all.Items[i]
		if p.Spec.ArgoCDNamespace != policy.Spec.ArgoCDNamespace || p.DeletionTimestamp != nil ||
			(p.Namespace == policy.Namespace && p.Name == policy.Name) || !policyOlder(p, policy) {
			continue
		}
		older = append(older, p)
	}
	slices.SortFunc(older, func(a, b *backflowv1alpha1.BackflowPolicy) int {
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
	changed, ok := obj.(*backflowv1alpha1.BackflowPolicy)
	if !ok {
		return nil
	}
	var list backflowv1alpha1.BackflowPolicyList
	if err := r.List(ctx, &list); err != nil {
		return nil
	}
	var requests []reconcile.Request
	for _, p := range list.Items {
		if p.Spec.ArgoCDNamespace != changed.Spec.ArgoCDNamespace ||
			(p.Namespace == changed.Namespace && p.Name == changed.Name) {
			continue
		}
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{Namespace: p.Namespace, Name: p.Name},
		})
	}
	return requests
}

// summarizeApplication extracts what Backflow needs from an Application.
// The fields describe the first source of a multi-source Application; its
// sources are listed by summarizeSources.
func summarizeApplication(app *unstructured.Unstructured) backflowv1alpha1.ApplicationSummary {
	s := backflowv1alpha1.ApplicationSummary{Name: app.GetName()}

	src, found, _ := unstructured.NestedMap(app.Object, "spec", "source")
	if !found {
		if sources, ok, _ := unstructured.NestedSlice(app.Object, "spec", "sources"); ok && len(sources) > 0 {
			src, _ = sources[0].(map[string]any)
		}
	}

	s.RepoURL, _, _ = unstructured.NestedString(src, "repoURL")
	s.TargetRevision, _, _ = unstructured.NestedString(src, "targetRevision")
	s.Path, _, _ = unstructured.NestedString(src, "path")
	chart, _, _ := unstructured.NestedString(src, "chart")
	if s.Path == "" {
		s.Path = chart
	}

	s.SyncedRevision, _, _ = unstructured.NestedString(app.Object, fieldStatus, "sync", "revision")
	if s.SyncedRevision == "" {
		if revs, ok, _ := unstructured.NestedStringSlice(app.Object, fieldStatus, "sync", "revisions"); ok && len(revs) > 0 {
			s.SyncedRevision = revs[0]
		}
	}

	// Argo CD reports the detected type in status.sourceType. Fall back to
	// the spec when the Application has not been reconciled yet.
	s.SourceType, _, _ = unstructured.NestedString(app.Object, fieldStatus, "sourceType")
	if s.SourceType == "" {
		switch {
		case chart != "":
			s.SourceType = sourceTypeHelm
		case src["helm"] != nil:
			s.SourceType = sourceTypeHelm
		case src["kustomize"] != nil:
			s.SourceType = "Kustomize"
		default:
			s.SourceType = "Directory"
		}
	}

	if resources, ok, _ := unstructured.NestedSlice(app.Object, fieldStatus, "resources"); ok {
		s.ManagedResources = int32(len(resources))
	}
	return s
}

// matchSourceConnections sets the ScmConnection of every Git source and returns
// the one that stands for the Application: the first ref source's, since value
// files are what Backflow writes, else the first Git source's.
func matchSourceConnections(sources []backflowv1alpha1.SourceSummary, conns []backflowv1alpha1.ScmConnection) string {
	var first, ref string
	for i := range sources {
		if !isGitSource(sources[i]) {
			continue
		}
		sources[i].ScmConnection = connectionFor(sources[i].RepoURL, conns)
		if first == "" {
			first = sources[i].ScmConnection
		}
		if ref == "" && sources[i].Ref != "" {
			ref = sources[i].ScmConnection
		}
	}
	if ref != "" {
		return ref
	}
	return first
}

// missingConnection reports whether a repository Backflow reads or writes has
// no ScmConnection: the repository of a single-source Application, or any Git
// source of a multi-source one.
func missingConnection(s backflowv1alpha1.ApplicationSummary) bool {
	if s.Sources == nil {
		return s.ScmConnection == ""
	}
	for _, src := range s.Sources {
		if isGitSource(src) && src.ScmConnection == "" {
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
func connectionFor(repoURL string, conns []backflowv1alpha1.ScmConnection) string {
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
	var list backflowv1alpha1.BackflowPolicyList
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
		For(&backflowv1alpha1.BackflowPolicy{},
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&backflowv1alpha1.BackflowPolicy{},
			handler.EnqueueRequestsFromMapFunc(r.policiesSharingArgoCDNamespace),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&backflowv1alpha1.ScmConnection{},
			handler.EnqueueRequestsFromMapFunc(r.policiesInNamespace),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&backflowv1alpha1.DriftProposal{},
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
		if ref.Kind == "BackflowPolicy" && strings.HasPrefix(ref.APIVersion, backflowv1alpha1.GroupVersion.Group+"/") {
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
			oldP, ok1 := e.ObjectOld.(*backflowv1alpha1.DriftProposal)
			newP, ok2 := e.ObjectNew.(*backflowv1alpha1.DriftProposal)
			return !ok1 || !ok2 || isOpenPhase(oldP.Status.Phase) != isOpenPhase(newP.Status.Phase)
		},
	}
}
