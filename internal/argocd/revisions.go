package argocd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

var (
	// ErrUnsupported means the Argo CD does not know the revisions and
	// sourcePositions parameters (before 2.6) or refuses them.
	ErrUnsupported = errors.New("argocd: not supported by this Argo CD")
	// ErrAppDetailsFailed means Argo CD answered, but could not read the chart:
	// it does not exist at that version, or the repository cannot be reached.
	ErrAppDetailsFailed = errors.New("argocd: could not read the chart")
)

// RevisionsQuery renders an Application at given revisions, one per source.
type RevisionsQuery struct {
	AppName      string
	AppNamespace string
	// Revisions are the revisions to render, positional with SourcePositions:
	// a commit SHA for a Git source, the chart version for a Helm repository.
	Revisions []string
	// SourcePositions are the positions of the sources in spec.sources, from 1.
	SourcePositions []int
	// NoCache asks Argo CD to render for real: without it a revision that was
	// rendered before is answered from the manifest cache, so two renders of the
	// same revision are one render and a resource a chart names at random looks
	// stable. It needs no more permission than the render itself.
	NoCache bool
}

// GetManifestsByRevisions renders an Application at the given revision of each
// source (the unary GetManifests of Argo CD with revisions and
// sourcePositions). A commit Argo CD has not fetched is fetched by its SHA. The
// Application's own settings are used; nothing is sent but the revisions.
// The data of Secrets in the response is masked (see maskManifests).
// Errors wrap ErrUnauthorized, ErrNotFound, ErrUnreachable, ErrUnsupported or
// ErrRenderFailed.
func (c *Client) GetManifestsByRevisions(ctx context.Context, q RevisionsQuery) (*ManifestsResponse, error) {
	if q.AppName == "" {
		return nil, errors.New("argocd: application name is required")
	}
	if len(q.Revisions) != len(q.SourcePositions) || len(q.Revisions) == 0 {
		return nil, errors.New("argocd: one source position is needed for each revision")
	}
	v := url.Values{"appNamespace": {q.AppNamespace}}
	if q.NoCache {
		v.Set("noCache", "true")
	}
	for i, rev := range q.Revisions {
		v.Add("revisions", rev)
		v.Add("sourcePositions", strconv.Itoa(q.SourcePositions[i]))
	}
	u := fmt.Sprintf("%s/api/v1/applications/%s/manifests?%s", c.baseURL, url.PathEscape(q.AppName), v.Encode())

	ctx, cancel := context.WithTimeout(ctx, c.renderTimeout)
	defer cancel()
	body, status, err := c.do(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if err := httpError(status, body, q.AppName); err != nil {
		return nil, err
	}
	return decodeRevisionsResponse(body)
}

// decodeRevisionsResponse decodes the response of a render by revision and
// masks the Secrets in it. The body is not kept: from here on only masked
// manifests exist.
func decodeRevisionsResponse(body []byte) (*ManifestsResponse, error) {
	var parsed struct {
		Manifests  []string `json:"manifests"`
		Namespace  string   `json:"namespace"`
		Server     string   `json:"server"`
		Revision   string   `json:"revision"`
		SourceType string   `json:"sourceType"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		// The error of the decoder can name a value; say only that it is invalid.
		return nil, fmt.Errorf("%w: the response is not the expected JSON", ErrUnreachable)
	}
	masked, err := maskManifests(parsed.Manifests)
	if err != nil {
		return nil, err
	}
	return &ManifestsResponse{
		Manifests: masked, Namespace: parsed.Namespace, Server: parsed.Server,
		Revision: parsed.Revision, SourceType: parsed.SourceType,
	}, nil
}

// HelmDetails is what Argo CD reads from a chart without rendering it.
type HelmDetails struct {
	// Values is the text of the chart's own values.yaml, comments included.
	// Defaults of subcharts are not in it.
	Values string
	// ValueFiles are the value files of the chart, including the values.yaml of
	// subcharts (charts/<name>/values.yaml).
	ValueFiles []string
}

// GetHelmDetails returns the values of a chart from a Helm repository or a
// registry, or a Git source. source is the source of the Application at that
// position exactly as in spec.sources (Argo CD refuses a source that is not the
// Application's own), and index is its position, from 0. Errors wrap
// ErrUnauthorized (the account needs applications get and repositories get),
// ErrNotFound, ErrUnreachable or ErrAppDetailsFailed.
func (c *Client) GetHelmDetails(ctx context.Context, appName, appNamespace, appProject string,
	source map[string]any, index int) (*HelmDetails, error) {
	repoURL, _ := source["repoURL"].(string)
	if appName == "" || repoURL == "" {
		return nil, errors.New("argocd: application name and repository are required")
	}
	reqBody, err := json.Marshal(map[string]any{
		"source": source, "appName": appName, "appNamespace": appNamespace, "appProject": appProject, "sourceIndex": index,
	})
	if err != nil {
		return nil, err
	}
	u := fmt.Sprintf("%s/api/v1/repositories/%s/appdetails", c.baseURL, url.PathEscape(repoURL))
	ctx, cancel := context.WithTimeout(ctx, c.renderTimeout)
	defer cancel()
	body, status, err := c.do(ctx, http.MethodPost, u, reqBody)
	if err != nil {
		return nil, err
	}
	if err := httpError(status, body, appName); err != nil {
		if errors.Is(err, ErrRenderFailed) {
			return nil, fmt.Errorf("%w: %v", ErrAppDetailsFailed, strings.TrimPrefix(err.Error(), ErrRenderFailed.Error()+": "))
		}
		return nil, err
	}
	var parsed struct {
		Helm *struct {
			Values     string   `json:"values"`
			ValueFiles []string `json:"valueFiles"`
		} `json:"helm"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("%w: invalid response: %v", ErrUnreachable, err)
	}
	if parsed.Helm == nil {
		return nil, fmt.Errorf("%w: the source is not a Helm chart", ErrAppDetailsFailed)
	}
	return &HelmDetails{Values: parsed.Helm.Values, ValueFiles: parsed.Helm.ValueFiles}, nil
}

// do sends a request with the bearer token and returns the body and status.
func (c *Client) do(ctx context.Context, method, u string, body []byte) ([]byte, int, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, 0, fmt.Errorf("%w: reading response: %v", ErrUnreachable, err)
	}
	return data, resp.StatusCode, nil
}

// httpError maps a failed status of the REST gateway to an error, nil for 200.
// The gateway answers {"error": ..., "code": <gRPC code>, "message": ...}.
func httpError(status int, body []byte, app string) error {
	if status == http.StatusOK {
		return nil
	}
	var e struct {
		Message string `json:"message"`
		Code    int    `json:"code"`
	}
	_ = json.Unmarshal(body, &e)
	msg := sanitizeMessage(e.Message)
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return fmt.Errorf("%w: HTTP %d: %s", ErrUnauthorized, status, msg)
	case status == http.StatusNotFound:
		return fmt.Errorf("%w: application %q: %s", ErrNotFound, app, msg)
	case status == http.StatusNotImplemented || e.Code == 12 || status == http.StatusBadRequest && strings.Contains(strings.ToLower(msg), "unknown"):
		return fmt.Errorf("%w: %s", ErrUnsupported, msg)
	case status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout:
		return fmt.Errorf("%w: HTTP %d: %s", ErrUnreachable, status, msg)
	}
	return fmt.Errorf("%w: HTTP %d: %s", ErrRenderFailed, status, msg)
}
