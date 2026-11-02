package controller

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	backflowv1 "github.com/sserkanml/backflow/api/v1"
	"github.com/sserkanml/backflow/internal/argocd"
)

// testCA is a self-signed certificate that is used for nothing but to be valid PEM.
const testCA = `-----BEGIN CERTIFICATE-----
MIIBjDCCATOgAwIBAgIUHePIXlK5C+yf8V0tCQ7Q4dnfSt0wCgYIKoZIzj0EAwIw
GzEZMBcGA1UEAwwQYmFja2Zsb3ctdGVzdC1jYTAgFw0yNjExMDExMjQwNTJaGA8y
MTI2MTAwODEyNDA1MlowGzEZMBcGA1UEAwwQYmFja2Zsb3ctdGVzdC1jYTBZMBMG
ByqGSM49AgEGCCqGSM49AwEHA0IABEUnr3psS9QLptd38zFP7cSTW5FjTRdmeij9
5p/rYRrU8mKz62Emjv6GQTGlMUmP+AV8bHigNTe2qG1HV5cIQ5WjUzBRMB0GA1Ud
DgQWBBTzmt0SZNA9vrIcB93uFBrYp97egTAfBgNVHSMEGDAWgBTzmt0SZNA9vrIc
B93uFBrYp97egTAPBgNVHRMBAf8EBTADAQH/MAoGCCqGSM49BAMCA0cAMEQCICgi
qwJSZ4wyO64WxaED2FKPm/BI9XOEpUqjbwxDoLO8AiBfX5bA0NOkpE0f/vyxgTts
gSiDOimgK8TIn34vaZ83sQ==
-----END CERTIFICATE-----
`

func secretNamed(namespace, name, key, value string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Data:       map[string][]byte{key: []byte(value)},
	}
}

func policyIn(own *backflowv1.ArgoCDServer) *backflowv1.BackflowPolicy {
	return &backflowv1.BackflowPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "p"},
		Spec:       backflowv1.BackflowPolicySpec{ArgoCD: backflowv1.ArgoCDSpec{Server: own}},
	}
}

func operatorDefault() *ArgoCDDefaults {
	return &ArgoCDDefaults{Server: "https://openshift-gitops-server.openshift-gitops.svc",
		TokenSecretNamespace: "openshift-gitops", TokenSecretName: "backflow-local-user"}
}

func TestPolicyWithoutArgoCDUsesTheOperatorDefault(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(
		secretNamed("openshift-gitops", "backflow-local-user", "apiToken", " default-token\n"),
		// A Secret of the same name in the policy's own namespace must not be read.
		secretNamed("team-a", "backflow-local-user", "apiToken", "the-policys-namespace"),
	).Build()
	policy := policyIn(nil)

	cfg, err := argoCDConfig(context.Background(), c, policy, operatorDefault())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.URL != "https://openshift-gitops-server.openshift-gitops.svc" || cfg.Token != "default-token" || cfg.InsecureSkipTLSVerify {
		t.Errorf("config = %+v", cfg)
	}
	dev := operatorDefault()
	dev.InsecureSkipTLSVerify = true
	if cfg, _ = argoCDConfig(context.Background(), c, policy, dev); !cfg.InsecureSkipTLSVerify {
		t.Error("--argocd-insecure-skip-tls-verify is not passed on")
	}
	source, ok := argoCDSource(policy, operatorDefault())
	if !ok || source != backflowv1.ArgoCDSourceOperatorDefault {
		t.Errorf("source = %q, %v", source, ok)
	}
}

func TestPolicyWithOwnSettingsIsUnchanged(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(
		secretNamed("openshift-gitops", "backflow-local-user", "apiToken", "default-token"),
		secretNamed("team-a", "own-token", "token", "own"),
		secretNamed("team-a", "own-ca", "ca.crt", testCA),
	).Build()
	own := &backflowv1.ArgoCDServer{
		URL:            "https://argocd.example",
		TokenSecretRef: backflowv1.SecretKeyReference{Name: "own-token", Key: "token"},
		CASecretRef:    &backflowv1.SecretKeyReference{Name: "own-ca", Key: "ca.crt"},
	}
	policy := policyIn(own)

	cfg, err := argoCDConfig(context.Background(), c, policy, operatorDefault())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.URL != "https://argocd.example" || cfg.Token != "own" || string(cfg.CACert) != testCA {
		t.Errorf("config = %+v", cfg)
	}
	if source, _ := argoCDSource(policy, operatorDefault()); source != backflowv1.ArgoCDSourcePolicy {
		t.Errorf("source = %q", source)
	}
	// And the default does not need to exist for it.
	if _, err := argoCDConfig(context.Background(), c, policy, nil); err != nil {
		t.Errorf("without a default: %v", err)
	}
}

