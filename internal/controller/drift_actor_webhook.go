package controller

import (
	"context"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	backflowv1alpha1 "github.com/sserkanml/backflow/api/v1alpha1"
	"github.com/sserkanml/backflow/internal/actorwebhook"
	"github.com/sserkanml/backflow/internal/managedfields"
	"github.com/sserkanml/backflow/internal/metrics"
)

// ActorRecords pairs a new proposal with the ChangeRecords of the actor webhook. It is
// optional: without it, or without a record that the live managedFields confirm, the
// actor is what managedFields say and nothing more.
type ActorRecords struct {
	// Reader lists the ChangeRecords (a cache restricted to Namespace).
	Reader client.Reader
	// Namespace is where the records are.
	Namespace string
	// Tolerance is how far a record's time may be from the managedFields entry's.
	// Zero means actorwebhook.DefaultTolerance.
	Tolerance time.Duration
}

// Refine adds the user name and kind of a verified record to the actor. It never fails and
// never waits for more than one cached list: a proposal does not depend on the webhook.
// The record is believed only if the managedFields of the live object agree with it, so a
// review nobody made, or one that names someone else, does not set a name.
func (a *ActorRecords) Refine(ctx context.Context, live map[string]any, changes []backflowv1alpha1.FieldChange,
	actor *backflowv1alpha1.Actor, window actorwebhook.Window) *backflowv1alpha1.Actor {
	if a == nil || actor == nil || a.Reader == nil {
		return actor
	}
	uid := liveUID(live)
	if uid == "" {
		return actor
	}
	pointers := make([]string, 0, len(changes))
	for _, c := range changes {
		pointers = append(pointers, c.Path)
	}
	attr, err := managedfields.Attribute(live, pointers)
	if err != nil || attr == nil {
		return actor
	}
	var list backflowv1alpha1.ChangeRecordList
	if err := a.Reader.List(ctx, &list, client.InNamespace(a.Namespace),
		client.MatchingLabels{actorwebhook.LabelObjectUID: uid}); err != nil {
		logf.FromContext(ctx).V(1).Info("Cannot list the change records; the proposal has no user name", "error", err.Error())
		return actor
	}
	records := make([]actorwebhook.Record, 0, len(list.Items))
	for i := range list.Items {
		records = append(records, actorwebhook.FromChangeRecord(&list.Items[i]))
	}
	// A write to a subresource (kubectl scale) has no field manager in the request, and its
	// managedFields entry no time: the history of the Application bounds the records instead.
	entries, err := managedfields.Entries(live, pointers)
	if err != nil {
		entries = nil
	}
	verdict := actorwebhook.Resolve(uid, attr.Latest, entries, records, a.Tolerance, window)
	logf.FromContext(ctx).V(1).Info("Paired a proposal with the change records", "result", verdict.Result,
		"records", len(records), "manager", attr.Latest.Manager, "window", windowText(window))
	metrics.ActorMatch(verdict.Result)
	if verdict.Result != metrics.ActorMatched {
		return actor
	}
	out := actor.DeepCopy()
	out.Kind = backflowv1alpha1.ActorKind(verdict.Record.Kind)
	out.Username = verdict.Record.Username
	out.Groups = verdict.Record.Groups
	return out
}

// windowText is a window for a log line: times only, no names.
func windowText(w actorwebhook.Window) string {
	f := func(t time.Time) string {
		if t.IsZero() {
			return "unknown"
		}
		return t.UTC().Format(time.RFC3339)
	}
	return f(w.After) + ".." + f(w.Before)
}

func liveUID(live map[string]any) string {
	meta, _ := live["metadata"].(map[string]any)
	uid, _ := meta["uid"].(string)
	return uid
}

// forText is the proposal as the text of a merge request and a commit sees it: without the
// user name unless usernameInMergeRequest is on. The proposal itself keeps it.
func (r *DriftProposalReconciler) forText(dp *backflowv1alpha1.DriftProposal) *backflowv1alpha1.DriftProposal {
	if r.UsernameInMergeRequest || dp.Spec.Actor == nil || (dp.Spec.Actor.Username == "" && len(dp.Spec.Actor.Groups) == 0) {
		return dp
	}
	c := dp.DeepCopy()
	c.Spec.Actor.Username, c.Spec.Actor.Groups = "", nil
	return c
}
