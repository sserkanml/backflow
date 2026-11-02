package migrate

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sigsyaml "sigs.k8s.io/yaml"
)

// A file as a team keeps it in the repository that Argo CD applies: comments, a Deployment next to the Backflow objects, a
// document of another group with a similar name, and a v1beta1 object that must not be touched.
const teamManifests = `# Backflow for team-a. Owner: platform@example.com
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name:   untouched   # odd spacing on purpose
  namespace: team-a
spec:
  replicas: 2
---
# The GitLab of the company.
apiVersion: backflow.io/v1alpha1
kind: ScmConnection
metadata:
  name: gitlab
  namespace: team-a
  labels:
    app.kubernetes.io/part-of: backflow
spec:
  provider: gitlab # lower case in v1alpha1
  url: https://gitlab.example.com
  hosts:
    - gitlab.example.com
    - git.example.com
    - gitlab.example.com
  tokenSecretRef:
    name: gitlab-token
    key: token
    optional: true
  caSecretRef: {name: company-ca, key: ca.crt}
  committer:
    name: Backflow
    email: backflow@example.com
---
apiVersion: backflow.io/v1alpha1
kind: BackflowPolicy
metadata:
  name: web
  namespace: team-a
spec:
  # Argo CD of the platform team
  argoCDNamespace: openshift-gitops
  argoCD:
    url: https://argocd.example.com   # not the cluster-local one
    insecureSkipTLSVerify: false
    tokenSecretRef:
      name: argocd-token
      key: token
  applications:
    names: [web, web, api]
  mode: MergeRequest
  include:
    - {group: apps, kind: Deployment}
    - {kind: ConfigMap}
  helm:
    fallbackToApplicationOverride: true
    addOverrides: false
  kustomize:
    editBases: true
    addPatches: false
  mergeRequest:
    branchPrefix: backflow/
    targetBranch: main
    labels: [backflow, drift]
    reviewers: [alice, alice]
    assignActor: false
  batchWindow: 30s
---
apiVersion: backflow.io/v1beta1
kind: BackflowPolicy
metadata:
  name: already
  namespace: team-a
spec:
  argoCD: {namespace: argocd}
  applications: {names: [x]}
---
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: backflow-objects
  namespace: argocd
spec:
  source: {repoURL: "https://gitlab.example.com/team-a/gitops.git", path: backflow}
`

func docs(t *testing.T, text string) []string {
	t.Helper()
	return splitDocuments(text)
}

func TestConvertManifestKeepsWhatItDoesNotConvertByteForByte(t *testing.T) {
	out, converted, manual := ConvertManifest([]byte(teamManifests))
	if len(manual) != 0 || len(converted) != 2 {
		t.Fatalf("converted %v, manual %v", converted, manual)
	}
	before, after := docs(t, teamManifests), docs(t, string(out))
	if len(before) != len(after) {
		t.Fatalf("%d documents became %d", len(before), len(after))
	}
	for i, d := range before {
		isOld := strings.Contains(d, "backflow.io/v1alpha1")
		if !isOld && d != after[i] {
			t.Errorf("document %d was not converted and changed:\n--- before\n%s--- after\n%s", i, d, after[i])
		}
		if isOld && d == after[i] {
			t.Errorf("document %d was not converted", i)
		}
	}
	if strings.Contains(string(out), "backflow.io/v1alpha1") {
		t.Errorf("v1alpha1 is still there:\n%s", out)
	}
	// The separators and the lead comment of the file survive.
	if !strings.HasPrefix(string(out), "# Backflow for team-a. Owner: platform@example.com\n---\napiVersion: apps/v1\n") {
		t.Errorf("the start of the file changed:\n%s", out[:120])
	}
}

func TestConvertManifestKeepsComments(t *testing.T) {
	out, _, _ := ConvertManifest([]byte(teamManifests))
	for _, c := range []string{"# The GitLab of the company.", "# lower case in v1alpha1", "# Argo CD of the platform team", "# not the cluster-local one"} {
		if !strings.Contains(string(out), c) {
			t.Errorf("the comment %q is gone:\n%s", c, out)
		}
	}
}

