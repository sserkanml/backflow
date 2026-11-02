/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	backflowv1beta1 "github.com/sserkanml/backflow/api/v1beta1"
	"github.com/sserkanml/backflow/internal/argocd"
	"github.com/sserkanml/backflow/internal/helmmap"
	"github.com/sserkanml/backflow/internal/metrics"
	"github.com/sserkanml/backflow/internal/rendermap"
	"github.com/sserkanml/backflow/internal/scm"
)

// DriftProposalReconciler is the only writer of DriftProposal status. The
// drift controller creates proposals and signals lifecycle events through
// annotations; this controller turns them into phases and maps each proposal
// to the Git file that defines the drifted resource.
type DriftProposalReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Repos reads the source repository at the synced commit. When nil,
	// mapping is disabled and proposals stay in Detected.
	Repos RepositoryReader
	// Writer writes proposals to Git. When nil, mapped proposals are not
	// proposed and stay in Mapping.
	Writer RepositoryWriter
	// NewProvider builds the merge request client of an ScmConnection.
	// Defaults to scm.New.
	NewProvider func(scm.Config) (scm.Provider, error)
	// TrackInterval is how often an open merge request is polled. Defaults to
	// two minutes.
	TrackInterval time.Duration
	// CommitScanLimit bounds the history searched for a proposal's own commit
	// in DirectCommit mode. Defaults to 500.
	CommitScanLimit int
	// Now returns the current time. Defaults to time.Now.
	Now func() time.Time
	// UsernameInMergeRequest puts the user name of the actor into the text of the merge
	// request and the commit. Off by default: Git keeps it for ever.
	UsernameInMergeRequest bool
	// Recorder emits Events about proposals. Optional.
	Recorder events.EventRecorder
	// NewRenderer builds the renderer of Helm sources from the Argo CD
	// connection of a policy. Defaults to a rendermap.ArgoRenderer over argocd.New.
	NewRenderer func(argocd.Config) (rendermap.Renderer, error)
	// NewChartArgo builds the Argo CD client that reads the values of a chart
	// and renders by revision, for multi-source Applications. Defaults to argocd.New.
	NewChartArgo func(argocd.Config) (ChartArgo, error)
	// ArgoCD is the connection policies without spec.argoCD use. Optional.
	ArgoCD *ArgoCDDefaults
	// Visibility tells which Applications the token of a policy with its own
	// spec.argoCD may get. Nil: not asked.
	Visibility *ApplicationVisibility
	// FileDeps lets the renders of charts with file:// dependencies upload them
	// unpacked, where that was shown to render the same. Nil switches it off.
	FileDeps *helmmap.UnpackedDeps
	// RenderBudget is the time all renders of one verification may take
	// together. Zero means rendermap.DefaultBudget.
	RenderBudget time.Duration

	renderers    argoClientCache[rendermap.Renderer] // per policy, so connections are reused
	chartClients argoClientCache[ChartArgo]
	memos        mappingMemos // the mapping of a proposal, for the push (see mapForHead)
}

// trackEvery is how often an open merge request is polled.
func (r *DriftProposalReconciler) trackEvery() time.Duration {
	if r.TrackInterval > 0 {
		return r.TrackInterval
	}
	return trackInterval
}

// +kubebuilder:rbac:groups=backflow.io,resources=driftproposals,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=backflow.io,resources=driftproposals/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=backflow.io,resources=driftproposals/finalizers,verbs=update
// +kubebuilder:rbac:groups=backflow.io,resources=backflowpolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups=backflow.io,resources=scmconnections,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get
// +kubebuilder:rbac:groups=argoproj.io,resources=applications,verbs=get;list;watch

// isTerminalPhase reports whether a proposal has reached an end state that
// must never change again.
func isTerminalPhase(p backflowv1beta1.ProposalPhase) bool {
	switch p {
	case backflowv1beta1.PhaseMerged, backflowv1beta1.PhaseRejected, backflowv1beta1.PhaseReverted,
		backflowv1beta1.PhaseSuperseded, backflowv1beta1.PhaseFailed:
		return true
	}
	return false
}

