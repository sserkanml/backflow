package migrate

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// dpOpen is an open proposal of the last release: several sources, the actor with every field, an open merge request, a
// declined push and a helm mapping with every list, and one of the controller's annotations.
const dpOpen = `
apiVersion: backflow.io/v1alpha1
kind: DriftProposal
metadata:
  name: dp-open
  namespace: team-a
  uid: 11111111-2222-3333-4444-555555555555
  resourceVersion: "9"
  generation: 1
  creationTimestamp: "2026-11-01T08:00:00Z"
  labels: {backflow.io/policy: p1, backflow.io/application: a}
  annotations: {note: keep me, backflow.io/live-reverted: abc123}
  finalizers: [example.com/hold]
  ownerReferences: [{apiVersion: backflow.io/v1alpha1, kind: BackflowPolicy, name: p1, uid: old-uid, controller: true}]
  managedFields: [{manager: backflow, apiVersion: backflow.io/v1alpha1}]
spec:
  policyName: p1
  application: {name: a, namespace: argocd}
  resource: {group: apps, version: v1, kind: Deployment, namespace: demo, name: web}
  source: {repoURL: "https://gitlab.example.com/x/chart", revision: "1.2.3", targetRevision: "1.*", path: web, type: Helm}
  sources:
    - {position: 1, role: Render, repoURL: "https://charts.example.com", revision: "1.2.3", chart: web, type: Helm}
    - {position: 2, role: Values, repoURL: "https://gitlab.example.com/x/values.git", revision: def456, targetRevision: main, ref: values}
  changes:
    - {path: /spec/replicas, op: Replace, desired: "1", live: "3"}
    - {path: /metadata/labels/x, op: Add, live: '"y"'}
    - {path: /spec/paused, op: Remove, desired: "true"}
  actor:
    username: alice
    groups: [dev, ops]
    kind: person
    userAgent: kubectl/1.30
    manager: kubectl-edit
    operation: Update
    time: "2026-11-01T07:59:00Z"
    fields: [/spec/replicas]
    others:
      - {manager: Mozilla, operation: Update, time: "2026-11-01T07:58:00Z", fields: [/metadata/labels/x]}
      - {manager: kube-controller-manager, operation: Apply}
  detectedAt: "2026-11-01T08:00:00Z"
status:
  phase: Proposed
  message: The merge request is open.
  branch: backflow/web-1
  pushRejections: 2
  lastPushRejectedAt: "2026-11-01T08:05:00Z"
  mergeRequest: {url: "https://gitlab.example.com/x/values/-/merge_requests/7", number: 7, branch: backflow/web-1, state: opened}
  mapping:
    strategy: HelmValues
    repoURL: "https://gitlab.example.com/x/values.git"
    edits:
      - {file: web/values.yaml, location: replicaCount, value: "3", changeIndex: 0}
      - {file: web/values.yaml, location: labels.x, value: '"y"', changeIndex: 1}
    verified: true
    diff: "--- a\n+++ b\n"
    collateral:
      - {group: apps, kind: Deployment, namespace: demo, name: web, path: /spec/template/x, current: "1", new: "2"}
    unmapped:
      - {path: /spec/paused, reason: NoVerifiedCandidate, message: not found}
  conditions:
    - {type: Mapped, status: "True", reason: Verified, message: ok, lastTransitionTime: "2026-11-01T08:01:00Z"}
    - {type: Proposed, status: "True", reason: MergeRequestOpen, message: open, lastTransitionTime: "2026-11-01T08:02:00Z"}
`

func convertProposal(t *testing.T, text, policyUID string) *Result {
	t.Helper()
	res, err := ConvertDriftProposal(parse(t, text), policyUID, nil)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	return res
}