func TestConvertManifestGivesWhatConvertGives(t *testing.T) {
	out, _, _ := ConvertManifest([]byte(teamManifests))
	byName := map[string]obj{}
	for _, d := range docs(t, string(out)) {
		var o obj
		if err := sigsyaml.Unmarshal([]byte(strings.TrimPrefix(d, "---\n")), &o); err != nil || o == nil {
			continue
		}
		byName[asString(o[keyMetadata].(obj)[keyName])] = o
	}
	same(t, "connection", byName["gitlab"][keySpec], `
provider: GitLab
url: https://gitlab.example.com
hosts: [gitlab.example.com, git.example.com]
tokenSecretRef: {name: gitlab-token, key: token}
caSecretRef: {name: company-ca, key: ca.crt}
committer: {name: Backflow, email: backflow@example.com}`)
	same(t, "policy", byName["web"][keySpec], `
argoCD:
  namespace: openshift-gitops
  server:
    url: https://argocd.example.com
    insecureSkipTLSVerify: false
    tokenSecretRef: {name: argocd-token, key: token}
applications: {names: [web, api]}
mode: MergeRequest
include:
  - {group: apps, kind: Deployment}
  - {kind: ConfigMap}
helm: {allowApplicationOverride: true, allowValueOverrides: false}
kustomize: {allowBaseEdits: true, allowNewPatches: false}
git:
  branchPrefix: backflow/
  targetBranch: main
  mergeRequest:
    labels: [backflow, drift]
    reviewers: [alice]
    assignActor: false
batchWindow: 30s`)
	if byName["gitlab"][keyAPIVersion] != newAPIVersion || byName["web"][keyAPIVersion] != newAPIVersion {
		t.Error("apiVersion")
	}
	if byName["already"][keyAPIVersion] != newAPIVersion || byName["backflow-objects"][keyAPIVersion] != "argoproj.io/v1alpha1" {
		t.Error("another document was touched")
	}
}

func TestConvertManifestIsIdempotent(t *testing.T) {
	once, _, _ := ConvertManifest([]byte(teamManifests))
	twice, converted, manual := ConvertManifest(once)
	if !bytes.Equal(once, twice) || len(converted) != 0 || len(manual) != 0 {
		t.Errorf("a second pass changed something: %v %v", converted, manual)
	}
}

func TestConvertManifestOnlyWhatMentionsV1alpha1IsLookedAt(t *testing.T) {
	const text = "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: x}\ndata: {a: 'b: [unbalanced'}\n"
	out, converted, manual := ConvertManifest([]byte(text))
	if string(out) != text || converted != nil || manual != nil {
		t.Error("a file without backflow.io/v1alpha1 was looked at")
	}
}

func TestConvertManifestNeedsAPersonForTemplatesAndAnchors(t *testing.T) {
	template := `{{- if .Values.backflow.enabled }}
apiVersion: backflow.io/v1alpha1
kind: BackflowPolicy
metadata:
  name: {{ .Release.Name }}
spec:
  argoCDNamespace: {{ .Values.argocd.namespace }}
{{- end }}
`
	out, converted, manual := ConvertManifest([]byte(template))
	if string(out) != template || len(converted) != 0 || len(manual) != 1 || !strings.Contains(manual[0], "template") {
		t.Errorf("template: converted %v, manual %v", converted, manual)
	}

	anchors := `apiVersion: backflow.io/v1alpha1
kind: ScmConnection
metadata: {name: gl, namespace: team-a}
spec:
  provider: gitlab
  url: https://gitlab.example.com
  tokenSecretRef: &tok {name: t, key: token}
  caSecretRef: *tok
`
	out, converted, manual = ConvertManifest([]byte(anchors))
	if string(out) != anchors || len(converted) != 0 || len(manual) != 1 || !strings.Contains(manual[0], "anchors") {
		t.Errorf("anchors: converted %v, manual %v", converted, manual)
	}

	// An object the conversion rejects (an unknown provider) is not guessed at.
	bad := strings.Replace(teamManifests, "provider: gitlab", "provider: bitbucket", 1)
	_, converted, manual = ConvertManifest([]byte(bad))
	if len(converted) != 1 || len(manual) != 1 || !strings.Contains(manual[0], "bitbucket") {
		t.Errorf("bad provider: converted %v, manual %v", converted, manual)
	}
}

