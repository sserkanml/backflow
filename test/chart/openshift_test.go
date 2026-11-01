package chart

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

const (
	caTemplate         = "templates/openshift-ca.yaml"
	deploymentTemplate = "templates/deployment.yaml"
)

// argocdSet are the values of an install on OpenShift GitOps.
var argocdSet = []string{
	"--set", "argocd.server=https://openshift-gitops-server.openshift-gitops.svc",
	"--set", "argocd.tokenSecret.namespace=openshift-gitops",
	"--set", "argocd.tokenSecret.name=backflow-local-user",
}

func managerArgs(t *testing.T, objs []map[string]any) ([]string, map[string]any) {
	t.Helper()
	for _, obj := range objs {
		if obj["kind"] != "Deployment" {
			continue
		}
		spec, _ := obj["spec"].(map[string]any)
		tmpl, _ := spec["template"].(map[string]any)
		pod, _ := tmpl["spec"].(map[string]any)
		containers, _ := pod["containers"].([]any)
		manager, _ := containers[0].(map[string]any)
		var args []string
		for _, a := range manager["args"].([]any) {
			args = append(args, fmt.Sprint(a))
		}
		return args, pod
	}
	t.Fatal("no Deployment")
	return nil, nil
}

func TestOpenShiftCAObjectsAreOffByDefault(t *testing.T) {
	if got := renderTemplates(t, []string{caTemplate}); len(got) != 0 {
		t.Errorf("the default chart renders:\n%s", text(t, got))
	}
	args, _ := managerArgs(t, renderTemplates(t, []string{deploymentTemplate}))
	for _, a := range args {
		if strings.HasPrefix(a, "--argocd-") || strings.HasPrefix(a, "--trusted-ca-file") {
			t.Errorf("the default chart passes %s", a)
		}
	}
}

func TestServiceCAAndTrustedCABundle(t *testing.T) {
	set := append(slices.Clone(argocdSet), "--set", "argocd.serviceCA=true", "--set", "trustedCABundle=true")
	got := renderTemplates(t, []string{caTemplate}, set...)
	golden(t, "openshift-ca.yaml", got)
	if len(got) != 2 {
		t.Fatalf("want the service CA and the trusted CA ConfigMaps:\n%s", text(t, got))
	}
	var service, trusted map[string]any
	for _, obj := range got {
		meta, _ := obj["metadata"].(map[string]any)
		if strings.HasSuffix(fmt.Sprint(meta["name"]), "-service-ca") {
			service = meta
		} else {
			trusted = meta
		}
	}
	if ann, _ := service["annotations"].(map[string]any); ann["service.beta.openshift.io/inject-cabundle"] != "true" {
		t.Errorf("the service CA ConfigMap is not annotated for injection: %v", service)
	}
	if labels, _ := trusted["labels"].(map[string]any); labels["config.openshift.io/inject-trusted-cabundle"] != "true" {
		t.Errorf("the trusted CA ConfigMap is not labelled for injection: %v", trusted)
	}

	args, pod := managerArgs(t, renderTemplates(t, []string{deploymentTemplate}, set...))
	for _, want := range []string{
		"--argocd-server=https://openshift-gitops-server.openshift-gitops.svc",
		"--argocd-token-secret-namespace=openshift-gitops",
		"--argocd-token-secret-name=backflow-local-user",
		"--argocd-token-secret-key=apiToken",
		"--argocd-ca-file=/var/run/argocd-ca/service-ca.crt",
		"--trusted-ca-file=/var/run/trusted-ca/ca-bundle.crt",
	} {
		if !slices.Contains(args, want) {
			t.Errorf("no %s in %v", want, args)
		}
	}
	volumes := fmt.Sprint(pod["volumes"])
	for _, want := range []string{"backflow-service-ca", "backflow-trusted-ca"} {
		if !strings.Contains(volumes, want) {
			t.Errorf("no volume from %s: %s", want, volumes)
		}
	}
}

func TestEachCAOptionStandsAlone(t *testing.T) {
	got := renderTemplates(t, []string{caTemplate}, "--set", "trustedCABundle=true")
	if len(got) != 1 || !strings.HasSuffix(fmt.Sprint(got[0]["metadata"].(map[string]any)["name"]), "-trusted-ca") {
		t.Errorf("only the trusted CA ConfigMap is wanted:\n%s", text(t, got))
	}
	args, _ := managerArgs(t, renderTemplates(t, []string{deploymentTemplate}, "--set", "trustedCABundle=true"))
	if slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "--argocd-") }) {
		t.Errorf("trustedCABundle alone sets Argo CD flags: %v", args)
	}
	// serviceCA is the CA of the Argo CD server: without argocd.server it is refused.
	renderFails(t, deploymentTemplate, "need argocd.server", "--set", "argocd.serviceCA=true")
	renderFails(t, deploymentTemplate, "needs argocd.tokenSecret.namespace", "--set", "argocd.server=https://x")
	renderFails(t, deploymentTemplate, "not both", append(slices.Clone(argocdSet), "--set", "argocd.serviceCA=true", "--set", "argocd.caFile=/x.crt")...)
}

func TestTokenSecretNamespaceGetsARole(t *testing.T) {
	set := append(slices.Clone(argocdSet), "--set", "secretNamespaces={team-a}")
	got := renderTemplates(t, templates, set...)
	roles := map[string]bool{}
	for _, obj := range got {
		if obj["kind"] == "Role" {
			meta, _ := obj["metadata"].(map[string]any)
			roles[fmt.Sprint(meta["namespace"])] = true
		}
	}
	for _, ns := range []string{"team-a", "backflow-system", "openshift-gitops"} {
		if !roles[ns] {
			t.Errorf("no Role to read Secrets in %s: %v", ns, roles)
		}
	}
	checkSecretVerbs(t, got)

	// Without secretNamespaces the ClusterRole already reads Secrets everywhere.
	got = renderTemplates(t, templates, argocdSet...)
	for _, obj := range got {
		if obj["kind"] == "Role" {
			t.Errorf("a Role although secretNamespaces is empty:\n%s", text(t, got))
		}
	}
}
