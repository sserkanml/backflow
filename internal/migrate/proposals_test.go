package migrate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	backflowv1beta1 "github.com/sserkanml/backflow/api/v1beta1"
)

// A proposal of each kind of end, with the state the operator of the last release left in them.
const (
	dpMergedYAML = `
apiVersion: backflow.io/v1alpha1
kind: DriftProposal
metadata: {name: dp-merged, namespace: team-a}
spec:
  policyName: p1
  application: {name: a, namespace: argocd}
  resource: {version: v1, kind: ConfigMap, namespace: demo, name: cfg}
  source: {repoURL: "https://gitlab.example.com/x/y.git", revision: abc, type: Directory}
  changes: [{path: /data/a, op: Replace, desired: '"1"', live: '"2"'}]
  detectedAt: "2026-11-01T08:00:00Z"
status:
  phase: Merged
  commitSHA: 0123456789abcdef
  message: The merge request was merged.
  branch: backflow/cfg
  mergeRequest: {url: "https://gitlab.example.com/x/y/-/merge_requests/3", number: 3, branch: backflow/cfg, state: merged}
  conditions: [{type: Proposed, status: "True", reason: Merged, message: merged, lastTransitionTime: "2026-10-20T09:00:00Z"}]
`
	dpUnmappedYAML = `
apiVersion: backflow.io/v1alpha1
kind: DriftProposal
metadata: {name: dp-unmapped, namespace: team-a}
spec:
  policyName: p1
  application: {name: a, namespace: argocd}
  resource: {version: v1, kind: Secret, namespace: demo, name: s}
  source: {repoURL: "https://gitlab.example.com/x/y.git", revision: abc, type: Helm}
  changes: [{path: /data/a, op: Add, live: '"2"'}]
  detectedAt: "2026-11-01T08:00:00Z"
status:
  phase: Unmapped
  message: no file owns it
  mapping: {strategy: Unmapped, reason: NoVerifiedCandidate}
  conditions: [{type: Mapped, status: "False", reason: NoVerifiedCandidate, message: no file owns it, lastTransitionTime: "2026-11-01T08:01:00Z"}]
`
	dpSupersededYAML = `
apiVersion: backflow.io/v1alpha1
kind: DriftProposal
metadata:
  name: dp-superseded
  namespace: team-a
  annotations: {backflow.io/superseded-by: dp-open}
spec:
  policyName: p1
  application: {name: a, namespace: argocd}
  resource: {version: v1, kind: ConfigMap, namespace: demo, name: cfg}
  source: {repoURL: "https://gitlab.example.com/x/y.git", revision: abc, type: Directory}
  changes: [{path: /data/a, op: Replace, desired: '"1"', live: '"5"'}]
  detectedAt: "2026-11-01T07:00:00Z"
status:
  phase: Superseded
  supersededBy: dp-open
  conditions: [{type: Proposed, status: "False", reason: Superseded, message: replaced, lastTransitionTime: "2026-11-01T08:00:30Z"}]
`
	// A finished proposal with no condition at all: it finished at its creation, as far as anyone can tell.
	dpRejectedYAML = `
apiVersion: backflow.io/v1alpha1
kind: DriftProposal
metadata:
  name: dp-rejected
  namespace: team-a
  annotations: {backflow.io/rejection-reported: "true"}
spec:
  policyName: p1
  application: {name: a, namespace: argocd}
  resource: {version: v1, kind: ConfigMap, namespace: demo, name: cfg}
  source: {repoURL: "https://gitlab.example.com/x/y.git", revision: abc, type: Directory}
  changes: [{path: /data/a, op: Replace, desired: '"1"', live: '"6"'}]
  detectedAt: "2026-11-01T06:00:00Z"
status: {phase: Rejected}
`
)

// openProposal is dpOpen as the operator would have created it: without what the API server fills in.
func openProposal() string {
	lines := strings.Split(dpOpen, "\n")
	keep := make([]string, 0, len(lines))
	for _, line := range lines {
		switch key, _, _ := strings.Cut(strings.TrimSpace(line), ":"); key {
		case "uid", "resourceVersion", "generation", "creationTimestamp", "finalizers", "ownerReferences", "managedFields":
			continue
		}
		keep = append(keep, line)
	}
	return strings.Join(keep, "\n")
}

// withProposals is a cluster of the last release with one proposal of each end, owned by p1 as the operator makes them.
func withProposals(t *testing.T) *cluster {
	t.Helper()
	return upgraded(t, func(cl *cluster) {
		for _, text := range []string{openProposal(), dpMergedYAML, dpUnmappedYAML, dpSupersededYAML, dpRejectedYAML} {
			cl.createOld("DriftProposal", text)
		}
		cl.ownProposals("p1")
	})
}

