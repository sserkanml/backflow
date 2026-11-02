package migrate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	backflowv1beta1 "github.com/sserkanml/backflow/api/v1beta1"
)

const (
	oldScm = `
apiVersion: backflow.io/v1alpha1
kind: ScmConnection
metadata: {name: gitlab, namespace: team-a}
spec:
  provider: gitlab
  url: https://gitlab.example.com
  hosts: [gitlab.example.com, gitlab.example.com]
  tokenSecretRef: {name: gitlab-token, key: token, optional: true}
  committer: {name: Backflow, email: bf@example.com}
status:
  authenticatedAs: bot
  conditions: [{type: Ready, status: "True", reason: Authenticated, message: ok, lastTransitionTime: "2026-11-02T09:00:00Z"}]
`
	oldPolicy = `
apiVersion: backflow.io/v1alpha1
kind: BackflowPolicy
metadata: {name: %s, namespace: team-a}
spec:
  argoCDNamespace: openshift-gitops
  applications: {names: [a, b]}
  mode: MergeRequest
  argoCD:
    url: https://argocd.example.com
    tokenSecretRef: {name: argocd-token, key: token}
  helm: {addOverrides: false, fallbackToApplicationOverride: true}
  kustomize: {editBases: true, addPatches: false}
  mergeRequest: {branchPrefix: bf/, labels: [backflow], assignActor: false}
  batchWindow: %s
status:
  openProposals: 1
  matchedApplications: [a]
  excludedApplications: 1
  excludedReason: NotVisibleToToken
  argoCD: {source: Policy, url: "https://argocd.example.com"}
  applications:
    - {name: a, repoURL: "https://gitlab.example.com/x/y.git", path: apps/a, syncedRevision: abc, sourceType: Directory, managedResources: 2, scmConnection: gitlab}
`
	oldProposal = `
apiVersion: backflow.io/v1alpha1
kind: DriftProposal
metadata: {name: dp-1, namespace: team-a}
spec:
  policyName: p1
  application: {name: a, namespace: argocd}
  resource: {version: v1, kind: ConfigMap, namespace: demo, name: cfg}
  source: {repoURL: "https://gitlab.example.com/x/y.git", revision: abc, type: Directory}
  changes: [{path: /data/a, op: Replace, desired: '"1"', live: '"2"'}]
  detectedAt: "2026-11-02T09:00:00Z"
status: {phase: Proposed}
`
	oldChangeRecord = `
apiVersion: backflow.io/v1alpha1
kind: ChangeRecord
metadata: {name: %s, namespace: backflow-system}
spec: {uid: u, username: alice, count: 1, firstSeen: "2026-11-02T09:00:00Z", lastSeen: "2026-11-02T09:00:00Z", kind: person, name: cfg, operation: UPDATE, paths: [/data/a], resource: configmaps}
`
)

func policyYAML(name, window string) string {
	return strings.Replace(strings.Replace(oldPolicy, "%s", name, 1), "%s", window, 1)
}

// upgraded is a cluster of the last release: its CRDs, and its objects stored as v1alpha1. The tool does the rest.
func upgraded(t *testing.T, before ...func(*cluster)) *cluster {
	t.Helper()
	cl := startCluster(t)
	cl.installLastRelease()
	cl.createOld("ScmConnection", oldScm)
	cl.createOld("BackflowPolicy", policyYAML("p1", "30s"))
	cl.createOld("BackflowPolicy", policyYAML("p2", "1m"))
	cl.createOld("DriftProposal", oldProposal)
	cl.createOld("ChangeRecord", strings.Replace(oldChangeRecord, "%s", "cr-1", 1))
	cl.createOld("ChangeRecord", strings.Replace(oldChangeRecord, "%s", "cr-2", 1))
	for _, f := range before {
		f(cl)
	}
	return cl
}

func (cl *cluster) proposal(name string) *backflowv1beta1.DriftProposal {
	cl.t.Helper()
	p := &backflowv1beta1.DriftProposal{}
	if err := cl.typ.Get(context.Background(), types.NamespacedName{Namespace: "team-a", Name: name}, p); err != nil {
		cl.t.Fatal(err)
	}
	return p
}

