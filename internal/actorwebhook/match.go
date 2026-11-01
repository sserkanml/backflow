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
