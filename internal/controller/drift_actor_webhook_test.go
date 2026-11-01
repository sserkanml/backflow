package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	backflowv1alpha1 "github.com/sserkanml/backflow/api/v1alpha1"
	"github.com/sserkanml/backflow/internal/actorwebhook"
)

// failingLister fails every List.
type failingLister struct{ client.Reader }

func (failingLister) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return errors.New("the API server is not answering")
}

var _ = Describe("Actor records", func() {
	const ns = "default"
	var (
		ctx     = context.Background()
		t0      = time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
		created []client.Object
	)
	live := func(uid, manager string, at time.Time) map[string]any {
		return map[string]any{
			"metadata": map[string]any{"uid": uid, "managedFields": []any{map[string]any{
				"manager": manager, "operation": "Update", "time": at.Format(time.RFC3339), "fieldsType": "FieldsV1", "apiVersion": "v1",
				"fieldsV1": map[string]any{"f:data": map[string]any{"f:LOG_LEVEL": map[string]any{}}},
			}}},
			"data": map[string]any{"LOG_LEVEL": "debug"},
		}
	}
	changes := []backflowv1alpha1.FieldChange{{Path: "/data/LOG_LEVEL", Op: backflowv1alpha1.OpReplace, Desired: `"info"`, Live: `"debug"`}}
	phase1 := func(mgr string) *backflowv1alpha1.Actor {
		return &backflowv1alpha1.Actor{Manager: mgr, Operation: "Update", Time: &metav1.Time{Time: t0}}
	}
	record := func(mut func(*actorwebhook.Record)) actorwebhook.Record {
		r := actorwebhook.Record{
			UID: "uid-actor-1", Resource: "configmaps", Namespace: "demo", Name: "cfg", FieldManager: "kubectl-edit",
			Username: "alice", Groups: []string{"devs"}, Kind: actorwebhook.KindPerson, Paths: []string{"/data/LOG_LEVEL"},
			FirstSeen: t0.Add(2 * time.Second), LastSeen: t0.Add(2 * time.Second), Count: 1,
		}
		mut(&r)
		return r
	}
	put := func(r actorwebhook.Record) {
		obj := actorwebhook.ToChangeRecord(r, ns)
		ExpectWithOffset(1, k8sClient.Create(ctx, obj)).To(Succeed())
		created = append(created, obj)
	}
	AfterEach(func() {
		for _, o := range created {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, o))).To(Succeed())
		}
		created = nil
	})
	refine := func(a *ActorRecords, uid string, mgr string) *backflowv1alpha1.Actor {
		return a.Refine(ctx, live(uid, mgr, t0), changes, phase1(mgr))
	}
	actors := func() *ActorRecords { return &ActorRecords{Reader: k8sClient, Namespace: ns} }

	It("names the user of a record that the managedFields confirm", func() {
		put(record(func(*actorwebhook.Record) {}))
		got := refine(actors(), "uid-actor-1", "kubectl-edit")
		Expect(got.Username).To(Equal("alice"))
		Expect(got.Groups).To(ConsistOf("devs"))
		Expect(string(got.Kind)).To(Equal("person"))
		Expect(got.Manager).To(Equal("kubectl-edit"), "the manager, operation and time stay those of managedFields")
	})

	It("does not believe a forged record: another manager, another time, other fields", func() {
		put(record(func(r *actorwebhook.Record) { r.FieldManager = "kubectl-patch"; r.Username = "mallory" }))
		put(record(func(r *actorwebhook.Record) { r.Username = "mallory2"; r.LastSeen = t0.Add(time.Hour); r.Name = "late" }))
		put(record(func(r *actorwebhook.Record) {
			r.Username = "mallory3"
			r.Paths = []string{"/data/OTHER"}
			r.FieldManager = "kubectl-edit"
			r.Name = "paths"
		}))
		got := refine(actors(), "uid-actor-1", "kubectl-edit")
		Expect(got.Username).To(BeEmpty())
		Expect(got.Groups).To(BeEmpty())
		Expect(got.Manager).To(Equal("kubectl-edit"), "phase 1 behaviour")
	})

	It("keeps only the kind when the record holds no name", func() {
		put(record(func(r *actorwebhook.Record) { r.Username, r.Groups, r.Kind = "", nil, actorwebhook.KindController }))
		got := refine(actors(), "uid-actor-1", "kubectl-edit")
		Expect(got.Username).To(BeEmpty())
		Expect(string(got.Kind)).To(Equal("controller"))
	})

	It("ignores the records of another object", func() {
		put(record(func(*actorwebhook.Record) {}))
		Expect(refine(actors(), "uid-other", "kubectl-edit").Username).To(BeEmpty())
	})

	It("never fails or waits: no records, a failing API, no uid and no webhook leave the actor as it is", func() {
		Expect(refine(actors(), "uid-none", "kubectl-edit")).To(Equal(phase1("kubectl-edit")))
		failing := &ActorRecords{Reader: failingLister{k8sClient}, Namespace: ns}
		Expect(refine(failing, "uid-actor-1", "kubectl-edit")).To(Equal(phase1("kubectl-edit")))
		Expect(actors().Refine(ctx, map[string]any{}, changes, phase1("kubectl-edit"))).To(Equal(phase1("kubectl-edit")))
		var off *ActorRecords
		Expect(off.Refine(ctx, live("uid-actor-1", "kubectl-edit", t0), changes, phase1("kubectl-edit"))).To(Equal(phase1("kubectl-edit")))
		Expect(actors().Refine(ctx, live("uid-actor-1", "kubectl-edit", t0), changes, nil)).To(BeNil(), "no managedFields actor, nothing to refine")
	})

	It("uses the tolerance it is given", func() {
		put(record(func(r *actorwebhook.Record) { r.LastSeen = t0.Add(25 * time.Second) }))
		Expect(refine(actors(), "uid-actor-1", "kubectl-edit").Username).To(BeEmpty())
		wide := &ActorRecords{Reader: k8sClient, Namespace: ns, Tolerance: time.Minute}
		Expect(refine(wide, "uid-actor-1", "kubectl-edit").Username).To(Equal("alice"))
	})

	Context("the text of a merge request", func() {
		dp := func() *backflowv1alpha1.DriftProposal {
			return &backflowv1alpha1.DriftProposal{Spec: backflowv1alpha1.DriftProposalSpec{
				Actor: &backflowv1alpha1.Actor{Username: "alice", Groups: []string{"devs"}, Manager: "kubectl-edit"},
			}}
		}
		It("leaves the user name out unless usernameInMergeRequest is on", func() {
			r := &DriftProposalReconciler{}
			p := dp()
			Expect(r.forText(p).Spec.Actor.Username).To(BeEmpty())
			Expect(r.forText(p).Spec.Actor.Groups).To(BeEmpty())
			Expect(p.Spec.Actor.Username).To(Equal("alice"), "the proposal keeps it")
			Expect(mergeRequestBody(r.forText(p), nil, nil, "diff")).NotTo(ContainSubstring("alice"))
			Expect(commitMessage(r.forText(p), nil, nil)).NotTo(ContainSubstring("alice"))
			r.UsernameInMergeRequest = true
			Expect(mergeRequestBody(r.forText(p), nil, nil, "diff")).To(ContainSubstring("alice"))
			Expect(commitMessage(r.forText(p), nil, nil)).To(ContainSubstring("alice"))
		})
	})

	Context("the flusher and the sweeper", func() {
		newFlusher := func(b *actorwebhook.Buffer) *actorwebhook.Flusher {
			return &actorwebhook.Flusher{Client: k8sClient, Reader: k8sClient, Buffer: b, Namespace: ns}
		}
		get := func(r actorwebhook.Record) *backflowv1alpha1.ChangeRecord {
			var c backflowv1alpha1.ChangeRecord
			ExpectWithOffset(1, k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: r.ObjectName()}, &c)).To(Succeed())
			created = append(created, &c)
			return &c
		}
		It("writes the buffer, and merges the next flush and another replica's into the same object", func() {
			r := record(func(r *actorwebhook.Record) { r.UID = "uid-flush" })
			b1, b2 := actorwebhook.NewBuffer(10), actorwebhook.NewBuffer(10)
			b1.Add(r)
			newFlusher(b1).Flush(ctx)
			first := get(r)
			Expect(first.Spec.Paths).To(ConsistOf("/data/LOG_LEVEL"))
			Expect(first.Labels).To(HaveKeyWithValue(actorwebhook.LabelObjectUID, "uid-flush"))
			Expect(first.Spec.Username).To(Equal("alice"))

			second := r
			second.Paths = []string{"/data/OTHER"}
			second.LastSeen = r.LastSeen.Add(5 * time.Second)
			second.ResourceVersion = "9"
			b2.Add(second) // another replica saw the next change of the same actor
			newFlusher(b2).Flush(ctx)
			merged := get(r)
			Expect(merged.Spec.Paths).To(ConsistOf("/data/LOG_LEVEL", "/data/OTHER"))
			Expect(merged.Spec.Count).To(BeEquivalentTo(2))
			Expect(merged.Spec.ResourceVersion).To(Equal("9"))
			Expect(merged.Spec.LastSeen.Time.Equal(second.LastSeen)).To(BeTrue())
			Expect(b1.Len() + b2.Len()).To(BeZero())
		})

		It("keeps a record it cannot write for the next flush", func() {
			b := actorwebhook.NewBuffer(10)
			b.Add(record(func(r *actorwebhook.Record) { r.UID = "uid-keep" }))
			f := &actorwebhook.Flusher{Client: k8sClient, Reader: k8sClient, Buffer: b, Namespace: "no-such-namespace"}
			f.Flush(ctx)
			Expect(b.Len()).To(Equal(1))
		})

		It("sweeps the expired records and the oldest beyond the maximum", func() {
			now := time.Now()
			for i := range 4 {
				r := record(func(r *actorwebhook.Record) {
					r.UID = fmt.Sprintf("uid-sweep-%d", i)
					r.LastSeen = now.Add(-time.Duration(i) * time.Minute)
					r.FirstSeen = r.LastSeen
				})
				put(r)
			}
			old := record(func(r *actorwebhook.Record) {
				r.UID = "uid-sweep-old"
				r.LastSeen = now.Add(-3 * time.Hour)
				r.FirstSeen = r.LastSeen
			})
			put(old)
			s := &actorwebhook.Sweeper{Client: k8sClient, Namespace: ns, Retention: 2 * time.Hour, Max: 3}
			s.Sweep(ctx)
			var list backflowv1alpha1.ChangeRecordList
			Expect(k8sClient.List(ctx, &list, client.InNamespace(ns))).To(Succeed())
			var uids []string
			for _, c := range list.Items {
				if strings.HasPrefix(c.Spec.UID, "uid-sweep-") {
					uids = append(uids, c.Spec.UID)
				}
			}
			Expect(uids).To(ConsistOf("uid-sweep-0", "uid-sweep-1", "uid-sweep-2"), "the expired one and the oldest beyond 3 are gone")
		})
	})
})