// ownProposals gives the proposals the owner reference the operator puts on them.
func (cl *cluster) ownProposals(policy string) {
	cl.t.Helper()
	uid := cl.raw(OldVersion, "BackflowPolicy", policy)["metadata"].(obj)["uid"]
	for _, name := range []string{"dp-1", "dp-open", "dp-merged", "dp-unmapped", "dp-superseded", "dp-rejected"} {
		if !cl.exists(OldVersion, "DriftProposal", "team-a", name) {
			continue
		}
		o := cl.raw(OldVersion, "DriftProposal", name)
		o["metadata"].(obj)["ownerReferences"] = []any{obj{"apiVersion": oldAPIVersion, "kind": "BackflowPolicy", "name": policy, "uid": uid}}
		if err := cl.c.Update(context.Background(), asUnstructured(o)); err != nil {
			cl.t.Fatal(err)
		}
	}
}

func (cl *cluster) proposalExists(name string) bool {
	return cl.exists(NewVersion, "DriftProposal", "team-a", name)
}

// fault makes the tool's client fail where a test says, as a crash of the tool would at that point.
type fault struct {
	client.Client
	create, status, del func(client.Object) error
}

func (f *fault) Create(ctx context.Context, o client.Object, opts ...client.CreateOption) error {
	co := &client.CreateOptions{}
	co.ApplyOptions(opts)
	if f.create != nil && len(co.DryRun) == 0 {
		if err := f.create(o); err != nil {
			return err
		}
	}
	return f.Client.Create(ctx, o, opts...)
}

func (f *fault) Delete(ctx context.Context, o client.Object, opts ...client.DeleteOption) error {
	if f.del != nil {
		if err := f.del(o); err != nil {
			return err
		}
	}
	return f.Client.Delete(ctx, o, opts...)
}

type faultStatus struct {
	client.SubResourceWriter
	f *fault
}

func (f *fault) Status() client.SubResourceWriter { return &faultStatus{f.Client.Status(), f} }

func (s *faultStatus) Update(ctx context.Context, o client.Object, opts ...client.SubResourceUpdateOption) error {
	if s.f.status != nil && o.GetObjectKind().GroupVersionKind().Kind == "DriftProposal" {
		if err := s.f.status(o); err != nil {
			return err
		}
	}
	return s.SubResourceWriter.Update(ctx, o, opts...)
}

var errCrash = errors.New("the tool stopped here")

func nameIs(name string) func(client.Object) error {
	return func(o client.Object) error {
		if o.GetName() == name || strings.HasPrefix(o.GetName(), name) {
			return errCrash
		}
		return nil
	}
}

// runWith runs the tool with a client that fails.
func (cl *cluster) runWith(f *fault, opt Options) (*Report, string, error) {
	cl.t.Helper()
	real := cl.c
	f.Client = real
	cl.c = f
	defer func() { cl.c = real }()
	return cl.run(opt)
}

func TestMigrateDriftProposalsRoundTripsEveryStatusField(t *testing.T) {
	cl := withProposals(t)
	policyUID := cl.raw(OldVersion, "BackflowPolicy", "p1")["metadata"].(obj)["uid"]
	dir := filepath.Join(t.TempDir(), "backup")
	rep, out, err := cl.run(Options{BackupDir: dir})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	t.Log("\n" + out)
	if rep.Failed != 0 || len(rep.Proposals) != 0 || !rep.Complete() {
		t.Fatalf("report %+v", rep)
	}
	if got := cl.storedVersions("driftproposals.backflow.io"); len(got) != 1 || got[0] != NewVersion {
		t.Errorf("storedVersions = %v", got)
	}

	checkOpenProposal(t, cl, policyUID)
	checkEndedProposals(t, cl)
	checkProposalBackups(t, dir)
}

