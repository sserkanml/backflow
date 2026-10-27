// Package chart renders the Helm chart and compares the RBAC it produces with
// golden files, so a change to what the operator is allowed to do shows up in
// review. Run it with -update to accept the current output.
package chart

import (
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// render runs helm template for the RBAC templates only.
func render(t *testing.T, args ...string) string {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		if os.Getenv("CI") == "true" {
			t.Fatal("the helm binary is not installed; CI must install it, or these tests silently check nothing")
		}
		t.Skip("the helm binary is not installed")
	}
	base := []string{"template", "backflow", "../../charts/backflow", "--namespace", "backflow-system",
		"--show-only", "templates/clusterrole.yaml", "--show-only", "templates/secrets-role.yaml"}
	out, err := exec.Command("helm", append(base, args...)...).CombinedOutput()
	text := string(out)
	// --show-only fails for a template that renders nothing; render each on its own.
	if err != nil {
		var parts []string
		for _, tpl := range []string{"templates/clusterrole.yaml", "templates/secrets-role.yaml"} {
			one, err := exec.Command("helm", append([]string{"template", "backflow", "../../charts/backflow",
				"--namespace", "backflow-system", "--show-only", tpl}, args...)...).CombinedOutput()
			if err != nil {
				if strings.Contains(string(one), "could not find template") {
					continue
				}
				t.Fatalf("helm template %s: %v\n%s", tpl, err, one)
			}
			parts = append(parts, string(one))
		}
		text = strings.Join(parts, "")
	}
	return versionless(text)
}

// versionless blanks the chart version and appVersion labels, which change with
// every release but are not what the golden files are about.
func versionless(text string) string {
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		switch {
		case strings.Contains(l, "helm.sh/chart:"):
			lines[i] = strings.Split(l, ":")[0] + ": backflow-<version>"
		case strings.Contains(l, "app.kubernetes.io/version:"):
			lines[i] = strings.Split(l, ":")[0] + ": <version>"
		}
	}
	return strings.Join(lines, "\n")
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if string(want) != got {
		t.Errorf("%s differs from the chart; run go test ./test/chart -update and review the diff.\n%s", name, got)
	}
}

func TestDefaultGrantsGetOnSecretsClusterWide(t *testing.T) {
	got := render(t)
	golden(t, "rbac-default.yaml", got)
	if !strings.Contains(got, "kind: ClusterRole") || strings.Contains(got, "kind: Role\n") {
		t.Errorf("the default chart should render a ClusterRole and no Role:\n%s", got)
	}
	checkSecretVerbs(t, got)
}

func TestSecretNamespacesReplaceTheClusterWideRule(t *testing.T) {
	got := render(t, "--set", "secretNamespaces={team-a,team-b}")
	golden(t, "rbac-secret-namespaces.yaml", got)
	clusterRole := ""
	for doc := range strings.SplitSeq(got, "\n---\n") {
		if strings.Contains(doc, "kind: ClusterRole") {
			clusterRole = doc
		}
	}
	if clusterRole == "" {
		t.Fatalf("no ClusterRole rendered:\n%s", got)
	}
	if strings.Contains(clusterRole, "secrets") {
		t.Errorf("the ClusterRole must not mention Secrets when secretNamespaces is set:\n%s", clusterRole)
	}
	for _, ns := range []string{"team-a", "team-b", "backflow-system"} {
		if !strings.Contains(got, "namespace: "+ns) {
			t.Errorf("no Role in namespace %s", ns)
		}
	}
	checkSecretVerbs(t, got)
}

// checkSecretVerbs fails when a rule on Secrets grants anything but get.
func checkSecretVerbs(t *testing.T, manifest string) {
	t.Helper()
	for doc := range strings.SplitSeq(manifest, "\n- apiGroups:") {
		if !strings.Contains(doc, "\n  - secrets\n") {
			continue
		}
		rule := doc
		if i := strings.Index(rule, "\n- "); i >= 0 {
			rule = rule[:i]
		}
		for _, verb := range []string{"list", "watch", "create", "update", "patch", "delete", `"*"`} {
			if strings.Contains(rule, "\n  - "+verb+"\n") {
				t.Errorf("the rule on Secrets grants %s:\n%s", verb, rule)
			}
		}
	}
}