func TestConvertDriftProposalSpec(t *testing.T) {
	in := parse(t, dpOpen)
	before := norm(t, in)
	res, err := ConvertDriftProposal(in, "new-uid", nil)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(norm(t, in)) != fmt.Sprint(before) {
		t.Error("the conversion changed the object it was given")
	}
	if res.Object["apiVersion"] != "backflow.io/v1beta1" || res.Object["kind"] != "DriftProposal" {
		t.Errorf("apiVersion/kind = %v %v", res.Object["apiVersion"], res.Object["kind"])
	}
	same(t, "metadata", res.Object["metadata"], `
name: dp-open
namespace: team-a
labels: {backflow.io/policy: p1, backflow.io/application: a}
annotations:
  note: keep me
  migrate.backflow.io/migrated-from: v1alpha1
  migrate.backflow.io/created: "2026-11-01T08:00:00Z"
ownerReferences: [{apiVersion: backflow.io/v1beta1, kind: BackflowPolicy, name: p1, uid: new-uid, controller: true}]
`)
	same(t, "spec", res.Object["spec"], `
policyRef: {name: p1}
applicationRef: {name: a, namespace: argocd}
resourceRef: {group: apps, version: v1, kind: Deployment, namespace: demo, name: web}
sources:
  - {position: 1, role: Render, repoURL: "https://charts.example.com", revision: "1.2.3", chart: web, type: Helm}
  - {position: 2, role: Values, repoURL: "https://gitlab.example.com/x/values.git", revision: def456, targetRevision: main, ref: values}
changes:
  - {path: /spec/replicas, op: Replace, desired: "1", live: "3"}
  - {path: /metadata/labels/x, op: Add, live: '"y"'}
  - {path: /spec/paused, op: Remove, desired: "true"}
actor: {username: alice, groups: [dev, ops], kind: Person}
managers:
  - {manager: kubectl-edit, operation: Update, time: "2026-11-01T07:59:00Z", fields: [/spec/replicas]}
  - {manager: Mozilla, operation: Update, time: "2026-11-01T07:58:00Z", fields: [/metadata/labels/x]}
  - {manager: kube-controller-manager, operation: Apply}
detectedAt: "2026-11-01T08:00:00Z"
`)
	for _, want := range []string{"spec.policyName -> spec.policyRef.name", "spec.application -> spec.applicationRef", "spec.resource -> spec.resourceRef",
		"spec.source is dropped", "spec.actor.kind: person -> Person", "spec.actor.userAgent is dropped", "-> spec.managers (3)",
		"metadata.finalizers", "metadata.annotations[backflow.io/live-reverted] -> status.signals.liveRevertedAtRevision"} {
		if !hasNote(res, want) {
			t.Errorf("no note %q in %v", want, res.Notes)
		}
	}
}

func TestConvertDriftProposalStatusRoundTripsEveryField(t *testing.T) {
	res := convertProposal(t, dpOpen, "")
	same(t, "status", res.Status, `
phase: Proposed
message: The merge request is open.
git:
  branch: backflow/web-1
  mergeRequest: {url: "https://gitlab.example.com/x/values/-/merge_requests/7", number: 7, state: opened}
  push: {rejections: 2, lastRejectedAt: "2026-11-01T08:05:00Z"}
mapping:
  strategy: HelmValues
  edits:
    - {repoURL: "https://gitlab.example.com/x/values.git", file: web/values.yaml, location: replicaCount, value: "3", changeIndex: 0}
    - {repoURL: "https://gitlab.example.com/x/values.git", file: web/values.yaml, location: labels.x, value: '"y"', changeIndex: 1}
  verified: true
  diff: "--- a\n+++ b\n"
  collateral:
    - {group: apps, kind: Deployment, namespace: demo, name: web, path: /spec/template/x, current: "1", new: "2"}
  unmapped:
    - {path: /spec/paused, reason: NoVerifiedCandidate, message: not found}
conditions:
  - {type: Mapped, status: "True", reason: Verified, message: ok, lastTransitionTime: "2026-11-01T08:01:00Z"}
  - {type: Proposed, status: "True", reason: MergeRequestOpen, message: open, lastTransitionTime: "2026-11-01T08:02:00Z"}
signals: {liveRevertedAtRevision: abc123}
`)
	for _, want := range []string{"status.mergeRequest -> status.git.mergeRequest", "status.mapping.repoURL -> status.mapping.edits[].repoURL",
		"status.pushRejections, lastPushRejectedAt -> status.git.push"} {
		if !hasNote(res, want) {
			t.Errorf("no note %q in %v", want, res.Notes)
		}
	}
}