func checkOpenProposal(t *testing.T, cl *cluster, policyUID any) {
	t.Helper()
	open := cl.proposal("dp-open")
	if open.Status.Phase != backflowv1beta1.PhaseProposed || open.Status.Message != "The merge request is open." {
		t.Errorf("phase/message: %q %q", open.Status.Phase, open.Status.Message)
	}
	g := open.Status.Git
	if g == nil || g.Branch != "backflow/web-1" || g.MergeRequest == nil || g.MergeRequest.URL != "https://gitlab.example.com/x/values/-/merge_requests/7" ||
		g.MergeRequest.Number != 7 || g.MergeRequest.State != "opened" || g.Push == nil || g.Push.Rejections != 2 || g.Push.LastRejectedAt == nil ||
		!g.Push.LastRejectedAt.Equal(&metav1.Time{Time: time.Date(2026, 11, 1, 8, 5, 0, 0, time.UTC)}) {
		t.Errorf("git = %+v", g)
	}
	m := open.Status.Mapping
	if m == nil || m.Strategy != backflowv1beta1.StrategyHelmValues || !m.Verified || len(m.Edits) != 2 ||
		m.Edits[0].RepoURL != "https://gitlab.example.com/x/values.git" || m.Edits[1].Location != "labels.x" || len(m.Collateral) != 1 ||
		len(m.Unmapped) != 1 || m.Diff != "--- a\n+++ b\n" {
		t.Errorf("mapping = %+v", m)
	}
	if len(open.Status.Conditions) != 2 || open.Status.Conditions[1].Type != "Proposed" ||
		open.Status.Conditions[1].LastTransitionTime.UTC() != time.Date(2026, 11, 1, 8, 2, 0, 0, time.UTC) {
		t.Errorf("conditions = %+v", open.Status.Conditions)
	}
	if open.Status.Signals == nil || open.Status.Signals.LiveRevertedAtRevision != "abc123" || open.Status.FinishedAt != nil {
		t.Errorf("signals = %+v, finishedAt %v", open.Status.Signals, open.Status.FinishedAt)
	}
	checkOpenProposalSpec(t, open, policyUID)
}

func checkOpenProposalSpec(t *testing.T, open *backflowv1beta1.DriftProposal, policyUID any) {
	t.Helper()
	// The spec.
	if open.Spec.PolicyRef.Name != "p1" || open.Spec.ApplicationRef.Name != "a" || open.Spec.ResourceRef.Kind != "Deployment" ||
		len(open.Spec.Sources) != 2 || open.Spec.Sources[1].Role != backflowv1beta1.RoleValues || len(open.Spec.Changes) != 3 ||
		open.Spec.Actor == nil || open.Spec.Actor.Username != "alice" || open.Spec.Actor.Kind != backflowv1beta1.ActorPerson ||
		len(open.Spec.Managers) != 3 || open.Spec.Managers[0].Manager != "kubectl-edit" {
		t.Errorf("spec = %+v", open.Spec)
	}
	if open.Annotations["note"] != "keep me" || open.Annotations["backflow.io/live-reverted"] != "" ||
		open.Annotations[AnnotationMigrated] != OldVersion || open.Labels["backflow.io/policy"] != "p1" {
		t.Errorf("metadata = %v %v", open.Annotations, open.Labels)
	}
	if _, err := time.Parse(time.RFC3339, open.Annotations[AnnotationCreated]); err != nil {
		t.Errorf("created annotation = %q", open.Annotations[AnnotationCreated])
	}

	// The owner reference is the policy's, with the uid it had before.
	if len(open.OwnerReferences) != 1 || string(open.OwnerReferences[0].UID) != policyUID || open.OwnerReferences[0].APIVersion != "backflow.io/v1beta1" ||
		open.OwnerReferences[0].Name != "p1" || open.OwnerReferences[0].Kind != "BackflowPolicy" {
		t.Errorf("ownerReferences = %+v, want the policy %v", open.OwnerReferences, policyUID)
	}
}

func checkEndedProposals(t *testing.T, cl *cluster) {
	t.Helper()
	merged := cl.proposal("dp-merged")
	if merged.Status.Git == nil || merged.Status.Git.CommitSHA != "0123456789abcdef" || merged.Status.Git.MergeRequest.State != "merged" ||
		merged.Status.FinishedAt == nil || !merged.Status.FinishedAt.Time.Equal(time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("merged = %+v", merged.Status)
	}
	unmapped := cl.proposal("dp-unmapped")
	if unmapped.Status.Phase != backflowv1beta1.PhaseUnmapped || unmapped.Status.FinishedAt != nil ||
		(unmapped.Status.Mapping != nil && unmapped.Status.Mapping.Strategy != "") {
		t.Errorf("unmapped = %+v", unmapped.Status)
	}
	superseded := cl.proposal("dp-superseded")
	if superseded.Status.SupersededBy != "dp-open" || superseded.Status.Signals == nil || superseded.Status.Signals.SupersededBy != "dp-open" ||
		superseded.Annotations["backflow.io/superseded-by"] != "" || superseded.Status.FinishedAt == nil {
		t.Errorf("superseded = %+v / %v", superseded.Status, superseded.Annotations)
	}
	rejected := cl.proposal("dp-rejected")
	if rejected.Status.Signals == nil || !rejected.Status.Signals.RejectionReported {
		t.Errorf("rejected signals = %+v", rejected.Status.Signals)
	}
	// No condition: it finished when it was created, which is the creationTimestamp of the old object.
	created, _ := time.Parse(time.RFC3339, rejected.Annotations[AnnotationCreated])
	if rejected.Status.FinishedAt == nil || !rejected.Status.FinishedAt.Time.Equal(created) {
		t.Errorf("rejected finishedAt = %v, created %v", rejected.Status.FinishedAt, created)
	}
}

