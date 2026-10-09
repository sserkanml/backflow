package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	backflowv1alpha1 "github.com/sserkanml/backflow/api/v1alpha1"
	"github.com/sserkanml/backflow/internal/argocd"
	"github.com/sserkanml/backflow/internal/drift"
)

const (
	labelPolicy      = "backflow.io/policy"
	labelApplication = "backflow.io/application"
	labelResource    = "backflow.io/resource"

	// Lifecycle events the drift controller signals to the DriftProposal
	// controller, which is the only writer of DriftProposal status.
	annotationSupersededBy = "backflow.io/superseded-by"
	annotationReverted     = "backflow.io/reverted"
	// annotationLiveReverted carries the revision of the sync that reset the
	// cluster to Git while the proposal's merge request is still open.
	annotationLiveReverted = "backflow.io/live-reverted"

	// Periodic resync, in case an Application event was missed.
	driftResyncInterval = 2 * time.Minute

	defaultArgoCDURL = "https://argocd-server.argocd.svc"
	syncOutOfSync    = "OutOfSync"
)

// ManagedResourcesGetter is the part of the Argo CD client the drift
// controller needs.
type ManagedResourcesGetter interface {
	GetManagedResources(ctx context.Context, appName, appNamespace string) ([]argocd.ManagedResource, error)
}

// DriftReconciler turns OutOfSync resources of the Applications a Ready
// BackflowPolicy watches into DriftProposals. It reconciles per policy.
// It only reads the cluster and writes DriftProposals.
type DriftReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
	// NewArgoClient builds the Argo CD client. Defaults to argocd.New.
	NewArgoClient func(argocd.Config) (ManagedResourcesGetter, error)

	// Git lists the files that differ between two commits. It lets the
	// pause for "Git is ahead of the last sync" be limited to commits that
	// touch the Application's own directory. When nil, or when it cannot
	// answer, the pause applies to every commit.
	Git GitDiffer

	// Now returns the current time. Defaults to time.Now.
	Now func() time.Time

	aheadMu       sync.Mutex
	aheadReported map[string]string // per Application: the revisions an Event was last emitted for
	aheadChecks   map[aheadKey]aheadCheck
}

// +kubebuilder:rbac:groups=backflow.io,resources=backflowpolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups=backflow.io,resources=driftproposals,verbs=get;list;watch;create;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=argoproj.io,resources=applications,verbs=get;list;watch

func (r *DriftReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var policy backflowv1alpha1.BackflowPolicy
	if err := r.Get(ctx, req.NamespacedName, &policy); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if policy.Spec.ArgoCD == nil || !meta.IsStatusConditionTrue(policy.Status.Conditions, "Ready") {
		return ctrl.Result{}, nil
	}

	argo, err := r.argoClient(ctx, &policy)
	if err != nil {
		// The policy controller owns policy status, so report through the log and an Event.
		log.Error(err, "Argo CD connection is not usable; skipping drift detection")
		r.event(&policy, corev1.EventTypeWarning, "ArgoCDConnection", err.Error())
		return ctrl.Result{RequeueAfter: driftResyncInterval}, nil
	}

	var proposals backflowv1alpha1.DriftProposalList
	if err := r.List(ctx, &proposals, client.InNamespace(policy.Namespace),
		client.MatchingLabels{labelPolicy: safeLabel(policy.Name)}); err != nil {
		return ctrl.Result{}, err
	}

	// Some decisions are about the moment (a sync has just finished). They ask
	// for an earlier look instead of waiting for the next resync.
	again := &recheck{}
	ctx = context.WithValue(ctx, recheckKey{}, again)

	var firstErr error
	for _, summary := range policy.Status.Applications {
		if err := r.reconcileApplication(ctx, &policy, argo, summary, proposals.Items); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		return ctrl.Result{}, firstErr
	}
	return ctrl.Result{RequeueAfter: again.within(driftResyncInterval)}, nil
}

// appResource is one entry of an Application's status.resources.
type appResource struct {
	Group, Version, Kind, Namespace, Name, Status string
}

func (a appResource) hash() string { return resourceHash(a.Group, a.Kind, a.Namespace, a.Name) }