func (cl *cluster) policy(name string) *backflowv1beta1.BackflowPolicy {
	cl.t.Helper()
	p := &backflowv1beta1.BackflowPolicy{}
	if err := cl.typ.Get(context.Background(), types.NamespacedName{Namespace: "team-a", Name: name}, p); err != nil {
		cl.t.Fatal(err)
	}
	return p
}

func TestTheFixtureIsTheLastRelease(t *testing.T) {
	cl := upgraded(t)
	for name := range crdFiles {
		if got, want := cl.versions(name), []string{OldVersion}; len(got) != 1 || got[0] != want[0] {
			t.Errorf("%s: versions = %v, want %v", name, got, want)
		}
		if got := cl.storedVersions(name); len(got) != 1 || got[0] != OldVersion {
			t.Errorf("%s: storedVersions = %v", name, got)
		}
	}
}

func TestMigrateConvertsInPlace(t *testing.T) {
	cl := upgraded(t)
	uid := cl.raw(OldVersion, "BackflowPolicy", "p1")["metadata"].(obj)["uid"]
	dir := filepath.Join(t.TempDir(), "backup")

	rep, out, err := cl.run(Options{BackupDir: dir})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	t.Log("\n" + out)
	if rep.Converted != 4 || rep.Failed != 0 || rep.Skipped != 0 {
		t.Errorf("converted %d, failed %d, skipped %d, want 4, 0, 0", rep.Converted, rep.Failed, rep.Skipped)
	}

	checkConvertedPolicy(t, cl, uid)
	checkConvertedConnection(t, cl)

	// The CRDs.
	for name := range crdFiles {
		if got := cl.storedVersions(name); len(got) != 1 || got[0] != NewVersion {
			t.Errorf("%s: storedVersions = %v, want [v1beta1]", name, got)
		}
	}
	if len(rep.Finalized) != 3 {
		t.Errorf("finalized = %v", rep.Finalized)
	}
	for name := range crdFiles {
		if got := cl.versions(name); len(got) != 1 || got[0] != NewVersion {
			t.Errorf("%s: versions = %v, want only %s: the tool adds %s while it works and removes it after", name, got, NewVersion, OldVersion)
		}
	}
	if !cl.crdExists("changerecords.internal.backflow.io") {
		t.Error("the CRD of the ChangeRecords of this release was not created")
	}

	// The proposal is created again as v1beta1; the ChangeRecords are gone and not backed up.
	dp := cl.proposal("dp-1")
	if dp.Spec.PolicyRef.Name != "p1" || dp.Status.Phase != backflowv1beta1.PhaseProposed || dp.Annotations[AnnotationMigrated] != OldVersion {
		t.Errorf("the proposal = %+v / %+v / %v", dp.Spec, dp.Status, dp.Annotations)
	}
	if len(rep.Proposals) != 0 || !strings.Contains(out, "team-a/dp-1 (phase Proposed): migrated") {
		t.Errorf("proposals = %v\n%s", rep.Proposals, out)
	}
	if rep.ChangeRecordsDropped != 2 || cl.exists(OldVersion, "ChangeRecord", "backflow-system", "cr-1") || !strings.Contains(out, "not backed up") {
		t.Errorf("change records: dropped %d", rep.ChangeRecordsDropped)
	}
	if _, err := os.Stat(filepath.Join(dir, "ChangeRecord")); !os.IsNotExist(err) {
		t.Error("ChangeRecords were written to the backup")
	}
	for _, f := range []string{"ScmConnection/team-a.gitlab.yaml", "BackflowPolicy/team-a.p1.yaml", "BackflowPolicy/team-a.p2.yaml", "DriftProposal/team-a.dp-1.yaml"} {
		data, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			t.Errorf("backup %s: %v", f, err)
			continue
		}
		if !strings.Contains(string(data), "apiVersion: backflow.io/v1alpha1") {
			t.Errorf("backup %s is not the v1alpha1 object:\n%s", f, data)
		}
	}
	data, _ := os.ReadFile(filepath.Join(dir, "BackflowPolicy", "team-a.p1.yaml"))
	if !strings.Contains(string(data), "argoCDNamespace: openshift-gitops") || !strings.Contains(string(data), "mergeRequest:") {
		t.Errorf("the backup of a policy is not what was in the cluster before:\n%s", data)
	}
	if code := Summarize(os.Stderr, Options{}, rep); code != ExitDone {
		t.Errorf("exit code %d, want %d", code, ExitDone)
	}
}