func TestAPolicyCannotPointToAnotherNamespace(t *testing.T) {
	// The only token is in another namespace than the policy's. A policy names a Secret
	// by name only, so the operator looks in the policy's namespace and finds nothing.
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(
		secretNamed("kube-system", "stolen", "token", "secret-of-another-namespace"),
	).Build()
	own := &backflowv1.ArgoCDServer{
		TokenSecretRef: backflowv1.SecretKeyReference{Name: "stolen", Key: "token"},
	}
	_, err := argoCDConfig(context.Background(), c, policyIn(own), operatorDefault())
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v, want the Secret not found in the policy's namespace", err)
	}
	if strings.Contains(err.Error(), "secret-of-another-namespace") {
		t.Error("the error shows a token")
	}
}

func TestNoDefaultNoPolicySettingsIsAnError(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).Build()
	policy := policyIn(nil)
	if _, ok := argoCDSource(policy, nil); ok {
		t.Error("a source without any connection")
	}
	if _, err := argoCDConfig(context.Background(), c, policy, &ArgoCDDefaults{}); err == nil {
		t.Error("no error")
	}
	if st := argoCDStatus(policy, nil); st != nil {
		t.Errorf("status = %+v", st)
	}
}

func TestDefaultErrorsNameTheSecretNeverTheToken(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).Build()
	_, err := argoCDConfig(context.Background(), c, policyIn(nil), operatorDefault())
	if err == nil || !strings.Contains(err.Error(), "operator default, Secret openshift-gitops/backflow-local-user") {
		t.Errorf("err = %v", err)
	}
}

func TestStatusAndMessageSaySourceNotToken(t *testing.T) {
	d := operatorDefault()
	st := argoCDStatus(policyIn(nil), d)
	if st.Source != backflowv1.ArgoCDSourceOperatorDefault || st.URL != d.Server {
		t.Errorf("status = %+v", st)
	}
	if got := describeArgoCD(st); got != "Argo CD API: operator default (https://openshift-gitops-server.openshift-gitops.svc)" {
		t.Errorf("message = %q", got)
	}
	own := argoCDStatus(policyIn(&backflowv1.ArgoCDServer{}), d)
	if own.Source != backflowv1.ArgoCDSourcePolicy || own.URL != defaultArgoCDURL {
		t.Errorf("own = %+v", own)
	}
}