// appResources reads status.resources. known is false when Argo CD has not
// reported resources yet, so an empty list can be told from "not reconciled".
func appResources(app *unstructured.Unstructured) (res []appResource, known bool) {
	items, found, _ := unstructured.NestedSlice(app.Object, "status", "resources")
	if !found {
		return nil, false
	}
	for _, it := range items {
		m, ok := it.(map[string]interface{})
		if !ok {
			continue
		}
		str := func(k string) string { s, _ := m[k].(string); return s }
		res = append(res, appResource{
			Group: str("group"), Version: str("version"), Kind: str("kind"),
			Namespace: str("namespace"), Name: str("name"), Status: str("status"),
		})
	}
	return res, true
}

func (r *DriftReconciler) reconcileApplication(ctx context.Context, policy *backflowv1alpha1.BackflowPolicy,
	argo ManagedResourcesGetter, summary backflowv1alpha1.ApplicationSummary, all []backflowv1alpha1.DriftProposal) error {
	log := logf.FromContext(ctx).WithValues("application", summary.Name)

	app := &unstructured.Unstructured{}
	app.SetGroupVersionKind(applicationGVK)
	if err := r.Get(ctx, types.NamespacedName{Namespace: policy.Spec.ArgoCDNamespace, Name: summary.Name}, app); err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			log.Info("Application not found; leaving its proposals untouched")
			return nil
		}
		return err
	}
	resources, known := appResources(app)
	if !known {
		return nil
	}

	outOfSync := map[string]bool{}
	var candidates []appResource
	for _, res := range resources {
		if res.Status != syncOutOfSync {
			continue
		}
		outOfSync[res.hash()] = true
		if drift.Allowed(res.Group, res.Kind, policy.Spec.Include, policy.Spec.Exclude) {
			candidates = append(candidates, res)
		}
	}

	// While Git is ahead of the last sync, an OutOfSync resource may differ
	// from the cluster only because a commit has not been applied yet. A
	// proposal made now could undo that commit, so no new one is made.
	paused := len(candidates) > 0 && r.detectionPaused(ctx, policy, summary, app)

	// The Argo CD API is only worth calling for an application that has
	// something to compare (an OutOfSync resource) or something to follow up
	// on (an open proposal). Everything else is settled by the Application
	// status alone, which keeps the periodic and reconciledAt-triggered
	// reconciles of healthy applications free of API traffic.
	hasOpenProposal := false
	for i := range all {
		p := &all[i]
		if p.Labels[labelApplication] == safeLabel(summary.Name) && isOpen(p) {
			hasOpenProposal = true
			break
		}
	}
	if len(outOfSync) == 0 && !hasOpenProposal {
		return nil
	}

	drifting := map[string][]backflowv1alpha1.FieldChange{}
	// deleted marks OutOfSync resources that exist only in Git: they were
	// removed from the cluster, which is not a revert.
	deleted := map[string]appResource{}
	apiOK := true
	if len(candidates) > 0 {
		managed, err := argo.GetManagedResources(ctx, summary.Name, policy.Spec.ArgoCDNamespace)
		if err != nil {
			apiOK = false
			log.Error(err, "Cannot read managed resources from Argo CD")
			r.event(policy, corev1.EventTypeWarning, argoReason(err),
				fmt.Sprintf("Application %s: %v", summary.Name, err))
		} else {
			byKey := map[string]argocd.ManagedResource{}
			for _, m := range managed {
				byKey[resourceHash(m.Group, m.Kind, m.Namespace, m.Name)] = m
			}
			var firstErr error
			for _, res := range candidates {
				m, ok := byKey[res.hash()]
				if ok && m.PredictedLiveState != nil && m.NormalizedLiveState == nil {
					deleted[res.hash()] = res
				}
				if !ok || m.PredictedLiveState == nil || m.NormalizedLiveState == nil {
					log.Info("Skipping resource that exists only in Git or only in the cluster",
						"kind", res.Kind, "name", res.Name)
					continue
				}
				changes := drift.Diff(m.PredictedLiveState, m.NormalizedLiveState,
					drift.IgnorePointers(policy.Spec.IgnoreFields, res.Group, res.Kind, res.Name)...)
				if len(changes) == 0 {
					continue
				}
				drifting[res.hash()] = changes
				if err := r.ensureProposal(ctx, policy, summary, res, changes, all, paused); err != nil && firstErr == nil {
					firstErr = err
				}
			}
			if firstErr != nil {
				return firstErr
			}
		}
	}

	// Close open proposals whose drift is gone. Without a successful API call
	// only resources that are in sync (or gone) can be judged.
	var managed managedResourcesCache
	for i := range all {
		p := &all[i]
		if p.Labels[labelApplication] != safeLabel(summary.Name) || !isOpen(p) {
			continue
		}
		rh := p.Labels[labelResource]
		if _, still := drifting[rh]; still {
			continue
		}
		if res, gone := deleted[rh]; gone {
			// Leave the proposal as it is; what to do about a deleted
			// resource is a human decision.
			log.Info("Resource of an open proposal was deleted from the cluster", "proposal", p.Name)
			r.event(policy, corev1.EventTypeWarning, "ResourceDeleted",
				fmt.Sprintf("%s %s in application %s was deleted from the cluster; proposal %s is left untouched",
					res.Kind, res.Name, summary.Name, p.Name))
			continue
		}
		var msg string
		switch {
		case !outOfSync[rh]:
			// Synced again. If a new sync did it while a merge request still
			// holds the change, the change is not lost: it waits in the merge
			// request. Otherwise it is a plain revert.
			if hasOpenMergeRequest(p) {
				held, err := r.resetBySync(ctx, argo, policy, summary, app, p, &managed)
				if err != nil {
					log.Info("Cannot tell why the resource is in sync again; will look again", "proposal", p.Name, "cause", err.Error())
					continue
				}
				if held {
					continue
				}
			}
			msg = "The live resource is back in sync with Git, or no longer part of the Application."
		case apiOK:
			msg = "The live resource no longer differs from Git after ignored fields and filters."
		default:
			continue
		}
		if err := r.annotate(ctx, p, annotationReverted, msg); err != nil {
			return err
		}
		log.Info("Proposal reverted", "proposal", p.Name)
	}
	return nil
}

