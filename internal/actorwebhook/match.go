package actorwebhook

import (
	"math"
	"strings"
	"time"

	"github.com/sserkanml/backflow/internal/managedfields"
	"github.com/sserkanml/backflow/internal/metrics"
)

// DefaultTolerance is how far the time of a record may be from the time of the managedFields
// entry it is checked against.
const DefaultTolerance = 10 * time.Second

// Verdict is the result of pairing a manager of the live object with the records.
type Verdict struct {
	// Record is the record that matched; zero unless Result is metrics.ActorMatched.
	Record Record
	// Result is one of the metrics.Actor* results.
	Result string
}

// Match finds the record that says who is behind a manager's change, and checks it
// against the managedFields entry of that manager, so that a record the cluster did not
// write (a forged review) is not believed. A record counts only when all hold: it is for
// this object (uid), its field manager is the entry's manager, the time of its last change
// is within tolerance of the entry's time, and it names a path the entry owns (the same
// path, or one above or below it). Of several, the closest in time wins.
//
// The owner is what managedfields.Attribute returned for the drifted paths.
func Match(uid string, owner managedfields.Owner, records []Record, tolerance time.Duration) Verdict {
	if tolerance <= 0 {
		tolerance = DefaultTolerance
	}
	best, bestDelta := -1, time.Duration(math.MaxInt64)
	result := metrics.ActorNoRecord
	rank := map[string]int{metrics.ActorNoRecord: 0, metrics.ActorManagerMismatch: 1, metrics.ActorTimeMismatch: 2, metrics.ActorPathMismatch: 3}
	note := func(r string) {
		if rank[r] > rank[result] {
			result = r
		}
	}
	for i, rec := range records {
		if rec.UID != uid {
			continue
		}
		if rec.FieldManager == "" || rec.FieldManager != owner.Manager {
			note(metrics.ActorManagerMismatch)
			continue
		}
		delta := rec.LastSeen.Sub(owner.Time).Abs()
		if owner.Time.IsZero() || delta > tolerance {
			note(metrics.ActorTimeMismatch)
			continue
		}
		if !overlap(rec.Paths, owner.Paths) {
			note(metrics.ActorPathMismatch)
			continue
		}
		if delta < bestDelta {
			best, bestDelta = i, delta
		}
	}
	if best < 0 {
		return Verdict{Result: result}
	}
	return Verdict{Record: records[best], Result: metrics.ActorMatched}
}

// overlap reports whether some path of a is a path of b, or above or below one.
func overlap(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y || strings.HasPrefix(x, y+"/") || strings.HasPrefix(y, x+"/") {
				return true
			}
		}
	}
	return false
}

// Window is the span of time in which the change of a proposal can have been made, from the
// history of the Application: after the last successful sync (which put the resource back to Git's
// state) and until the proposal is made.
type Window struct {
	// After is when the last successful sync of the Application finished. Zero: not known.
	After time.Time
	// Before is when the proposal is made.
	Before time.Time
}

// MatchSubresource is Match for the records that have no field manager. The API server leaves
// options.fieldManager empty for a write to a subresource (kubectl scale, oc scale) although it
// writes a managedFields entry for it, with the manager derived from the user agent and the
// subresource in it. Such a record counts when an entry of the same manager and operation as
// the actor (the latest owner, so the name goes with the manager that is shown) has the
// record's subresource, the object's uid is the record's, and the entry owns a path the record
// names. A record that has a manager is never looked at here: Match keeps the strict check.
//
// The entry of a subresource has no time (observed for kubectl scale: the API server writes the
// entry without one). When it has one, the record's last change must be within tolerance of it and
// the closest wins, as in Match. When it has none, the time cannot tie the record to the change, so
// the history of the Application does: only records after the last successful sync (the change was
// made after the resource was put back to Git's state) and not after the proposal (plus tolerance)
// count. Without a known sync there is no window and nobody is named (no_window). Of the records in
// the window the name is used only if they all name the same user; different users are ambiguous.
func MatchSubresource(uid string, actor managedfields.Owner, entries []managedfields.Entry, records []Record,
	tolerance time.Duration, window Window) Verdict {
	if tolerance <= 0 {
		tolerance = DefaultTolerance
	}
	best, bestDelta := -1, time.Duration(math.MaxInt64)
	var timeless []int
	result := metrics.ActorNoRecord
	rank := map[string]int{metrics.ActorNoRecord: 0, metrics.ActorManagerMismatch: 1, metrics.ActorTimeMismatch: 2, metrics.ActorPathMismatch: 3, metrics.ActorNoWindow: 4}
	note := func(r string) {
		if rank[r] > rank[result] {
			result = r
		}
	}
	for i, rec := range records {
		if rec.UID != uid || rec.FieldManager != "" || rec.SubResource == "" {
			continue
		}
		var near []managedfields.Entry // the entries of the actor with the record's subresource
		for _, e := range entries {
			if e.Manager == actor.Manager && e.Operation == actor.Operation && e.Subresource == rec.SubResource {
				near = append(near, e)
			}
		}
		if len(near) == 0 {
			note(metrics.ActorManagerMismatch)
			continue
		}
		for _, e := range near {
			switch delta := rec.LastSeen.Sub(e.Time).Abs(); {
			case !overlap(rec.Paths, e.Paths):
				note(metrics.ActorPathMismatch)
			case e.Time.IsZero():
				switch {
				case window.After.IsZero():
					note(metrics.ActorNoWindow)
				case rec.LastSeen.After(window.After) && (window.Before.IsZero() || !rec.LastSeen.After(window.Before.Add(tolerance))):
					timeless = append(timeless, i)
				default:
					note(metrics.ActorTimeMismatch)
				}
			case delta > tolerance:
				note(metrics.ActorTimeMismatch)
			case delta < bestDelta:
				best, bestDelta = i, delta
			}
		}
	}
	if best >= 0 {
		return Verdict{Record: records[best], Result: metrics.ActorMatched}
	}
	return pickTimeless(records, timeless, result)
}

// pickTimeless decides among the records that fit an entry without a time: the latest of one
// user's, or nothing when two users fit.
func pickTimeless(records []Record, fits []int, otherwise string) Verdict {
	if len(fits) == 0 {
		return Verdict{Result: otherwise}
	}
	latest := fits[0]
	for _, i := range fits {
		if records[i].Username != records[fits[0]].Username || records[i].Kind != records[fits[0]].Kind {
			return Verdict{Result: metrics.ActorAmbiguous}
		}
		if records[i].LastSeen.After(records[latest].LastSeen) {
			latest = i
		}
	}
	return Verdict{Record: records[latest], Result: metrics.ActorMatched}
}
