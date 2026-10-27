// Package chart renders the Helm chart and compares the RBAC it produces with
// golden files, so a change to what the operator is allowed to do shows up in
// review. The comparison is of the parsed objects, not of the text, so it holds
// for Helm 3 and Helm 4, which differ in blank lines and comments. Run the tests
// with -update to accept the current output.
package chart

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/pmezard/go-difflib/difflib"
	"sigs.k8s.io/yaml"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// templates are the RBAC templates of the chart.
var templates = []string{"templates/clusterrole.yaml", "templates/secrets-role.yaml"}

// render runs helm template for the RBAC templates and returns the normalised
// objects.
func render(t *testing.T, args ...string) []map[string]any {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		if os.Getenv("CI") == "true" {
			t.Fatal("the helm binary is not installed; CI must install it, or these tests silently check nothing")
		}
		t.Skip("the helm binary is not installed")
	}
	var text strings.Builder
	for _, tpl := range templates {
		cmd := append([]string{"template", "backflow", "../../charts/backflow", "--namespace", "backflow-system",
			"--show-only", tpl}, args...)
		out, err := exec.Command("helm", cmd...).CombinedOutput()
		if err != nil {
			// --show-only fails for a template that renders nothing.
			if strings.Contains(string(out), "could not find template") {
				continue
			}
			t.Fatalf("helm %s: %v\n%s", strings.Join(cmd, " "), err, out)
		}
		text.Write(out)
		text.WriteString("\n---\n")
	}
	return normalise(t, text.String())
}

// normalise parses the YAML documents of text, drops empty ones (comments and
// blank lines vanish with parsing), masks the chart version labels, which change
// with every release, and sorts the objects by kind, namespace and name.
func normalise(t *testing.T, text string) []map[string]any {
	t.Helper()
	var objs []map[string]any
	for doc := range strings.SplitSeq(text, "\n---") {
		var obj map[string]any
		if err := yaml.Unmarshal([]byte(strings.TrimPrefix(doc, "\n")), &obj); err != nil {
			t.Fatalf("not valid YAML: %v\n%s", err, doc)
		}
		if len(obj) == 0 {
			continue
		}
		if meta, ok := obj["metadata"].(map[string]any); ok {
			if labels, ok := meta["labels"].(map[string]any); ok {
				if _, has := labels["helm.sh/chart"]; has {
					labels["helm.sh/chart"] = "backflow-<version>"
				}
				if _, has := labels["app.kubernetes.io/version"]; has {
					labels["app.kubernetes.io/version"] = "<version>"
				}
			}
		}
		objs = append(objs, obj)
	}
	sort.SliceStable(objs, func(i, j int) bool { return identity(objs[i]) < identity(objs[j]) })
	return objs
}

func identity(obj map[string]any) string {
	meta, _ := obj["metadata"].(map[string]any)
	return fmt.Sprintf("%v/%v/%v", obj["kind"], meta["namespace"], meta["name"])
}

// text is the normalised objects as the YAML kept in the golden files.
func text(t *testing.T, objs []map[string]any) string {
	t.Helper()
	var b strings.Builder
	for _, obj := range objs {
		out, err := yaml.Marshal(obj)
		if err != nil {
			t.Fatal(err)
		}
		b.WriteString("---\n")
		b.Write(out)
	}
	return b.String()
}

func golden(t *testing.T, name string, got []map[string]any) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, []byte(text(t, got)), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	want := normalise(t, string(raw))
	if reflect.DeepEqual(want, got) {
		return
	}
	diff, _ := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
		A: difflib.SplitLines(text(t, want)), B: difflib.SplitLines(text(t, got)),
		FromFile: name, ToFile: "rendered", Context: 3,
	})
	t.Errorf("%s differs from the chart; run go test ./test/chart -update and review the diff.\n%s", name, diff)
}

func TestDefaultGrantsGetOnSecretsClusterWide(t *testing.T) {
	got := render(t)
	golden(t, "rbac-default.yaml", got)
	if len(got) != 1 || got[0]["kind"] != "ClusterRole" {
		t.Errorf("the default chart should render one ClusterRole and no Role:\n%s", text(t, got))
	}
	checkSecretVerbs(t, got)
}

func TestSecretNamespacesReplaceTheClusterWideRule(t *testing.T) {
	got := render(t, "--set", "secretNamespaces={team-a,team-b}")
	golden(t, "rbac-secret-namespaces.yaml", got)
	roles := map[string]bool{}
	for _, obj := range got {
		meta, _ := obj["metadata"].(map[string]any)
		switch obj["kind"] {
		case "ClusterRole":
			if rendered := text(t, []map[string]any{obj}); strings.Contains(rendered, "secrets") {
				t.Errorf("the ClusterRole must not mention Secrets when secretNamespaces is set:\n%s", rendered)
			}
		case "Role":
			roles[fmt.Sprint(meta["namespace"])] = true
		}
	}
	for _, ns := range []string{"team-a", "team-b", "backflow-system"} {
		if !roles[ns] {
			t.Errorf("no Role in namespace %s", ns)
		}
	}
	checkSecretVerbs(t, got)
}

// checkSecretVerbs fails when a rule on Secrets grants anything but get.
func checkSecretVerbs(t *testing.T, objs []map[string]any) {
	t.Helper()
	for _, obj := range objs {
		rules, _ := obj["rules"].([]any)
		for _, r := range rules {
			rule, _ := r.(map[string]any)
			if !contains(rule["resources"], "secrets") {
				continue
			}
			verbs, _ := rule["verbs"].([]any)
			if len(verbs) != 1 || verbs[0] != "get" {
				t.Errorf("%s: the rule on Secrets grants %v, want only get", identity(obj), verbs)
			}
		}
	}
}

func contains(list any, want string) bool {
	items, _ := list.([]any)
	for _, i := range items {
		if i == want {
			return true
		}
	}
	return false
}