func TestConvertDriftProposalSingleSource(t *testing.T) {
	res := convertProposal(t, `
metadata: {name: dp, namespace: team-a, creationTimestamp: "2026-11-01T08:00:00Z"}
spec:
  policyName: p1
  application: {name: a, namespace: argocd}
  resource: {version: v1, kind: ConfigMap, namespace: demo, name: cfg}
  source: {repoURL: "https://gitlab.example.com/x/y.git", revision: abc, targetRevision: main, path: apps/demo, type: Directory}
  changes: [{path: /data/a, op: Replace, desired: '"1"', live: '"2"'}]
  detectedAt: "2026-11-01T08:00:00Z"
status: {phase: Detected}
`, "")
	same(t, "sources", res.Object["spec"].(obj)["sources"], `
- {position: 1, role: Render, repoURL: "https://gitlab.example.com/x/y.git", revision: abc, targetRevision: main, path: apps/demo, type: Directory}`)
	spec := res.Object["spec"].(obj)
	if _, has := spec["actor"]; has {
		t.Error("an actor out of nothing")
	}
	if _, has := spec["managers"]; has {
		t.Error("managers out of nothing")
	}
	if _, has := spec["truncated"]; has {
		t.Error("truncated out of nothing")
	}
	same(t, "status", res.Status, "{phase: Detected}")
}

func TestConvertDriftProposalActor(t *testing.T) {
	base := `
metadata: {name: dp, namespace: team-a}
spec:
  policyName: p1
  application: {name: a, namespace: argocd}
  resource: {version: v1, kind: ConfigMap, name: cfg}
  source: {repoURL: "https://x/y.git", revision: abc, type: Directory}
  changes: [{path: /data/a, op: Add, live: '"2"'}]
  detectedAt: "2026-11-01T08:00:00Z"
  actor: %s
`
	tests := []struct {
		name, actor, who, managers string
	}{
		{"controller, no webhook names", `{kind: controller}`, `{kind: Controller}`, ""},
		{"the managedFields only", `{manager: kubectl-patch, operation: Apply}`, "", `[{manager: kubectl-patch, operation: Apply}]`},
		{"the webhook only", `{username: bob, groups: [g], kind: person}`, `{username: bob, groups: [g], kind: Person}`, ""},
		{"nothing", `{}`, "", ""},
		{"a kind that is already CamelCase", `{kind: Person}`, `{kind: Person}`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := convertProposal(t, fmt.Sprintf(base, tt.actor), "")
			spec := res.Object["spec"].(obj)
			if tt.who == "" {
				if _, has := spec["actor"]; has {
					t.Errorf("actor = %v", spec["actor"])
				}
			} else {
				same(t, "actor", spec["actor"], tt.who)
			}
			if tt.managers == "" {
				if _, has := spec["managers"]; has {
					t.Errorf("managers = %v", spec["managers"])
				}
			} else {
				same(t, "managers", spec["managers"], tt.managers)
			}
		})
	}
}

func TestConvertDriftProposalSignalsFromTheFiveAnnotations(t *testing.T) {
	res := convertProposal(t, `
metadata:
  name: dp
  namespace: team-a
  annotations:
    backflow.io/superseded-by: dp-new
    backflow.io/reverted: the drift is gone
    backflow.io/live-reverted: abc123
    backflow.io/access-revoked: "true"
    backflow.io/rejection-reported: "true"
    kept: yes
spec:
  policyName: p1
  application: {name: a, namespace: argocd}
  resource: {version: v1, kind: ConfigMap, name: cfg}
  source: {repoURL: "https://x/y.git", revision: abc, type: Directory}
  changes: [{path: /data/a, op: Add, live: '"2"'}]
  detectedAt: "2026-11-01T08:00:00Z"
status: {phase: Superseded, supersededBy: dp-new}
`, "")
	same(t, "signals", res.Status["signals"], `
supersededBy: dp-new
reverted: the drift is gone
liveRevertedAtRevision: abc123
accessRevoked: true
rejectionReported: true`)
	if res.Status["supersededBy"] != "dp-new" {
		t.Errorf("status.supersededBy = %v", res.Status["supersededBy"])
	}
	ann := res.Object["metadata"].(obj)["annotations"].(obj)
	for k := range ann {
		if strings.HasPrefix(k, "backflow.io/") {
			t.Errorf("the annotation %s is still there", k)
		}
	}
	if ann["kept"] != "yes" && ann["kept"] != true {
		t.Errorf("an annotation that is not state was lost: %v", ann)
	}
}

