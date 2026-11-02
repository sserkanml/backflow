package migrate

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// parse reads YAML into the maps the API server's JSON gives.
func parse(t *testing.T, text string) obj {
	t.Helper()
	var o obj
	if err := yaml.Unmarshal([]byte(text), &o); err != nil {
		t.Fatalf("bad test YAML: %v\n%s", err, text)
	}
	return o
}

// norm makes a value comparable: numbers are float64 after a round trip through JSON.
func norm(t *testing.T, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func same(t *testing.T, what string, got any, want string) {
	t.Helper()
	var w any
	if err := yaml.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("bad test YAML: %v\n%s", err, want)
	}
	if !reflect.DeepEqual(norm(t, got), norm(t, w)) {
		g, _ := yaml.Marshal(got)
		t.Errorf("%s\n--- got\n%s--- want\n%s", what, g, want)
	}
}

func mustConvert(t *testing.T, kind ObjectKind, text string) *Result {
	t.Helper()
	in := parse(t, text)
	before := norm(t, in)
	res, err := Convert(kind, in)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if !reflect.DeepEqual(norm(t, in), before) {
		t.Error("the conversion changed the object it was given")
	}
	if res.Object["apiVersion"] != "backflow.io/v1" || res.Object["kind"] != string(kind) {
		t.Errorf("apiVersion/kind = %v %v", res.Object["apiVersion"], res.Object["kind"])
	}
	return res
}

func mustFail(t *testing.T, kind ObjectKind, text, contains string) {
	t.Helper()
	_, err := Convert(kind, parse(t, text))
	if err == nil {
		t.Fatalf("an invalid object was converted:\n%s", text)
	}
	if _, ok := err.(*ConversionError); !ok {
		t.Errorf("error %T, want *ConversionError", err)
	}
	if !strings.Contains(err.Error(), contains) {
		t.Errorf("error %q does not mention %q", err, contains)
	}
}

func hasNote(res *Result, part string) bool {
	for _, n := range res.Notes {
		if strings.Contains(n, part) {
			return true
		}
	}
	return false
}

const scmFull = `
apiVersion: backflow.io/v1alpha1
kind: ScmConnection
metadata:
  name: gitlab
  namespace: team-a
  uid: 11111111-2222-3333-4444-555555555555
  resourceVersion: "812"
  labels: {app: x}
  annotations: {note: keep me}
  managedFields: [{manager: kubectl, apiVersion: backflow.io/v1alpha1}]
spec:
  provider: gitlab
  url: https://gitlab.example.com
  hosts: [gitlab.example.com, git.example.com, gitlab.example.com]
  tokenSecretRef: {name: gitlab-token, key: token, optional: true}
  caSecretRef: {name: gitlab-ca, key: ca.crt}
  committer: {name: Backflow, email: bf@example.com}
status:
  observedGeneration: 3
  authenticatedAs: bot
  lastCheckTime: "2026-11-02T09:00:00Z"
  conditions:
    - {type: Ready, status: "True", reason: Authenticated, message: ok, lastTransitionTime: "2026-11-02T09:00:00Z"}
`

func TestConvertScmConnection(t *testing.T) {
	res := mustConvert(t, KindScmConnection, scmFull)
	same(t, "spec", res.Object["spec"], `
provider: GitLab
url: https://gitlab.example.com
hosts: [gitlab.example.com, git.example.com]
tokenSecretRef: {name: gitlab-token, key: token}
caSecretRef: {name: gitlab-ca, key: ca.crt}
committer: {name: Backflow, email: bf@example.com}
`)
	meta := res.Object["metadata"].(obj)
	same(t, "annotations (the existing ones stay, the marker is added)", meta["annotations"], `
note: keep me
migrate.backflow.io/migrated-from: v1alpha1
`)
	in := parse(t, scmFull)["metadata"].(obj)
	for _, k := range []string{"name", "namespace", "uid", "resourceVersion", "labels"} {
		if !reflect.DeepEqual(meta[k], in[k]) {
			t.Errorf("metadata.%s changed: %v -> %v", k, in[k], meta[k])
		}
	}
	same(t, "status is kept where it still applies", res.Status, `
observedGeneration: 3
authenticatedAs: bot
lastCheckTime: "2026-11-02T09:00:00Z"
conditions:
  - {type: Ready, status: "True", reason: Authenticated, message: ok, lastTransitionTime: "2026-11-02T09:00:00Z"}
`)
	if _, has := res.Object["status"]; has {
		t.Error("the status is returned apart, not in the object")
	}
	for _, want := range []string{"spec.provider: gitlab -> GitLab", "duplicate", "optional was true", "migrate.backflow.io/migrated-from"} {
		if !hasNote(res, want) {
			t.Errorf("no note about %q in %v", want, res.Notes)
		}
	}
}