func checkConvertedPolicy(t *testing.T, cl *cluster, uid any) {
	t.Helper()
	// The policy, as the operator reads it.
	p := cl.policy("p1")
	if string(p.UID) != uid || p.Spec.ArgoCD.Namespace != "openshift-gitops" || p.Spec.ArgoCD.Server == nil ||
		p.Spec.ArgoCD.Server.URL != "https://argocd.example.com" || p.Spec.ArgoCD.Server.TokenSecretRef.Name != "argocd-token" {
		t.Errorf("argoCD: uid %s (was %v), %+v", p.UID, uid, p.Spec.ArgoCD)
	}
	if p.Spec.Git.BranchPrefix != "bf/" || len(p.Spec.Git.MergeRequest.Labels) != 1 || p.Spec.Git.MergeRequest.AssignActor == nil || *p.Spec.Git.MergeRequest.AssignActor {
		t.Errorf("git: %+v: assignActor false must stay false", p.Spec.Git)
	}
	if p.Spec.OverridesAllowed() || p.Spec.Helm == nil || !p.Spec.Helm.AllowApplicationOverride || p.Spec.Kustomize == nil ||
		!p.Spec.Kustomize.AllowBaseEdits || p.Spec.KustomizeAddsPatches() {
		t.Errorf("helm %+v, kustomize %+v", p.Spec.Helm, p.Spec.Kustomize)
	}
	if p.Spec.BatchWindow == nil || p.Spec.BatchWindow.Duration != 30*time.Second {
		t.Errorf("batchWindow = %v", p.Spec.BatchWindow)
	}
	if p.Spec.Retention.FinishedProposals == nil || p.Spec.Retention.FinishedProposals.Duration != 14*24*time.Hour {
		t.Errorf("retention = %v: the API default applies", p.Spec.Retention.FinishedProposals)
	}
	if p.Annotations[AnnotationMigrated] != OldVersion {
		t.Errorf("annotations = %v", p.Annotations)
	}
	if p.Status.OpenProposals != 1 || p.Status.ArgoCD == nil || p.Status.ArgoCD.HiddenCount() != 1 || p.Status.ApplicationCount != 1 ||
		len(p.Status.Applications) != 1 || p.Status.Applications[0].First().ScmConnectionName != "gitlab" ||
		p.Status.Applications[0].First().PathOrChart() != "apps/a" {
		t.Errorf("status = %+v", p.Status)
	}

}

func checkConvertedConnection(t *testing.T, cl *cluster) {
	t.Helper()
	// The connection.
	c := &backflowv1beta1.ScmConnection{}
	if err := cl.typ.Get(context.Background(), types.NamespacedName{Namespace: "team-a", Name: "gitlab"}, c); err != nil {
		t.Fatal(err)
	}
	if c.Spec.Provider != backflowv1beta1.ScmProviderGitLab || len(c.Spec.Hosts) != 1 || c.Spec.TokenSecretRef.Key != "token" ||
		c.Status.AuthenticatedAs != "bot" || len(c.Status.Conditions) != 1 {
		t.Errorf("connection = %+v / %+v", c.Spec, c.Status)
	}

}

