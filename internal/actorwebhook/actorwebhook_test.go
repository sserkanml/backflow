package actorwebhook

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/cel-go/cel"

	"github.com/sserkanml/backflow/internal/managedfields"
	"github.com/sserkanml/backflow/internal/metrics"
)

func obj(m map[string]any) map[string]any { return m }

func TestChangedPaths(t *testing.T) {
	base := func() map[string]any {
		return obj(map[string]any{
			"metadata": obj(map[string]any{
				"name": "cfg", "resourceVersion": "1", "generation": 1.0,
				"managedFields": []any{obj(map[string]any{"manager": "a"})},
				"annotations":   obj(map[string]any{lastApplied: "x", "keep": "1"}),
			}),
			"data": obj(map[string]any{"LOG_LEVEL": "info", "A/B": "1", "T~": "1"}),
			"spec": obj(map[string]any{"ports": []any{obj(map[string]any{"port": 80.0}), obj(map[string]any{"port": 81.0})}, "list": []any{"a"}}),
		})
	}
	tests := []struct {
		name   string
		change func(m map[string]any)
		want   []string
	}{
		{name: "no change", change: func(map[string]any) {}, want: nil},
		{name: "a value", change: func(m map[string]any) { m["data"].(map[string]any)["LOG_LEVEL"] = "debug" }, want: []string{"/data/LOG_LEVEL"}},
		{name: "a key added", change: func(m map[string]any) { m["data"].(map[string]any)["NEW"] = "1" }, want: []string{"/data/NEW"}},
		{name: "a key removed", change: func(m map[string]any) { delete(m["data"].(map[string]any), "LOG_LEVEL") }, want: []string{"/data/LOG_LEVEL"}},
		{name: "keys are pointer-escaped", change: func(m map[string]any) {
			d := m["data"].(map[string]any)
			d["A/B"], d["T~"] = "2", "2"
		}, want: []string{"/data/A~1B", "/data/T~0"}},
		{name: "a list element", change: func(m map[string]any) {
			m["spec"].(map[string]any)["ports"].([]any)[1].(map[string]any)["port"] = 82.0
		}, want: []string{"/spec/ports/1/port"}},
		{name: "a list whose length changed is one path", change: func(m map[string]any) {
			m["spec"].(map[string]any)["list"] = []any{"a", "b"}
		}, want: []string{"/spec/list"}},
		{name: "managedFields, resourceVersion, generation and last-applied are not changes", change: func(m map[string]any) {
			md := m["metadata"].(map[string]any)
			md["resourceVersion"], md["generation"], md["managedFields"] = "2", 2.0, []any{}
			md["annotations"].(map[string]any)[lastApplied] = "y"
		}, want: nil},
		{name: "another annotation is", change: func(m map[string]any) {
			m["metadata"].(map[string]any)["annotations"].(map[string]any)["keep"] = "2"
		}, want: []string{"/metadata/annotations/keep"}},
		{name: "a type change", change: func(m map[string]any) { m["data"] = "text" }, want: []string{"/data"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cur := base()
			tt.change(cur)
			got := ChangedPaths(base(), cur)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestChangedPathsHoldNoValues(t *testing.T) {
	old := obj(map[string]any{"data": obj(map[string]any{"PASSWORD": "hunter2-old"})})
	cur := obj(map[string]any{"data": obj(map[string]any{"PASSWORD": "hunter2-new"})})
	for _, p := range ChangedPaths(old, cur) {
		if strings.Contains(p, "hunter2") {
			t.Fatalf("a path holds a value: %s", p)
		}
	}
}

func TestBufferMergesAndDropsTheOldest(t *testing.T) {
	t0 := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	rec := func(uid, user, mgr string, at time.Duration, paths ...string) Record {
		return Record{UID: uid, Username: user, Kind: KindPerson, FieldManager: mgr, Paths: paths, FirstSeen: t0.Add(at), LastSeen: t0.Add(at), Count: 1}
	}
	b := NewBuffer(2)
	b.Add(rec("u1", "alice", "kubectl-edit", 0, "/a"))
	b.Add(rec("u1", "alice", "kubectl-edit", time.Second, "/b", "/a"))
	if b.Len() != 1 {
		t.Fatalf("the same actor and manager are one record, got %d", b.Len())
	}
	b.Add(rec("u1", "bob", "kubectl-edit", 2*time.Second, "/c"))
	if b.Len() != 2 {
		t.Fatalf("another user is another record, got %d", b.Len())
	}
	b.Add(rec("u2", "carol", "kubectl", 3*time.Second, "/d"))
	got := b.Drain()
	if len(got) != 2 {
		t.Fatalf("a full buffer keeps its limit, got %d", len(got))
	}
	for _, r := range got {
		if r.Username == "alice" {
			t.Errorf("the oldest record (alice) should have been dropped: %+v", got)
		}
	}
	if b.Len() != 0 {
		t.Error("Drain empties the buffer")
	}
	m := rec("u1", "alice", "m", 0, "/a")
	m.Merge(rec("u1", "alice", "m", time.Minute, "/b"))
	if m.Count != 2 || !m.LastSeen.Equal(t0.Add(time.Minute)) || !m.FirstSeen.Equal(t0) || !reflect.DeepEqual(m.Paths, []string{"/a", "/b"}) {
		t.Errorf("merge: %+v", m)
	}
}

func TestRecordKeysHoldNoNameWhenNamesAreNotRecorded(t *testing.T) {
	a := Record{UID: "u", Kind: KindPerson, FieldManager: "m"}
	b := Record{UID: "u", Kind: KindController, FieldManager: "m"}
	if a.Key() == b.Key() || strings.Contains(a.Key(), "alice") {
		t.Errorf("keys: %q %q", a.Key(), b.Key())
	}
	if a.ObjectName() == b.ObjectName() || !strings.HasPrefix(a.ObjectName(), "cr-") {
		t.Errorf("names: %s %s", a.ObjectName(), b.ObjectName())
	}
	if KindOf("system:serviceaccount:ns:ci") != KindController || KindOf("alice") != KindPerson {
		t.Error("kinds")
	}
}

const sample = `{"apiVersion":"admission.k8s.io/v1","kind":"AdmissionReview","request":{
 "uid":"req-1","operation":"UPDATE","resource":{"group":"","version":"v1","resource":"configmaps"},
 "namespace":"team-a","name":"cfg","userInfo":{"username":"alice","groups":["devs","system:authenticated"]},
 "options":{"kind":"UpdateOptions","fieldManager":"kubectl-patch"},
 "oldObject":{"metadata":{"uid":"U1","resourceVersion":"5"},"data":{"K":"secret-old"}},
 "object":{"metadata":{"uid":"U1","resourceVersion":"6"},"data":{"K":"secret-new"}}}}`

func post(t *testing.T, h http.Handler, body string) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/observe/x", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("answer is not JSON: %v: %s", err, rec.Body.String())
	}
	resp, _ := out["response"].(map[string]any)
	if resp["allowed"] != true {
		t.Fatalf("not allowed: %s", rec.Body.String())
	}
	return resp
}

func TestHandlerAlwaysAllows(t *testing.T) {
	b := NewBuffer(100)
	h := NewHandler(Options{Buffer: b, RecordUsername: true})
	big := strings.Repeat("x", maxBody+10)
	for name, body := range map[string]string{
		"garbage":           "not json",
		"empty":             "",
		"no request":        `{"kind":"AdmissionReview"}`,
		"wrong types":       `{"request":{"uid":5,"object":"x"}}`,
		"oversized":         big,
		"a request of null": `{"request":null}`,
	} {
		t.Run(name, func(t *testing.T) { post(t, h, body) })
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"allowed":true`) {
		t.Errorf("a GET is answered allowed too: %d %s", rec.Code, rec.Body.String())
	}
	if b.Len() != 0 {
		t.Errorf("nothing of that is recorded, got %d", b.Len())
	}
}

func TestHandlerRecordsPathsOnly(t *testing.T) {
	b := NewBuffer(100)
	now := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	h := NewHandler(Options{Buffer: b, RecordUsername: true, Now: func() time.Time { return now }})
	resp := post(t, h, sample)
	if resp["uid"] != "req-1" {
		t.Errorf("the answer carries the request uid, got %v", resp["uid"])
	}
	got := b.Drain()
	if len(got) != 1 {
		t.Fatalf("records: %d", len(got))
	}
	r := got[0]
	if r.UID != "U1" || r.Username != "alice" || r.FieldManager != "kubectl-patch" || r.ResourceVersion != "6" ||
		!reflect.DeepEqual(r.Paths, []string{"/data/K"}) || r.Kind != KindPerson || r.Count != 1 || !r.LastSeen.Equal(now) {
		t.Errorf("record: %+v", r)
	}
	text, _ := json.Marshal(r)
	if strings.Contains(string(text), "secret-") {
		t.Errorf("a value is in the record: %s", text)
	}
}

func TestHandlerPrivacySwitch(t *testing.T) {
	b := NewBuffer(100)
	h := NewHandler(Options{Buffer: b, RecordUsername: false})
	post(t, h, sample)
	post(t, h, strings.Replace(sample, `"username":"alice"`, `"username":"system:serviceaccount:team-a:ci"`, 1))
	got := b.Drain()
	if len(got) != 2 {
		t.Fatalf("records: %d", len(got))
	}
	for _, r := range got {
		text, _ := json.Marshal(r)
		if r.Username != "" || len(r.Groups) != 0 || strings.Contains(string(text), "alice") || strings.Contains(string(text), "devs") ||
			strings.Contains(string(text), "team-a:ci") {
			t.Errorf("a name was stored: %s", text)
		}
	}
	if got[0].Kind == got[1].Kind {
		t.Errorf("the kinds still differ: %v %v", got[0].Kind, got[1].Kind)
	}
}

func TestHandlerIgnores(t *testing.T) {
	tests := map[string]string{
		"a Secret":       strings.Replace(sample, `"resource":"configmaps"`, `"resource":"secrets"`, 1),
		"a dry run":      strings.Replace(sample, `"operation":"UPDATE"`, `"operation":"UPDATE","dryRun":true`, 1),
		"a create":       strings.Replace(sample, `"operation":"UPDATE"`, `"operation":"CREATE"`, 1),
		"a status write": strings.Replace(sample, `"namespace":"team-a"`, `"namespace":"team-a","subResource":"status"`, 1),
		"no uid":         strings.ReplaceAll(sample, `"uid":"U1",`, ""),
		"no change":      strings.Replace(sample, `"secret-new"`, `"secret-old"`, 1),
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			b := NewBuffer(10)
			post(t, NewHandler(Options{Buffer: b, RecordUsername: true}), body)
			if b.Len() != 0 {
				t.Errorf("recorded %d", b.Len())
			}
		})
	}
	b := NewBuffer(10)
	post(t, NewHandler(Options{Buffer: b, RecordUsername: true}), strings.Replace(sample, `"namespace":"team-a"`, `"namespace":"team-a","subResource":"scale"`, 1))
	if b.Len() != 1 {
		t.Error("a scale is recorded")
	}
}

func TestHandlerNeverBlocksWhenBusy(t *testing.T) {
	h := NewHandler(Options{Buffer: NewBuffer(10)})
	for range maxInFlight {
		h.inFlight <- struct{}{}
	}
	done := make(chan struct{})
	go func() { post(t, h, sample); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a busy handler must answer at once")
	}
}

func TestHandlerCountsCallsWithBoundedResults(t *testing.T) {
	// The counter takes only the four results; anything else is "invalid".
	for _, r := range []string{"recorded", "ignored", "invalid", "busy", "alice"} {
		metrics.ActorCall(r)
	}
}

// --- the CEL conditions ---

func celEnv(t *testing.T) *cel.Env {
	t.Helper()
	env, err := cel.NewEnv(cel.Variable("request", cel.DynType), cel.Variable("object", cel.DynType), cel.Variable("oldObject", cel.DynType))
	if err != nil {
		t.Fatal(err)
	}
	return env
}

// passes reports whether every condition is true for the request, and the first error.
func passes(t *testing.T, env *cel.Env, conds []Condition, request, object map[string]any) (bool, error) {
	t.Helper()
	for _, c := range conds {
		ast, iss := env.Compile(c.Expression)
		if iss.Err() != nil {
			t.Fatalf("%s does not compile: %v", c.Name, iss.Err())
		}
		prg, err := env.Program(ast)
		if err != nil {
			t.Fatal(err)
		}
		out, _, err := prg.Eval(map[string]any{"request": request, "object": object, "oldObject": object})
		if err != nil {
			return false, err
		}
		if out.Value() != true {
			return false, nil
		}
	}
	return true, nil
}

func TestConditionsOnSampleRequests(t *testing.T) {
	env := celEnv(t)
	conds := Conditions([]string{"system:serviceaccount:argocd:argocd-application-controller", "system:serviceaccount:backflow-system:backflow"})
	tracked := obj(map[string]any{"metadata": obj(map[string]any{"annotations": obj(map[string]any{"argocd.argoproj.io/tracking-id": "app:/ConfigMap:ns/cfg"})})})
	labelled := obj(map[string]any{"metadata": obj(map[string]any{"labels": obj(map[string]any{"app.kubernetes.io/instance": "app"})})})
	plain := obj(map[string]any{"metadata": obj(map[string]any{"name": "x"})})
	scaleObj := obj(map[string]any{"metadata": obj(map[string]any{"name": "web"})})
	req := func(user, resource, sub string, dry any) map[string]any {
		r := map[string]any{"operation": "UPDATE", "resource": map[string]any{"resource": resource}, "userInfo": map[string]any{"username": user}}
		if sub != "" {
			r["subResource"] = sub
		}
		if dry != nil {
			r["dryRun"] = dry
		}
		return r
	}
	tests := []struct {
		name string
		req  map[string]any
		obj  map[string]any
		want bool
	}{
		{"a tracked ConfigMap changed by a person (subResource and dryRun absent)", req("alice", "configmaps", "", nil), tracked, true},
		{"tracked by the instance label", req("alice", "deployments", "", nil), labelled, true},
		{"a ConfigMap Argo CD does not track", req("alice", "configmaps", "", nil), plain, false},
		{"a scale, whose object has no annotations", req("alice", "deployments", "scale", nil), scaleObj, true},
		{"a status write", req("alice", "deployments", "status", nil), tracked, false},
		{"a Secret", req("alice", "secrets", "", nil), tracked, false},
		{"a dry run", req("alice", "configmaps", "", true), tracked, false},
		{"dryRun false", req("alice", "configmaps", "", false), tracked, true},
		{"the Argo CD application controller", req("system:serviceaccount:argocd:argocd-application-controller", "configmaps", "", nil), tracked, false},
		{"Backflow's service account", req("system:serviceaccount:backflow-system:backflow", "configmaps", "", nil), tracked, false},
		{"a controller of kube-system", req("system:serviceaccount:kube-system:replicaset-controller", "replicasets", "", nil), tracked, false},
		{"a node", req("system:node:worker-1", "pods", "", nil), tracked, false},
		{"the controller manager", req("system:kube-controller-manager", "replicasets", "", nil), tracked, false},
		{"a service account of a team", req("system:serviceaccount:team-a:ci", "configmaps", "", nil), tracked, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := passes(t, env, conds, tt.req, tt.obj)
			if err != nil {
				t.Fatalf("a condition failed to evaluate, which under failurePolicy Ignore silently switches the webhook off: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestComparingAnAbsentSubResourceToEmptyIsAnError(t *testing.T) {
	// The trap the conditions avoid: subResource does not exist for the main resource.
	env := celEnv(t)
	ast, iss := env.Compile("request.subResource == ''")
	if iss.Err() != nil {
		t.Fatal(iss.Err())
	}
	prg, _ := env.Program(ast)
	if _, _, err := prg.Eval(map[string]any{"request": map[string]any{"operation": "UPDATE"}, "object": nil, "oldObject": nil}); err == nil {
		t.Fatal("expected an error for an absent field")
	}
	for _, c := range Conditions(nil) {
		if strings.Contains(c.Expression, "subResource == ''") {
			t.Errorf("%s compares a possibly absent field to ''", c.Name)
		}
	}
}

func TestConditionsWithoutUsersToExclude(t *testing.T) {
	env := celEnv(t)
	tracked := obj(map[string]any{"metadata": obj(map[string]any{"annotations": obj(map[string]any{"argocd.argoproj.io/tracking-id": "x"})})})
	ok, err := passes(t, env, Conditions(nil), map[string]any{"resource": map[string]any{"resource": "configmaps"}, "userInfo": map[string]any{"username": "alice"}}, tracked)
	if err != nil || !ok {
		t.Errorf("ok=%v err=%v", ok, err)
	}
}

// --- matching ---

func TestMatchChecksTheRecordAgainstManagedFields(t *testing.T) {
	at := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	owner := managedfields.Owner{Manager: "kubectl-edit", Operation: "Update", Time: at, Paths: []string{"/data/LOG_LEVEL"}}
	rec := func(mut func(*Record)) Record {
		r := Record{UID: "U1", Username: "alice", Kind: KindPerson, FieldManager: "kubectl-edit", Paths: []string{"/data/LOG_LEVEL"}, LastSeen: at.Add(3 * time.Second)}
		mut(&r)
		return r
	}
	tests := []struct {
		name    string
		records []Record
		owner   managedfields.Owner
		want    string
		user    string
	}{
		{"a record that agrees", []Record{rec(func(*Record) {})}, owner, metrics.ActorMatched, "alice"},
		{"no record", nil, owner, metrics.ActorNoRecord, ""},
		{"a record of another object", []Record{rec(func(r *Record) { r.UID = "U2" })}, owner, metrics.ActorNoRecord, ""},
		{"a forged record with another manager", []Record{rec(func(r *Record) { r.FieldManager = "kubectl-patch" })}, owner, metrics.ActorManagerMismatch, ""},
		{"a record without a manager", []Record{rec(func(r *Record) { r.FieldManager = "" })}, owner, metrics.ActorManagerMismatch, ""},
		{"a forged record for a change made long ago", []Record{rec(func(r *Record) { r.LastSeen = at.Add(time.Hour) })}, owner, metrics.ActorTimeMismatch, ""},
		{"a record just outside the tolerance", []Record{rec(func(r *Record) { r.LastSeen = at.Add(11 * time.Second) })}, owner, metrics.ActorTimeMismatch, ""},
		{"a record just before, inside the tolerance", []Record{rec(func(r *Record) { r.LastSeen = at.Add(-10 * time.Second) })}, owner, metrics.ActorMatched, "alice"},
		{"a record for other fields", []Record{rec(func(r *Record) { r.Paths = []string{"/data/OTHER"} })}, owner, metrics.ActorPathMismatch, ""},
		{"a record for a path above", []Record{rec(func(r *Record) { r.Paths = []string{"/data"} })}, owner, metrics.ActorMatched, "alice"},
		{"a record for a path below", []Record{rec(func(r *Record) { r.Paths = []string{"/data/LOG_LEVEL/x"} })}, owner, metrics.ActorMatched, "alice"},
		{"an entry without a time", []Record{rec(func(*Record) {})}, managedfields.Owner{Manager: "kubectl-edit", Paths: []string{"/data/LOG_LEVEL"}}, metrics.ActorTimeMismatch, ""},
		{"the closest of two wins", []Record{
			rec(func(r *Record) { r.Username = "far"; r.LastSeen = at.Add(8 * time.Second) }),
			rec(func(r *Record) { r.Username = "near"; r.LastSeen = at.Add(time.Second) }),
		}, owner, metrics.ActorMatched, "near"},
		{"a forged record does not beat a real one", []Record{
			rec(func(r *Record) { r.Username = "mallory"; r.FieldManager = "other" }),
			rec(func(*Record) {}),
		}, owner, metrics.ActorMatched, "alice"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := Match("U1", tt.owner, tt.records, DefaultTolerance)
			if v.Result != tt.want || v.Record.Username != tt.user {
				t.Errorf("result %q user %q, want %q %q", v.Result, v.Record.Username, tt.want, tt.user)
			}
		})
	}
	if v := Match("U1", owner, []Record{rec(func(r *Record) { r.LastSeen = at.Add(20 * time.Second) })}, 30*time.Second); v.Result != metrics.ActorMatched {
		t.Errorf("a wider tolerance accepts it: %q", v.Result)
	}
}