func TestConvertScmConnectionProviders(t *testing.T) {
	for in, want := range map[string]string{"gitlab": "GitLab", "github": "GitHub", "GitLab": "GitLab", "GitHub": "GitHub"} {
		res := mustConvert(t, KindScmConnection, fmt.Sprintf(`
metadata: {name: c}
spec: {provider: %s, url: "https://x.example", tokenSecretRef: {name: t, key: k}}`, in))
		if got := res.Object["spec"].(obj)["provider"]; got != want {
			t.Errorf("provider %s -> %v, want %s", in, got, want)
		}
	}
}

func TestConvertScmConnectionMinimalHasNoOptionalFields(t *testing.T) {
	res := mustConvert(t, KindScmConnection, `
metadata: {name: c, namespace: ns}
spec: {provider: github, url: "https://github.com", tokenSecretRef: {name: t, key: k}}`)
	same(t, "spec", res.Object["spec"], `{provider: GitHub, url: "https://github.com", tokenSecretRef: {name: t, key: k}}`)
	if res.Status != nil {
		t.Errorf("a status that is not there is not made: %v", res.Status)
	}
}

func TestConvertScmConnectionInvalid(t *testing.T) {
	const ok = `tokenSecretRef: {name: t, key: k}`
	tests := []struct{ name, text, want string }{
		{"no name", `{spec: {provider: gitlab, url: "https://x", ` + ok + `}}`, "metadata.name"},
		{"no spec", `{metadata: {name: c}}`, "no spec"},
		{"unknown provider", `{metadata: {name: c}, spec: {provider: bitbucket, url: "https://x", ` + ok + `}}`, "bitbucket"},
		{"no provider", `{metadata: {name: c}, spec: {url: "https://x", ` + ok + `}}`, "provider is empty"},
		{"no url", `{metadata: {name: c}, spec: {provider: gitlab, ` + ok + `}}`, "url is empty"},
		{"no token", `{metadata: {name: c}, spec: {provider: gitlab, url: "https://x"}}`, "tokenSecretRef is missing"},
		{"token without a key", `{metadata: {name: c}, spec: {provider: gitlab, url: "https://x", tokenSecretRef: {name: t}}}`, "name and a key"},
		{"ca without a name", `{metadata: {name: c}, spec: {provider: gitlab, url: "https://x", ` + ok + `, caSecretRef: {key: k}}}`, "caSecretRef"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { mustFail(t, KindScmConnection, tt.text, tt.want) })
	}
}