func TestMigrateIsIdempotent(t *testing.T) {
	cl := upgraded(t)
	dir := filepath.Join(t.TempDir(), "backup")
	if _, out, err := cl.run(Options{BackupDir: dir}); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	before := map[string]any{}
	for _, n := range []string{"p1", "p2"} {
		before[n] = cl.raw(NewVersion, "BackflowPolicy", n)["metadata"].(obj)["resourceVersion"]
	}
	scmRV := cl.raw(NewVersion, "ScmConnection", "gitlab")["metadata"].(obj)["resourceVersion"]
	crdBefore := cl.storedVersions("backflowpolicies.backflow.io")
	dpRV := cl.raw(NewVersion, "DriftProposal", "dp-1")["metadata"].(obj)["resourceVersion"]

	rep, out, err := cl.run(Options{BackupDir: dir})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if rep.Converted != 0 || rep.Failed != 0 || rep.BackedUp != 0 {
		t.Errorf("a second run changed something: %+v\n%s", rep, out)
	}
	for n, rv := range before {
		if got := cl.raw(NewVersion, "BackflowPolicy", n)["metadata"].(obj)["resourceVersion"]; got != rv {
			t.Errorf("policy %s was written again (%v -> %v)", n, rv, got)
		}
	}
	if got := cl.raw(NewVersion, "ScmConnection", "gitlab")["metadata"].(obj)["resourceVersion"]; got != scmRV {
		t.Error("the connection was written again")
	}
	if got := cl.storedVersions("backflowpolicies.backflow.io"); len(got) != len(crdBefore) {
		t.Errorf("storedVersions = %v", got)
	}
	if got := cl.raw(NewVersion, "DriftProposal", "dp-1")["metadata"].(obj)["resourceVersion"]; got != dpRV {
		t.Errorf("the proposal was written again (%v -> %v)", dpRV, got)
	}
	if rep.KeptBackups != 0 || len(rep.Proposals) != 0 {
		t.Errorf("nothing is left, so nothing is backed up or listed: kept %d, proposals %v", rep.KeptBackups, rep.Proposals)
	}
}

func TestMigrateAfterAPartialFailureDoesOnlyWhatIsLeft(t *testing.T) {
	// A server URL without a scheme is valid in v1alpha1 and is refused by the pattern of v1beta1: one object cannot be converted.
	cl := upgraded(t, func(cl *cluster) {
		cl.createOld("BackflowPolicy", strings.Replace(policyYAML("p3", "30s"), "url: https://argocd.example.com", "url: argocd.example.com", 1))
	})
	dir := filepath.Join(t.TempDir(), "backup")

	rep, out, err := cl.run(Options{BackupDir: dir})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if rep.Converted != 4 || rep.Failed != 1 || !strings.Contains(out, "BackflowPolicy team-a/p3: FAILED") || !strings.Contains(out, "should match") {
		t.Errorf("converted %d failed %d\n%s", rep.Converted, rep.Failed, out)
	}
	if got := cl.storedVersions("backflowpolicies.backflow.io"); len(got) != 2 {
		t.Errorf("storedVersions of policies = %v: it must not change while one is left", got)
	}
	if got := cl.storedVersions("scmconnections.backflow.io"); len(got) != 1 {
		t.Errorf("storedVersions of connections = %v: they are all done", got)
	}
	if Summarize(os.Stderr, Options{}, rep) != ExitError {
		t.Error("a failure is an error exit")
	}
	cl.setServed(true) // the policy that failed is read the old way
	p3 := cl.raw(OldVersion, "BackflowPolicy", "p3")
	cl.setServed(false)
	if p3["spec"].(obj)["argoCD"].(obj)["url"] != "argocd.example.com" {
		t.Errorf("the failed object was changed: %v", p3["spec"])
	}

	// The person corrects the v1beta1 object the tool wrote for them, and replaces the object with it: a write through
	// v1alpha1 would drop the v1alpha1 fields.
	rejected := filepath.Join(dir, RejectedDir, "BackflowPolicy", "team-a.p3.v1beta1.yaml")
	data, err := os.ReadFile(rejected)
	if err != nil || !strings.Contains(out, "kubectl replace -f "+rejected) || !strings.Contains(out, "do NOT edit the object itself") {
		t.Fatalf("no object to correct: %v\n%s", err, out)
	}
	fixedObj := parse(t, strings.Replace(string(data), "url: argocd.example.com", "url: https://argocd.example.com", 1))
	if fixedObj["apiVersion"] != "backflow.io/v1beta1" {
		t.Fatalf("the object to correct is %v", fixedObj["apiVersion"])
	}
	if err := cl.c.Update(context.Background(), asUnstructured(fixedObj)); err != nil {
		t.Fatalf("kubectl replace of the corrected object: %v", err)
	}
	rep, out, err = cl.run(Options{BackupDir: dir})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if rep.Converted != 0 || rep.Failed != 0 || rep.Skipped != 3 {
		// The connections are not looked at: their CRD stores no v1alpha1 any more.
		t.Errorf("second run: converted %d, failed %d, skipped %d, want 0, 0, 3\n%s", rep.Converted, rep.Failed, rep.Skipped, out)
	}
	if got := cl.storedVersions("backflowpolicies.backflow.io"); len(got) != 1 || got[0] != NewVersion {
		t.Errorf("storedVersions of policies = %v, want [v1beta1]", got)
	}
	if p3 := cl.policy("p3"); p3.Spec.ArgoCD.Server == nil || p3.Spec.ArgoCD.Server.URL != "https://argocd.example.com" ||
		p3.Spec.ArgoCD.Namespace != "openshift-gitops" || *p3.Spec.Git.MergeRequest.AssignActor {
		t.Errorf("p3 is not converted: %+v\n%s", p3.Spec.ArgoCD, out)
	}
	// The backup of p3 is the v1alpha1 object as it was.
	data, _ = os.ReadFile(filepath.Join(dir, "BackflowPolicy", "team-a.p3.yaml"))
	if !strings.Contains(string(data), "url: argocd.example.com") || !strings.Contains(string(data), "argoCDNamespace") {
		t.Errorf("the backup of the first run was replaced:\n%s", data)
	}
}

