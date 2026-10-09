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
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	backflowv1alpha1 "github.com/sserkanml/backflow/api/v1alpha1"
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
	// Now returns the current time. Defaults to time.Now.
	Now func() time.Time
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
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=argoproj.io,resources=applications,verbs=get;list;watch

// isTerminalPhase reports whether a proposal has reached an end state that
// must never change again.
func isTerminalPhase(p backflowv1alpha1.ProposalPhase) bool {
	switch p {
	case backflowv1alpha1.PhaseMerged, backflowv1alpha1.PhaseRejected, backflowv1alpha1.PhaseReverted,
		backflowv1alpha1.PhaseSuperseded, backflowv1alpha1.PhaseFailed:
		return true
	}
	return false
}

func (r *DriftProposalReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var dp backflowv1alpha1.DriftProposal
	if err := r.Get(ctx, req.NamespacedName, &dp); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if isTerminalPhase(dp.Status.Phase) {
		if r.Writer != nil && needsCleanup(&dp) {
			return r.cleanup(ctx, &dp)
		}
		return ctrl.Result{}, nil
	}
	if r.Writer != nil && dp.Status.Phase == backflowv1alpha1.PhaseProposed && dp.Status.MergeRequest != nil {
		return r.trackProposed(ctx, &dp)
	}

	orig := dp.DeepCopy()
	if applyLifecycle(&dp) {
		return ctrl.Result{}, r.patchStatus(ctx, &dp, orig)
	}

	if dp.Status.Phase == "" {
		dp.Status.Phase = backflowv1alpha1.PhaseDetected
		dp.Status.Message = "Drift detected between Git and the cluster."
	}
	if dp.Status.Phase == backflowv1alpha1.PhaseDetected && r.Repos != nil {
		dp.Status.Phase = backflowv1alpha1.PhaseMapping
		dp.Status.Message = "Mapping the drift to the file in Git that defines the resource."
	}
	if err := r.patchStatus(ctx, &dp, orig); err != nil {
		return ctrl.Result{}, err
	}
	if dp.Status.Phase != backflowv1alpha1.PhaseDetected && orig.Status.Phase != dp.Status.Phase {
		log.Info("Proposal phase set", "proposal", dp.Name, "phase", dp.Status.Phase)
	}

	if r.Repos == nil || dp.Status.Phase != backflowv1alpha1.PhaseMapping {
		return ctrl.Result{}, nil
	}
	if !mappingSettled(&dp) {
		res, err := r.mapProposal(ctx, &dp)
		// Recording the mapping does not change the generation, so go on to
		// propose in this pass.
		if err != nil || !res.IsZero() || dp.Status.Phase != backflowv1alpha1.PhaseMapping ||
			!meta.IsStatusConditionTrue(dp.Status.Conditions, conditionMapped) {
			return res, err
		}
	}
	if r.Writer == nil {
		return ctrl.Result{}, nil
	}
	return r.propose(ctx, &dp)
}

// patchStatus writes the status changes made to dp since orig. It is a no-op
// when nothing changed.
func (r *DriftProposalReconciler) patchStatus(ctx context.Context, dp, orig *backflowv1alpha1.DriftProposal) error {
	patch := client.MergeFrom(orig)
	if data, err := patch.Data(dp); err == nil && string(data) == "{}" {
		return nil
	}
	return client.IgnoreNotFound(r.Status().Patch(ctx, dp, patch))
}

// lifecycleAnnotationsChanged lets updates through when the generation did
// not change but a lifecycle annotation did (metadata changes do not bump
// the generation).
func lifecycleAnnotationsChanged() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			for _, k := range []string{annotationSupersededBy, annotationReverted} {
				if e.ObjectOld.GetAnnotations()[k] != e.ObjectNew.GetAnnotations()[k] {
					return true
				}
			}
			return false
		},
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *DriftProposalReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&backflowv1alpha1.DriftProposal{},
			builder.WithPredicates(predicate.Or(predicate.GenerationChangedPredicate{}, lifecycleAnnotationsChanged()))).
		// A proposal that has no access to its repository recovers by itself
		// once the token Secret or the ScmConnection is fixed.
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.authFailedProposals)).
		// The policy's status names the ScmConnection that matches a repository.
		Watches(&backflowv1alpha1.BackflowPolicy{}, handler.EnqueueRequestsFromMapFunc(r.authFailedProposals)).
		Watches(&backflowv1alpha1.ScmConnection{}, handler.EnqueueRequestsFromMapFunc(r.authFailedProposals),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("driftproposal").
		Complete(r)
}