// ensureProposal makes sure exactly one open proposal with these changes
// exists for the resource, superseding open proposals with other changes.
// The spec of an existing proposal is never modified.
func (r *DriftReconciler) ensureProposal(ctx context.Context, policy *backflowv1alpha1.BackflowPolicy,
	summary backflowv1alpha1.ApplicationSummary, res appResource,
	changes []backflowv1alpha1.FieldChange, all []backflowv1alpha1.DriftProposal, paused bool) error {
	log := logf.FromContext(ctx)

	srcType, ok := proposalSourceType(summary.SourceType)
	if !ok {
		r.event(policy, corev1.EventTypeWarning, "UnsupportedSource",
			fmt.Sprintf("Application %s uses source type %q, which is not supported yet", summary.Name, summary.SourceType))
		return nil
	}

	rh := res.hash()
	names := make(map[string]bool, len(all))
	var open []*backflowv1alpha1.DriftProposal
	for i := range all {
		p := &all[i]
		names[p.Name] = true
		if p.Labels[labelResource] == rh && isOpen(p) {
			open = append(open, p)
		}
	}

	var current *backflowv1alpha1.DriftProposal
	for _, p := range open {
		if reflect.DeepEqual(p.Spec.Changes, changes) {
			current = p
			break
		}
	}

	switch {
	case current == nil && paused:
		// Git is ahead of the last sync: nothing is created or superseded.
		return nil
	case current == nil:
		if blocker := blockingProposal(all, rh, changes, summary.SyncedRevision); blocker != nil {
			return r.noteBlocked(ctx, policy, summary, blocker)
		}
		attempt := 0
		for names[proposalName(summary.Name, res, changes, attempt)] {
			attempt++
		}
		dp := &backflowv1alpha1.DriftProposal{
			ObjectMeta: metav1.ObjectMeta{
				Name:      proposalName(summary.Name, res, changes, attempt),
				Namespace: policy.Namespace,
				Labels: map[string]string{
					labelPolicy:      safeLabel(policy.Name),
					labelApplication: safeLabel(summary.Name),
					labelResource:    rh,
				},
			},
			Spec: backflowv1alpha1.DriftProposalSpec{
				PolicyName:  policy.Name,
				Application: backflowv1alpha1.ObjectRef{Name: summary.Name, Namespace: policy.Spec.ArgoCDNamespace},
				Resource: backflowv1alpha1.ResourceRef{
					Group: res.Group, Version: res.Version, Kind: res.Kind, Namespace: res.Namespace, Name: res.Name,
				},
				Source: backflowv1alpha1.SourceRef{
					RepoURL:        summary.RepoURL,
					Revision:       summary.SyncedRevision,
					TargetRevision: summary.TargetRevision,
					Path:           summary.Path,
					Type:           srcType,
				},
				Changes:    changes,
				DetectedAt: metav1.Now(),
			},
		}
		if err := controllerutil.SetOwnerReference(policy, dp, r.Scheme); err != nil {
			return err
		}
		if err := r.Create(ctx, dp); err != nil {
			return err // AlreadyExists means a stale cache; the retry sees the object.
		}
		log.Info("Drift detected", "proposal", dp.Name, "kind", res.Kind, "resource", res.Name, "changes", len(changes))
		r.event(policy, corev1.EventTypeNormal, "DriftDetected",
			fmt.Sprintf("%s %s in application %s drifted (proposal %s)", res.Kind, res.Name, summary.Name, dp.Name))
		current = dp
	}

	// The change is live again, so a proposal that was waiting in its merge
	// request after a reset is no longer reset.
	if current.Annotations[annotationLiveReverted] != "" {
		if err := r.unannotate(ctx, current, annotationLiveReverted); err != nil {
			return err
		}
	}

	for _, p := range open {
		if p.Name == current.Name {
			continue
		}
		if err := r.annotate(ctx, p, annotationSupersededBy, current.Name); err != nil {
			return err
		}
		log.Info("Proposal superseded", "proposal", p.Name, "supersededBy", current.Name)
	}
	return nil
}

