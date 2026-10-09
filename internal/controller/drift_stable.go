package controller

import (
	"encoding/json"
	"time"

	backflowv1alpha1 "github.com/sserkanml/backflow/api/v1alpha1"
)

// pendingKey identifies a drifted resource of an Application under a policy.
type pendingKey struct {
	policy, app, resource string
}

// pendingDrift is what has been seen of a drift that is not yet a proposal.
type pendingDrift struct {
	changes          string // the changes, encoded
	synced, compared string // the revisions they were seen at
	firstSeen        time.Time
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
func (r *DriftReconciler) stable(policy *backflowv1alpha1.BackflowPolicy, app, resource string,
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
		p = &pendingDrift{changes: string(enc), synced: st.synced, compared: st.compared, firstSeen: now}
		r.pending[key] = p
	}
	p.observations++

	elapsed := now.Sub(p.firstSeen)
	if p.observations >= 2 && elapsed >= window {
		return true, 0
	}
	// Come back when the window is over, and at least soon enough to observe
	// a second time.
	return false, max(window-elapsed, time.Second)
}

// forgetStable drops what was seen of the resources of an Application that
// are no longer drifting (they are Synced again, or gone). keep lists the
// resources that still drift.
func (r *DriftReconciler) forgetStable(policy *backflowv1alpha1.BackflowPolicy, app string, keep map[string]bool) {
	pol := policy.Namespace + "/" + policy.Name
	r.stableMu.Lock()
	defer r.stableMu.Unlock()
	for k := range r.pending {
		if k.policy == pol && k.app == app && !keep[k.resource] {
			delete(r.pending, k)
		}
	}
}
