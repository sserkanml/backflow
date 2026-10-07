// Package argocd is a minimal client for the Argo CD REST API. It uses
// net/http only; the Argo CD Go module is deliberately not imported.
package argocd

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var (
	// ErrUnauthorized means Argo CD rejected the token or denied access.
	ErrUnauthorized = errors.New("argocd: unauthorized")
	// ErrNotFound means the Application does not exist.
	ErrNotFound = errors.New("argocd: not found")
	// ErrUnreachable means Argo CD could not be contacted or answered unexpectedly.
	ErrUnreachable = errors.New("argocd: unreachable")
)

const defaultTimeout = 30 * time.Second

// Config describes how to reach Argo CD.
type Config struct {
	// Base URL, e.g. https://argocd-server.argocd.svc.
	URL string
	// API token, sent as a bearer token.
	Token string
	// Optional PEM CA bundle used to verify the server certificate.
	CACert []byte
	// Skip server certificate verification.
	InsecureSkipTLSVerify bool
	// Request timeout. Defaults to 30s.
	Timeout time.Duration
}

// Client talks to the Argo CD API.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// ManagedResource is one entry of an Application's managed resources.
type ManagedResource struct {
	Group     string
	Kind      string
	Namespace string
	Name      string
	// PredictedLiveState is what Git wants, rendered and defaulted.
	PredictedLiveState map[string]interface{}
	// NormalizedLiveState is what is live, normalised.
	NormalizedLiveState map[string]interface{}
}

// New builds a Client from cfg.
func New(cfg Config) (*Client, error) {
	if cfg.URL == "" {
		return nil, errors.New("argocd: URL is required")
	}
	if cfg.Token == "" {
		return nil, errors.New("argocd: token is required")
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: cfg.InsecureSkipTLSVerify} //nolint:gosec // opt-in
	if len(cfg.CACert) > 0 {
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(cfg.CACert) {
			return nil, errors.New("argocd: CA bundle contains no valid PEM certificate")
		}
		tlsCfg.RootCAs = pool
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &Client{
		baseURL: strings.TrimRight(cfg.URL, "/"),
		token:   cfg.Token,
		http: &http.Client{
			Timeout:   timeout,
			Transport: &http.Transport{TLSClientConfig: tlsCfg, Proxy: http.ProxyFromEnvironment},
		},
	}, nil
}

type managedResourcesResponse struct {
	Items []struct {
		Group               string `json:"group"`
		Kind                string `json:"kind"`
		Namespace           string `json:"namespace"`
		Name                string `json:"name"`
		PredictedLiveState  string `json:"predictedLiveState"`
		NormalizedLiveState string `json:"normalizedLiveState"`
	} `json:"items"`
}

// GetManagedResources returns the managed resources of an Application.
// Errors wrap ErrUnauthorized, ErrNotFound or ErrUnreachable.
func (c *Client) GetManagedResources(ctx context.Context, appName, appNamespace string) ([]ManagedResource, error) {
	u := fmt.Sprintf("%s/api/v1/applications/%s/managed-resources?appNamespace=%s",
		c.baseURL, url.PathEscape(appName), url.QueryEscape(appNamespace))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("%w: reading response: %v", ErrUnreachable, err)
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("%w: HTTP %d for application %q", ErrUnauthorized, resp.StatusCode, appName)
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("%w: application %q", ErrNotFound, appName)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("%w: HTTP %d", ErrUnreachable, resp.StatusCode)
	}

	var parsed managedResourcesResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("%w: invalid response: %v", ErrUnreachable, err)
	}
	out := make([]ManagedResource, 0, len(parsed.Items))
	for _, it := range parsed.Items {
		predicted, err := decodeState(it.PredictedLiveState)
		if err != nil {
			return nil, fmt.Errorf("%w: predictedLiveState of %s/%s: %v", ErrUnreachable, it.Kind, it.Name, err)
		}
		normalized, err := decodeState(it.NormalizedLiveState)
		if err != nil {
			return nil, fmt.Errorf("%w: normalizedLiveState of %s/%s: %v", ErrUnreachable, it.Kind, it.Name, err)
		}
		out = append(out, ManagedResource{
			Group: it.Group, Kind: it.Kind, Namespace: it.Namespace, Name: it.Name,
			PredictedLiveState: predicted, NormalizedLiveState: normalized,
		})
	}
	return out, nil
}

// decodeState decodes a JSON-encoded object. Empty and "null" yield nil.
func decodeState(s string) (map[string]interface{}, error) {
	if s == "" || s == "null" {
		return nil, nil
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil, err
	}
	return m, nil
}
