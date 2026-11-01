package controller

import (
	"context"
	"slices"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	backflowv1alpha1 "github.com/sserkanml/backflow/api/v1alpha1"
	"github.com/sserkanml/backflow/internal/managedfields"
)

const (
	maxActorFields = 16
	maxActorOthers = 4
)

// actorOf says, from the managedFields of the live object Argo CD returned, which manager
// wrote the drifted fields last, and how that manager is to be counted
// (interactive, controller or unknown, the label of the metric). It returns a nil Actor
// when nothing can be told, and never an error: a proposal does not wait for it.
func actorOf(ctx context.Context, live map[string]any, changes []backflowv1alpha1.FieldChange) (*backflowv1alpha1.Actor, string) {
	pointers := make([]string, 0, len(changes))
	for _, c := range changes {
		pointers = append(pointers, c.Path)
	}
	attr, err := managedfields.Attribute(live, pointers)
	if err != nil {
		logf.FromContext(ctx).V(1).Info("The managedFields of the live object cannot be read; the proposal has no actor", "error", err.Error())
		return nil, string(managedfields.Unknown)
	}
	if attr == nil {
		return nil, string(managedfields.Unknown)
	}
	actor := &backflowv1alpha1.Actor{
		Manager:   attr.Latest.Manager,
		Operation: operationOf(attr.Latest.Operation),
		Time:      timeOf(attr.Latest.Time),
	}
	if len(attr.Others) > 0 {
		actor.Fields = capped(attr.Latest.Paths, maxActorFields)
		for _, o := range attr.Others[:min(len(attr.Others), maxActorOthers)] {
			actor.Others = append(actor.Others, backflowv1alpha1.ActorManager{
				Manager: o.Manager, Operation: operationOf(o.Operation), Time: timeOf(o.Time), Fields: capped(o.Paths, maxActorFields),
			})
		}
	}
	return actor, string(managedfields.Classify(actor.Manager))
}

// timeOf is the time of a managedFields entry; an entry without one has none.
func timeOf(t time.Time) *metav1.Time {
	if t.IsZero() {
		return nil
	}
	return &metav1.Time{Time: t}
}

// operationOf keeps the two operations the API accepts; anything else is left out.
func operationOf(op string) string {
	if op == managedfields.OperationUpdate || op == managedfields.OperationApply {
		return op
	}
	return ""
}

func capped(in []string, limit int) []string {
	if len(in) > limit {
		in = in[:limit]
	}
	return slices.Clone(in)
}