// annotate records a lifecycle event on a proposal. The DriftProposal
// controller owns the status and turns the annotation into a phase.
func (r *DriftReconciler) annotate(ctx context.Context, p *backflowv1alpha1.DriftProposal, key, value string) error {
	orig := p.DeepCopy()
	if p.Annotations == nil {
		p.Annotations = map[string]string{}
	}
	p.Annotations[key] = value
	return r.Patch(ctx, p, client.MergeFrom(orig))
}

// annotationRejectionReported marks a Rejected proposal whose rejection was
// already reported as an Event, so it is reported once.
const annotationRejectionReported = "backflow.io/rejection-reported"

// blockingProposal returns a terminal proposal that makes a new proposal for
// the same drift pointless, or nil. Without selfHeal a closed merge request
// leaves the live value in place and the resource OutOfSync, so the next
// cycle would see the same drift again and reopen what a person just closed.
// A proposal blocks when it ended in Rejected, Failed, or Superseded because
// Git changed, had identical changes, and was made at the same synced
// revision. Other changes or a new synced revision are new information.
// Reverted, Merged and Superseded by a newer drift never block: the drift
// went away or was handled, and when it comes back it is a new event.
func blockingProposal(all []backflowv1alpha1.DriftProposal, resourceHash string,
	changes []backflowv1alpha1.FieldChange, revision string) *backflowv1alpha1.DriftProposal {
	for i := range all {
		p := &all[i]
		if p.Labels[labelResource] != resourceHash || p.Spec.Source.Revision != revision ||
			!reflect.DeepEqual(p.Spec.Changes, changes) {
			continue
		}
		switch p.Status.Phase {
		case backflowv1alpha1.PhaseRejected, backflowv1alpha1.PhaseFailed:
			return p
		case backflowv1alpha1.PhaseSuperseded:
			if p.Status.SupersededBy == "" && p.Annotations[annotationSupersededBy] == "" {
				return p // replaced because the source changed in Git
			}
		}
	}
	return nil
}

// noteBlocked leaves a blocked drift alone. A Rejected one is reported once
// per proposal, because the cluster still differs from Git and only the
// user can resolve that.
func (r *DriftReconciler) noteBlocked(ctx context.Context, policy *backflowv1alpha1.BackflowPolicy,
	summary backflowv1alpha1.ApplicationSummary, blocker *backflowv1alpha1.DriftProposal) error {
	logf.FromContext(ctx).V(1).Info("Not proposing the same drift again",
		"proposal", blocker.Name, "phase", blocker.Status.Phase)
	if blocker.Status.Phase != backflowv1alpha1.PhaseRejected || blocker.Annotations[annotationRejectionReported] != "" {
		return nil
	}
	// Mark first: a missed Event is better than one on every cycle.
	if err := r.annotate(ctx, blocker, annotationRejectionReported, "true"); err != nil {
		return err
	}
	where := ""
	if blocker.Status.MergeRequest != nil {
		where = " " + blocker.Status.MergeRequest.URL
	}
	r.event(policy, corev1.EventTypeNormal, "ChangeRejected", fmt.Sprintf(
		"The merge request%s for %s was closed without merging, and the cluster still differs from Git. "+
			"Sync Application %s in Argo CD to revert the cluster to what Git says.",
		where, resourceLabel(blocker.Spec.Resource), summary.Name))
	return nil
}