func TestMigrateKeepsABackupThatIsThereAndWritesNothingAfterAFailedBackup(t *testing.T) {
	cl := upgraded(t)
	dir := filepath.Join(t.TempDir(), "backup")
	if err := os.MkdirAll(filepath.Join(dir, "BackflowPolicy"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "BackflowPolicy", "team-a.p1.yaml"), []byte("earlier\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, out, err := cl.run(Options{BackupDir: dir})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "BackflowPolicy", "team-a.p1.yaml")); string(data) != "earlier\n" {
		t.Errorf("an existing backup was overwritten: %q", data)
	}
	if rep.KeptBackups != 1 || rep.BackedUp != 3 {
		t.Errorf("kept %d, written %d", rep.KeptBackups, rep.BackedUp)
	}

	// A backup that cannot be written stops the run before anything is changed.
	cl2 := upgraded(t)
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, out, err = cl2.run(Options{BackupDir: filepath.Join(blocked, "backup")})
	if err == nil || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("err = %v\n%s", err, out)
	}
	if cl2.storedVersions("backflowpolicies.backflow.io")[0] != OldVersion || !cl2.exists(OldVersion, "ChangeRecord", "backflow-system", "cr-1") {
		t.Error("something was changed after the backup failed")
	}
	if p := cl2.raw(OldVersion, "BackflowPolicy", "p1"); p["spec"].(obj)["argoCDNamespace"] != "openshift-gitops" {
		t.Error("a policy was changed after the backup failed")
	}
}

