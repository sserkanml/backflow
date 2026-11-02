//nolint:lll // tables of helm arguments read better on one line
package chart

import (
	"encoding/base64"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/sserkanml/backflow/internal/actorwebhook"
)

// actorTemplates are the templates of the actor webhook.
var actorTemplates = []string{
	"templates/actor-webhook-config.yaml", "templates/actor-webhook-service.yaml",
	"templates/actor-webhook-networkpolicy.yaml", "templates/actor-webhook-role.yaml",
}

// fixed makes the render deterministic: the path, the TLS Secret and the CA are given.
var fixed = []string{
	"--set", "actorWebhook.enabled=true",
	"--set", "actorWebhook.path=/observe/fixed-token",
	"--set", "actorWebhook.tls.mode=secret", "--set", "actorWebhook.tls.secretName=webhook-tls",
	"--set", "actorWebhook.tls.caBundle=PEM-OF-THE-CA",
	"--set", "actorWebhook.networkPolicy.apiserverCIDRs={10.0.0.4/32,10.0.0.5/32}",
}

func with(extra ...string) []string { return append(append([]string{}, fixed...), extra...) }

// renderOnce renders the templates in one run of helm, so that what is random (a certificate)
// is the same in all of them.
func renderOnce(t *testing.T, templates []string, args ...string) []map[string]any {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("the helm binary is not installed")
	}
	cmd := make([]string, 0, 5+2*len(templates))
	cmd = append(cmd, "template", "backflow", "../../charts/backflow", "--namespace", "backflow-system")
	for _, tpl := range templates {
		cmd = append(cmd, "--show-only", tpl)
	}
	out, err := exec.Command("helm", append(cmd, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("helm %s: %v\n%s", strings.Join(cmd, " "), err, out)
	}
	return normalise(t, string(out))
}

func find(objs []map[string]any, kind string) map[string]any {
	for _, o := range objs {
		if o["kind"] == kind {
			return o
		}
	}
	return nil
}

func TestActorWebhookIsOffByDefault(t *testing.T) {
	if got := renderTemplates(t, actorTemplates); len(got) != 0 {
		t.Errorf("the default chart renders %d object(s) of the actor webhook:\n%s", len(got), text(t, got))
	}
	args := strings.Join(operatorArgs(t), " ")
	for _, flag := range []string{"--actor-webhook", "--webhook-cert-path", "--webhook-port"} {
		if strings.Contains(args, flag) {
			t.Errorf("the default Deployment has %s", flag)
		}
	}
}

func TestActorWebhookSnapshot(t *testing.T) {
	got := renderTemplates(t, actorTemplates, fixed...)
	golden(t, "actor-webhook.yaml", got)

	cfg := find(got, "ValidatingWebhookConfiguration")
	if cfg == nil {
		t.Fatal("no webhook configuration")
	}
	hook := cfg["webhooks"].([]any)[0].(map[string]any)
	for field, want := range map[string]any{"failurePolicy": "Ignore", "sideEffects": "None", "timeoutSeconds": float64(1)} {
		if hook[field] != want {
			t.Errorf("%s = %v, want %v: the webhook must never block the cluster", field, hook[field], want)
		}
	}
	rule := hook["rules"].([]any)[0].(map[string]any)
	if !reflect.DeepEqual(rule["operations"], []any{"UPDATE"}) || rule["scope"] != "Namespaced" ||
		!reflect.DeepEqual(rule["resources"], []any{"*", "*/scale"}) {
		t.Errorf("rules: %v", rule)
	}
}

func TestActorWebhookMatchConditionsAreTheOnesTheCodeIsTestedWith(t *testing.T) {
	got := renderTemplates(t, actorTemplates, with("--set", "actorWebhook.excludeUsers={system:serviceaccount:argocd:argocd-application-controller,system:serviceaccount:team:bot}")...)
	hook := find(got, "ValidatingWebhookConfiguration")["webhooks"].([]any)[0].(map[string]any)
	raw := hook["matchConditions"].([]any)
	conds := make([]actorwebhook.Condition, 0, len(raw))
	for _, c := range raw {
		m := c.(map[string]any)
		conds = append(conds, actorwebhook.Condition{Name: m["name"].(string), Expression: m["expression"].(string)})
	}
	want := actorwebhook.Conditions([]string{
		"system:serviceaccount:argocd:argocd-application-controller", "system:serviceaccount:team:bot",
		"system:serviceaccount:backflow-system:backflow-controller-manager",
	})
	if !reflect.DeepEqual(conds, want) {
		t.Errorf("the chart's conditions differ from internal/actorwebhook (run make helm-sync):\n got %v\nwant %v", conds, want)
	}
	for _, c := range conds {
		if strings.Contains(c.Expression, "subResource == ''") {
			t.Errorf("%s compares a possibly absent field to ''", c.Name)
		}
	}
}