func (r *DriftProposalReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var dp backflowv1beta1.DriftProposal
	if err := r.Get(ctx, req.NamespacedName, &dp); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if isTerminalPhase(dp.Status.Phase) {
		return r.reconcileFinished(ctx, &dp)
	}
	if r.Writer != nil && dp.Status.Phase == backflowv1beta1.PhaseProposed && dp.Status.GitMergeRequest() != nil {
		return r.trackProposed(ctx, &dp)
	}

	orig := dp.DeepCopy()
	if applyLifecycle(&dp) {
		if err := r.patchStatus(ctx, &dp, orig); err != nil {
			return ctrl.Result{}, err
		}
		// A proposal retired before its merge request was recorded can still
		// have pushed a branch and opened a merge request.
		if r.Writer != nil && needsCleanup(&dp) {
			return r.cleanup(ctx, &dp)
		}
		return ctrl.Result{}, nil
	}

	// Defense in depth: whatever the policy controller recorded, a proposal of an
	// Application the policy may not see makes no call to Argo CD.
	if done, res, err := r.retireIfRevoked(ctx, &dp, orig); done {
		return res, err
	}

	// A drift with more changes than a proposal holds lists the first ones only: part of a drift is
	// never proposed to Git. It waits Unmapped for a person.
	if dp.Spec.Truncated {
		return ctrl.Result{}, r.unmapTruncated(ctx, &dp)
	}

	if dp.Status.Phase == "" {
		dp.Status.Phase = backflowv1beta1.PhaseDetected
		dp.Status.Message = "Drift detected between Git and the cluster."
	}
	if dp.Status.Phase == backflowv1beta1.PhaseDetected && r.Repos != nil {
		dp.Status.Phase = backflowv1beta1.PhaseMapping
		dp.Status.Message = "Mapping the drift to the file in Git that defines the resource."
	}
	if err := r.patchStatus(ctx, &dp, orig); err != nil {
		return ctrl.Result{}, err
	}
	if dp.Status.Phase != backflowv1beta1.PhaseDetected && orig.Status.Phase != dp.Status.Phase {
		log.Info("Proposal phase set", "proposal", dp.Name, "phase", dp.Status.Phase)
	}

	if r.Repos == nil || dp.Status.Phase != backflowv1beta1.PhaseMapping {
		return ctrl.Result{}, nil
	}
	if !mappingSettled(&dp) {
		res, err := r.mapProposal(ctx, &dp)
		// Recording the mapping does not change the generation, so go on to
		// propose in this pass.
		if err != nil || !res.IsZero() || dp.Status.Phase != backflowv1beta1.PhaseMapping ||
			!meta.IsStatusConditionTrue(dp.Status.Conditions, conditionMapped) {
			return res, err
		}
	}
	if r.Writer == nil {
		return ctrl.Result{}, nil
	}
	res, err := r.propose(ctx, &dp)
	if err == nil && r.Writer != nil && needsCleanup(&dp) {
		// Superseded by a change in Git after a branch or merge request was left behind.
		return r.cleanup(ctx, &dp)
	}
	return res, err
}

// reconcileFinished is a pass over a proposal that reached a final phase: the cleanup of its branch and merge request
// when one is left, then the retention.
func (r *DriftProposalReconciler) reconcileFinished(ctx context.Context, dp *backflowv1beta1.DriftProposal) (ctrl.Result, error) {
	if r.Writer != nil && needsCleanup(dp) {
		return r.cleanup(ctx, dp)
	}
	return r.retain(ctx, dp)
}

// unmapTruncated leaves a proposal of a drift with more changes than a proposal holds Unmapped.
func (r *DriftProposalReconciler) unmapTruncated(ctx context.Context, dp *backflowv1beta1.DriftProposal) error {
	if dp.Status.Phase == backflowv1beta1.PhaseUnmapped {
		return nil
	}
	return r.unmappedWith(ctx, dp, backflowv1beta1.ReasonTooManyChanges, fmt.Sprintf(
		"The drift has more than %d field changes. Only the first %d are listed, and nothing is proposed to Git for part of a drift.",
		backflowv1beta1.MaxChanges, backflowv1beta1.MaxChanges), nil)
}

// maxRetentionCheck is the longest a finished proposal waits to be looked at again for its retention.
const maxRetentionCheck = 12 * time.Hour

