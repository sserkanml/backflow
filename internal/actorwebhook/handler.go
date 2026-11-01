package actorwebhook

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/sserkanml/backflow/internal/metrics"
)

// maxBody is the largest AdmissionReview that is read. A bigger one is answered "allowed"
// without being looked at.
const maxBody = 8 << 20

// maxInFlight is how many reviews are looked at at once; the others are answered at once.
const maxInFlight = 64

// Options configure the handler.
type Options struct {
	// Buffer receives the records.
	Buffer *Buffer
	// RecordUsername says whether user names and groups are stored. False: only the kind.
	RecordUsername bool
	// Now returns the time. Defaults to time.Now.
	Now func() time.Time
}

// Handler is the http.Handler of the webhook. It answers every request with an
// AdmissionReview that allows it, whatever it received, and never waits for anything but
// the body: no API call, no lock held across I/O.
type Handler struct {
	opts     Options
	inFlight chan struct{}
}

// NewHandler returns the handler.
func NewHandler(o Options) *Handler {
	return &Handler{opts: o, inFlight: make(chan struct{}, maxInFlight)}
}

type review struct {
	Request *request `json:"request"`
}

type request struct {
	UID         string                           `json:"uid"`
	Operation   string                           `json:"operation"`
	Resource    struct{ Group, Resource string } `json:"resource"`
	SubResource string                           `json:"subResource"`
	Namespace   string                           `json:"namespace"`
	Name        string                           `json:"name"`
	DryRun      bool                             `json:"dryRun"`
	UserInfo    struct {
		Username string   `json:"username"`
		Groups   []string `json:"groups"`
	} `json:"userInfo"`
	Options *struct {
		FieldManager string `json:"fieldManager"`
	} `json:"options"`
	Object    map[string]any `json:"object"`
	OldObject map[string]any `json:"oldObject"`
}

func (h *Handler) now() time.Time {
	if h.opts.Now != nil {
		return h.opts.Now()
	}
	return time.Now()
}

// ServeHTTP answers allowed, always.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	uid := ""
	defer func() {
		_ = recover() // nothing that happens here may fail a request of the cluster
		writeAllowed(w, uid)
	}()
	select {
	case h.inFlight <- struct{}{}:
		defer func() { <-h.inFlight }()
	default:
		metrics.ActorCall(metrics.ActorCallBusy)
		return
	}
	if r.Method != http.MethodPost || r.Body == nil {
		metrics.ActorCall(metrics.ActorCallInvalid)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil || len(body) > maxBody {
		metrics.ActorCall(metrics.ActorCallInvalid)
		return
	}
	var rv review
	if err := json.Unmarshal(body, &rv); err != nil || rv.Request == nil {
		metrics.ActorCall(metrics.ActorCallInvalid)
		return
	}
	uid = rv.Request.UID
	if rec, ok := h.record(rv.Request); ok {
		h.opts.Buffer.Add(rec)
		metrics.ActorCall(metrics.ActorCallRecorded)
		return
	}
	metrics.ActorCall(metrics.ActorCallIgnored)
}

// record is the record a request leaves, if it leaves one. Everything it does not keep is
// dropped with the request.
func (h *Handler) record(req *request) (Record, bool) {
	switch {
	case req.Operation != "UPDATE", req.DryRun:
		return Record{}, false
	case req.Resource.Resource == "secrets":
		return Record{}, false
	case req.SubResource != "" && req.SubResource != "scale":
		return Record{}, false
	}
	meta, _ := req.Object["metadata"].(map[string]any)
	uid, _ := meta["uid"].(string)
	if uid == "" {
		return Record{}, false
	}
	paths := ChangedPaths(req.OldObject, req.Object)
	if len(paths) == 0 {
		return Record{}, false
	}
	if len(paths) > MaxPaths {
		paths = paths[:MaxPaths]
	}
	rv, _ := meta["resourceVersion"].(string)
	now := h.now()
	rec := Record{
		UID: uid, Group: req.Resource.Group, Resource: req.Resource.Resource, SubResource: req.SubResource,
		Namespace: req.Namespace, Name: req.Name, ResourceVersion: rv, Kind: KindOf(req.UserInfo.Username),
		Paths: paths, FirstSeen: now, LastSeen: now, Count: 1,
	}
	if req.Options != nil {
		rec.FieldManager = req.Options.FieldManager
	}
	if h.opts.RecordUsername {
		rec.Username = req.UserInfo.Username
		rec.Groups = req.UserInfo.Groups
		if len(rec.Groups) > MaxGroups {
			rec.Groups = rec.Groups[:MaxGroups]
		}
	}
	return rec, true
}

func writeAllowed(w http.ResponseWriter, uid string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"apiVersion": "admission.k8s.io/v1",
		"kind":       "AdmissionReview",
		"response":   map[string]any{"uid": uid, "allowed": true},
	})
}