func checkProposalBackups(t *testing.T, dir string) {
	t.Helper()
	// The backups are the v1alpha1 objects, each marked done.
	for _, n := range []string{"dp-open", "dp-merged", "dp-unmapped", "dp-superseded", "dp-rejected", "dp-1"} {
		data, err := os.ReadFile(filepath.Join(dir, "DriftProposal", "team-a."+n+".yaml"))
		if err != nil || !strings.Contains(string(data), "apiVersion: backflow.io/v1alpha1") || !strings.Contains(string(data), "policyName: p1") {
			t.Errorf("backup of %s: %v\n%s", n, err, data)
		}
		if !(Backup{Dir: dir}).Done(KindProposal, "team-a", n) {
			t.Errorf("%s is not marked done", n)
		}
	}
}

func TestMigrateDriftProposalsRetentionDoesNotRestart(t *testing.T) {
	cl := withProposals(t)
	if _, out, err := cl.run(Options{BackupDir: filepath.Join(t.TempDir(), "backup")}); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	// The retention counts from status.finishedAt, as the proposal controller does (finishedTime): a proposal that finished
	// on 20 October is past a 14 day retention on 5 November whenever it was created again.
	merged := cl.proposal("dp-merged")
	if merged.Status.FinishedAt == nil || merged.Status.FinishedAt.After(time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("finishedAt = %v", merged.Status.FinishedAt)
	}
	created := merged.CreationTimestamp.Time
	if !created.After(merged.Status.FinishedAt.Add(time.Hour)) {
		t.Fatalf("the proposal was created again at %v, after it finished at %v; the retention must not count from there", created, merged.Status.FinishedAt)
	}
	policy := cl.policy("p1")
	retention := policy.Spec.FinishedRetention()
	if retention != 14*24*time.Hour {
		t.Fatalf("retention = %v", retention)
	}
	if deadline := merged.Status.FinishedAt.Add(retention); !deadline.Before(time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("the proposal is deleted at %v", deadline)
	}
	// An open or Unmapped proposal has no finishedAt: retention never applies.
	for _, n := range []string{"dp-open", "dp-unmapped"} {
		if cl.proposal(n).Status.FinishedAt != nil {
			t.Errorf("%s has finishedAt", n)
		}
	}
}

func TestMigrateDriftProposalsIsIdempotent(t *testing.T) {
	cl := withProposals(t)
	dir := filepath.Join(t.TempDir(), "backup")
	if _, out, err := cl.run(Options{BackupDir: dir}); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	rvs := map[string]string{}
	for _, n := range []string{"dp-open", "dp-merged", "dp-unmapped", "dp-superseded", "dp-rejected"} {
		rvs[n] = cl.proposal(n).ResourceVersion
	}
	for i := range 2 {
		rep, out, err := cl.run(Options{BackupDir: dir})
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		if rep.Converted != 0 || rep.Failed != 0 || len(rep.Proposals) != 0 || rep.ProposalsRestored != 0 || rep.BackedUp != 0 {
			t.Errorf("run %d changed something: %+v\n%s", i+2, rep, out)
		}
	}
	for n, rv := range rvs {
		if got := cl.proposal(n).ResourceVersion; got != rv {
			t.Errorf("%s was written again (%s -> %s)", n, rv, got)
		}
	}
}