func TestConvertDriftProposalFinishedAt(t *testing.T) {
	proposal := func(phase, created, conds string) string {
		return fmt.Sprintf(`
metadata: {name: dp, namespace: team-a, creationTimestamp: "%s"}
spec:
  policyName: p1
  application: {name: a, namespace: argocd}
  resource: {version: v1, kind: ConfigMap, name: cfg}
  source: {repoURL: "https://x/y.git", revision: abc, type: Directory}
  changes: [{path: /data/a, op: Add, live: '"2"'}]
  detectedAt: "%s"
status: {phase: %s, conditions: %s}
`, created, created, phase, conds)
	}
	const created = "2026-10-01T00:00:00Z"
	tests := []struct {
		name, phase, conds, want string // want "" means no finishedAt
	}{
		{"the last change of a condition", "Merged",
			`[{type: Mapped, status: "True", reason: R, message: m, lastTransitionTime: "2026-10-02T00:00:00Z"}, {type: Proposed, status: "True", reason: R, message: m, lastTransitionTime: "2026-10-05T12:00:00Z"}]`,
			"2026-10-05T12:00:00Z"},
		{"no condition: the creation", "Rejected", "[]", created},
		{"every phase that is final", "Failed", "[]", created},
		{"Superseded", "Superseded", "[]", created},
		{"Reverted", "Reverted", "[]", created},
		{"open: none", "Proposed", `[{type: Mapped, status: "True", reason: R, message: m, lastTransitionTime: "2026-10-02T00:00:00Z"}]`, ""},
		{"Unmapped stays for a person: none", "Unmapped", "[]", ""},
		{"Detected: none", "Detected", "[]", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := convertProposal(t, proposal(tt.phase, created, tt.conds), "")
			got, has := res.Status["finishedAt"]
			if tt.want == "" {
				if has {
					t.Errorf("finishedAt = %v", got)
				}
				return
			}
			if got != tt.want {
				t.Errorf("finishedAt = %v, want %s", got, tt.want)
			}
			if _, err := time.Parse(time.RFC3339, got.(string)); err != nil {
				t.Error(err)
			}
		})
	}
}

func TestConvertDriftProposalUnmappedMapping(t *testing.T) {
	res := convertProposal(t, `
metadata: {name: dp, namespace: team-a, creationTimestamp: "2026-11-01T08:00:00Z"}
spec:
  policyName: p1
  application: {name: child, namespace: argocd}
  resource: {version: v1, kind: ConfigMap, name: cfg}
  source: {repoURL: "https://x/y.git", revision: abc, type: Helm}
  changes: [{path: /data/a, op: Add, live: '"2"'}]
  detectedAt: "2026-11-01T08:00:00Z"
status:
  phase: Unmapped
  mapping:
    strategy: Unmapped
    reason: SetByApplication
    application:
      application: argocd/child
      option: spec.source.helm.parameters[name=image.tag]
      optionPath: /spec/source/helm/parameters/0/value
      current: '"1"'
      suggested: '"2"'
      parent: {state: Located, name: argocd/parent, repoURL: "https://x/apps.git", revision: abc, file: apps/child.yaml, location: /spec/x, diff: d, message: m}
  conditions: [{type: Mapped, status: "False", reason: SetByApplication, message: set by the Application, lastTransitionTime: "2026-11-01T08:01:00Z"}]
`, "")
	same(t, "mapping", res.Status["mapping"], `
applicationOption:
  applicationRef: {name: child, namespace: argocd}
  option: spec.source.helm.parameters[name=image.tag]
  optionPath: /spec/source/helm/parameters/0/value
  current: '"1"'
  suggested: '"2"'
  parent: {state: Located, name: argocd/parent, repoURL: "https://x/apps.git", revision: abc, file: apps/child.yaml, location: /spec/x, diff: d, message: m}`)
	if _, has := res.Status["finishedAt"]; has {
		t.Error("Unmapped never finishes")
	}

	// Nothing else in the mapping: it is not written at all.
	res = convertProposal(t, `
metadata: {name: dp, namespace: team-a}
spec:
  policyName: p1
  application: {name: a, namespace: argocd}
  resource: {version: v1, kind: ConfigMap, name: cfg}
  source: {repoURL: "https://x/y.git", revision: abc, type: Directory}
  changes: [{path: /data/a, op: Add, live: '"2"'}]
  detectedAt: "2026-11-01T08:00:00Z"
status: {phase: Unmapped, mapping: {strategy: Unmapped, reason: NoVerifiedCandidate}}
`, "")
	if _, has := res.Status["mapping"]; has {
		t.Errorf("mapping = %v", res.Status["mapping"])
	}
	if !hasNote(res, "status.mapping.strategy: Unmapped is dropped") || !hasNote(res, "status.mapping.reason is dropped") {
		t.Errorf("notes = %v", res.Notes)
	}
}

