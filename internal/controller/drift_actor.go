package controller

import (
	"context"
	"slices"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	backflowv1 "github.com/sserkanml/backflow/api/v1"
	"github.com/sserkanml/backflow/internal/managedfields"
)

const (
	maxActorFields = 16
	maxActorOthers = 4
)

// changedByNone is the label of a drift for which no manager could be named: there is no actor.
// "unknown" is a manager that was named but is neither a known tool nor a known controller.
const changedByNone = "none"

// managersOf says, from the managedFields of the live object Argo CD returned, which managers
// wrote the drifted fields, the latest first (spec.managers), and how the latest is to be counted
// (interactive, controller, unknown or, without any, none: the label of the metric). It returns
// no managers when nothing can be told, and never an error: a proposal does not wait for it.
func managersOf(ctx context.Context, live map[string]any, changes []backflowv1.FieldChange) ([]backflowv1.ManagerWrite, string) {
	pointers := make([]string, 0, len(changes))
	for _, c := range changes {
		pointers = append(pointers, c.Path)
	}
	attr, err := managedfields.Attribute(live, pointers)
	if err != nil {
		logf.FromContext(ctx).V(1).Info("The managedFields of the live object cannot be read; the proposal has no managers", "error", err.Error())
		return nil, changedByNone
	}
	if attr == nil {
		return nil, changedByNone
	}
	managers := []backflowv1.ManagerWrite{writeOf(attr.Latest)}
	for _, o := range attr.Others[:min(len(attr.Others), maxActorOthers)] {
		managers = append(managers, writeOf(o))
	}
	return managers, string(managedfields.Classify(attr.Latest.Manager))
}

func writeOf(o managedfields.Owner) backflowv1.ManagerWrite {
	return backflowv1.ManagerWrite{
		Manager: o.Manager, Operation: operationOf(o.Operation), Time: timeOf(o.Time), Fields: capped(o.Paths, maxActorFields),
	}
}

// timeOf is the time of a managedFields entry; an entry without one has none.
func timeOf(t time.Time) *metav1.Time {
	if t.IsZero() {
		return nil
	}
	return &metav1.Time{Time: t}
}

// operationOf keeps the two operations the API accepts; anything else is left out.
func operationOf(op string) backflowv1.ManagedFieldsOperation {
	if op == managedfields.OperationUpdate || op == managedfields.OperationApply {
		return backflowv1.ManagedFieldsOperation(op)
	}
	return ""
}

func capped(in []string, limit int) []string {
	if len(in) > limit {
		in = in[:limit]
	}
	return slices.Clone(in)
}
