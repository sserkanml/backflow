package controller

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/controller/priorityqueue"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	backflowv1alpha1 "github.com/sserkanml/backflow/api/v1alpha1"
)

// controller-runtime (v0.25, priority queue on by default) keeps one entry per
// request. A RequeueAfter is a waiting entry; an event for the same request
// makes the entry ready at once, so the pass it starts uses the timer up, and
// the only timer after that pass is the one that pass returns. A pass that
// asked for nothing therefore leaves the periodic resync as the next look.
// This is how the queue is meant to work; the reconciler has to ask again on
// every pass, which nextStableCheck does for drifts that are still waiting.
func TestRequeueAfterIsUsedUpByAnEvent(t *testing.T) {
	q := priorityqueue.New[reconcile.Request]("requeue-test")
	defer q.ShutDown()
	req := reconcile.Request{}

	q.AddWithOpts(priorityqueue.AddOpts{After: 400 * time.Millisecond}, req) // pass 1: "look again in 400ms"
	q.Add(req)                                                               // an event arrives meanwhile

	got, _ := q.Get() // the event's pass starts at once, before the timer
	if got != req {
		t.Fatalf("got %v", got)
	}
	// The pass returned the periodic resync (no recheck asked), like a pass that returned early.
	q.AddWithOpts(priorityqueue.AddOpts{After: time.Hour}, req)
	q.Done(req)

	time.Sleep(700 * time.Millisecond) // past the timer pass 1 asked for
	if n := q.Len(); n != 0 {
		t.Fatalf("%d request(s) are ready: the 400ms timer survived, which is not how the queue works", n)
	}
}

func TestNextStableCheck(t *testing.T) {
	now := time.Date(2026, 3, 4, 10, 0, 0, 0, time.UTC)
	window := 30 * time.Second
	policy := &backflowv1alpha1.BackflowPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"},
		Spec:       backflowv1alpha1.BackflowPolicySpec{BatchWindow: &metav1.Duration{Duration: window}},
	}
	seen := func(app, pol string, firstAgo, lastAgo time.Duration, observations int) (pendingKey, *pendingDrift) {
		return pendingKey{policy: pol, app: app, resource: "r"},
			&pendingDrift{firstSeen: now.Add(-firstAgo), lastSeen: now.Add(-lastAgo), observations: observations}
	}
	type entry struct {
		app, pol          string
		firstAgo, lastAgo time.Duration
		observations      int
	}
	tests := []struct {
		name    string
		entries []entry
		want    time.Duration
	}{
		{"nothing is waiting", nil, 0},
		{"a drift seen 10s ago waits for the rest of the window", []entry{{"a", "ns/p", 10 * time.Second, 10 * time.Second, 1}}, 20 * time.Second},
		{"a drift whose window is over but was seen once", []entry{{"a", "ns/p", 31 * time.Second, 0, 1}}, time.Second},
		{"a drift that is ready is observed again soon", []entry{{"a", "ns/p", 40 * time.Second, 0, 2}}, retryStableAfter},
		{"a drift not seen for two windows is left to the resync", []entry{{"a", "ns/p", 5 * time.Minute, 61 * time.Second, 3}}, 0},
		{"another policy's drifts are not ours", []entry{{"a", "ns/other", 10 * time.Second, 10 * time.Second, 1}}, 0},
		{"the earliest wins", []entry{
			{"a", "ns/p", 5 * time.Second, 5 * time.Second, 1},
			{"b", "ns/p", 25 * time.Second, 25 * time.Second, 1},
		}, 5 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &DriftReconciler{Now: func() time.Time { return now }, pending: map[pendingKey]*pendingDrift{}}
			for _, e := range tt.entries {
				k, p := seen(e.app, e.pol, e.firstAgo, e.lastAgo, e.observations)
				r.pending[k] = p
			}
			if got := r.nextStableCheck(policy); got != tt.want {
				t.Errorf("nextStableCheck = %v, want %v", got, tt.want)
			}
		})
	}
}