func TestMigrateDriftProposalsACrashBetweenDeleteAndCreate(t *testing.T) {
	cl := withProposals(t)
	dir := filepath.Join(t.TempDir(), "backup")
	oldUID := cl.raw(OldVersion, "DriftProposal", "dp-open")["metadata"].(obj)["uid"]

	rep, out, err := cl.runWith(&fault{create: nameIs("dp-open")}, Options{BackupDir: dir})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if rep.Failed != 1 || len(rep.Proposals) != 1 || rep.Proposals[0] != "team-a/dp-open" || !strings.Contains(out, "deleted and not created") {
		t.Fatalf("report %+v\n%s", rep, out)
	}
	if cl.exists(NewVersion, "DriftProposal", "team-a", "dp-open") {
		t.Fatal("the proposal is still there: the crash is not between the delete and the create")
	}
	if got := cl.storedVersions("driftproposals.backflow.io"); len(got) != 2 {
		t.Errorf("storedVersions = %v: a proposal is missing, so it must not change", got)
	}
	if (Backup{Dir: dir}).Done(KindProposal, "team-a", "dp-open") {
		t.Fatal("marked done")
	}

	// The next run creates it from the backup, with its status, and finishes the CRD.
	rep, out, err = cl.run(Options{BackupDir: dir})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	t.Log("\n" + out)
	if rep.Failed != 0 || rep.ProposalsRestored != 1 || !rep.Complete() || !strings.Contains(out, "created again from its backup") {
		t.Fatalf("report %+v\n%s", rep, out)
	}
	open := cl.proposal("dp-open")
	if open.Status.Git == nil || open.Status.Git.MergeRequest == nil || open.Status.Git.MergeRequest.Number != 7 || open.Status.Phase != backflowv1beta1.PhaseProposed ||
		open.Spec.PolicyRef.Name != "p1" || len(open.OwnerReferences) != 1 {
		t.Errorf("the proposal after the second run: %+v / %+v", open.Spec, open.Status)
	}
	if string(open.UID) == oldUID {
		t.Error("the old object is back?")
	}
	if got := cl.storedVersions("driftproposals.backflow.io"); len(got) != 1 {
		t.Errorf("storedVersions = %v", got)
	}
	// The original creation time survives in the annotation, from the backup.
	if open.Annotations[AnnotationCreated] == "" {
		t.Error("the creationTimestamp of the old proposal was lost")
	}
}

func TestMigrateDriftProposalsACrashBetweenCreateAndStatus(t *testing.T) {
	cl := withProposals(t)
	dir := filepath.Join(t.TempDir(), "backup")

	rep, out, err := cl.runWith(&fault{status: nameIs("dp-merged")}, Options{BackupDir: dir})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if rep.Failed != 1 || !strings.Contains(out, "created without its status") {
		t.Fatalf("report %+v\n%s", rep, out)
	}
	bare := cl.proposal("dp-merged")
	if bare.Status.Phase != "" || bare.Status.Git != nil || bare.Annotations[AnnotationMigrated] != OldVersion {
		t.Fatalf("a proposal with no status was expected: %+v", bare.Status)
	}
	if got := cl.storedVersions("driftproposals.backflow.io"); len(got) != 2 {
		t.Errorf("storedVersions = %v", got)
	}

	rep, out, err = cl.run(Options{BackupDir: dir})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if rep.Failed != 0 || !rep.Complete() {
		t.Fatalf("report %+v\n%s", rep, out)
	}
	merged := cl.proposal("dp-merged")
	if merged.Status.Phase != backflowv1beta1.PhaseMerged || merged.Status.Git == nil || merged.Status.Git.CommitSHA != "0123456789abcdef" ||
		merged.Status.FinishedAt == nil || merged.UID != bare.UID {
		t.Errorf("the status was not restored on the object that was there: %+v", merged.Status)
	}
	if got := cl.storedVersions("driftproposals.backflow.io"); len(got) != 1 {
		t.Errorf("storedVersions = %v", got)
	}
}

func TestMigrateDriftProposalsACrashBeforeTheDelete(t *testing.T) {
	cl := withProposals(t)
	dir := filepath.Join(t.TempDir(), "backup")
	rep, out, err := cl.runWith(&fault{del: nameIs("dp-rejected")}, Options{BackupDir: dir})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if rep.Failed != 1 || !cl.exists(NewVersion, "DriftProposal", "team-a", "dp-rejected") {
		t.Fatalf("the proposal must be untouched: %+v\n%s", rep, out)
	}
	rep, out, err = cl.run(Options{BackupDir: dir})
	if err != nil || rep.Failed != 0 || !rep.Complete() || cl.proposal("dp-rejected").Status.Phase != backflowv1beta1.PhaseRejected {
		t.Fatalf("%v %+v\n%s", err, rep, out)
	}
}

func TestMigrateDriftProposalsWhatTheAPIServerWouldRefuseIsNotDeleted(t *testing.T) {
	// Longer than the 1024 characters v1beta1 allows for a path.
	long := "/" + strings.Repeat("a", 1100)
	text := strings.Replace(dpMergedYAML, "name: dp-merged", "name: dp-long", 1)
	text = strings.Replace(text, "path: /data/a", "path: "+long, 1)
	cl := upgraded(t, func(cl *cluster) {
		cl.createOld("DriftProposal", text)
	})
	dir := filepath.Join(t.TempDir(), "backup")
	rep, out, err := cl.run(Options{BackupDir: dir})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if rep.Failed != 1 || !strings.Contains(out, "would refuse") || !strings.Contains(out, "so the proposal is not deleted") {
		t.Fatalf("report %+v\n%s", rep, out)
	}
	if !cl.exists(NewVersion, "DriftProposal", "team-a", "dp-long") {
		t.Fatal("the proposal was deleted although the new one would be refused")
	}
	if _, err := os.Stat(filepath.Join(dir, RejectedDir, "DriftProposal", "team-a.dp-long.v1beta1.yaml")); err != nil {
		t.Errorf("no file with what was tried: %v", err)
	}
	if got := cl.storedVersions("driftproposals.backflow.io"); len(got) != 2 {
		t.Errorf("storedVersions = %v", got)
	}
	if !cl.proposalExists("dp-1") {
		t.Error("the other proposal is not migrated")
	}
}

