package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	backflowv1alpha1 "github.com/sserkanml/backflow/api/v1alpha1"
	"github.com/sserkanml/backflow/internal/argocd"
)

// DefaultArgoCDTokenKey is the key of the token in the Secret OpenShift GitOps
// writes for a local user (<name>-local-user).
const DefaultArgoCDTokenKey = "apiToken"

// ArgoCDDefaults is the Argo CD connection the installer gave the operator, for the
// policies that have no spec.argoCD of their own. It is set by flags and by nothing
// else: a policy cannot choose the namespace of the Secret, so a user who may create
// policies cannot make the operator read a Secret anywhere else.
type ArgoCDDefaults struct {
	// Server is the URL of the Argo CD API server.
	Server string
	// TokenSecretNamespace, TokenSecretName and TokenSecretKey name the Secret that
	// holds the API token. The key defaults to apiToken.
	TokenSecretNamespace string
	TokenSecretName      string
	TokenSecretKey       string
	// CAFile is a PEM bundle that verifies the server, read again when it changes.
	CAFile string
	// InsecureSkipTLSVerify turns the verification of the server off. For a kind cluster
	// in development, behind a port-forward; the chart has no value for it.
	InsecureSkipTLSVerify bool
	// Access says which Applications the policies of which namespaces may match
	// through this connection (see AccessRule). Empty: only the policies of
	// OperatorNamespace, for every project.
	Access AccessRules
	// OperatorNamespace is the namespace the operator runs in.
	OperatorNamespace string

	once sync.Once
	ca   *fileBundle
}

// Validate says what is missing, for a start-up error. A zero value is valid: no default.
func (d *ArgoCDDefaults) Validate() error {
	if d == nil {
		return nil
	}
	hasToken := d.TokenSecretNamespace != "" || d.TokenSecretName != ""
	switch {
	case d.Server == "" && (hasToken || d.CAFile != ""):
		return errors.New("--argocd-token-secret-* and --argocd-ca-file need --argocd-server")
	case d.Server != "" && (d.TokenSecretNamespace == "" || d.TokenSecretName == ""):
		return errors.New("--argocd-server needs --argocd-token-secret-namespace and --argocd-token-secret-name")
	}
	return nil
}

// Configured reports whether there is a default.
func (d *ArgoCDDefaults) Configured() bool {
	return d != nil && d.Server != "" && d.TokenSecretNamespace != "" && d.TokenSecretName != ""
}

func (d *ArgoCDDefaults) tokenRef() corev1.SecretKeySelector {
	key := d.TokenSecretKey
	if key == "" {
		key = DefaultArgoCDTokenKey
	}
	return corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: d.TokenSecretName}, Key: key}
}

func (d *ArgoCDDefaults) caBundle() ([]byte, error) {
	d.once.Do(func() {
		if d.CAFile != "" {
			d.ca = newFileBundle(d.CAFile)
		}
	})
	if d.ca == nil {
		return nil, nil
	}
	return d.ca.get()
}

// secretNamespace is where the token of a policy is read: its own namespace when it
// has spec.argoCD, else the namespace the installer named.
func argoSecretNamespace(policy *backflowv1alpha1.BackflowPolicy, d *ArgoCDDefaults) string {
	if policy.Spec.ArgoCD == nil && d.Configured() {
		return d.TokenSecretNamespace
	}
	return policy.Namespace
}

// argoCDSource says which connection a policy uses: its own, the operator's default,
// or none (ok false), in which case drift detection is off for it.
func argoCDSource(policy *backflowv1alpha1.BackflowPolicy, d *ArgoCDDefaults) (backflowv1alpha1.ArgoCDSource, bool) {
	switch {
	case policy.Spec.ArgoCD != nil:
		return backflowv1alpha1.ArgoCDSourcePolicy, true
	case d.Configured():
		return backflowv1alpha1.ArgoCDSourceOperatorDefault, true
	}
	return "", false
}

// argoCDStatus is status.argoCD of a policy.
func argoCDStatus(policy *backflowv1alpha1.BackflowPolicy, d *ArgoCDDefaults) *backflowv1alpha1.ArgoCDStatus {
	source, ok := argoCDSource(policy, d)
	if !ok {
		return nil
	}
	if source == backflowv1alpha1.ArgoCDSourcePolicy {
		url := policy.Spec.ArgoCD.URL
		if url == "" {
			url = defaultArgoCDURL
		}
		return &backflowv1alpha1.ArgoCDStatus{Source: source, URL: url}
	}
	return &backflowv1alpha1.ArgoCDStatus{Source: source, URL: d.Server}
}

// describeArgoCD is the sentence that goes into a condition message: which
// connection, never a token.
func describeArgoCD(st *backflowv1alpha1.ArgoCDStatus) string {
	if st == nil {
		return ""
	}
	if st.Source == backflowv1alpha1.ArgoCDSourceOperatorDefault {
		return fmt.Sprintf("Argo CD API: operator default (%s)", st.URL)
	}
	return fmt.Sprintf("Argo CD API: from the policy (%s)", st.URL)
}

// defaultArgoCDConfig builds the connection from the operator's defaults.
func defaultArgoCDConfig(ctx context.Context, c client.Reader, d *ArgoCDDefaults) (argocd.Config, error) {
	ref := d.tokenRef()
	token, err := secretKey(ctx, c, d.TokenSecretNamespace, ref)
	if err != nil {
		return argocd.Config{}, fmt.Errorf("token for Argo CD (operator default, Secret %s/%s): %w", d.TokenSecretNamespace, ref.Name, err)
	}
	cfg := argocd.Config{URL: d.Server, Token: strings.TrimSpace(string(token)), InsecureSkipTLSVerify: d.InsecureSkipTLSVerify}
	if cfg.CACert, err = d.caBundle(); err != nil {
		return argocd.Config{}, fmt.Errorf("CA bundle for Argo CD (operator default): %w", err)
	}
	return cfg, nil
}
