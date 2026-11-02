package migrate

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const lastV1beta1Release = "testdata/crds-0.18.1"

const (
	betaScm = `
apiVersion: backflow.io/v1beta1
kind: ScmConnection
metadata: {name: gitlab, namespace: team-a}
spec:
  provider: GitLab
  url: https://gitlab.example.com
  tokenSecretRef: {name: gitlab-token, key: token}
status:
  authenticatedAs: bot
  conditions: [{type: Ready, status: "True", reason: Authenticated, message: ok, lastTransitionTime: "2026-11-02T09:00:00Z"}]
`
	betaPolicy = `
apiVersion: backflow.io/v1beta1
kind: BackflowPolicy
metadata: {name: p1, namespace: team-a}
spec:
  argoCD: {namespace: argocd}
  applications: {names: [web]}
  git: {mergeRequest: {assignActor: false}}
status:
  openProposals: 1
`
	betaProposal = `
apiVersion: backflow.io/v1beta1
kind: DriftProposal
metadata:
  name: dp-1
  namespace: team-a
  annotations: {note: kept}
spec:
  policyRef: {name: p1}
  applicationRef: {name: web, namespace: argocd}
  resourceRef: {version: v1, kind: ConfigMap, namespace: demo, name: cfg}
  sources: [{position: 1, role: Render, repoURL: "https://gitlab.example.com/x/y.git", revision: abc, type: Directory}]
  changes: [{path: /data/a, op: Replace, desired: '"1"', live: '"2"'}]
  detectedAt: "2026-11-02T09:00:00Z"
status:
  phase: Proposed
  git: {branch: backflow/x, mergeRequest: {url: "https://gitlab.example.com/x/y/-/merge_requests/7", number: 7, state: opened}}
  conditions: [{type: Proposed, status: "True", reason: MergeRequestOpen, message: open, lastTransitionTime: "2026-11-02T09:01:00Z"}]
`
)

// release018 is a cluster of release 0.18: its CRDs have v1beta1 only, and the objects are stored as v1beta1.
func release018(t *testing.T, running bool) *cluster {
	t.Helper()
	cl := startCluster(t)
	cl.installCRDsOf(lastV1beta1Release, "internal.backflow.io_changerecords.yaml")
	for _, name := range []string{"scmconnections.backflow.io", "backflowpolicies.backflow.io", "driftproposals.backflow.io"} {
		if got := cl.versions(name); !slices.Equal(got, []string{BetaVersion}) {
			t.Fatalf("%s: the CRD of 0.18 has the versions %v", name, got)
		}
	}
	cl.createAt(BetaVersion, "ScmConnection", betaScm)
	cl.createAt(BetaVersion, "BackflowPolicy", betaPolicy)
	cl.createAt(BetaVersion, "DriftProposal", betaProposal)
	if running {
		cl.deployment("backflow", 1, map[string]string{"app.kubernetes.io/name": "backflow"})
	}
	return cl
}

func (cl *cluster) resourceVersions() map[string]string {
	cl.t.Helper()
	out := map[string]string{}
	for _, k := range []struct{ version, kind, name string }{
		{BetaVersion, "ScmConnection", "gitlab"}, {BetaVersion, "BackflowPolicy", "p1"}, {BetaVersion, "DriftProposal", "dp-1"},
	} {
		out[k.kind] = asString(cl.raw(k.version, k.kind, k.name)[keyMetadata].(obj)["resourceVersion"])
	}
	return out
}

