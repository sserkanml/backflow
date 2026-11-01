package actorwebhook

import (
	"slices"
	"sync"

	"github.com/sserkanml/backflow/internal/metrics"
)

// Buffer holds the records of a pod until they are flushed. It is bounded: when it is
// full the oldest record is dropped, counted, and the new one kept. It does no I/O.
type Buffer struct {
	mu      sync.Mutex
	records map[string]*Record
	max     int
}

// NewBuffer returns a buffer that keeps at most limit records (at least 1).
func NewBuffer(limit int) *Buffer {
	return &Buffer{records: map[string]*Record{}, max: max(limit, 1)}
}

// Add merges a record into the buffer.
func (b *Buffer) Add(r Record) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if have, ok := b.records[r.Key()]; ok {
		have.Merge(r)
		return
	}
	if len(b.records) >= b.max {
		b.dropOldest()
	}
	c := r
	c.Paths = slices.Clone(r.Paths)
	c.Groups = slices.Clone(r.Groups)
	b.records[r.Key()] = &c
}

func (b *Buffer) dropOldest() {
	var oldestKey string
	var oldest *Record
	for k, r := range b.records {
		if oldest == nil || r.LastSeen.Before(oldest.LastSeen) {
			oldestKey, oldest = k, r
		}
	}
	if oldest != nil {
		delete(b.records, oldestKey)
		metrics.ActorRecordsDropped(metrics.ActorDropBuffer, 1)
	}
}

// Drain returns every record and empties the buffer.
func (b *Buffer) Drain() []Record {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Record, 0, len(b.records))
	for _, r := range b.records {
		out = append(out, *r)
	}
	clear(b.records)
	slices.SortFunc(out, func(x, y Record) int { return x.LastSeen.Compare(y.LastSeen) })
	return out
}

// Len is the number of records held.
func (b *Buffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.records)
}