func TestConvertDriftProposalMergedKeepsTheCommit(t *testing.T) {
	res := convertProposal(t, `
metadata: {name: dp, namespace: team-a, creationTimestamp: "2026-11-01T08:00:00Z"}
spec:
  policyName: p1
  application: {name: a, namespace: argocd}
  resource: {version: v1, kind: ConfigMap, name: cfg}
  source: {repoURL: "https://x/y.git", revision: abc, type: Directory}
  changes: [{path: /data/a, op: Add, live: '"2"'}]
  detectedAt: "2026-11-01T08:00:00Z"
status:
  phase: Merged
  commitSHA: 0123456789abcdef
  mergeRequest: {url: "https://x/y/-/merge_requests/3", number: 3, branch: backflow/cfg, state: merged}
  conditions: [{type: Proposed, status: "True", reason: Merged, message: merged, lastTransitionTime: "2026-11-02T09:00:00Z"}]
`, "")
	same(t, "git", res.Status["git"], `
branch: backflow/cfg
commitSHA: 0123456789abcdef
mergeRequest: {url: "https://x/y/-/merge_requests/3", number: 3, state: merged}`)
	if res.Status["finishedAt"] != "2026-11-02T09:00:00Z" {
		t.Errorf("finishedAt = %v", res.Status["finishedAt"])
	}
}

func TestConvertDriftProposalConditionsAreKeyedByType(t *testing.T) {
	res := convertProposal(t, `
metadata: {name: dp, namespace: team-a}
spec:
  policyName: p1
  application: {name: a, namespace: argocd}
  resource: {version: v1, kind: ConfigMap, name: cfg}
  source: {repoURL: "https://x/y.git", revision: abc, type: Directory}
  changes: [{path: /data/a, op: Add, live: '"2"'}]
  detectedAt: "2026-11-01T08:00:00Z"
status:
  phase: Detected
  conditions:
    - {type: Mapped, status: "False", reason: A, message: first, lastTransitionTime: "2026-11-01T08:00:00Z"}
    - {type: Mapped, status: "True", reason: B, message: second, lastTransitionTime: "2026-11-01T08:01:00Z"}
`, "")
	conds := res.Status["conditions"].([]any)
	if len(conds) != 1 || conds[0].(obj)["message"] != "second" || !hasNote(res, "listed twice") {
		t.Errorf("conditions = %v\n%v", conds, res.Notes)
	}
}

func TestConvertDriftProposalLongValuesAreCutAndSaid(t *testing.T) {
	long := strings.Repeat("x", 3000)
	res := convertProposal(t, fmt.Sprintf(`
metadata: {name: dp, namespace: team-a}
spec:
  policyName: p1
  application: {name: a, namespace: argocd}
  resource: {version: v1, kind: ConfigMap, name: cfg}
  source: {repoURL: "https://x/y.git", revision: abc, type: Directory}
  changes: [{path: /data/a, op: Add, live: '"2"'}]
  detectedAt: "2026-11-01T08:00:00Z"
status: {phase: Detected, message: %s}
`, long), "")
	if got := res.Status["message"].(string); len(got) != 2048 || !hasNote(res, "status.message: cut to 2048") {
		t.Errorf("message of %d characters\n%v", len(got), res.Notes)
	}
}