func TestDefaultsValidate(t *testing.T) {
	for name, tt := range map[string]struct {
		d  *ArgoCDDefaults
		ok bool
	}{
		"none":                 {&ArgoCDDefaults{}, true},
		"complete":             {operatorDefault(), true},
		"server without token": {&ArgoCDDefaults{Server: "https://x"}, false},
		"token without server": {&ArgoCDDefaults{TokenSecretNamespace: "a", TokenSecretName: "b"}, false},
		"ca without server":    {&ArgoCDDefaults{CAFile: "/ca.crt"}, false},
	} {
		if err := tt.d.Validate(); (err == nil) != tt.ok {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestTheDefaultCAFileIsReadAgainWhenItChanges(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ca.crt")
	write := func(content string, mod time.Time) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
	base := time.Now()
	write(testCA, base)
	now := base
	b := newFileBundle(path)
	b.now = func() time.Time { return now }

	first, err := b.get()
	if err != nil || string(first) != testCA {
		t.Fatalf("first = %q, %v", first, err)
	}
	// Changed on disk, but looked at within the TTL: the old content.
	changed := strings.Replace(testCA, "MIIBjz", "MIIBjy", 1)
	write(changed, base.Add(time.Minute))
	now = base.Add(time.Second)
	if got, _ := b.get(); string(got) != testCA {
		t.Error("the file was read again within the TTL")
	}
	// After the TTL the new content, without a restart.
	now = base.Add(DefaultFileTTL + time.Second)
	if got, err := b.get(); err != nil || string(got) != changed {
		t.Errorf("after the TTL: %v", err)
	}
	// Removed or emptied: an error, never the old CA.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	now = base.Add(3 * DefaultFileTTL)
	if _, err := b.get(); err == nil {
		t.Error("a removed file still serves the old content")
	}
	write("not a certificate", base.Add(time.Hour))
	now = base.Add(5 * DefaultFileTTL)
	if _, err := b.get(); err == nil {
		t.Error("a file without a certificate is accepted")
	}
}

func TestDefaultConfigCarriesTheCAFileAndRebuildsTheClientWhenItChanges(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "service-ca.crt")
	if err := os.WriteFile(path, []byte(testCA), 0o600); err != nil {
		t.Fatal(err)
	}
	d := operatorDefault()
	d.CAFile = path
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(
		secretNamed("openshift-gitops", "backflow-local-user", "apiToken", "t"),
	).Build()
	cfg, err := argoCDConfig(context.Background(), c, policyIn(nil), d)
	if err != nil || string(cfg.CACert) != testCA {
		t.Fatalf("cfg = %+v, %v", cfg, err)
	}

	var cache argoClientCache[string]
	builds := 0
	build := func(argocd.Config) (string, error) { builds++; return "client", nil }
	_, _ = cache.get("ns/p", cfg, build)
	_, _ = cache.get("ns/p", cfg, build)
	cfg.CACert = []byte("another bundle")
	_, _ = cache.get("ns/p", cfg, build)
	if builds != 2 {
		t.Errorf("builds = %d, want 2: one for the first bundle, one after it changed", builds)
	}
}

// TestRenewedTokenIsUsedByTheNextCall is what OpenShift GitOps does every few weeks:
// it writes a new token into the Secret. The Secret is kept for a short TTL; after the
// token was refused the copy is dropped and the next call reads the new one.
func TestRenewedTokenIsUsedByTheNextCall(t *testing.T) {
	ctx := context.Background()
	kube := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(
		secretNamed("openshift-gitops", "backflow-local-user", "apiToken", "old-token"),
	).Build()
	now := time.Now()
	sc := NewSecretClient(kube, kube, 30*time.Second).(*secretClient)
	sc.now = func() time.Time { return now }
	policy := policyIn(nil)
	d := operatorDefault()

	cfg, err := argoCDConfig(ctx, sc, policy, d)
	if err != nil || cfg.Token != "old-token" {
		t.Fatalf("cfg = %+v, %v", cfg, err)
	}
	renewed := secretNamed("openshift-gitops", "backflow-local-user", "apiToken", "new-token")
	var current corev1.Secret
	if err := kube.Get(ctx, client.ObjectKeyFromObject(renewed), &current); err != nil {
		t.Fatal(err)
	}
	current.Data["apiToken"] = []byte("new-token")
	if err := kube.Update(ctx, &current); err != nil {
		t.Fatal(err)
	}

	// Within the TTL the copy in memory answers.
	if cfg, _ = argoCDConfig(ctx, sc, policy, d); cfg.Token != "old-token" {
		t.Errorf("token within the TTL = %q", cfg.Token)
	}
	// Argo CD refused it: the copy is dropped, and the next call has the new token.
	forgetArgoCDSecrets(sc, policy, d)
	if cfg, _ = argoCDConfig(ctx, sc, policy, d); cfg.Token != "new-token" {
		t.Errorf("token after it was refused = %q", cfg.Token)
	}
	// The token is part of what a client is built from, so the new one gets a client.
	var cache argoClientCache[string]
	builds := 0
	build := func(argocd.Config) (string, error) { builds++; return "c", nil }
	_, _ = cache.get("ns/p", argocd.Config{URL: "u", Token: "old-token"}, build)
	_, _ = cache.get("ns/p", argocd.Config{URL: "u", Token: "new-token"}, build)
	if builds != 2 {
		t.Errorf("builds = %d", builds)
	}
	// And after the TTL alone, with nothing refused, the new token is read too.
	current.Data["apiToken"] = []byte("newer-token")
	if err := kube.Update(ctx, &current); err != nil {
		t.Fatal(err)
	}
	now = now.Add(31 * time.Second)
	if cfg, _ = argoCDConfig(ctx, sc, policy, d); cfg.Token != "newer-token" {
		t.Errorf("token after the TTL = %q", cfg.Token)
	}
}

// renewalArgo answers like an Argo CD that accepts only one token.
type renewalArgo struct {
	accepts string
	token   string
	calls   *[]string
}

func (f renewalArgo) GetManagedResources(context.Context, string, string) ([]argocd.ManagedResource, error) {
	*f.calls = append(*f.calls, f.token)
	if f.token != f.accepts {
		return nil, argocd.ErrUnauthorized
	}
	return nil, nil
}

// TestDriftDetectionRetriesOnceWithTheRenewedToken: the client of the run holds the old
// token and Argo CD refuses it; the run builds a client from the Secret as it is now.
func TestDriftDetectionRetriesOnceWithTheRenewedToken(t *testing.T) {
	ctx := context.Background()
	kube := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(
		secretNamed("openshift-gitops", "backflow-local-user", "apiToken", "new-token"),
	).Build()
	sc := NewSecretClient(kube, kube, time.Minute)
	var calls []string
	rec := events.NewFakeRecorder(10)
	r := &DriftReconciler{
		Client: sc, ArgoCD: operatorDefault(), Recorder: rec,
		NewArgoClient: func(cfg argocd.Config) (ManagedResourcesGetter, error) {
			return renewalArgo{accepts: "new-token", token: cfg.Token, calls: &calls}, nil
		},
	}
	policy := policyIn(nil)
	run := &appRun{
		policy: policy, summary: backflowv1.ApplicationSummary{Name: "app"},
		argo:       renewalArgo{accepts: "new-token", token: "old-token", calls: &calls},
		candidates: []appResource{{Kind: "ConfigMap", Name: "c", Namespace: "demo", Status: syncOutOfSync}},
		drifting:   map[string][]backflowv1.FieldChange{}, deleted: map[string]appResource{}, apiOK: true,
	}
	if err := r.detectDrift(ctx, run); err != nil {
		t.Fatal(err)
	}
	if !run.apiOK {
		t.Error("the run failed although the renewed token works")
	}
	if got := strings.Join(calls, ","); got != "old-token,new-token" {
		t.Errorf("calls = %s, want the old token once and then the renewed one", got)
	}
	select {
	case e := <-rec.Events:
		t.Errorf("an Event was emitted: %s", e)
	default:
	}

	// A token that is really refused is reported, after one retry. (The Secret changes, so
	// that the run does not get the client that was built for the token above.)
	var current corev1.Secret
	if err := kube.Get(ctx, client.ObjectKey{Namespace: "openshift-gitops", Name: "backflow-local-user"}, &current); err != nil {
		t.Fatal(err)
	}
	current.Data["apiToken"] = []byte("refused-token")
	if err := kube.Update(ctx, &current); err != nil {
		t.Fatal(err)
	}
	forgetArgoCDSecrets(sc, policy, r.ArgoCD)
	calls = nil
	r.NewArgoClient = func(cfg argocd.Config) (ManagedResourcesGetter, error) {
		return renewalArgo{accepts: "nothing", token: cfg.Token, calls: &calls}, nil
	}
	run.argo = renewalArgo{accepts: "nothing", token: "old-token", calls: &calls}
	run.apiOK = true
	if err := r.detectDrift(ctx, run); err != nil {
		t.Fatal(err)
	}
	if run.apiOK || len(calls) != 2 {
		t.Errorf("apiOK = %v, calls = %v", run.apiOK, calls)
	}
	select {
	case e := <-rec.Events:
		if !strings.Contains(e, "ArgoCDUnauthorized") || strings.Contains(e, "new-token") {
			t.Errorf("event = %s", e)
		}
	default:
		t.Error("no Event for a token Argo CD refuses")
	}
}

func TestTrustedCAIsUsedByConnectionsWithoutTheirOwn(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "ca-bundle.crt")
	if err := os.WriteFile(path, []byte(testCA), 0o600); err != nil {
		t.Fatal(err)
	}
	connection := func(name string, ca *backflowv1.SecretKeyReference) *backflowv1.ScmConnection {
		return &backflowv1.ScmConnection{
			ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: name},
			Spec: backflowv1.ScmConnectionSpec{
				Provider: backflowv1.ScmProviderGitLab, URL: "https://gitlab.example.com", CASecretRef: ca,
				TokenSecretRef: backflowv1.SecretKeyReference{Name: "tok", Key: "token"},
			},
		}
	}
	own := &backflowv1.SecretKeyReference{Name: "own-ca", Key: "ca.crt"}
	sch := runtime.NewScheme()
	if err := corev1.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	if err := backflowv1.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(
		secretNamed("team-a", "tok", "token", "t"),
		secretNamed("team-a", "own-ca", "ca.crt", "own bundle"),
		connection("plain", nil), connection("with-ca", own),
	).Build()

	UseTrustedCAFile("")
	acc, err := scmAccessByName(ctx, c, "team-a", "plain")
	if err != nil || acc.ca != nil {
		t.Fatalf("without a trusted bundle: ca = %q, %v", acc.ca, err)
	}

	UseTrustedCAFile(path)
	t.Cleanup(func() { UseTrustedCAFile("") })
	acc, err = scmAccessByName(ctx, c, "team-a", "plain")
	if err != nil || string(acc.ca) != testCA || string(acc.auth.CABundle) != testCA {
		t.Fatalf("with a trusted bundle: %v", err)
	}
	acc, err = scmAccessByName(ctx, c, "team-a", "with-ca")
	if err != nil || string(acc.ca) != "own bundle" {
		t.Errorf("a connection with its own CA keeps it: ca = %q, %v", acc.ca, err)
	}

	r := &ScmConnectionReconciler{Client: c}
	hc, err := r.httpClient(ctx, connection("plain", nil))
	if err != nil || hc.Transport.(*http.Transport).TLSClientConfig == nil {
		t.Errorf("the check of a connection does not trust the bundle: %v", err)
	}
	UseTrustedCAFile(filepath.Join(dir, "missing.crt"))
	if _, err := r.httpClient(ctx, connection("plain", nil)); err == nil {
		t.Error("an unreadable trusted bundle is ignored")
	}
}
