package metrics

import "github.com/prometheus/client_golang/prometheus"

// The results of a call to the actor webhook. Bounded: they are the only label values.
const (
	ActorCallRecorded = "recorded" // a change was merged into the buffer
	ActorCallIgnored  = "ignored"  // not a change to record (a Secret, a dry run, a status write, no changed field)
	ActorCallInvalid  = "invalid"  // the body could not be read as an AdmissionReview
	ActorCallBusy     = "busy"     // too many calls at once: answered without looking
)

// The reasons a record was dropped.
const (
	ActorDropBuffer = "buffer" // the buffer of a pod was full
	ActorDropCap    = "cap"    // more records than actorWebhook.maxRecords: the oldest were deleted
	ActorDropWrite  = "write"  // the record could not be written and was given up
)

// The results of pairing a proposal with the records.
const (
	ActorMatched         = "matched"
	ActorNoRecord        = "no_record"
	ActorManagerMismatch = "manager_mismatch"
	ActorTimeMismatch    = "time_mismatch"
	ActorPathMismatch    = "path_mismatch"
	ActorAmbiguous       = "ambiguous" // records of different users fit a change whose entry has no time
)

var (
	actorCalls = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Name: "actor_webhook_calls_total",
		Help: "Calls to the actor webhook by result: recorded, ignored, invalid, busy. The webhook always answers allowed.",
	}, []string{lResult})
	actorRecordsWritten = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace, Name: "actor_records_written_total",
		Help: "ChangeRecords created or updated by the flusher of the actor webhook.",
	})
	actorRecordsDropped = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Name: "actor_records_dropped_total",
		Help: "Records dropped by reason: buffer (a pod's buffer was full), cap (more ChangeRecords than the maximum), write (could not be written).",
	}, []string{lReason})
	actorMatches = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Name: "actor_matches_total",
		Help: "Pairings of a new proposal with a ChangeRecord by result: matched, no_record, or why a record was refused by the managedFields cross-check (manager_mismatch, time_mismatch, path_mismatch, ambiguous).",
	}, []string{lResult})
)

// init adds the collectors to the list that metrics.go registers (this file's init runs first).
func init() {
	collectors = append(collectors, actorCalls, actorRecordsWritten, actorRecordsDropped, actorMatches)
}

// ActorCall counts a call to the actor webhook.
func ActorCall(result string) {
	safe(func() { actorCalls.WithLabelValues(actorCallLabel(result)).Inc() })
}

func actorCallLabel(v string) string {
	switch v {
	case ActorCallRecorded, ActorCallIgnored, ActorCallInvalid, ActorCallBusy:
		return v
	}
	return ActorCallInvalid
}

// ActorRecordsWritten counts ChangeRecords written.
func ActorRecordsWritten(n int) {
	if n > 0 {
		safe(func() { actorRecordsWritten.Add(float64(n)) })
	}
}

// ActorRecordsDropped counts records dropped.
func ActorRecordsDropped(reason string, n int) {
	if n <= 0 {
		return
	}
	switch reason {
	case ActorDropBuffer, ActorDropCap, ActorDropWrite:
	default:
		reason = ActorDropWrite
	}
	safe(func() { actorRecordsDropped.WithLabelValues(reason).Add(float64(n)) })
}

// ActorMatch counts the pairing of a proposal with the records.
func ActorMatch(result string) {
	switch result {
	case ActorMatched, ActorNoRecord, ActorManagerMismatch, ActorTimeMismatch, ActorPathMismatch, ActorAmbiguous:
	default:
		result = ActorNoRecord
	}
	safe(func() { actorMatches.WithLabelValues(result).Inc() })
}