func TestMigrateDryRunChangesNothing(t *testing.T) {
	cl := upgraded(t)
	rvs := map[string]any{}
	for _, n := range []string{"p1", "p2"} {
		rvs[n] = cl.raw(OldVersion, "BackflowPolicy", n)["metadata"].(obj)["resourceVersion"]
	}
	stored := cl.storedVersions("backflowpolicies.backflow.io")
	dir := filepath.Join(t.TempDir(), "backup")

	rep, out, err := cl.run(Options{BackupDir: dir, DryRun: true})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	t.Log("\n" + out)
	for _, want := range []string{"DRY RUN", "BackflowPolicy team-a/p1: would be converted", "spec.argoCDNamespace -> spec.argoCD.namespace",
		"spec.mergeRequest.assignActor (false)", "ScmConnection team-a/gitlab: would be converted", "2 object(s) would be deleted", "would be set to [v1beta1]",
		"would be backed up", "DriftProposal team-a/dp-1 (phase Proposed): would be deleted and created again as v1beta1",
		"spec.policyName -> spec.policyRef.name"} {
		if !strings.Contains(out, want) {
			t.Errorf("the dry run does not say %q", want)
		}
	}
	if rep.Planned != 4 || rep.Converted != 0 {
		t.Errorf("planned %d, converted %d", rep.Planned, rep.Converted)
	}
	for n, rv := range rvs {
		if got := cl.raw(OldVersion, "BackflowPolicy", n)["metadata"].(obj)["resourceVersion"]; got != rv {
			t.Errorf("policy %s was written", n)
		}
	}
	if got := cl.storedVersions("backflowpolicies.backflow.io"); len(got) != len(stored) {
		t.Errorf("storedVersions changed: %v", got)
	}
	if !cl.exists(OldVersion, "ChangeRecord", "backflow-system", "cr-1") {
		t.Error("a ChangeRecord was deleted by a dry run")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("a dry run wrote a backup")
	}
	if Summarize(os.Stderr, Options{DryRun: true}, rep) != ExitDone {
		t.Error("exit code")
	}
	if !cl.exists(OldVersion, "DriftProposal", "team-a", "dp-1") || cl.raw(OldVersion, "DriftProposal", "dp-1")["metadata"].(obj)["annotations"] != nil {
		t.Error("a dry run changed the proposal")
	}
	// A dry run needs no backup directory.
	if _, _, err := cl.run(Options{DryRun: true}); err != nil {
		t.Errorf("a dry run without --backup-dir: %v", err)
	}
}

func TestMigrateRefusesToRunWhileTheOperatorRuns(t *testing.T) {
	cl := upgraded(t)
	dir := filepath.Join(t.TempDir(), "backup")
	one := int32(1)
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "backflow-system", Name: "backflow-controller-manager", Labels: map[string]string{"app.kubernetes.io/name": "backflow"}},
		Spec: appsv1.DeploymentSpec{
			Replicas: &one, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"a": "b"}},
			Template: podTemplate(),
		},
	}
	if err := cl.c.Create(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	_, out, err := cl.run(Options{BackupDir: dir})
	if _, ok := err.(*RunningOperatorError); !ok {
		t.Fatalf("err = %v\n%s", err, out)
	}
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Error("a backup was written although the operator is running")
	}
	if cl.storedVersions("backflowpolicies.backflow.io")[0] != OldVersion {
		t.Error("a CRD was changed")
	}
	if _, _, err := cl.run(Options{DryRun: true}); err == nil {
		t.Error("even a dry run reads a cluster that is changing under it: it refuses too")
	}

	zero := int32(0)
	d.Spec.Replicas = &zero
	if err := cl.c.Update(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if _, out, err := cl.run(Options{BackupDir: dir}); err != nil {
		t.Fatalf("scaled to zero: %v\n%s", err, out)
	}
}

func TestMigrateSkipsObjectsMadeThroughV1beta1(t *testing.T) {
	// A policy the tool cannot convert leaves the CRD with both versions; an object made through v1beta1 then is not touched.
	cl := upgraded(t, func(cl *cluster) {
		cl.createOld("BackflowPolicy", strings.Replace(policyYAML("p3", "30s"), "url: https://argocd.example.com", "url: argocd.example.com", 1))
	})
	dir := filepath.Join(t.TempDir(), "backup")
	if _, out, err := cl.run(Options{BackupDir: dir}); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if got := cl.versions("backflowpolicies.backflow.io"); len(got) != 2 {
		t.Fatalf("versions = %v: with a policy left, v1alpha1 stays (unserved)", got)
	}
	made := &backflowv1beta1.BackflowPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "fresh"},
		Spec: backflowv1beta1.BackflowPolicySpec{
			Applications: backflowv1beta1.ApplicationSelector{Names: []string{"x"}},
			ArgoCD:       backflowv1beta1.ArgoCDSpec{Namespace: "argocd"},
		},
	}
	if err := cl.typ.Create(context.Background(), made); err != nil {
		t.Fatal(err)
	}
	rv := made.ResourceVersion
	rep, out, err := cl.run(Options{BackupDir: dir})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if rep.Failed != 1 || rep.Skipped != 3 {
		t.Errorf("failed %d, skipped %d (p1, p2 and fresh)\n%s", rep.Failed, rep.Skipped, out)
	}
	got := cl.policy("fresh")
	if got.ResourceVersion != rv || got.Spec.ArgoCD.Namespace != "argocd" || got.Spec.Applications.Names[0] != "x" {
		t.Errorf("a v1beta1 object was touched: %+v", got.Spec)
	}
}