func TestMigrateDriftProposalsSkipFinished(t *testing.T) {
	cl := withProposals(t)
	dir := filepath.Join(t.TempDir(), "backup")
	rep, out, err := cl.run(Options{BackupDir: dir, SkipFinished: true})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	t.Log("\n" + out)
	if rep.ProposalsDropped != 3 || rep.Failed != 0 || !rep.Complete() {
		t.Fatalf("report %+v\n%s", rep, out)
	}
	for _, n := range []string{"dp-merged", "dp-superseded", "dp-rejected"} {
		if cl.exists(NewVersion, "DriftProposal", "team-a", n) {
			t.Errorf("%s was not dropped", n)
		}
		if _, err := os.Stat(filepath.Join(dir, "DriftProposal", "team-a."+n+".yaml")); err != nil {
			t.Errorf("the backup of %s is gone: %v", n, err)
		}
	}
	// Open, Unmapped and the other fixture proposal (Proposed) are migrated.
	for _, n := range []string{"dp-open", "dp-unmapped", "dp-1"} {
		if !cl.proposalExists(n) {
			t.Errorf("%s is not migrated", n)
		}
	}
	if got := cl.storedVersions("driftproposals.backflow.io"); len(got) != 1 {
		t.Errorf("storedVersions = %v", got)
	}
	// A second run does not bring the dropped ones back.
	rep, out, err = cl.run(Options{BackupDir: dir, SkipFinished: true})
	if err != nil || rep.ProposalsDropped != 0 || rep.ProposalsRestored != 0 {
		t.Errorf("%v %+v\n%s", err, rep, out)
	}
	for _, n := range []string{"dp-merged", "dp-superseded", "dp-rejected"} {
		if cl.proposalExists(n) {
			t.Errorf("%s came back", n)
		}
	}
}

func TestMigrateDriftProposalsADroppedOneDeletedByACrashIsNotBroughtBack(t *testing.T) {
	cl := withProposals(t)
	dir := filepath.Join(t.TempDir(), "backup")
	// The first run drops dp-merged, fails on dp-superseded (so the CRD is not finished), and we then take away the file that
	// says dp-merged is complete, as if the tool had stopped after the delete.
	rep, out, err := cl.runWith(&fault{del: nameIs("dp-superseded")}, Options{BackupDir: dir, SkipFinished: true})
	if err != nil || rep.ProposalsDropped != 2 || rep.Failed != 1 {
		t.Fatalf("%v %+v\n%s", err, rep, out)
	}
	if err := os.Remove(filepath.Join(dir, "DriftProposal", "team-a.dp-merged.done")); err != nil {
		t.Fatal(err)
	}
	rep, out, err = cl.run(Options{BackupDir: dir, SkipFinished: true})
	if err != nil || rep.Failed != 0 || cl.proposalExists("dp-merged") || cl.proposalExists("dp-superseded") || rep.ProposalsDropped != 2 {
		t.Fatalf("%v %+v\n%s", err, rep, out)
	}
}

