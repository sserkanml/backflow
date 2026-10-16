package controller

import (
	"context"
	"encoding/json"
	"time"

	logf "sigs.k8s.io/controller-runtime/pkg/log"

	backflowv1alpha1 "github.com/sserkanml/backflow/api/v1alpha1"
)

// pendingKey identifies a drifted resource of an Application under a policy.
type pendingKey struct {
	policy, app, resource string
}

// pendingDrift is what has been seen of a drift that is not yet a proposal.
type pendingDrift struct {
	label            string // kind and name of the resource, for logs
	changes          string // the changes, encoded
	synced, compared string // the revisions they were seen at
	firstSeen        time.Time
	lastSeen         time.Time
	observations     int
}

// stable decides whether a drift has lasted long enough to become a proposal.
// It must have been seen with the same changes at the same synced and compared
// revisions for the policy's batchWindow, across at least two observations.
// This does not depend on Argo CD's internals: a resource that looks OutOfSync
// for a moment after a sync, or edits that follow each other quickly, never
// reach the window with the same changes, so no proposal is made for them and
// the final state gets one. When it is not ready, wait says when to look again.
//
// The state is kept in memory. Losing it on a restart only delays a proposal.
func (r *DriftReconciler) stable(policy *backflowv1alpha1.BackflowPolicy, app, resource, label string,
	changes []backflowv1alpha1.FieldChange, st aheadState) (ready bool, wait time.Duration) {
	enc, _ := json.Marshal(changes)
	key := pendingKey{policy: policy.Namespace + "/" + policy.Name, app: app, resource: resource}
	window := policy.Spec.EffectiveBatchWindow()
	now := r.now()

	r.stableMu.Lock()
	defer r.stableMu.Unlock()
	if r.pending == nil {
		r.pending = map[pendingKey]*pendingDrift{}
	}
	p := r.pending[key]
	if p == nil || p.changes != string(enc) || p.synced != st.synced || p.compared != st.compared {
		p = &pendingDrift{label: label, changes: string(enc), synced: st.synced, compared: st.compared, firstSeen: now}
		r.pending[key] = p
	}
	p.observations++
	p.lastSeen = now

	elapsed := now.Sub(p.firstSeen)
	if p.observations >= 2 && elapsed >= window {
		return true, 0
	}
	// Come back when the window is over, and at least soon enough to observe
	// a second time.
	return false, max(window-elapsed, time.Second)
}

// nextStableCheck says when the policy should be looked at again for the drifts
// that are waiting to become stable, or 0 when none is. It is derived from what
// has been seen, not from what the current pass did: the pass that asked for a
// recheck may be followed by another one (started by an event) that found the
// recheck already used and, taking an early path, asked for nothing.
//
// A drift is waited for until its window is over; one that is ready only needs
// one more observation, which is asked for soon. A drift that has not been seen
// for two windows (the Application is paused or held) is left to the periodic
// resync and to events, so a paused Application does not make the controller
// spin.
func (r *DriftReconciler) nextStableCheck(policy *backflowv1alpha1.BackflowPolicy) time.Duration {
	pol := policy.Namespace + "/" + policy.Name
	window := policy.Spec.EffectiveBatchWindow()
	now := r.now()
	var next time.Duration

	r.stableMu.Lock()
	defer r.stableMu.Unlock()
	for k, p := range r.pending {
		if k.policy != pol || now.Sub(p.lastSeen) > 2*window {
			continue
		}
		wait := max(p.firstSeen.Add(window).Sub(now), time.Second)
		if p.observations >= 2 && now.Sub(p.firstSeen) >= window {
			wait = retryStableAfter
		}
		if next == 0 || wait < next {
			next = wait
		}
	}
	return next
}

// retryStableAfter is how soon a drift that is ready, but was not observed again
// in the last pass, is looked at.
const retryStableAfter = 5 * time.Second

// forgetStable drops what was seen of the resources of an Application that
// are no longer drifting (they are Synced again, or gone). keep lists the
// resources that still drift; outOfSync those Argo CD still reports OutOfSync,
// which tells why a resource was dropped. Each drop is logged at V(1): a drift
// that appears and goes away is never proposed, and without the line there is
// nothing to tell why.
func (r *DriftReconciler) forgetStable(ctx context.Context, policy *backflowv1alpha1.BackflowPolicy, app string,
	keep, outOfSync map[string]bool) {
	pol := policy.Namespace + "/" + policy.Name
	type dropped struct {
		label, reason string
		observations  int
		seen          time.Duration
	}
	var drops []dropped
	now := r.now()
	r.stableMu.Lock()
	for k, p := range r.pending {
		if k.policy != pol || k.app != app || keep[k.resource] {
			continue
		}
		reason := "the resource is no longer OutOfSync"
		if outOfSync[k.resource] {
			reason = "the resource no longer differs from Git after ignored fields and filters"
		}
		drops = append(drops, dropped{label: p.label, reason: reason, observations: p.observations, seen: now.Sub(p.firstSeen)})
		delete(r.pending, k)
	}
	r.stableMu.Unlock()
	log := logf.FromContext(ctx)
	for _, d := range drops {
		log.V(1).Info("Dropping a drift that was waiting to become stable",
			"application", app, "resource", d.label, "reason", d.reason,
			"observations", d.observations, "seenFor", d.seen.String())
	}
}