func TestConvertDriftProposalMoreThan256Changes(t *testing.T) {
	changes := make([]string, 300)
	for i := range changes {
		changes[i] = fmt.Sprintf(`{path: /data/k%d, op: Add, live: '"v"'}`, i)
	}
	text := func(phase string) string {
		return fmt.Sprintf(`
metadata: {name: dp, namespace: team-a}
spec:
  policyName: p1
  application: {name: a, namespace: argocd}
  resource: {version: v1, kind: ConfigMap, name: cfg}
  source: {repoURL: "https://x/y.git", revision: abc, type: Directory}
  changes: [%s]
  detectedAt: "2026-11-01T08:00:00Z"
status:
  phase: %s
  mapping:
    strategy: Direct
    edits:
      - {file: a.yaml, location: /data/k1, value: '"v"', changeIndex: 1}
      - {file: a.yaml, location: /data/k290, value: '"v"', changeIndex: 290}
`, strings.Join(changes, ","), phase)
	}
	// A finished one keeps the first 256, as the operator does for a drift that big, and says so.
	res := convertProposal(t, text("Merged"), "")
	spec := res.Object["spec"].(obj)
	if len(spec["changes"].([]any)) != 256 || spec["truncated"] != true {
		t.Errorf("changes %d, truncated %v", len(spec["changes"].([]any)), spec["truncated"])
	}
	edits := res.Status["mapping"].(obj)["edits"].([]any)
	if len(edits) != 1 || !hasNote(res, "the first 256 of 300") || !hasNote(res, "beyond the 256 kept") {
		t.Errorf("edits %v\n%v", edits, res.Notes)
	}
	// An open one is not cut: that would drop what its merge request is about.
	mustFailProposal(t, text("Proposed"), "holds 256")
	mustFailProposal(t, text("Detected"), "holds 256")
	if res := convertProposal(t, text("Unmapped"), ""); res.Object["spec"].(obj)["truncated"] != true {
		t.Error("an Unmapped proposal is cut like a finished one")
	}
}

func mustFailProposal(t *testing.T, text, contains string) {
	t.Helper()
	_, err := ConvertDriftProposal(parse(t, text), "", nil)
	if err == nil {
		t.Fatalf("an invalid proposal was converted:\n%s", text)
	}
	if _, ok := err.(*ConversionError); !ok {
		t.Errorf("error %T, want *ConversionError", err)
	}
	if !strings.Contains(err.Error(), contains) {
		t.Errorf("error %q does not mention %q", err, contains)
	}
}

func TestConvertDriftProposalInvalid(t *testing.T) {
	const ok = `
metadata: {name: dp, namespace: team-a}
spec:
  policyName: p1
  application: {name: a, namespace: argocd}
  resource: {version: v1, kind: ConfigMap, name: cfg}
  source: {repoURL: "https://x/y.git", revision: abc, type: Directory}
  changes: [{path: /data/a, op: Add, live: '"2"'}]
  detectedAt: "2026-11-01T08:00:00Z"
`
	convertProposal(t, ok, "")
	tests := []struct{ name, text, want string }{
		{"no spec", "metadata: {name: dp, namespace: team-a}", "no spec"},
		{"no name", strings.Replace(ok, "name: dp,", "", 1), "metadata.name"},
		{"no namespace", strings.Replace(ok, ", namespace: team-a", "", 1), "metadata.name"},
		{"no policy", strings.Replace(ok, "policyName: p1", "policyName: ''", 1), "policyName"},
		{"no application", strings.Replace(ok, "application: {name: a, namespace: argocd}", "", 1), "spec.application"},
		{"no resource", strings.Replace(ok, "resource: {version: v1, kind: ConfigMap, name: cfg}", "", 1), "spec.resource"},
		{"no source", strings.Replace(ok, `source: {repoURL: "https://x/y.git", revision: abc, type: Directory}`, "", 1), "spec.source"},
		{"no changes", strings.Replace(ok, `changes: [{path: /data/a, op: Add, live: '"2"'}]`, "changes: []", 1), "spec.changes"},
		{"no detectedAt", strings.Replace(ok, `detectedAt: "2026-11-01T08:00:00Z"`, "", 1), "detectedAt"},
		{"an actor kind that does not exist", ok + "  actor: {kind: robot}\n", "robot"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { mustFailProposal(t, tt.text, tt.want) })
	}
}

func TestConvertDriftProposalOwnerReferences(t *testing.T) {
	const text = `
metadata:
  name: dp
  namespace: team-a
  ownerReferences:
    - {apiVersion: backflow.io/v1alpha1, kind: BackflowPolicy, name: p1, uid: old}
    - {apiVersion: v1, kind: ConfigMap, name: other, uid: keep}
spec:
  policyName: p1
  application: {name: a, namespace: argocd}
  resource: {version: v1, kind: ConfigMap, name: cfg}
  source: {repoURL: "https://x/y.git", revision: abc, type: Directory}
  changes: [{path: /data/a, op: Add, live: '"2"'}]
  detectedAt: "2026-11-01T08:00:00Z"
`
	res := convertProposal(t, text, "live")
	same(t, "owners", res.Object["metadata"].(obj)["ownerReferences"], `
- {apiVersion: backflow.io/v1beta1, kind: BackflowPolicy, name: p1, uid: live}
- {apiVersion: v1, kind: ConfigMap, name: other, uid: keep}`)
	// Without a uid to put in, the old one is kept and only the apiVersion changes.
	res = convertProposal(t, text, "")
	if refs := res.Object["metadata"].(obj)["ownerReferences"].([]any); refs[0].(obj)["uid"] != "old" || refs[0].(obj)["apiVersion"] != "backflow.io/v1beta1" {
		t.Errorf("owners = %v", refs)
	}
}