const policyFull = `
apiVersion: backflow.io/v1alpha1
kind: BackflowPolicy
metadata: {name: p, namespace: team-a, resourceVersion: "9", ownerReferences: [{apiVersion: v1, kind: ConfigMap, name: o, uid: u}]}
spec:
  argoCDNamespace: openshift-gitops
  applications:
    names: [a, b, a]
    selector: {matchLabels: {env: test}}
  mode: MergeRequest
  argoCD:
    url: https://argocd.example.com
    insecureSkipTLSVerify: true
    tokenSecretRef: {name: argocd-token, key: token, optional: true}
    caSecretRef: {name: argocd-ca, key: ca.crt}
  include: [{group: apps, kind: Deployment}]
  exclude: [{kind: Secret}]
  ignoreFields: [{group: apps, kind: Deployment, name: demo, jsonPointers: [/spec/replicas]}]
  helm:
    mappingMode: ExplicitOnly
    targetValuesFile: values-prod.yaml
    mappings: [{kind: Deployment, fieldPointer: /spec/replicas, valuesPath: api.replicaCount}]
    fallbackToApplicationOverride: true
    allowCollateralChanges: [Labels]
    addOverrides: false
  kustomize:
    editBases: true
    addPatches: false
  mapping: {allowPartial: true, scratchBranches: Never}
  mergeRequest:
    branchPrefix: bf/
    targetBranch: main
    labels: [backflow, backflow, extra]
    reviewers: [alice]
    assignActor: false
  batchWindow: 45s
status:
  observedGeneration: 2
  openProposals: 1
  matchedApplications: [a]
  excludedApplications: 2
  excludedReason: ProjectNotAllowed
  argoCD: {source: Policy, url: "https://argocd.example.com"}
  conditions: [{type: Ready, status: "True", reason: Resolved, message: ok, lastTransitionTime: "2026-11-02T09:00:00Z"}]
  applications:
    - name: a
      repoURL: https://gitlab.com/x/y.git
      path: apps/a
      targetRevision: main
      syncedRevision: abc123
      sourceType: Directory
      managedResources: 3
      scmConnection: gitlab
    - name: multi
      repoURL: ghcr.io/x/charts
      path: podinfo
      managedResources: 2
      scmConnection: gitlab
      sources:
        - {position: 1, repoURL: ghcr.io/x/charts, chart: podinfo, targetRevision: 6.7.*, revision: 6.7.1, syncedRevision: 6.7.0, sourceType: Helm}
        - {position: 2, repoURL: https://gitlab.com/x/values.git, targetRevision: main, revision: def, ref: values, scmConnection: gitlab}
    - name: creds
      repoURL: https://REDACTED@gitlab.com/x/y.git
      reason: CredentialsInRepoURL
      managedResources: 0
`

func TestConvertBackflowPolicyEveryRenamedAndMovedField(t *testing.T) {
	res := mustConvert(t, KindPolicy, policyFull)
	same(t, "spec", res.Object["spec"], `
argoCD:
  namespace: openshift-gitops
  server:
    url: https://argocd.example.com
    insecureSkipTLSVerify: true
    tokenSecretRef: {name: argocd-token, key: token}
    caSecretRef: {name: argocd-ca, key: ca.crt}
applications:
  names: [a, b]
  selector: {matchLabels: {env: test}}
mode: MergeRequest
include: [{group: apps, kind: Deployment}]
exclude: [{kind: Secret}]
ignoreFields: [{group: apps, kind: Deployment, name: demo, jsonPointers: [/spec/replicas]}]
helm:
  mappingMode: ExplicitOnly
  targetValuesFile: values-prod.yaml
  mappings: [{kind: Deployment, fieldPointer: /spec/replicas, valuesPath: api.replicaCount}]
  allowApplicationOverride: true
  allowCollateralChanges: [Labels]
  allowValueOverrides: false
kustomize:
  allowBaseEdits: true
  allowNewPatches: false
mapping: {allowPartial: true, scratchBranches: Never}
git:
  branchPrefix: bf/
  targetBranch: main
  mergeRequest:
    labels: [backflow, extra]
    reviewers: [alice]
    assignActor: false
batchWindow: 45s
`)
	spec := res.Object["spec"].(obj)
	for _, gone := range []string{"argoCDNamespace", "mergeRequest", "retention"} {
		if _, has := spec[gone]; has {
			t.Errorf("spec.%s is in the result", gone)
		}
	}
	for _, want := range []string{
		"spec.argoCDNamespace -> spec.argoCD.namespace",
		"spec.argoCD.{url, tokenSecretRef, caSecretRef, insecureSkipTLSVerify} -> spec.argoCD.server",
		"spec.helm.fallbackToApplicationOverride -> spec.helm.allowApplicationOverride",
		"spec.helm.addOverrides -> spec.helm.allowValueOverrides",
		"spec.kustomize.editBases -> spec.kustomize.allowBaseEdits",
		"spec.kustomize.addPatches -> spec.kustomize.allowNewPatches",
		"spec.mergeRequest.branchPrefix -> spec.git.branchPrefix",
		"spec.mergeRequest.targetBranch -> spec.git.targetBranch",
		"spec.mergeRequest.labels -> spec.git.mergeRequest.labels",
		"spec.mergeRequest.reviewers -> spec.git.mergeRequest.reviewers",
		"spec.mergeRequest.assignActor (false)",
		"spec.applications.names: 1 duplicate",
		"spec.argoCD.tokenSecretRef.optional",
	} {
		if !hasNote(res, want) {
			t.Errorf("no note %q in\n%s", want, strings.Join(res.Notes, "\n"))
		}
	}
}

