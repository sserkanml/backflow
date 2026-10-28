package controller

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/sserkanml/backflow/internal/metrics"
)

const (
	// DefaultSecretTTL is how long a Secret that was read stays in memory. It is
	// short, so a rotated token is picked up by the next verification or
	// proposal, and long enough that a burst of reconciles reads it once.
	DefaultSecretTTL = 30 * time.Second

	// reasonSecretForbidden is the reason of a condition when the operator is
	// not allowed to read the Secret an ScmConnection names.
	reasonSecretForbidden = "SecretForbidden"
)

// errListSecrets is returned for every attempt to list Secrets: the operator
// reads the few Secrets it needs by name and has no permission to list them.
var errListSecrets = errors.New("listing Secrets is not supported; Secrets are read by name")

// secretClient is a client.Client that reads Secrets by name straight from the
// API server and never through the manager's cache, so no Secret informer
// exists and the operator needs only "get" on Secrets. Everything else goes to
// the wrapped client. A Secret read is kept in memory for a short TTL; it is
// never logged.
type secretClient struct {
	client.Client
	reader client.Reader
	ttl    time.Duration
	now    func() time.Time

	mu      sync.Mutex
	entries map[types.NamespacedName]secretEntry
}

type secretEntry struct {
	secret *corev1.Secret
	at     time.Time
}

// NewSecretClient wraps c so that Secrets are read by name with reader (the
// manager's uncached API reader), kept for ttl. A ttl of zero or less reads the
// API server every time.
func NewSecretClient(c client.Client, reader client.Reader, ttl time.Duration) client.Client {
	return &secretClient{Client: c, reader: reader, ttl: ttl, now: time.Now, entries: map[types.NamespacedName]secretEntry{}}
}

func (s *secretClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	secret, ok := obj.(*corev1.Secret)
	if !ok {
		return s.Client.Get(ctx, key, obj, opts...)
	}
	if s.ttl > 0 {
		s.mu.Lock()
		e, hit := s.entries[key]
		s.mu.Unlock()
		if hit && s.now().Sub(e.at) < s.ttl {
			e.secret.DeepCopyInto(secret)
			return nil
		}
	}
	err := s.reader.Get(ctx, key, secret, opts...)
	metrics.SecretRead(metrics.SecretResult(err, apierrors.IsForbidden(err), apierrors.IsNotFound(err)))
	if err != nil {
		return err
	}
	if s.ttl > 0 {
		s.mu.Lock()
		s.entries[key] = secretEntry{secret: secret.DeepCopy(), at: s.now()}
		s.mu.Unlock()
	}
	return nil
}

func (s *secretClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*corev1.SecretList); ok {
		return errListSecrets
	}
	if gvk := list.GetObjectKind().GroupVersionKind(); gvk.Group == "" && gvk.Kind == "SecretList" {
		return errListSecrets
	}
	return s.Client.List(ctx, list, opts...)
}

// forget drops the Secrets read from a namespace, so the next read goes to the
// API server. A caller does this when what a Secret held was rejected.
func (s *secretClient) forget(namespace string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.entries {
		if k.Namespace == namespace {
			delete(s.entries, k)
		}
	}
}

// forgetSecrets drops the Secrets kept in memory for a namespace, when c reads
// Secrets through NewSecretClient. Use it after the provider rejected a token:
// it may have been rotated since the Secret was read.
func forgetSecrets(c client.Reader, namespace string) {
	if s, ok := c.(*secretClient); ok {
		s.forget(namespace)
	}
}

// secretReadError turns a Forbidden answer for a Secret into a checkError that
// names the namespace and the chart value that grants access. Other errors are
// returned as they are.
func secretReadError(err error, namespace, name string) error {
	if !apierrors.IsForbidden(err) {
		return err
	}
	return &checkError{reason: reasonSecretForbidden, msg: fmt.Sprintf(
		"the operator is not allowed to read Secret %q in namespace %q; add %q to secretNamespaces in the Helm chart values "+
			"(or grant the operator get on secrets in that namespace)", name, namespace, namespace)}
}

// isSecretForbidden reports whether err came from secretReadError.
func isSecretForbidden(err error) bool {
	ce, ok := errors.AsType[*checkError](err)
	return ok && ce.reason == reasonSecretForbidden
}

// ClientOptions are the manager's client options: Argo CD Applications, which
// are read as unstructured objects, are served from the cache, and Secrets are
// never cached (no informer is started for them).
func ClientOptions() client.Options {
	return client.Options{
		Cache: &client.CacheOptions{
			Unstructured: true,
			DisableFor:   []client.Object{&corev1.Secret{}},
		},
	}
}
