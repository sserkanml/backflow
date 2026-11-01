package actorwebhook

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"time"
)

// MaxPaths is how many paths a record keeps (the ChangeRecord CRD allows 64).
const MaxPaths = 64

// MaxGroups is how many groups a record keeps.
const MaxGroups = 16

// Kind is what kind of identity made a change.
type Kind string

const (
	// KindPerson is a user, or anything that is not a service account.
	KindPerson Kind = "person"
	// KindController is a service account.
	KindController Kind = "controller"
)

// KindOf classifies a user name.
func KindOf(username string) Kind {
	if strings.HasPrefix(username, "system:serviceaccount:") {
		return KindController
	}
	return KindPerson
}

// Record is one actor's changes to one object: who, with which manager, which fields, when.
// It holds paths and never values.
type Record struct {
	UID             string
	Group           string
	Resource        string
	SubResource     string
	Namespace       string
	Name            string
	ResourceVersion string
	FieldManager    string
	// Username and Groups are empty when names are not recorded.
	Username  string
	Groups    []string
	Kind      Kind
	Paths     []string
	FirstSeen time.Time
	LastSeen  time.Time
	Count     int32
}

// Key identifies a record: changes of one object by one actor with one manager are one
// record. When names are not recorded the actor is its kind, so the key holds no name either.
func (r Record) Key() string {
	actor := r.Username
	if actor == "" {
		actor = "kind:" + string(r.Kind)
	}
	return r.UID + "\x00" + actor + "\x00" + r.FieldManager + "\x00" + r.SubResource
}

// ObjectName is the name of the ChangeRecord that holds the record: the same record
// from any replica lands on the same object.
func (r Record) ObjectName() string {
	h := sha256.Sum256([]byte(r.Key()))
	return "cr-" + hex.EncodeToString(h[:8])
}

// Merge folds other (the same key) into r: paths are united, times widened, counts added.
// The resourceVersion and the user's groups are the ones of the later change.
func (r *Record) Merge(other Record) {
	r.Paths = UnionPaths(r.Paths, other.Paths)
	if r.FirstSeen.IsZero() || (!other.FirstSeen.IsZero() && other.FirstSeen.Before(r.FirstSeen)) {
		r.FirstSeen = other.FirstSeen
	}
	if other.LastSeen.After(r.LastSeen) {
		r.LastSeen = other.LastSeen
		if other.ResourceVersion != "" {
			r.ResourceVersion = other.ResourceVersion
		}
		if len(other.Groups) > 0 {
			r.Groups = other.Groups
		}
	}
	r.Count += other.Count
}

// UnionPaths is the sorted union of two path lists, cut to MaxPaths.
func UnionPaths(a, b []string) []string {
	out := slices.Clone(a)
	for _, p := range b {
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	if len(out) > MaxPaths {
		out = out[:MaxPaths]
	}
	return out
}