func TestMigrateDriftProposalsWaitsForTheOldOneToGo(t *testing.T) {
	// A finalizer keeps the deleted proposal. The tool does not create over it, and says why.
	cl := upgraded(t, func(cl *cluster) {
		cl.createOld("DriftProposal", strings.Replace(dpRejectedYAML, "annotations:", "finalizers: [example.com/hold]\n  annotations:", 1))
		cl.ownProposals("p1")
	})
	dir := filepath.Join(t.TempDir(), "backup")
	rep, out, err := cl.run(Options{BackupDir: dir, DeleteTimeout: 1500 * time.Millisecond})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if rep.Failed != 1 || !strings.Contains(out, "finalizers [example.com/hold]") || !strings.Contains(out, "still there after") {
		t.Fatalf("report %+v\n%s", rep, out)
	}
	cl.setServed(true) // the proposal that is held is read and patched through v1alpha1
	if meta := cl.raw(OldVersion, "DriftProposal", "dp-rejected")["metadata"].(obj); meta["deletionTimestamp"] == nil || migrated(cl.raw(OldVersion, "DriftProposal", "dp-rejected")) {
		t.Fatalf("created over an object that is still there: %v", meta)
	}
	// The finalizer is removed (by whoever holds it); the next run goes on.
	old := &unstructured.Unstructured{}
	old.SetGroupVersionKind(cl.gvkOf(OldVersion, "DriftProposal"))
	old.SetNamespace("team-a")
	old.SetName("dp-rejected")
	if err := cl.c.Patch(context.Background(), old, client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"finalizers":null}}`))); err != nil {
		t.Fatal(err)
	}
	rep, out, err = cl.run(Options{BackupDir: dir})
	if err != nil || rep.Failed != 0 || !rep.Complete() || !cl.proposalExists("dp-rejected") {
		t.Fatalf("%v %+v\n%s", err, rep, out)
	}
	if cl.proposal("dp-rejected").Status.Phase != backflowv1beta1.PhaseRejected {
		t.Error("the status was not restored")
	}
}

func TestMigrateDriftProposalsWithoutItsPolicyIsNotTouched(t *testing.T) {
	text := strings.Replace(dpMergedYAML, "policyName: p1", "policyName: gone", 1)
	text = strings.Replace(text, "name: dp-merged", "name: dp-orphan", 1)
	cl := upgraded(t, func(cl *cluster) { cl.createOld("DriftProposal", text) })
	rep, out, err := cl.run(Options{BackupDir: filepath.Join(t.TempDir(), "backup")})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if rep.Failed != 1 || !strings.Contains(out, "its policy team-a/gone does not exist") || !cl.exists(NewVersion, "DriftProposal", "team-a", "dp-orphan") {
		t.Fatalf("report %+v\n%s", rep, out)
	}
}

func TestMigrateDriftProposalsDryRunChangesNothingAndSaysWhatItWould(t *testing.T) {
	cl := withProposals(t)
	dir := filepath.Join(t.TempDir(), "backup")
	before := cl.raw(OldVersion, "DriftProposal", "dp-open")["metadata"].(obj)["resourceVersion"]
	rep, out, err := cl.run(Options{BackupDir: dir, DryRun: true, SkipFinished: true})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"DriftProposal team-a/dp-open (phase Proposed): would be deleted and created again as v1beta1",
		"status.mergeRequest -> status.git.mergeRequest", "DriftProposal team-a/dp-merged (phase Merged): would be deleted, not migrated (--skip-finished)",
		"metadata.annotations[backflow.io/live-reverted] -> status.signals.liveRevertedAtRevision", "storedVersions of driftproposals.backflow.io would be set"} {
		if !strings.Contains(out, want) {
			t.Errorf("the dry run does not say %q\n%s", want, out)
		}
	}
	if rep.Failed != 0 {
		t.Errorf("report %+v", rep)
	}
	if cl.raw(OldVersion, "DriftProposal", "dp-open")["metadata"].(obj)["resourceVersion"] != before || !cl.exists(OldVersion, "DriftProposal", "team-a", "dp-merged") {
		t.Error("a dry run changed a proposal")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("a dry run wrote files")
	}
}

func TestMigrateDriftProposalsDryRunBeforeV1beta1IsThereSaysItDoesNotCheck(t *testing.T) {
	text := strings.Replace(dpMergedYAML, "name: dp-merged", "name: dp-long", 1)
	text = strings.Replace(text, "path: /data/a", "path: /"+strings.Repeat("a", 1100), 1)
	cl := upgraded(t, func(cl *cluster) { cl.createOld("DriftProposal", text) })
	dir := filepath.Join(t.TempDir(), "backup")
	_, out, err := cl.run(Options{BackupDir: dir, DryRun: true})
	// The CRD has no v1beta1 yet, so there is nothing to ask: the real run asks before it deletes anything.
	if err != nil || strings.Contains(out, "FAILED") || !strings.Contains(out, "not checked against its schema here") {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("a dry run wrote files")
	}
}

func TestMigrateDriftProposalsReadsTheChartNameFromTheApplication(t *testing.T) {
	helm := func(name, application string) string {
		text := strings.Replace(dpMergedYAML, "name: dp-merged", "name: "+name, 1)
		text = strings.Replace(text, "application: {name: a, namespace: argocd}", "application: {name: "+application+", namespace: argocd}", 1)
		return strings.Replace(text, `source: {repoURL: "https://gitlab.example.com/x/y.git", revision: abc, type: Directory}`,
			`source: {repoURL: "https://charts.example.com", revision: "1.2.3", path: web, type: Helm}`, 1)
	}
	cl := upgraded(t, func(cl *cluster) {
		cl.createOld("DriftProposal", helm("dp-chart", "web"))
		cl.createOld("DriftProposal", helm("dp-nochart", "gone"))
		cl.ownProposals("p1")
	})
	data, err := os.ReadFile(filepath.Join("..", "controller", "testdata", "application-crd.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := yaml.Unmarshal(data, crd); err != nil {
		t.Fatal(err)
	}
	if err := cl.c.Create(context.Background(), crd); err != nil {
		t.Fatal(err)
	}
	cl.establish(crd.Name)
	app := &unstructured.Unstructured{Object: parse(t, `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata: {name: web, namespace: argocd}
