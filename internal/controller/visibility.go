package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	backflowv1alpha1 "github.com/sserkanml/backflow/api/v1alpha1"
	"github.com/sserkanml/backflow/internal/argocd"
)

// DefaultVisibilityTTL is how long the answer to "may this token get this
// Application" is remembered.
const DefaultVisibilityTTL = time.Minute

// ApplicationGetter is the part of the Argo CD client that tells whether a token may
// get an Application.
type ApplicationGetter interface {
	GetApplication(ctx context.Context, appName, appNamespace string) error
}

// ApplicationVisibility answers whether the token of a policy that has its own
// spec.argoCD may see an Application, by asking Argo CD with that token: the
// operator reads Applications cluster-wide from its cache, so without this a policy
// with a token of its own could list Applications Argo CD would not show it. The
// answer is kept for a short TTL per token and Application. The token is only part
// of a hash key.
type ApplicationVisibility struct {
	// TTL is how long an answer is kept. Zero means DefaultVisibilityTTL.
	TTL time.Duration
	// New builds the Argo CD client. Defaults to argocd.New.
	New func(argocd.Config) (ApplicationGetter, error)
	// Now returns the current time. Defaults to time.Now.
	Now func() time.Time

	mu      sync.Mutex
	entries map[string]visibilityEntry
	clients argoClientCache[ApplicationGetter]
}

type visibilityEntry struct {
	visible bool
	at      time.Time
}

// Visibility is the answer to "may this token get this Application".
type Visibility int

const (
	// VisibilityHidden: Argo CD refused the token or does not know the Application.
	VisibilityHidden Visibility = iota
	// VisibilityVisible: Argo CD gave the Application to the token.
	VisibilityVisible
	// VisibilityUnknown: Argo CD could not be asked and no answer was ever confirmed.
	VisibilityUnknown
)

func (v *ApplicationVisibility) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

func (v *ApplicationVisibility) ttl() time.Duration {
	if v.TTL > 0 {
		return v.TTL
	}
	return DefaultVisibilityTTL
}

func answer(visible bool) Visibility {
	if visible {
		return VisibilityVisible
	}
	return VisibilityHidden
}

// Check says whether the token of cfg may get the Application. A definite answer of
// Argo CD (the Application, or 401, 403, 404) is kept for the TTL. When Argo CD cannot
// be reached, the last answer that was confirmed stays, however old, so that an outage
// neither hides what the policy was shown nor shows what it never was: an Application
// whose visibility was never confirmed is VisibilityUnknown and is not shown.
func (v *ApplicationVisibility) Check(ctx context.Context, policyKey string, cfg argocd.Config, name, namespace string) (Visibility, error) {
	h := sha256.Sum256([]byte(cfg.URL + "\x00" + cfg.Token))
	key := hex.EncodeToString(h[:8]) + "/" + namespace + "/" + name

	v.mu.Lock()
	e, hit := v.entries[key]
	v.mu.Unlock()
	if hit && v.now().Sub(e.at) < v.ttl() {
		return answer(e.visible), nil
	}

	build := v.New
	if build == nil {
		build = func(c argocd.Config) (ApplicationGetter, error) { return argocd.New(c) }
	}
	getter, err := v.clients.get(policyKey, cfg, build)
	if err == nil {
		err = getter.GetApplication(ctx, name, namespace)
	}
	visible := err == nil
	if err != nil && (errors.Is(err, argocd.ErrUnauthorized) || errors.Is(err, argocd.ErrNotFound)) {
		err = nil
	}
	if err != nil {
		// Argo CD could not be asked. The last confirmed answer, however old, still stands.
		if hit {
			return answer(e.visible), nil
		}
		return VisibilityUnknown, nil
	}

	v.mu.Lock()
	if v.entries == nil {
		v.entries = map[string]visibilityEntry{}
	}
	if len(v.entries) > 4096 {
		// Keep the answers that make Applications visible; they are the ones an outage must not lose.
		for k, old := range v.entries {
			if !old.visible || v.now().Sub(old.at) > 24*time.Hour {
				delete(v.entries, k)
			}
		}
	}
	v.entries[key] = visibilityEntry{visible: visible, at: v.now()}
	v.mu.Unlock()
	return answer(visible), nil
}

// forget drops what is kept for a policy that is gone.
func (v *ApplicationVisibility) forget(policyKey string) {
	v.clients.forget(policyKey)
}

// tokenVisible is the answer for a policy with its own spec.argoCD: what its own
// token can get. Without a visibility checker nothing is asked and everything is
// visible (a test, or an operator that does not check).
func tokenVisible(ctx context.Context, v *ApplicationVisibility, c client.Reader, policy *backflowv1alpha1.BackflowPolicy,
	defaults *ArgoCDDefaults, name, namespace string) (Visibility, error) {
	if v == nil {
		return VisibilityVisible, nil
	}
	cfg, err := argoCDConfig(ctx, c, policy, defaults)
	if err != nil {
		return VisibilityHidden, err
	}
	return v.Check(ctx, policy.Namespace+"/"+policy.Name, cfg, name, namespace)
}