func TestFinishedPhase(t *testing.T) {
	for phase, want := range map[string]bool{"Merged": true, "Rejected": true, "Reverted": true, "Superseded": true, "Failed": true,
		"Detected": false, "Mapping": false, "Proposed": false, "Unmapped": false, "": false} {
		if FinishedPhase(phase) != want {
			t.Errorf("FinishedPhase(%q) = %v", phase, !want)
		}
	}
}

const dpHelmSingle = `
metadata: {name: dp, namespace: team-a, creationTimestamp: "2026-11-01T08:00:00Z"}
spec:
  policyName: p1
  application: {name: web, namespace: argocd}
  resource: {version: v1, kind: ConfigMap, name: cfg}
  source: {repoURL: "https://charts.example.com", revision: "1.2.3", targetRevision: "1.*", path: web, type: Helm}
  changes: [{path: /data/a, op: Add, live: '"2"'}]
  detectedAt: "2026-11-01T08:00:00Z"
`

func TestConvertDriftProposalChartNameFromTheApplication(t *testing.T) {
	app := func(spec string) obj { return parse(t, "metadata: {name: web, namespace: argocd}\nspec: "+spec) }
	first := func(res *Result) obj { return res.Object[keySpec].(obj)["sources"].([]any)[0].(obj) }
	tests := []struct {
		name      string
		app       obj
		wantChart string // "" means the chart stays in path
		wantNote  string
	}{
		{"the Application has that chart there (multi-source)",
			app(`{sources: [{repoURL: "https://charts.example.com", chart: web, targetRevision: "1.*"}, {repoURL: "https://g/v.git", ref: values}]}`),
			"web", "path -> chart \"web\""},
		{"the Application has that chart there (single source)",
			app(`{source: {repoURL: "https://charts.example.com/", chart: web}}`), "web", "path -> chart"},
		{"the Application has no chart there: a path in Git",
			app(`{sources: [{repoURL: "https://charts.example.com", path: web}]}`), "", "a path in a Git repository"},
		{"the Application has another chart now",
			app(`{sources: [{repoURL: "https://charts.example.com", chart: other}]}`), "", "has the chart \"other\" now"},
		{"the repository at that position is another",
			app(`{sources: [{repoURL: "https://elsewhere.example.com", chart: web}]}`), "", "another repository"},
		{"the Application has no source at that position",
			app(`{sources: []}`), "", "another repository"},
		{"the Application is not there", nil, "", "the Application was not found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := ConvertDriftProposal(parse(t, dpHelmSingle), "", tt.app)
			if err != nil {
				t.Fatal(err)
			}
			src := first(res)
			if tt.wantChart != "" {
				if src["chart"] != tt.wantChart || src[keyPath] != nil {
					t.Errorf("source = %v: want the chart %q and no path", src, tt.wantChart)
				}
			} else if src["chart"] != nil || src[keyPath] != "web" {
				t.Errorf("source = %v: want the chart name to stay in path", src)
			}
			if !hasNote(res, tt.wantNote) {
				t.Errorf("no note %q in %v", tt.wantNote, res.Notes)
			}
		})
	}
}

func TestConvertDriftProposalChartLookupIsOnlyForASingleHelmSource(t *testing.T) {
	// A Directory source keeps its path and says nothing about charts.
	text := strings.Replace(dpHelmSingle, "type: Helm", "type: Directory", 1)
	res, err := ConvertDriftProposal(parse(t, text), "", parse(t, "spec: {source: {repoURL: \"https://charts.example.com\", chart: web}}"))
	if err != nil {
		t.Fatal(err)
	}
	src := res.Object[keySpec].(obj)["sources"].([]any)[0].(obj)
	if src[keyPath] != "web" || src["chart"] != nil || hasNote(res, "chart") {
		t.Errorf("source = %v, notes %v", src, res.Notes)
	}
}