func (r *DriftReconciler) unannotate(ctx context.Context, p *backflowv1alpha1.DriftProposal, key string) error {
	orig := p.DeepCopy()
	delete(p.Annotations, key)
	return r.Patch(ctx, p, client.MergeFrom(orig))
}

// hasOpenMergeRequest reports whether a proposal's change waits in an open
// merge request.
func hasOpenMergeRequest(p *backflowv1alpha1.DriftProposal) bool {
	return p.Status.Phase == backflowv1alpha1.PhaseProposed && p.Status.MergeRequest != nil
}

// syncedRevision is the revision Argo CD compared the Application with.
func syncedRevision(app *unstructured.Unstructured) string {
	rev, _, _ := unstructured.NestedString(app.Object, "status", "sync", "revision")
	if rev == "" {
		if revs, ok, _ := unstructured.NestedStringSlice(app.Object, "status", "sync", "revisions"); ok && len(revs) > 0 {
			rev = revs[0]
		}
	}
	return rev
}

// managedResourcesCache reads an Application's managed resources at most once
// per reconcile, and only when needed.
type managedResourcesCache struct {
	done  bool
	byKey map[string]argocd.ManagedResource
	err   error
}

func (c *managedResourcesCache) get(ctx context.Context, argo ManagedResourcesGetter, appName, appNamespace string) (map[string]argocd.ManagedResource, error) {
	if !c.done {
		c.done = true
		managed, err := argo.GetManagedResources(ctx, appName, appNamespace)
		if err != nil {
			c.err = err
		} else {
			c.byKey = make(map[string]argocd.ManagedResource, len(managed))
			for _, m := range managed {
				c.byKey[resourceHash(m.Group, m.Kind, m.Namespace, m.Name)] = m
			}
		}
	}
	return c.byKey, c.err
}

// resetBySync decides, for a proposal with an open merge request whose
// resource is in sync again, whether Argo CD synced a new revision that reset
// the cluster to Git. If so it marks the proposal live-reverted (it is not
// retired: the change waits in the merge request) and reports true.
//
// Two things must hold. The Application's revision differs from the one the
// proposal was made at, so a sync of other commits is what changed the
// resource; and the changed fields hold the values Git had, not the recorded
// live ones, so Git did not simply adopt the change. A reset in place, at the
// same revision, is an ordinary revert.
func (r *DriftReconciler) resetBySync(ctx context.Context, argo ManagedResourcesGetter,
	policy *backflowv1alpha1.BackflowPolicy, summary backflowv1alpha1.ApplicationSummary,
	app *unstructured.Unstructured, p *backflowv1alpha1.DriftProposal, managed *managedResourcesCache) (bool, error) {
	rev := syncedRevision(app)
	if rev == "" || rev == p.Spec.Source.Revision {
		return false, nil
	}
	if p.Annotations[annotationLiveReverted] != "" {
		return true, nil // already recorded; nothing more to do each cycle
	}
	byKey, err := managed.get(ctx, argo, summary.Name, policy.Spec.ArgoCDNamespace)
	if err != nil {
		return false, err
	}
	m, ok := byKey[p.Labels[labelResource]]
	if !ok || m.NormalizedLiveState == nil {
		return false, fmt.Errorf("the live state of the resource is not available")
	}
	atDesired, _ := drift.Where(m.NormalizedLiveState, p.Spec.Changes)
	if !atDesired {
		return false, nil
	}
	if err := r.annotate(ctx, p, annotationLiveReverted, rev); err != nil {
		return false, err
	}
	logf.FromContext(ctx).Info("Live change reset by a sync; it waits in its merge request",
		"proposal", p.Name, "revision", rev)
	return true, nil
}

func (r *DriftReconciler) event(policy *backflowv1alpha1.BackflowPolicy, eventType, reason, msg string) {
	if r.Recorder != nil {
		r.Recorder.Eventf(policy, nil, eventType, reason, "DriftDetection", "%s", msg)
	}
}