func TestActorWebhookNamespacesAreExcluded(t *testing.T) {
	got := renderTemplates(t, actorTemplates, with("--set", "actorWebhook.excludeNamespaces={kube-system,argocd}")...)
	hook := find(got, "ValidatingWebhookConfiguration")["webhooks"].([]any)[0].(map[string]any)
	sel := hook["namespaceSelector"].(map[string]any)["matchExpressions"].([]any)[0].(map[string]any)
	if sel["operator"] != "NotIn" || !reflect.DeepEqual(sel["values"], []any{"backflow-system", "kube-system", "argocd"}) {
		t.Errorf("namespaceSelector: %v", sel)
	}
}

func TestActorWebhookTLSModes(t *testing.T) {
	t.Run("OpenShift service CA, chosen by auto", func(t *testing.T) {
		got := renderTemplates(t, append(actorTemplates, "templates/actor-webhook-tls.yaml"),
			"--set", "actorWebhook.enabled=true", "--set", "actorWebhook.path=/observe/x", "--api-versions", "route.openshift.io/v1")
		golden(t, "actor-webhook-openshift.yaml", got)
		svc := find(got, "Service")["metadata"].(map[string]any)["annotations"].(map[string]any)
		cfg := find(got, "ValidatingWebhookConfiguration")["metadata"].(map[string]any)["annotations"].(map[string]any)
		if svc["service.beta.openshift.io/serving-cert-secret-name"] != "backflow-controller-manager-actor-webhook-tls" ||
			cfg["service.beta.openshift.io/inject-cabundle"] != "true" {
			t.Errorf("annotations: %v %v", svc, cfg)
		}
		hook := find(got, "ValidatingWebhookConfiguration")["webhooks"].([]any)[0].(map[string]any)
		if _, set := hook["clientConfig"].(map[string]any)["caBundle"]; set {
			t.Error("the service CA injects the bundle; the chart sets none")
		}
		np := find(got, "NetworkPolicy")["spec"].(map[string]any)["ingress"].([]any)[0].(map[string]any)["from"].([]any)[0].(map[string]any)
		sel := np["namespaceSelector"].(map[string]any)["matchLabels"].(map[string]any)
		if _, ok := sel["policy-group.network.openshift.io/host-network"]; !ok {
			t.Errorf("OpenShift admits the API server through the host-network policy group, got %v", np)
		}
	})
	t.Run("cert-manager, chosen by auto when its API is there", func(t *testing.T) {
		got := renderTemplates(t, append(actorTemplates, "templates/actor-webhook-tls.yaml"),
			with("--set", "actorWebhook.tls.mode=auto", "--api-versions", "cert-manager.io/v1")...)
		golden(t, "actor-webhook-certmanager.yaml", got)
		if find(got, "Issuer") == nil || find(got, "Certificate") == nil {
			t.Fatalf("no Issuer and Certificate:\n%s", text(t, got))
		}
		ann := find(got, "ValidatingWebhookConfiguration")["metadata"].(map[string]any)["annotations"].(map[string]any)
		if ann["cert-manager.io/inject-ca-from"] != "backflow-system/backflow-controller-manager-actor-webhook" {
			t.Errorf("annotations: %v", ann)
		}
	})
	t.Run("cert-manager with an issuer of its own", func(t *testing.T) {
		got := renderTemplates(t, []string{"templates/actor-webhook-tls.yaml"},
			with("--set", "actorWebhook.tls.mode=certManager", "--set", "actorWebhook.tls.issuerRef.name=corp", "--set", "actorWebhook.tls.issuerRef.kind=ClusterIssuer")...)
		if find(got, "Issuer") != nil || find(got, "Certificate") == nil {
			t.Fatalf("expected a Certificate and no Issuer:\n%s", text(t, got))
		}
	})
	t.Run("a certificate made by Helm", func(t *testing.T) {
		got := renderOnce(t, append(actorTemplates, "templates/actor-webhook-tls.yaml"),
			"--set", "actorWebhook.enabled=true", "--set", "actorWebhook.path=/observe/x", "--set", "actorWebhook.tls.mode=helm",
			"--set", "actorWebhook.networkPolicy.apiserverCIDRs={10.0.0.4/32}")
		secret := find(got, "Secret")["data"].(map[string]any)
		hook := find(got, "ValidatingWebhookConfiguration")["webhooks"].([]any)[0].(map[string]any)
		bundle := hook["clientConfig"].(map[string]any)["caBundle"]
		if bundle == nil || bundle != secret["ca.crt"] {
			t.Errorf("the bundle of the configuration must be the CA of the Secret (one certificate per render)")
		}
		for _, k := range []string{"ca.crt", "tls.crt", "tls.key"} {
			raw, err := base64.StdEncoding.DecodeString(secret[k].(string))
			if err != nil || !strings.Contains(string(raw), "-----BEGIN") {
				t.Errorf("%s is not PEM", k)
			}
		}
	})
	t.Run("a Secret of your own", func(t *testing.T) {
		got := renderTemplates(t, actorTemplates, fixed...)
		hook := find(got, "ValidatingWebhookConfiguration")["webhooks"].([]any)[0].(map[string]any)
		if hook["clientConfig"].(map[string]any)["caBundle"] != base64.StdEncoding.EncodeToString([]byte("PEM-OF-THE-CA")) {
			t.Errorf("caBundle: %v", hook["clientConfig"])
		}
	})
}