func TestConvertManifestOtherKindsOfBackflowAreLeftAlone(t *testing.T) {
	const text = "apiVersion: backflow.io/v1alpha1\nkind: DriftProposal\nmetadata: {name: dp, namespace: team-a}\nspec: {}\n"
	out, converted, manual := ConvertManifest([]byte(text))
	if string(out) != text || converted != nil || manual != nil {
		t.Errorf("a DriftProposal is made by the operator, not kept in Git: %v %v", converted, manual)
	}
}

func TestConvertFilesRunWalksDirectoriesAndChecks(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, content string, mode os.FileMode) string {
		t.Helper()
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		return path
	}
	a := write("team-a/backflow.yaml", teamManifests, 0o640)
	b := write("team-b/policy.yml", strings.Replace(teamManifests, "name: gitlab", "name: gitlab-b", 1), 0o644)
	write("team-b/README.md", teamManifests, 0o644)  // not YAML by its name
	write(".git/hooks/x.yaml", teamManifests, 0o644) // not looked at
	write("vendor/x/y.yaml", teamManifests, 0o644)   // not looked at
	clean := write("team-c/kustomization.yaml", "resources: [a.yaml]\n", 0o644)

	var out bytes.Buffer
	res, err := ConvertFilesRun([]string{dir}, true, &out)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Changed) != 2 || res.Files != 3 || !res.Pending() {
		t.Fatalf("check: %+v\n%s", res, out.String())
	}
	if got, _ := os.ReadFile(a); string(got) != teamManifests {
		t.Error("--check wrote a file")
	}
	if !strings.Contains(out.String(), "ScmConnection gitlab: would be converted") {
		t.Errorf("output:\n%s", out.String())
	}

	out.Reset()
	if res, err = ConvertFilesRun([]string{dir}, false, &out); err != nil || len(res.Changed) != 2 {
		t.Fatalf("%v %+v\n%s", err, res, out.String())
	}
	if got, _ := os.ReadFile(a); strings.Contains(string(got), "backflow.io/v1alpha1") {
		t.Errorf("not converted:\n%s", got)
	}
	if info, _ := os.Stat(a); info.Mode().Perm() != 0o640 {
		t.Errorf("the mode of the file changed: %v", info.Mode())
	}
	if got, _ := os.ReadFile(clean); string(got) != "resources: [a.yaml]\n" {
		t.Error("a file without Backflow objects changed")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "team-b", "README.md")); string(got) != teamManifests {
		t.Error("a file that is not YAML by its name was converted")
	}
	_ = b
	// Nothing is left for a second pass, which is what a CI check says.
	res, err = ConvertFilesRun([]string{dir}, true, &out)
	if err != nil || res.Pending() {
		t.Errorf("a second check: %v %+v", err, res)
	}
	// A path named on its own is converted whatever its name.
	md := filepath.Join(dir, "team-b", "README.md")
	if res, err = ConvertFilesRun([]string{md}, false, &out); err != nil || len(res.Changed) != 1 {
		t.Errorf("explicit file: %v %+v", err, res)
	}
	if _, err := ConvertFilesRun([]string{filepath.Join(dir, "nope")}, true, &out); err == nil {
		t.Error("a path that is not there is an error")
	}
}

func TestConvertFilesMainExitCodes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "m.yaml")
	if err := os.WriteFile(path, []byte(teamManifests), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := ConvertFilesMain([]string{"--check", dir}, &out, &errOut); code != ExitError {
		t.Errorf("--check with a file to convert: exit %d, want 1\n%s", code, out.String())
	}
	if code := ConvertFilesMain([]string{dir}, &out, &errOut); code != ExitDone {
		t.Errorf("convert: exit %d\n%s%s", code, out.String(), errOut.String())
	}
	if code := ConvertFilesMain([]string{"--check", dir}, &out, &errOut); code != ExitDone {
		t.Errorf("--check when nothing is left: exit %d", code)
	}
	tmpl := filepath.Join(dir, "t.yaml")
	if err := os.WriteFile(tmpl, []byte("apiVersion: backflow.io/v1alpha1\nkind: BackflowPolicy\nmetadata: {name: {{ .x }}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := ConvertFilesMain([]string{dir}, &out, &errOut); code != ExitError || !strings.Contains(out.String(), "NEEDS A PERSON") {
		t.Errorf("a template: exit %d\n%s", code, out.String())
	}
	if code := ConvertFilesMain([]string{"--nope"}, &out, &errOut); code != ExitError {
		t.Errorf("a bad flag: exit %d", code)
	}
}