func TestConvertBackflowPolicyStatus(t *testing.T) {
	res := mustConvert(t, KindPolicy, policyFull)
	same(t, "status", res.Status, `
observedGeneration: 2
openProposals: 1
conditions: [{type: Ready, status: "True", reason: Resolved, message: ok, lastTransitionTime: "2026-11-02T09:00:00Z"}]
argoCD:
  source: Policy
  url: https://argocd.example.com
  hiddenApplications: [{reason: ProjectNotAllowed, count: 2}]
applicationCount: 3
applications:
  - name: a
    managedResources: 3
    sources:
      - {position: 1, repoURL: "https://gitlab.com/x/y.git", path: apps/a, targetRevision: main, revision: abc123, syncedRevision: abc123, type: Directory, scmConnectionName: gitlab}
  - name: multi
    managedResources: 2
    sources:
      - {position: 1, repoURL: ghcr.io/x/charts, chart: podinfo, targetRevision: 6.7.*, revision: 6.7.1, syncedRevision: 6.7.0, type: Helm}
      - {position: 2, repoURL: "https://gitlab.com/x/values.git", targetRevision: main, revision: def, ref: values, scmConnectionName: gitlab}
  - name: creds
    managedResources: 0
    untrackedReason: CredentialsInRepoURL
    sources:
      - {position: 1, repoURL: "https://REDACTED@gitlab.com/x/y.git"}
`)
	if _, has := res.Status["matchedApplications"]; has {
		t.Error("matchedApplications is dropped")
	}
}

func TestConvertBackflowPolicyStatusCapsTheApplicationsAndDropsWhatHasNoPlace(t *testing.T) {
	var b strings.Builder
	b.WriteString("metadata: {name: p}\nspec: {applications: {names: [a]}}\nstatus:\n  applications:\n")
	for i := range 130 {
		fmt.Fprintf(&b, "    - {name: app-%03d, managedResources: 1, repoURL: \"https://x/y.git\", sourceType: Jsonnet}\n", i)
	}
	res := mustConvert(t, KindPolicy, b.String())
	apps := res.Status["applications"].([]any)
	if len(apps) != 100 || res.Status["applicationCount"] != int64(130) || res.Status["applicationsTruncated"] != true {
		t.Errorf("applications %d, count %v, truncated %v", len(apps), res.Status["applicationCount"], res.Status["applicationsTruncated"])
	}
	first := apps[0].(obj)["sources"].([]any)[0].(obj)
	if _, has := first["type"]; has {
		t.Errorf("a source type that is not in the enum is dropped, not carried: %v", first)
	}
}

func TestConvertBackflowPolicyMinimal(t *testing.T) {
	res := mustConvert(t, KindPolicy, `
metadata: {name: p, namespace: ns}
spec: {applications: {names: [demo-app]}}`)
	same(t, "spec", res.Object["spec"], `{applications: {names: [demo-app]}}`)
	if res.Status != nil {
		t.Errorf("status = %v", res.Status)
	}
}

func TestConvertBackflowPolicyArgoCD(t *testing.T) {
	t.Run("a namespace alone", func(t *testing.T) {
		res := mustConvert(t, KindPolicy, `{metadata: {name: p}, spec: {argoCDNamespace: argocd, applications: {names: [a]}}}`)
		same(t, "argoCD", res.Object["spec"].(obj)["argoCD"], `{namespace: argocd}`)
	})
	t.Run("a server alone, without the default URL", func(t *testing.T) {
		res := mustConvert(t, KindPolicy, `{metadata: {name: p}, spec: {applications: {names: [a]}, argoCD: {tokenSecretRef: {name: t, key: k}}}}`)
		same(t, "argoCD", res.Object["spec"].(obj)["argoCD"], `{server: {tokenSecretRef: {name: t, key: k}}}`)
	})
}