func TestActorWebhookNetworkPolicyAllowsOnlyTheAPIServer(t *testing.T) {
	got := renderTemplates(t, actorTemplates, fixed...)
	np := find(got, "NetworkPolicy")["spec"].(map[string]any)
	rule := np["ingress"].([]any)[0].(map[string]any)
	if len(rule["from"].([]any)) != 2 || rule["ports"].([]any)[0].(map[string]any)["port"] != float64(9443) {
		t.Errorf("ingress: %v", rule)
	}
	off := renderTemplates(t, actorTemplates, with("--set", "actorWebhook.networkPolicy.enabled=false")...)
	if find(off, "NetworkPolicy") != nil {
		t.Error("networkPolicy.enabled=false renders no policy")
	}
}

func TestActorWebhookRoleIsInTheReleaseNamespace(t *testing.T) {
	got := renderTemplates(t, append(actorTemplates, "templates/clusterrole.yaml"), fixed...)
	role := find(got, "Role")
	if role == nil || role["metadata"].(map[string]any)["namespace"] != "backflow-system" {
		t.Fatalf("no Role in the release namespace:\n%s", text(t, got))
	}
	if strings.Contains(text(t, []map[string]any{find(got, "ClusterRole")}), "changerecords") {
		t.Error("the ClusterRole must not grant changerecords: they live in one namespace")
	}
}

func TestActorWebhookDeploymentFlags(t *testing.T) {
	args := operatorArgs(t, with("--set", "actorWebhook.recordUsername=false", "--set", "actorWebhook.retention=1h",
		"--set", "actorWebhook.usernameInMergeRequest=true")...)
	for _, want := range []string{
		"--actor-webhook", "--actor-webhook-path=/observe/fixed-token", "--webhook-port=9443",
		"--webhook-cert-path=/var/run/actor-webhook-cert", "--operator-namespace=backflow-system", "--actor-retention=1h",
		"--actor-record-username=false", "--username-in-merge-request=true", "--actor-tolerance=10s",
		"--actor-flush-interval=2s", "--actor-max-records=5000",
	} {
		found := false
		for _, a := range args {
			found = found || a == want
		}
		if !found {
			t.Errorf("flag %s missing from %v", want, args)
		}
	}
}

func TestActorWebhookRefusesWhatWouldBeUnsafeOrUseless(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("the helm binary is not installed")
	}
	base := []string{"template", "backflow", "../../charts/backflow", "--set", "actorWebhook.enabled=true", "--set", "actorWebhook.path=/x",
		"--set", "actorWebhook.tls.mode=helm"}
	tests := []struct {
		name  string
		extra []string
		want  string
	}{
		{"no API server address outside OpenShift", nil, "apiserverCIDRs"},
		{"a timeout of 5 seconds", []string{"--set", "actorWebhook.networkPolicy.enabled=false", "--set", "actorWebhook.timeoutSeconds=5"}, "timeoutSeconds must be 1 or 2"},
		{"Kubernetes before 1.30, the minimum of the chart", []string{"--set", "actorWebhook.networkPolicy.enabled=false", "--kube-version", "1.29.0"}, "1.30"},
		{"a TLS mode that does not exist", []string{"--set", "actorWebhook.networkPolicy.enabled=false", "--set", "actorWebhook.tls.mode=magic"}, "tls.mode"},
		{"mode secret without a Secret", []string{"--set", "actorWebhook.networkPolicy.enabled=false", "--set", "actorWebhook.tls.mode=secret"}, "secretName"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := exec.Command("helm", append(append([]string{}, base...), tt.extra...)...).CombinedOutput()
			if err == nil || !strings.Contains(string(out), tt.want) {
				t.Errorf("want a failure mentioning %q, got %v\n%s", tt.want, err, out)
			}
		})
	}
}