func TestMigrateStoresV1beta1ObjectsAgainAsV1WithTheOperatorRunning(t *testing.T) {
	cl := release018(t, true)
	before := cl.resourceVersions()
	beforeSpec := cl.raw(BetaVersion, "DriftProposal", "dp-1")["spec"]
	beforeStatus := cl.raw(BetaVersion, "DriftProposal", "dp-1")["status"]

	// The operator keeps running: only a no-op update of each object is made. No backup directory is needed either.
	rep, out, err := cl.run(Options{})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	t.Log("\n" + out)
	if rep.Failed != 0 || rep.Restored != 3 || rep.Converted != 0 || !rep.Complete() {
		t.Fatalf("report %+v", rep)
	}
	if r, ann := cl.replicas("backflow"); r != 1 || ann != "" {
		t.Errorf("the operator was scaled: %d replicas, annotation %q", r, ann)
	}
	for _, name := range []string{"scmconnections.backflow.io", "backflowpolicies.backflow.io", "driftproposals.backflow.io"} {
		if got := cl.storedVersions(name); !slices.Equal(got, []string{NewVersion}) {
			t.Errorf("%s: storedVersions = %v, want [v1]", name, got)
		}
		if got := cl.versions(name); !slices.Equal(got, []string{NewVersion, BetaVersion}) {
			t.Errorf("%s: versions = %v: v1beta1 stays served until 2.0", name, got)
		}
	}

	// Each object was written (that is how it came to be stored as v1), and nothing in it changed.
	after := cl.resourceVersions()
	for kind, rv := range before {
		if after[kind] == rv {
			t.Errorf("%s was not written: the resourceVersion is still %s, so it is still stored as v1beta1", kind, rv)
		}
	}
	dp := cl.raw(NewVersion, "DriftProposal", "dp-1")
	if !jsonEqual(t, dp["spec"], beforeSpec) || !jsonEqual(t, dp["status"], beforeStatus) {
		t.Errorf("the proposal changed:\nspec %v\nstatus %v", dp["spec"], dp["status"])
	}
	if dp[keyAPIVersion] != newAPIVersion || dp[keyMetadata].(obj)["annotations"].(obj)["note"] != "kept" {
		t.Errorf("proposal = %v", dp)
	}
	if got := cl.raw(NewVersion, "BackflowPolicy", "p1")["spec"].(obj)["git"].(obj)["mergeRequest"].(obj)["assignActor"]; got != false {
		t.Errorf("assignActor = %v: false must stay false", got)
	}
	// v1beta1 is still served, with the same content.
	if cl.raw(BetaVersion, "DriftProposal", "dp-1")[keyAPIVersion] != betaAPIVersion {
		t.Error("v1beta1 is not served any more")
	}

	// A second run has nothing to do and writes nothing.
	again := cl.resourceVersions()
	rep, out, err = cl.run(Options{})
	if err != nil || rep.Restored != 0 || !strings.Contains(out, "nothing to migrate") {
		t.Errorf("%v %+v\n%s", err, rep, out)
	}
	for kind, rv := range cl.resourceVersions() {
		if rv != again[kind] {
			t.Errorf("%s was written by a run with nothing to do", kind)
		}
	}
}

func TestMigrateOfV1beta1ObjectsDryRunChangesNothing(t *testing.T) {
	cl := release018(t, false)
	before := cl.resourceVersions()
	rep, out, err := cl.run(Options{DryRun: true})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"the CRD would get v1 (served and stored) next to v1beta1", "would be stored again as v1", "would be set to [v1]"} {
		if !strings.Contains(out, want) {
			t.Errorf("the dry run does not say %q\n%s", want, out)
		}
	}
	if rep.Failed != 0 || rep.Restored != 0 {
		t.Errorf("report %+v", rep)
	}
	for kind, rv := range cl.resourceVersions() {
		if rv != before[kind] {
			t.Errorf("%s was written by a dry run", kind)
		}
	}
	if got := cl.versions("driftproposals.backflow.io"); !slices.Equal(got, []string{BetaVersion}) {
		t.Errorf("a dry run changed the CRD: %v", got)
	}
	if got := cl.storedVersions("driftproposals.backflow.io"); !slices.Equal(got, []string{BetaVersion}) {
		t.Errorf("storedVersions = %v", got)
	}
}

func TestMigrateOfV1beta1StopsHalfWayAndGoesOn(t *testing.T) {
	cl := release018(t, false)
	// A policy that disappears between the list and the patch is not an error; one the API server refuses is, and the CRD keeps
	// v1beta1 in storedVersions until the next run has done it.
	fail := &fault{patch: func(o interface{ GetName() string }) error {
		if o.GetName() == "p1" {
			return errCrash
		}
		return nil
	}}
	rep, out, err := cl.runWith(fail, Options{})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if rep.Failed != 1 || rep.Complete() {
		t.Fatalf("report %+v\n%s", rep, out)
	}
	if got := cl.storedVersions("backflowpolicies.backflow.io"); !slices.Equal(got, []string{BetaVersion, NewVersion}) {
		t.Errorf("storedVersions = %v: it must keep v1beta1 while an object may be stored in it", got)
	}
	if got := cl.storedVersions("scmconnections.backflow.io"); !slices.Equal(got, []string{NewVersion}) {
		t.Errorf("storedVersions of the connections = %v: they are all done", got)
	}
	rep, out, err = cl.run(Options{})
	if err != nil || rep.Failed != 0 || !rep.Complete() {
		t.Fatalf("%v %+v\n%s", err, rep, out)
	}
	if got := cl.storedVersions("backflowpolicies.backflow.io"); !slices.Equal(got, []string{NewVersion}) {
		t.Errorf("storedVersions = %v", got)
	}
}

func TestMigrateOfV1alpha1AlsoLeavesV1beta1Served(t *testing.T) {
	cl := upgraded(t)
	if _, out, err := cl.run(Options{BackupDir: filepath.Join(t.TempDir(), "backup")}); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	// A client of 0.18 can still read and write through v1beta1.
	if cl.raw(BetaVersion, "BackflowPolicy", "p1")[keyAPIVersion] != betaAPIVersion {
		t.Error("v1beta1 is not served after the migration of a v1alpha1 installation")
	}
}

func jsonEqual(t *testing.T, a, b any) bool {
	t.Helper()
	return string(mustJSON(t, a)) == string(mustJSON(t, b))
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