spec:
  project: default
  destination: {server: "https://kubernetes.default.svc", namespace: demo}
  sources:
    - {repoURL: "https://charts.example.com", chart: web, targetRevision: "1.*"}
`)}
	if err := cl.c.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "argocd"}}); err != nil {
		t.Fatal(err)
	}
	if err := cl.c.Create(context.Background(), app); err != nil {
		t.Fatal(err)
	}

	rep, out, err := cl.run(Options{BackupDir: filepath.Join(t.TempDir(), "backup")})
	if err != nil || rep.Failed != 0 {
		t.Fatalf("%v %+v\n%s", err, rep, out)
	}
	if src := cl.proposal("dp-chart").Spec.Sources[0]; src.Chart != "web" || src.Path != "" {
		t.Errorf("the chart of the Application was not used: %+v", src)
	}
	if src := cl.proposal("dp-nochart").Spec.Sources[0]; src.Chart != "" || src.Path != "web" {
		t.Errorf("without the Application the name stays in path: %+v", src)
	}
}

func TestRestoreRollsBackToTheLastRelease(t *testing.T) {
	cl := withProposals(t)
	dir := filepath.Join(t.TempDir(), "backup")
	if _, out, err := cl.run(Options{BackupDir: dir}); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	// The rollback: the CRDs (and with them the objects) go, the CRDs of the last release come back, the backup is restored.
	for name := range crdFiles {
		crd := &apiextensionsv1.CustomResourceDefinition{}
		crd.Name = name
		if err := cl.c.Delete(context.Background(), crd); err != nil {
			t.Fatal(err)
		}
	}
	for name := range crdFiles {
		deadline := time.Now().Add(30 * time.Second)
		for cl.crdExists(name) && time.Now().Before(deadline) {
			time.Sleep(200 * time.Millisecond)
		}
		if cl.crdExists(name) {
			t.Fatalf("%s is not deleted", name)
		}
	}
	for name := range crdFiles {
		data, err := os.ReadFile(filepath.Join(lastRelease, crdFiles[name]))
		if err != nil {
			t.Fatal(err)
		}
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := yaml.Unmarshal(data, crd); err != nil {
			t.Fatal(err)
		}
		if err := cl.c.Create(context.Background(), crd); err != nil {
			t.Fatal(err)
		}
		cl.establish(name)
	}
	// The policies are recreated first, then the proposals that name them: p1 is not there for them to point at.
	if _, err := Restore(context.Background(), cl.c, "", nil); err == nil {
		t.Error("a restore without a directory")
	}
	var out strings.Builder
	rep, err := Restore(context.Background(), cl.c, dir, &out)
	if err != nil || rep.Failed != 0 || rep.Created != 9 {
		t.Fatalf("%v %+v\n%s", err, rep, out.String())
	}
	policy := cl.raw(OldVersion, "BackflowPolicy", "p1")
	if policy["spec"].(obj)["argoCDNamespace"] != "openshift-gitops" {
		t.Errorf("the policy is not the old one: %v", policy["spec"])
	}
	open := cl.raw(OldVersion, "DriftProposal", "dp-open")
	st := open["status"].(obj)
	if st["phase"] != "Proposed" || st["mergeRequest"].(obj)["number"] != float64(7) && st["mergeRequest"].(obj)["number"] != int64(7) {
		t.Errorf("the status of the proposal: %v", st)
	}
	refs := open["metadata"].(obj)["ownerReferences"].([]any)
	if refs[0].(obj)["uid"] != policy["metadata"].(obj)["uid"] || refs[0].(obj)["apiVersion"] != oldAPIVersion {
		t.Errorf("the owner reference is not the policy as it is now: %v (policy %v)", refs, policy["metadata"].(obj)["uid"])
	}
	// Doing it again leaves everything.
	rep, err = Restore(context.Background(), cl.c, dir, &out)
	if err != nil || rep.Created != 0 || rep.Existing != 9 {
		t.Errorf("%v %+v", err, rep)
	}
}
