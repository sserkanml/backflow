package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	backflowv1alpha1 "github.com/sserkanml/backflow/api/v1alpha1"
	"github.com/sserkanml/backflow/internal/argocd"
)

const defaultArgoCDURL = "https://argocd-server.argocd.svc"

// secretKey returns one key of a Secret in the given namespace.
func secretKey(ctx context.Context, c client.Reader, namespace string, ref corev1.SecretKeySelector) ([]byte, error) {
	var secret corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: ref.Name}, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("secret %q not found", ref.Name)
		}
		return nil, secretReadError(err, namespace, ref.Name)
	}
	value, ok := secret.Data[ref.Key]
	if !ok || len(value) == 0 {
		return nil, fmt.Errorf("secret %q has no key %q", ref.Name, ref.Key)
	}
	return value, nil
}

// argoCDConfig builds how to reach Argo CD from the policy's spec.argoCD and
// the Secrets it names. The drift controller (managed resources) and the
// proposal controller (rendering) both use it, so they talk to Argo CD with
// the same URL, token and TLS settings.
func argoCDConfig(ctx context.Context, c client.Reader, policy *backflowv1alpha1.BackflowPolicy) (argocd.Config, error) {
	spec := policy.Spec.ArgoCD
	token, err := secretKey(ctx, c, policy.Namespace, spec.TokenSecretRef)
	if err != nil {
		return argocd.Config{}, fmt.Errorf("token for Argo CD: %w", err)
	}
	cfg := argocd.Config{
		URL:                   spec.URL,
		Token:                 strings.TrimSpace(string(token)),
		InsecureSkipTLSVerify: spec.InsecureSkipTLSVerify,
	}
	if cfg.URL == "" {
		cfg.URL = defaultArgoCDURL
	}
	if spec.CASecretRef != nil {
		if cfg.CACert, err = secretKey(ctx, c, policy.Namespace, *spec.CASecretRef); err != nil {
			return argocd.Config{}, fmt.Errorf("CA bundle for Argo CD: %w", err)
		}
	}
	return cfg, nil
}

// cachedArgoClient is the Argo CD client of a policy and what it was built from.
type cachedArgoClient[T any] struct {
	key    string
	client T
}

// argoClientCache keeps one Argo CD client per policy, so connections are
// reused. The zero value is ready to use.
type argoClientCache[T any] struct {
	mu      sync.Mutex
	clients map[string]cachedArgoClient[T]
}

// get returns the policy's client, built again only when what it is built
// from changed (URL, CA bundle, TLS option or token). A replaced client has
// its idle connections closed.
func (a *argoClientCache[T]) get(pol string, cfg argocd.Config, build func(argocd.Config) (T, error)) (T, error) {
	h := sha256.New()
	for _, part := range [][]byte{[]byte(cfg.URL), cfg.CACert, []byte(strconv.FormatBool(cfg.InsecureSkipTLSVerify)), []byte(cfg.Token)} {
		_, _ = fmt.Fprintf(h, "%d:", len(part))
		_, _ = h.Write(part)
	}
	key := hex.EncodeToString(h.Sum(nil))

	a.mu.Lock()
	defer a.mu.Unlock()
	if hit, ok := a.clients[pol]; ok && hit.key == key {
		return hit.client, nil
	}
	c, err := build(cfg)
	if err != nil {
		var zero T
		return zero, err
	}
	if old, ok := a.clients[pol]; ok {
		closeIdle(old.client)
	}
	if a.clients == nil {
		a.clients = map[string]cachedArgoClient[T]{}
	}
	a.clients[pol] = cachedArgoClient[T]{key: key, client: c}
	return c, nil
}

// forget closes and drops the policy's client.
func (a *argoClientCache[T]) forget(pol string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if old, ok := a.clients[pol]; ok {
		closeIdle(old.client)
		delete(a.clients, pol)
	}
}

// size is the number of clients kept.
func (a *argoClientCache[T]) size() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.clients)
}

func closeIdle(c any) {
	if cl, ok := c.(interface{ CloseIdleConnections() }); ok {
		cl.CloseIdleConnections()
	}
}
