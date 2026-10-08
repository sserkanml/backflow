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

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	backflowv1alpha1 "github.com/sserkanml/backflow/api/v1alpha1"
)

// DriftProposalReconciler is the only writer of DriftProposal status. The
// drift controller creates proposals and signals lifecycle events through
// annotations; this controller turns them into phases.
type DriftProposalReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=backflow.io,resources=driftproposals,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=backflow.io,resources=driftproposals/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=backflow.io,resources=driftproposals/finalizers,verbs=update

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
		return ctrl.Result{}, nil
	}

	orig := dp.DeepCopy()
	switch {
	case dp.Annotations[annotationSupersededBy] != "":
		dp.Status.Phase = backflowv1alpha1.PhaseSuperseded
		dp.Status.SupersededBy = dp.Annotations[annotationSupersededBy]
		dp.Status.Message = "A newer drift on the same resource replaced this proposal."
	case dp.Annotations[annotationReverted] != "":
		dp.Status.Phase = backflowv1alpha1.PhaseReverted
		dp.Status.Message = dp.Annotations[annotationReverted]
	case dp.Status.Phase == "":
		dp.Status.Phase = backflowv1alpha1.PhaseDetected
		dp.Status.Message = "Drift detected between Git and the cluster."
	default:
		return ctrl.Result{}, nil
	}

	if err := r.Status().Patch(ctx, &dp, client.MergeFrom(orig)); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	log.Info("Proposal phase set", "proposal", dp.Name, "phase", dp.Status.Phase)
	return ctrl.Result{}, nil
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
		Named("driftproposal").
		Complete(r)
}