// argoReason maps an Argo CD client error to an Event reason.
func argoReason(err error) string {
	switch {
	case errors.Is(err, argocd.ErrUnauthorized):
		return "ArgoCDUnauthorized"
	case errors.Is(err, argocd.ErrNotFound):
		return "ArgoCDApplicationNotFound"
	default:
		return "ArgoCDUnreachable"
	}
}

// argoClient builds the Argo CD client from spec.argoCD and its Secrets.
func (r *DriftReconciler) argoClient(ctx context.Context, policy *backflowv1alpha1.BackflowPolicy) (ManagedResourcesGetter, error) {
	spec := policy.Spec.ArgoCD
	token, err := r.secretValue(ctx, policy.Namespace, spec.TokenSecretRef)
	if err != nil {
		return nil, fmt.Errorf("Argo CD token: %w", err)
	}
	cfg := argocd.Config{
		URL:                   spec.URL,
		Token:                 strings.TrimSpace(string(token)),
		InsecureSkipTLSVerify: spec.InsecureSkipTLSVerify,
	}
	if cfg.URL == "" {
		cfg.URL = defaultArgoCDURL
	}
	if spec.CASecretRef != nil {
		if cfg.CACert, err = r.secretValue(ctx, policy.Namespace, *spec.CASecretRef); err != nil {
			return nil, fmt.Errorf("Argo CD CA bundle: %w", err)
		}
	}
	if r.NewArgoClient != nil {
		return r.NewArgoClient(cfg)
	}
	return argocd.New(cfg)
}

func (r *DriftReconciler) secretValue(ctx context.Context, namespace string, ref corev1.SecretKeySelector) ([]byte, error) {
	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: ref.Name}, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("secret %q not found", ref.Name)
		}
		return nil, err
	}
	value, ok := secret.Data[ref.Key]
	if !ok || len(value) == 0 {
		return nil, fmt.Errorf("secret %q has no key %q", ref.Name, ref.Key)
	}
	return value, nil
}

// proposalSourceType maps the source type Argo CD reports to the API enum.
func proposalSourceType(s string) (backflowv1alpha1.SourceType, bool) {
	switch backflowv1alpha1.SourceType(s) {
	case backflowv1alpha1.SourceDirectory, backflowv1alpha1.SourceKustomize, backflowv1alpha1.SourceHelm:
		return backflowv1alpha1.SourceType(s), true
	}
	return "", false
}

// isOpen reports whether a proposal is in flight and has no pending
// superseded/reverted annotation that its controller has yet to act on.
func isOpen(p *backflowv1alpha1.DriftProposal) bool {
	if _, ok := p.Annotations[annotationSupersededBy]; ok {
		return false
	}
	if _, ok := p.Annotations[annotationReverted]; ok {
		return false
	}
	return isOpenPhase(p.Status.Phase)
}

// isOpenPhase reports whether a proposal is still in flight. A proposal
// without a phase was just created and counts as open.
func isOpenPhase(p backflowv1alpha1.ProposalPhase) bool {
	switch p {
	case "", backflowv1alpha1.PhaseDetected, backflowv1alpha1.PhaseMapping,
		backflowv1alpha1.PhaseProposed, backflowv1alpha1.PhaseUnmapped:
		return true
	}
	return false
}

// resourceHash identifies a resource independent of its Application.
func resourceHash(group, kind, namespace, name string) string {
	sum := sha256.Sum256([]byte(group + "/" + kind + "/" + namespace + "/" + name))
	return hex.EncodeToString(sum[:])[:16]
}

// safeLabel makes a value usable as a label value (max 63 characters).
func safeLabel(s string) string {
	if len(s) <= 63 {
		return s
	}
	sum := sha256.Sum256([]byte(s))
	return strings.TrimRight(s[:46], "-_.") + "-" + hex.EncodeToString(sum[:])[:16]
}

// proposalName builds <app>-<kind>-<name>-<hash>, lowercase, DNS-1123 safe and
// at most 63 characters. The hash covers the resource identity and the
// changes. A non-zero attempt appends a counter, used when an older proposal
// with identical changes already exists but is closed.
func proposalName(app string, res appResource, changes []backflowv1alpha1.FieldChange, attempt int) string {
	raw, _ := json.Marshal(changes)
	sum := sha256.Sum256(append([]byte(res.hash()+"|"), raw...))
	tail := "-" + hex.EncodeToString(sum[:])[:8]
	if attempt > 0 {
		tail += "-" + strconv.Itoa(attempt)
	}
	prefix := dnsSafe(app + "-" + res.Kind + "-" + res.Name)
	if max := 63 - len(tail); len(prefix) > max {
		prefix = strings.Trim(prefix[:max], "-")
	}
	if prefix == "" {
		prefix = "drift"
	}
	return prefix + tail
}