// retain deletes a finished proposal once the policy's retention
// (spec.retention.finishedProposals, 14 days by default) has passed since it finished, and asks to
// be looked at again when it will. Proposals that are open, or Unmapped, never reach here. A
// proposal whose policy is gone is deleted with it by its owner reference. Deleting a proposal
// does not touch its merge request.
func (r *DriftProposalReconciler) retain(ctx context.Context, dp *backflowv1beta1.DriftProposal) (ctrl.Result, error) {
	policy, err := r.policyOf(ctx, dp)
	if err != nil {
		if _, held := errors.AsType[*heldError](err); held {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	retention := policy.Spec.FinishedRetention()
	if retention <= 0 {
		return ctrl.Result{}, nil
	}
	finished := finishedTime(dp)
	if dp.Status.FinishedAt == nil {
		// A proposal that finished before the field existed: count from its last change, and keep that.
		orig := dp.DeepCopy()
		dp.Status.FinishedAt = &metav1.Time{Time: finished}
		if err := r.patchStatus(ctx, dp, orig); err != nil {
			return ctrl.Result{}, err
		}
	}
	if left := finished.Add(retention).Sub(r.now()); left > 0 {
		return ctrl.Result{RequeueAfter: min(left+time.Second, maxRetentionCheck)}, nil
	}
	logf.FromContext(ctx).Info("Deleting a finished proposal after its retention", "proposal", dp.Name, "phase", dp.Status.Phase,
		"finished", finished.UTC().Format(time.RFC3339), "retention", retention.String())
	return ctrl.Result{}, client.IgnoreNotFound(r.Delete(ctx, dp, client.Preconditions{UID: &dp.UID}))
}

// finishedTime is when a proposal finished: status.finishedAt, else the latest time one of its
// conditions changed, else when it was created.
func finishedTime(dp *backflowv1beta1.DriftProposal) time.Time {
	if dp.Status.FinishedAt != nil {
		return dp.Status.FinishedAt.Time
	}
	t := dp.CreationTimestamp.Time
	for _, c := range dp.Status.Conditions {
		if c.LastTransitionTime.After(t) {
			t = c.LastTransitionTime.Time
		}
	}
	return t
}

// patchStatus writes the status changes made to dp since orig. It is a no-op
// when nothing changed.
func (r *DriftProposalReconciler) patchStatus(ctx context.Context, dp, orig *backflowv1beta1.DriftProposal) error {
	if dp.Status.Phase.Finished() && dp.Status.FinishedAt == nil {
		// The retention of finished proposals counts from here.
		finished := metav1.NewTime(r.now())
		dp.Status.FinishedAt = &finished
	}
	patch := client.MergeFrom(orig)
	if data, err := patch.Data(dp); err == nil && string(data) == "{}" {
		return nil
	}
	return client.IgnoreNotFound(r.Status().Patch(ctx, dp, patch))
}

// signalsChanged lets updates through when the generation did not change but the signals the policy
// and drift controllers leave in the status did (status changes do not bump the generation).
func signalsChanged() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldDP, ok1 := e.ObjectOld.(*backflowv1beta1.DriftProposal)
			newDP, ok2 := e.ObjectNew.(*backflowv1beta1.DriftProposal)
			return ok1 && ok2 && oldDP.Status.Signal() != newDP.Status.Signal()
		},
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *DriftProposalReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&backflowv1beta1.DriftProposal{},
			builder.WithPredicates(predicate.Or(predicate.GenerationChangedPredicate{}, signalsChanged()))).
		// A held proposal goes on by itself once what holds it is changed: the
		// ScmConnection is fixed, or the policy leaves ReportOnly mode. Secrets
		// are not watched: a held proposal retries with a growing delay, which
		// picks up a fixed token Secret.
		// The policy's status names the ScmConnection that matches a repository.
		Watches(&backflowv1beta1.BackflowPolicy{}, handler.EnqueueRequestsFromMapFunc(r.policyProposals)).
		Watches(&backflowv1beta1.ScmConnection{}, handler.EnqueueRequestsFromMapFunc(r.heldProposals),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("driftproposal").
		Complete(r)
}

// ProposalCounts returns what the backflow_proposals gauge reports: the
// proposals of each policy by phase, read from the cache of the manager. A
// proposal that has no phase yet is Pending.
func ProposalCounts(c client.Reader) func() []metrics.ProposalCount {
	return func() []metrics.ProposalCount {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var list backflowv1beta1.DriftProposalList
		if err := c.List(ctx, &list); err != nil {
			return nil
		}
		type key struct {
			policy metrics.Policy
			phase  string
		}
		counts := map[key]int{}
		for i := range list.Items {
			dp := &list.Items[i]
			phase := string(dp.Status.Phase)
			if phase == "" {
				phase = "Pending"
			}
			counts[key{metrics.Policy{Namespace: dp.Namespace, Name: dp.Spec.PolicyRef.Name}, phase}]++
		}
		out := make([]metrics.ProposalCount, 0, len(counts))
		for k, n := range counts {
			out = append(out, metrics.ProposalCount{Policy: k.policy, Phase: k.phase, Count: n})
		}
		return out
	}
}
