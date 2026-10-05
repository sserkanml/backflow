package controller

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	backflowv1alpha1 "github.com/sserkanml/backflow/api/v1alpha1" // <- kendi satırını koru
)

const (
	// How often a working connection is re-verified.
	scmRecheckInterval = 10 * time.Minute
	// How soon a failing connection is retried.
	scmRetryInterval = 1 * time.Minute
	// Timeout for a single call to the provider API.
	scmHTTPTimeout = 15 * time.Second
)

// ScmConnectionReconciler verifies that an ScmConnection's token works
// and reports the result in status.
type ScmConnectionReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=backflow.io,resources=scmconnections,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=backflow.io,resources=scmconnections/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=backflow.io,resources=scmconnections/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

func (r *ScmConnectionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var conn backflowv1alpha1.ScmConnection
	if err := r.Get(ctx, req.NamespacedName, &conn); err != nil {
		// Deleted in the meantime: nothing to do.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	user, checkErr := r.verify(ctx, &conn)

	now := metav1.Now()
	conn.Status.LastCheckTime = &now
	cond := metav1.Condition{
		Type:               "Ready",
		ObservedGeneration: conn.Generation,
	}
	if checkErr != nil {
		cond.Status = metav1.ConditionFalse
		cond.Reason = reasonOf(checkErr)
		cond.Message = checkErr.Error()
		conn.Status.AuthenticatedAs = ""
	} else {
		cond.Status = metav1.ConditionTrue
		cond.Reason = "Authenticated"
		cond.Message = fmt.Sprintf("Authenticated to %s as %s", conn.Spec.URL, user)
		conn.Status.AuthenticatedAs = user
	}
	meta.SetStatusCondition(&conn.Status.Conditions, cond)

	if err := r.Status().Update(ctx, &conn); err != nil {
		return ctrl.Result{}, err
	}

	if checkErr != nil {
		log.Info("SCM connection check failed", "reason", cond.Reason, "error", checkErr.Error())
		return ctrl.Result{RequeueAfter: scmRetryInterval}, nil
	}
	log.Info("SCM connection verified", "user", user)
	return ctrl.Result{RequeueAfter: scmRecheckInterval}, nil
}

// verify reads the token, calls the provider's "current user" endpoint
// and returns the username the token belongs to.
func (r *ScmConnectionReconciler) verify(ctx context.Context, conn *backflowv1alpha1.ScmConnection) (string, error) {
	tokenBytes, err := r.secretValue(ctx, conn.Namespace, conn.Spec.TokenSecretRef)
	if err != nil {
		return "", &checkError{reason: "SecretError", msg: err.Error()}
	}
	token := strings.TrimSpace(string(tokenBytes))

	httpClient, err := r.httpClient(ctx, conn)
	if err != nil {
		return "", &checkError{reason: "SecretError", msg: err.Error()}
	}

	baseURL := strings.TrimRight(conn.Spec.URL, "/")
	switch conn.Spec.Provider {
	case backflowv1alpha1.ScmProviderGitLab:
		var u struct {
			Username string `json:"username"`
		}
		err := getJSON(ctx, httpClient, baseURL+"/api/v4/user",
			map[string]string{"PRIVATE-TOKEN": token}, &u)
		return u.Username, err

	case backflowv1alpha1.ScmProviderGitHub:
		apiURL := "https://api.github.com"
		if baseURL != "https://github.com" {
			// GitHub Enterprise Server
			apiURL = baseURL + "/api/v3"
		}
		var u struct {
			Login string `json:"login"`
		}
		err := getJSON(ctx, httpClient, apiURL+"/user", map[string]string{
			"Authorization": "Bearer " + token,
			"Accept":        "application/vnd.github+json",
		}, &u)
		return u.Login, err

	default:
		return "", &checkError{reason: "UnsupportedProvider", msg: fmt.Sprintf("provider %q is not supported", conn.Spec.Provider)}
	}
}

// secretValue returns one key of a Secret in the given namespace.
func (r *ScmConnectionReconciler) secretValue(ctx context.Context, namespace string, ref corev1.SecretKeySelector) ([]byte, error) {
	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: ref.Name}, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("secret %q not found", ref.Name)
		}
		return nil, err
	}
	value, ok := secret.Data[ref.Key]
	if !ok || len(value) == 0 {
		return nil, fmt.Errorf("secret %q has no key %q", ref.Name, ref.Key)
	}
	return value, nil
}

// httpClient builds an HTTP client, trusting the optional custom CA.
func (r *ScmConnectionReconciler) httpClient(ctx context.Context, conn *backflowv1alpha1.ScmConnection) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()

	if conn.Spec.CASecretRef != nil {
		caPEM, err := r.secretValue(ctx, conn.Namespace, *conn.Spec.CASecretRef)
		if err != nil {
			return nil, fmt.Errorf("reading CA bundle: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("CA bundle in secret %q contains no valid PEM certificates", conn.Spec.CASecretRef.Name)
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}

	return &http.Client{Transport: transport, Timeout: scmHTTPTimeout}, nil
}

// getJSON performs a GET request and decodes the JSON response into out.
func getJSON(ctx context.Context, c *http.Client, url string, headers map[string]string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return &checkError{reason: "InvalidURL", msg: err.Error()}
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := c.Do(req)
	if err != nil {
		return &checkError{reason: "Unreachable", msg: err.Error()}
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return &checkError{reason: "Unauthorized", msg: fmt.Sprintf("token rejected by %s (HTTP %d)", url, resp.StatusCode)}
	case resp.StatusCode >= 300:
		return &checkError{reason: "UnexpectedResponse", msg: fmt.Sprintf("%s returned HTTP %d", url, resp.StatusCode)}
	}

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return &checkError{reason: "UnexpectedResponse", msg: fmt.Sprintf("decoding response from %s: %v", url, err)}
	}
	return nil
}

// checkError carries a machine-readable reason for the Ready condition.
type checkError struct {
	reason string
	msg    string
}

func (e *checkError) Error() string { return e.msg }

func reasonOf(err error) string {
	var ce *checkError
	if errors.As(err, &ce) {
		return ce.reason
	}
	return "CheckFailed"
}

// connectionsForSecret finds ScmConnections that reference a Secret,
// so a token rotation is picked up immediately.
func (r *ScmConnectionReconciler) connectionsForSecret(ctx context.Context, obj client.Object) []reconcile.Request {
	var list backflowv1alpha1.ScmConnectionList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var requests []reconcile.Request
	for _, c := range list.Items {
		usesSecret := c.Spec.TokenSecretRef.Name == obj.GetName() ||
			(c.Spec.CASecretRef != nil && c.Spec.CASecretRef.Name == obj.GetName())
		if usesSecret {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: c.Namespace, Name: c.Name},
			})
		}
	}
	return requests
}

// SetupWithManager sets up the controller with the Manager.
func (r *ScmConnectionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		// Only react to spec changes; our own status updates must not
		// trigger another reconcile (that would loop forever).
		For(&backflowv1alpha1.ScmConnection{},
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.connectionsForSecret)).
		Named("scmconnection").
		Complete(r)
}