func dnsSafe(s string) string {
	var b strings.Builder
	for _, c := range strings.ToLower(s) {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			b.WriteRune(c)
		} else {
			b.WriteByte('-')
		}
	}
	out := b.String()
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	return strings.Trim(out, "-")
}

// policiesForApplication enqueues every policy that reads Applications from
// the namespace of the changed Application.
func policiesForApplication(ctx context.Context, c client.Reader, obj client.Object) []reconcile.Request {
	var list backflowv1alpha1.BackflowPolicyList
	if err := c.List(ctx, &list); err != nil {
		return nil
	}
	var requests []reconcile.Request
	for _, p := range list.Items {
		if p.Spec.ArgoCDNamespace == obj.GetNamespace() {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: p.Namespace, Name: p.Name},
			})
		}
	}
	return requests
}

// resourceSyncStatuses maps each resource of an Application to its sync status.
func resourceSyncStatuses(obj client.Object) map[string]string {
	app, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil
	}
	resources, _ := appResources(app)
	out := make(map[string]string, len(resources))
	for _, res := range resources {
		out[res.Version+"/"+res.hash()+"/"+res.Kind] = res.Status
	}
	return out
}

// hasOutOfSync reports whether any resource of the Application is OutOfSync.
func hasOutOfSync(obj client.Object) bool {
	for _, status := range resourceSyncStatuses(obj) {
		if status == syncOutOfSync {
			return true
		}
	}
	return false
}

// applicationSyncChanged decides when an Application change is worth a
// reconcile of the policies that watch it.
//
// Sync status changes (Synced <-> OutOfSync) always fire. That is not enough:
// a resource that is already OutOfSync can drift further, for example a
// ConfigMap value going from "debug" to "trace", and then no resource's sync
// status changes. Argo CD does record every new comparison in
// status.reconciledAt, so while something is OutOfSync a new comparison fires
// too; without it a second edit would only be noticed at the next periodic
// resync (up to driftResyncInterval later).
//
// The extra trigger is limited to OutOfSync Applications on purpose. Argo CD
// compares every Application regularly, and for Synced ones the reconcile
// would have nothing to do. reconcileApplication additionally calls the Argo
// CD API only for Applications with an OutOfSync resource or an open proposal.
func applicationSyncChanged() predicate.Predicate {
	reconciledAt := func(o client.Object) string {
		app, ok := o.(*unstructured.Unstructured)
		if !ok {
			return ""
		}
		s, _, _ := unstructured.NestedString(app.Object, "status", "reconciledAt")
		return s
	}
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			if !reflect.DeepEqual(resourceSyncStatuses(e.ObjectOld), resourceSyncStatuses(e.ObjectNew)) {
				return true
			}
			return hasOutOfSync(e.ObjectNew) && reconciledAt(e.ObjectOld) != reconciledAt(e.ObjectNew)
		},
	}
}

// policyDriftTrigger fires on generation changes and when Ready changes.
func policyDriftTrigger() predicate.Predicate {
	ready := func(o client.Object) bool {
		p, ok := o.(*backflowv1alpha1.BackflowPolicy)
		return ok && meta.IsStatusConditionTrue(p.Status.Conditions, "Ready")
	}
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			return e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration() || ready(e.ObjectOld) != ready(e.ObjectNew)
		},
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *DriftReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		For(&backflowv1alpha1.BackflowPolicy{}, builder.WithPredicates(policyDriftTrigger())).
		Named("drift")

	// Only watch Applications when Argo CD is installed; otherwise the
	// manager would fail to start. The periodic resync covers late installs.
	if _, err := mgr.GetRESTMapper().RESTMapping(applicationGVK.GroupKind(), applicationGVK.Version); err == nil {
		app := &unstructured.Unstructured{}
		app.SetGroupVersionKind(applicationGVK)
		b = b.Watches(app,
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
				return policiesForApplication(ctx, r.Client, obj)
			}),
			builder.WithPredicates(applicationSyncChanged()))
	} else {
		mgr.GetLogger().Info("Argo CD Application CRD not found; drift detection relies on periodic resync",
			"interval", driftResyncInterval.String())
	}
	return b.Complete(r)
}
