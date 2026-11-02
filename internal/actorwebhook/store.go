package actorwebhook

import (
	"context"
	"slices"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	records "github.com/sserkanml/backflow/api/records/v1alpha1"
	"github.com/sserkanml/backflow/internal/metrics"
)

// LabelObjectUID is the label of a ChangeRecord that holds the uid of the changed object.
const LabelObjectUID = "backflow.io/object-uid"

// Defaults of the flusher and the sweeper.
const (
	DefaultFlushInterval = 2 * time.Second
	DefaultRetention     = 2 * time.Hour
	DefaultMaxRecords    = 5000
	sweepInterval        = time.Minute
	flushAttempts        = 3
)

// ToChangeRecord is the object that holds a record.
func ToChangeRecord(r Record, namespace string) *records.ChangeRecord {
	return &records.ChangeRecord{
		ObjectMeta: metav1.ObjectMeta{Name: r.ObjectName(), Namespace: namespace, Labels: map[string]string{LabelObjectUID: r.UID}},
		Spec: records.ChangeRecordSpec{
			UID: r.UID, Group: r.Group, Resource: r.Resource, Subresource: r.SubResource, Namespace: r.Namespace, Name: r.Name,
			ResourceVersion: r.ResourceVersion, FieldManager: r.FieldManager,
			Username: r.Username, Groups: slices.Clone(r.Groups), ActorKind: records.ActorKind(r.Kind),
			Paths: slices.Clone(r.Paths), FirstSeen: metav1.NewMicroTime(r.FirstSeen), LastSeen: metav1.NewMicroTime(r.LastSeen), Count: r.Count,
		},
	}
}

// FromChangeRecord is the record an object holds.
func FromChangeRecord(c *records.ChangeRecord) Record {
	s := c.Spec
	return Record{
		UID: s.UID, Group: s.Group, Resource: s.Resource, SubResource: s.Subresource, Namespace: s.Namespace, Name: s.Name,
		ResourceVersion: s.ResourceVersion, FieldManager: s.FieldManager, Username: s.Username, Groups: slices.Clone(s.Groups),
		Kind: Kind(s.ActorKind), Paths: slices.Clone(s.Paths), FirstSeen: s.FirstSeen.Time, LastSeen: s.LastSeen.Time, Count: s.Count,
	}
}

// Flusher writes the records of a buffer to ChangeRecords every interval. It runs in every
// replica (the webhook Service reaches any ready pod) and never on the request path. A
// record that cannot be written goes back to the buffer.
type Flusher struct {
	Client    client.Client
	Reader    client.Reader // reads without the cache, so a second replica's write is seen
	Buffer    *Buffer
	Namespace string
	Interval  time.Duration
}

// NeedLeaderElection: every replica serves the webhook, so every replica flushes.
func (f *Flusher) NeedLeaderElection() bool { return false }

// Start flushes until the context ends, and once more at the end.
func (f *Flusher) Start(ctx context.Context) error {
	interval := f.Interval
	if interval <= 0 {
		interval = DefaultFlushInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			f.Flush(context.WithoutCancel(ctx))
			return nil
		case <-t.C:
			f.Flush(ctx)
		}
	}
}

// Flush writes what the buffer holds.
func (f *Flusher) Flush(ctx context.Context) {
	records := f.Buffer.Drain()
	written := 0
	for _, r := range records {
		if err := f.write(ctx, r); err != nil {
			logf.FromContext(ctx).V(1).Info("Cannot write a change record; it is kept for the next flush", "error", err.Error())
			f.Buffer.Add(r)
			continue
		}
		written++
	}
	metrics.ActorRecordsWritten(written)
}

// write creates the record's object or merges the record into the one that exists.
func (f *Flusher) write(ctx context.Context, r Record) error {
	var err error
	for range flushAttempts {
		var have records.ChangeRecord
		getErr := f.Reader.Get(ctx, types.NamespacedName{Namespace: f.Namespace, Name: r.ObjectName()}, &have)
		switch {
		case apierrors.IsNotFound(getErr):
			err = f.Client.Create(ctx, ToChangeRecord(r, f.Namespace))
		case getErr != nil:
			return getErr
		default:
			merged := FromChangeRecord(&have)
			merged.Merge(r)
			have.Spec = ToChangeRecord(merged, f.Namespace).Spec
			err = f.Client.Update(ctx, &have)
		}
		if err == nil || (!apierrors.IsConflict(err) && !apierrors.IsAlreadyExists(err)) {
			return err
		}
	}
	return err
}

// Sweeper deletes the ChangeRecords older than the retention, and the oldest ones beyond
// the maximum. Only the leader sweeps.
type Sweeper struct {
	Client    client.Client
	Namespace string
	Retention time.Duration
	Max       int
	Interval  time.Duration
	Now       func() time.Time
}

// NeedLeaderElection: one replica cleans up.
func (s *Sweeper) NeedLeaderElection() bool { return true }

// Start sweeps until the context ends.
func (s *Sweeper) Start(ctx context.Context) error {
	interval := s.Interval
	if interval <= 0 {
		interval = sweepInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			s.Sweep(ctx)
		}
	}
}

// Sweep makes one pass.
func (s *Sweeper) Sweep(ctx context.Context) {
	log := logf.FromContext(ctx)
	var list records.ChangeRecordList
	if err := s.Client.List(ctx, &list, client.InNamespace(s.Namespace)); err != nil {
		log.V(1).Info("Cannot list the change records", "error", err.Error())
		return
	}
	retention, limit := s.Retention, s.Max
	if retention <= 0 {
		retention = DefaultRetention
	}
	if limit <= 0 {
		limit = DefaultMaxRecords
	}
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	items := list.Items
	slices.SortFunc(items, func(a, b records.ChangeRecord) int {
		return a.Spec.LastSeen.Compare(b.Spec.LastSeen.Time)
	})
	expired, capped := 0, 0
	for i := range items {
		overCap := len(items)-i > limit
		old := now.Sub(items[i].Spec.LastSeen.Time) > retention
		if !old && !overCap {
			break
		}
		if err := s.Client.Delete(ctx, &items[i]); client.IgnoreNotFound(err) != nil {
			log.V(1).Info("Cannot delete a change record", "error", err.Error())
			continue
		}
		if old {
			expired++
		} else {
			capped++
		}
	}
	metrics.ActorRecordsDropped(metrics.ActorDropCap, capped)
	if expired+capped > 0 {
		log.V(1).Info("Swept change records", "expired", expired, "overCap", capped)
	}
}