func TestMigrateAddsV1beta1AndRemovesV1alpha1(t *testing.T) {
	cl := upgraded(t, func(cl *cluster) {
		cl.createOld("BackflowPolicy", strings.Replace(policyYAML("p3", "30s"), "url: https://argocd.example.com", "url: argocd.example.com", 1))
	})
	dir := filepath.Join(t.TempDir(), "backup")
	const policies = "backflowpolicies.backflow.io"

	// A dry run leaves the CRDs alone and reads through the v1alpha1 endpoint that is there.
	if _, out, err := cl.run(Options{DryRun: true}); err != nil || !strings.Contains(out, "the CRD would get v1beta1") {
		t.Fatalf("%v\n%s", err, out)
	}
	if got := cl.versions(policies); len(got) != 1 || got[0] != OldVersion {
		t.Fatalf("a dry run changed the CRD: %v", got)
	}

	rep, out, err := cl.run(Options{BackupDir: dir})
	if err != nil || rep.Failed != 1 {
		t.Fatalf("%v %+v\n%s", err, rep, out)
	}
	// One policy is left: both versions are in the CRD, v1beta1 is stored, v1alpha1 is not served.
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := cl.c.Get(context.Background(), types.NamespacedName{Name: policies}, crd); err != nil {
		t.Fatal(err)
	}
	if !servesAndStores(crd, NewVersion) {
		t.Error("v1beta1 is not served and stored")
	}
	if i, served := versionServed(crd); i < 0 || served || crd.Spec.Versions[i].Storage {
		t.Errorf("v1alpha1 must stay for the object that is left, not served and not stored: %+v", crd.Spec.Versions)
	}
	if got := cl.storedVersions(policies); len(got) != 2 {
		t.Errorf("storedVersions = %v", got)
	}
	// The shape of the CRD is the one of this release: printer columns and names come from it.
	if len(crd.Spec.Names.ShortNames) == 0 || crd.Spec.Names.Plural != "backflowpolicies" {
		t.Errorf("names = %+v", crd.Spec.Names)
	}
	// The one that cannot be converted is read the old way until it is dealt with: serve v1alpha1 for the check.
	cl.setServed(true)
	if cl.raw(OldVersion, "BackflowPolicy", "p3")["spec"].(obj)["argoCD"].(obj)["url"] != "argocd.example.com" {
		t.Error("the object that failed was changed")
	}
	cl.setServed(false)

	// Delete it: nothing is left, and the next run removes v1alpha1.
	if err := cl.typ.Delete(context.Background(), &backflowv1beta1.BackflowPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "p3"}}); err != nil {
		t.Fatal(err)
	}
	rep, out, err = cl.run(Options{BackupDir: dir})
	if err != nil || rep.Failed != 0 {
		t.Fatalf("%v %+v\n%s", err, rep, out)
	}
	if got := cl.versions(policies); len(got) != 1 || got[0] != NewVersion {
		t.Errorf("versions = %v", got)
	}
	if got := cl.storedVersions(policies); len(got) != 1 || got[0] != NewVersion {
		t.Errorf("storedVersions = %v", got)
	}
}