func TestConvertBackflowPolicyAssignActor(t *testing.T) {
	for _, tt := range []struct {
		name, mr string
		want     any // nil: the field is not set (the default true applies)
	}{
		{"false stays false", `{assignActor: false}`, false},
		{"true stays true", `{assignActor: true}`, true},
		{"absent stays absent", `{branchPrefix: bf/}`, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			res := mustConvert(t, KindPolicy, `{metadata: {name: p}, spec: {applications: {names: [a]}, mergeRequest: `+tt.mr+`}}`)
			mr, _ := res.Object["spec"].(obj)["git"].(obj)["mergeRequest"].(obj)
			got, has := mr["assignActor"]
			if tt.want == nil && has || tt.want != nil && got != tt.want {
				t.Errorf("assignActor = %v (set %t), want %v", got, has, tt.want)
			}
		})
	}
}

func TestConvertBackflowPolicyKeepsUnrenamedFieldsAsTheyAre(t *testing.T) {
	res := mustConvert(t, KindPolicy, `
metadata: {name: p}
spec:
  applications: {selector: {matchExpressions: [{key: env, operator: In, values: [a, b]}]}}
  mode: ReportOnly
  helm: {mappingMode: Auto, targetValuesFile: v.yaml}
  kustomize: {}
  mapping: {scratchBranches: Auto}
  batchWindow: 0s`)
	same(t, "spec", res.Object["spec"], `
applications: {selector: {matchExpressions: [{key: env, operator: In, values: [a, b]}]}}
mode: ReportOnly
helm: {mappingMode: Auto, targetValuesFile: v.yaml}
kustomize: {}
mapping: {scratchBranches: Auto}
batchWindow: 0s`)
}

func TestConvertBackflowPolicyInvalid(t *testing.T) {
	tests := []struct{ name, text, want string }{
		{"no spec", `{metadata: {name: p}}`, "no spec"},
		{"no applications", `{metadata: {name: p}, spec: {mode: ReportOnly}}`, "spec.applications is missing"},
		{"a server without a token", `{metadata: {name: p}, spec: {applications: {names: [a]}, argoCD: {url: "https://a"}}}`, "spec.argoCD.tokenSecretRef is missing"},
		{"a token without a key", `{metadata: {name: p}, spec: {applications: {names: [a]}, argoCD: {tokenSecretRef: {name: t}}}}`, "name and a key"},
		{"a CA without a name", `{metadata: {name: p}, spec: {applications: {names: [a]}, argoCD: {tokenSecretRef: {name: t, key: k}, caSecretRef: {key: k}}}}`, "caSecretRef"},
		{"no name", `{spec: {applications: {names: [a]}}}`, "metadata.name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { mustFail(t, KindPolicy, tt.text, tt.want) })
	}
}

func TestConvertRefusesWhatItDoesNotConvert(t *testing.T) {
	for _, k := range []ObjectKind{KindChangeRecord, "Other"} {
		if _, err := Convert(k, obj{}); err == nil {
			t.Errorf("%s was converted", k)
		}
	}
}

func TestMigrated(t *testing.T) {
	tests := []struct {
		name string
		meta string
		want bool
	}{
		{"nothing says so", `{name: a}`, false},
		{"marked by the tool", `{name: a, annotations: {migrate.backflow.io/migrated-from: v1alpha1}}`, true},
		{"made through v1beta1", `{name: a, managedFields: [{apiVersion: backflow.io/v1, manager: kubectl}]}`, true},
		{"made through v1alpha1", `{name: a, managedFields: [{apiVersion: backflow.io/v1alpha1, manager: kubectl}]}`, false},
		{"both", `{name: a, managedFields: [{apiVersion: backflow.io/v1alpha1}, {apiVersion: backflow.io/v1}]}`, false},
		{"no managedFields", `{name: a, managedFields: []}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := migrated(parse(t, "{metadata: "+tt.meta+"}")); got != tt.want {
				t.Errorf("migrated = %v, want %v", got, tt.want)
			}
		})
	}
}