var _ = Describe("Actor webhook, from the review to the proposal", func() {
	const ns = "default"
	var (
		ctx     = context.Background()
		now     = time.Now().UTC().Truncate(time.Second)
		created []client.Object
	)
	review := func(user, manager, uid string) string {
		return fmt.Sprintf(`{"apiVersion":"admission.k8s.io/v1","kind":"AdmissionReview","request":{
 "uid":"r-1","operation":"UPDATE","resource":{"group":"","version":"v1","resource":"configmaps"},
 "namespace":"demo","name":"cfg","userInfo":{"username":%q,"groups":["devs","system:authenticated"]},
 "options":{"kind":"UpdateOptions","fieldManager":%q},
 "oldObject":{"metadata":{"uid":%q,"resourceVersion":"5"},"data":{"LOG_LEVEL":"info"}},
 "object":{"metadata":{"uid":%q,"resourceVersion":"6"},"data":{"LOG_LEVEL":"debug"}}}}`, user, manager, uid, uid)
	}
	live := func(uid, manager string) map[string]any {
		return map[string]any{
			"metadata": map[string]any{"uid": uid, "managedFields": []any{map[string]any{
				"manager": manager, "operation": "Update", "time": now.Format(time.RFC3339), "fieldsType": "FieldsV1",
				"fieldsV1": map[string]any{"f:data": map[string]any{"f:LOG_LEVEL": map[string]any{}}},
			}}},
			"data": map[string]any{"LOG_LEVEL": "debug"},
		}
	}
	changes := []backflowv1alpha1.FieldChange{{Path: "/data/LOG_LEVEL", Op: backflowv1alpha1.OpReplace, Desired: `"info"`, Live: `"debug"`}}
	flow := func(recordUsername bool, bodies ...string) *ActorRecords {
		buf := actorwebhook.NewBuffer(100)
		h := actorwebhook.NewHandler(actorwebhook.Options{Buffer: buf, RecordUsername: recordUsername, Now: func() time.Time { return now }})
		for _, b := range bodies {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/observe/x", strings.NewReader(b)))
			ExpectWithOffset(1, rec.Body.String()).To(ContainSubstring(`"allowed":true`))
		}
		for _, r := range buf.Drain() {
			obj := actorwebhook.ToChangeRecord(r, ns)
			ExpectWithOffset(1, k8sClient.Create(ctx, obj)).To(Succeed())
			created = append(created, obj)
		}
		return &ActorRecords{Reader: k8sClient, Namespace: ns}
	}
	AfterEach(func() {
		for _, o := range created {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, o))).To(Succeed())
		}
		created = nil
	})
	actor := func(mgr string) *backflowv1alpha1.Actor {
		return &backflowv1alpha1.Actor{Manager: mgr, Operation: "Update", Time: &metav1.Time{Time: now}}
	}

	It("names the user who patched the ConfigMap", func() {
		a := flow(true, review("alice", "kubectl-patch", "uid-e2e-1"))
		got := a.Refine(ctx, live("uid-e2e-1", "kubectl-patch"), changes, actor("kubectl-patch"))
		Expect(got.Username).To(Equal("alice"))
		Expect(got.Groups).To(ConsistOf("devs", "system:authenticated"))
		Expect(string(got.Kind)).To(Equal("person"))
	})

	It("does not name a user who is claimed by a review nobody made", func() {
		// Mallory posts a review for a change that managedFields attribute to kubectl-patch by someone else,
		// naming another manager and another time.
		forged := review("mallory", "kubectl-edit", "uid-e2e-2")
		a := flow(true, forged)
		got := a.Refine(ctx, live("uid-e2e-2", "kubectl-patch"), changes, actor("kubectl-patch"))
		Expect(got.Username).To(BeEmpty())
	})

	It("stores no name at all when it is told not to", func() {
		a := flow(false, review("alice", "kubectl-patch", "uid-e2e-3"))
		var list backflowv1alpha1.ChangeRecordList
		Expect(k8sClient.List(ctx, &list, client.InNamespace(ns), client.MatchingLabels{actorwebhook.LabelObjectUID: "uid-e2e-3"})).To(Succeed())
		Expect(list.Items).To(HaveLen(1))
		stored, _ := json.Marshal(list.Items[0])
		Expect(string(stored)).NotTo(Or(ContainSubstring("alice"), ContainSubstring("devs")))
		got := a.Refine(ctx, live("uid-e2e-3", "kubectl-patch"), changes, actor("kubectl-patch"))
		Expect(got.Username).To(BeEmpty())
		Expect(string(got.Kind)).To(Equal("person"))
	})

	It("never records a value", func() {
		flow(true, review("alice", "kubectl-patch", "uid-e2e-4"))
		var list backflowv1alpha1.ChangeRecordList
		Expect(k8sClient.List(ctx, &list, client.InNamespace(ns), client.MatchingLabels{actorwebhook.LabelObjectUID: "uid-e2e-4"})).To(Succeed())
		stored, _ := json.Marshal(list.Items[0].Spec)
		Expect(string(stored)).To(ContainSubstring("/data/LOG_LEVEL"))
		Expect(string(stored)).NotTo(Or(ContainSubstring(`"debug"`), ContainSubstring(`"info"`)))
	})
})