func TestMigrateWithNothingToDo(t *testing.T) {
	cl := upgraded(t)
	dir := filepath.Join(t.TempDir(), "backup")
	// The proposal is deleted, so nothing is left for it.
	if err := cl.c.Delete(context.Background(), asUnstructured(cl.raw(OldVersion, "DriftProposal", "dp-1"))); err != nil {
		t.Fatal(err)
	}
	rep, out, err := cl.run(Options{BackupDir: dir})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !rep.Complete() || len(rep.Finalized) != 3 {
		t.Errorf("complete %t, finalized %v\n%s", rep.Complete(), rep.Finalized, out)
	}
	if Summarize(os.Stderr, Options{}, rep) != ExitDone {
		t.Error("exit code")
	}
	rep, out, err = cl.run(Options{BackupDir: dir})
	if err != nil || !rep.Complete() || !strings.Contains(out, "nothing to do") {
		t.Errorf("a run when everything is done: %v\n%s", err, out)
	}
}

func (cl *cluster) deployment(name string, replicas int32, labels map[string]string) {
	cl.t.Helper()
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "backflow-system", Name: name, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"a": name}},
			Template: podTemplate(),
		},
	}
	d.Spec.Template.Labels = map[string]string{"a": name}
	if err := cl.c.Create(context.Background(), d); err != nil {
		cl.t.Fatal(err)
	}
}

func (cl *cluster) replicas(name string) (int32, string) {
	cl.t.Helper()
	d := &appsv1.Deployment{}
	if err := cl.c.Get(context.Background(), types.NamespacedName{Namespace: "backflow-system", Name: name}, d); err != nil {
		cl.t.Fatal(err)
	}
	return *d.Spec.Replicas, d.Annotations[annotationReplicas]
}

func TestMigrateStopsTheOperatorAndRestoresIt(t *testing.T) {
	cl := upgraded(t)
	cl.deployment("backflow", 2, map[string]string{"app.kubernetes.io/name": "bf", "app.kubernetes.io/instance": "r"})
	cl.deployment("other", 3, map[string]string{"app.kubernetes.io/name": "not-ours"})
	dir := filepath.Join(t.TempDir(), "backup")
	sel := "app.kubernetes.io/instance=r,app.kubernetes.io/name=bf"

	// Without the flag the tool refuses; with the wrong selector it does not see the operator at all.
	if _, _, err := cl.run(Options{BackupDir: dir, OperatorSelector: sel}); err == nil {
		t.Fatal("the operator runs and the tool went on")
	}
	rep, out, err := cl.run(Options{BackupDir: dir, OperatorSelector: sel, StopOperator: true, OperatorNamespace: "backflow-system"})
	if err != nil || rep.Failed != 0 {
		t.Fatalf("%v %+v\n%s", err, rep, out)
	}
	if r, ann := cl.replicas("backflow"); r != 0 || ann != "2" {
		t.Errorf("the operator has %d replicas, annotation %q; want 0 and \"2\"", r, ann)
	}
	if r, _ := cl.replicas("other"); r != 3 {
		t.Errorf("a Deployment that is not the operator was scaled: %d", r)
	}

	// Restoring scales it back and takes the note away; a second time there is nothing to do.
	rep, out, err = cl.run(Options{RestoreOperator: true, OperatorSelector: sel, OperatorNamespace: "backflow-system"})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if r, ann := cl.replicas("backflow"); r != 2 || ann != "" {
		t.Errorf("after the restore: %d replicas, annotation %q", r, ann)
	}
	if _, out, err = cl.run(Options{RestoreOperator: true, OperatorSelector: sel}); err != nil || !strings.Contains(out, "nothing to restore") {
		t.Errorf("%v\n%s", err, out)
	}
	_ = rep
}

func TestMigrateLeavesTheOperatorAloneWhenThereIsNothingToMigrate(t *testing.T) {
	cl := upgraded(t)
	dir := filepath.Join(t.TempDir(), "backup")
	if _, out, err := cl.run(Options{BackupDir: dir}); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	// Now the operator of the new version runs. A pre-upgrade hook that runs again must not stop it.
	cl.deployment("backflow", 1, map[string]string{"app.kubernetes.io/name": "backflow"})
	if _, out, err := cl.run(Options{BackupDir: dir, StopOperator: true}); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if r, ann := cl.replicas("backflow"); r != 1 || ann != "" {
		t.Errorf("the operator was scaled: %d replicas, annotation %q", r, ann)
	}
}
