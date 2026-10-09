// Package scm is a minimal client for the merge request APIs of GitLab and
// GitHub. It uses net/http only. Backflow uses it to open, track and close
// merge requests; Git content itself is written by package gitrepo.
package scm

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var (
	// ErrUnauthorized means the provider rejected the token (HTTP 401): it is
	// invalid or expired.
	ErrUnauthorized = errors.New("scm: unauthorized")
	// ErrForbidden means the token is valid but not allowed to do this (HTTP
	// 403): it lacks a scope or role, or the action is not permitted, e.g. a
	// protected branch. Rate limiting is reported as ErrUnavailable instead.
	ErrForbidden = errors.New("scm: forbidden")
	// ErrNotFound means the project, merge request, branch or user does not exist
	// (or is not visible to the token).
	ErrNotFound = errors.New("scm: not found")
	// ErrConflict means the request clashes with existing state, e.g. an open
	// merge request for the same branches already exists.
	ErrConflict = errors.New("scm: conflict")
	// ErrInvalid means the provider refused the request as malformed or
	// impossible, e.g. the branches have no difference.
	ErrInvalid = errors.New("scm: invalid request")
	// ErrUnavailable means the provider could not be reached or failed
	// (network error, rate limit, 5xx); retrying may help. See RetryAfter for
	// the wait the provider suggested.
	ErrUnavailable = errors.New("scm: unavailable")
)

// now is replaced in tests.
var now = time.Now

const (
	defaultTimeout = 30 * time.Second
	maxBody        = 16 << 20
)

// State is the provider-independent state of a merge request.
type State string

const (
	StateOpen   State = "open"
	StateMerged State = "merged"
	// StateClosed means closed without being merged.
	StateClosed State = "closed"
)

// MergeRequest is a GitLab merge request or a GitHub pull request.
type MergeRequest struct {
	// Number is the GitLab iid or the GitHub pull request number.
	Number       int64
	URL          string
	State        State
	SourceBranch string
	TargetBranch string
	// MergeCommitSHA is the commit that contains the change on the target
	// branch. Set only when State is StateMerged and the provider reports it.
	MergeCommitSHA string
	// Warnings lists optional parts of a created merge request that could not
	// be applied (an unknown reviewer, a label that failed). The merge
	// request itself exists.
	Warnings []string
}

// CreateRequest describes a merge request to open.
type CreateRequest struct {
	SourceBranch string
	TargetBranch string
	Title        string
	Body         string
	Labels       []string
	// Reviewers are usernames. Unknown usernames are skipped with a warning.
	Reviewers []string
	// Assignee is a username; empty means unassigned. An unknown username is
	// skipped with a warning.
	Assignee string
}

// User is an account on the provider.
type User struct {
	ID       int64
	Username string
}

// Provider is the part of a Git hosting API Backflow needs. project is the
// repository path, e.g. "group/subgroup/repo" (GitLab) or "owner/repo" (GitHub).
// Errors wrap ErrUnauthorized, ErrNotFound, ErrConflict, ErrInvalid or
// ErrUnavailable.
type Provider interface {
	// FindOpenMergeRequest returns the open merge request from sourceBranch,
	// or nil when there is none.
	FindOpenMergeRequest(ctx context.Context, project, sourceBranch string) (*MergeRequest, error)
	CreateMergeRequest(ctx context.Context, project string, req CreateRequest) (*MergeRequest, error)
	GetMergeRequest(ctx context.Context, project string, number int64) (*MergeRequest, error)
	// CloseMergeRequest closes an open merge request without merging it.
	CloseMergeRequest(ctx context.Context, project string, number int64) error
	CommentOnMergeRequest(ctx context.Context, project string, number int64, body string) error
	DeleteBranch(ctx context.Context, project, branch string) error
	// LookupUser returns the account with the given username, or ErrNotFound.
	LookupUser(ctx context.Context, username string) (*User, error)
}

// Config describes how to reach a provider.
type Config struct {
	// Provider is "gitlab" or "github".
	Provider string
	// BaseURL is the web URL of the provider, e.g. https://gitlab.example.com
	// or https://github.com.
	BaseURL string
	Token   string
	// CACert is an optional PEM bundle of extra certificate authorities.
	CACert []byte
	// Timeout per request. Defaults to 30s.
	Timeout time.Duration
}

// New builds the Provider for cfg.Provider.
func New(cfg Config) (Provider, error) {
	if cfg.BaseURL == "" {
		return nil, errors.New("scm: base URL is required")
	}
	if cfg.Token == "" {
		return nil, errors.New("scm: token is required")
	}
	httpClient, err := newHTTPClient(cfg.CACert, cfg.Timeout)
	if err != nil {
		return nil, err
	}
	base := strings.TrimRight(cfg.BaseURL, "/")
	switch strings.ToLower(cfg.Provider) {
	case "gitlab":
		return newGitLab(base, cfg.Token, httpClient), nil
	case "github":
		return newGitHub(base, cfg.Token, httpClient), nil
	}
	return nil, fmt.Errorf("scm: unsupported provider %q", cfg.Provider)
}

func newHTTPClient(caCert []byte, timeout time.Duration) (*http.Client, error) {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if len(caCert) > 0 {
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(caCert) {
			return nil, errors.New("scm: CA bundle contains no valid PEM certificate")
		}
		tlsCfg.RootCAs = pool
	}
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: &http.Transport{TLSClientConfig: tlsCfg, Proxy: http.ProxyFromEnvironment},
	}, nil
}

// api is the shared JSON-over-HTTP plumbing of both providers.
type api struct {
	base    string // API root, without trailing slash
	headers map[string]string
	http    *http.Client
}

// do sends a request and decodes a JSON response into out (when non-nil).
// body is JSON-encoded when non-nil. Non-2xx answers become wrapped sentinel errors.
func (a *api) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	u := a.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("%w: encoding request: %v", ErrInvalid, err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, reader)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range a.headers {
		req.Header.Set(k, v)
	}

	resp, err := a.http.Do(req)
	if err != nil {
		// url.Error may embed the request URL; it never contains the token
		// because the token travels in a header.
		return fmt.Errorf("%w: %s %s: %v", ErrUnavailable, method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return fmt.Errorf("%w: %s %s: reading response: %v", ErrUnavailable, method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return statusError(method, path, resp.StatusCode, resp.Header, data)
	}
	if out == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%w: %s %s: invalid response: %v", ErrUnavailable, method, path, err)
	}
	return nil
}

// Error is a failed provider call. It wraps one of the package sentinels, so
// errors.Is works, and carries the wait the provider suggested, if any.
type Error struct {
	kind    error
	msg     string
	retryIn time.Duration
}

func (e *Error) Error() string { return e.kind.Error() + ": " + e.msg }
func (e *Error) Unwrap() error { return e.kind }

// RetryAfter returns the wait the provider suggested for a rate-limited call
// (Retry-After or the rate limit reset time). It returns 0 when err carries
// no suggestion.
func RetryAfter(err error) time.Duration {
	var e *Error
	if errors.As(err, &e) {
		return e.retryIn
	}
	return 0
}

// statusError maps an HTTP failure to an *Error wrapping a sentinel.
func statusError(method, path string, status int, header http.Header, body []byte) error {
	var kind error
	var wait time.Duration
	switch {
	case status == http.StatusUnauthorized:
		kind = ErrUnauthorized
	case status == http.StatusForbidden || status == http.StatusTooManyRequests:
		// GitHub signals rate limits with 403 plus rate limit headers.
		if w, limited := rateLimitWait(status, header); limited {
			kind, wait = ErrUnavailable, w
		} else {
			kind = ErrForbidden
		}
	case status == http.StatusNotFound:
		kind = ErrNotFound
	case status == http.StatusConflict:
		kind = ErrConflict
	case status == http.StatusBadRequest || status == http.StatusUnprocessableEntity ||
		status == http.StatusMethodNotAllowed:
		kind = ErrInvalid
	default: // 5xx and anything unexpected
		kind = ErrUnavailable
		wait = retryAfterHeader(header)
	}
	msg := fmt.Sprintf("%s %s: HTTP %d", method, path, status)
	if m := apiMessage(body); m != "" {
		msg += ": " + m
	}
	return &Error{kind: kind, msg: msg, retryIn: wait}
}

// rateLimitWait reports whether a 403/429 response is rate limiting and how
// long to wait. 429 always is. A 403 is when the provider says so: a
// Retry-After header, or X-RateLimit-Remaining: 0 (GitHub).
func rateLimitWait(status int, h http.Header) (time.Duration, bool) {
	wait := retryAfterHeader(h)
	if wait > 0 {
		return wait, true
	}
	if h.Get("X-RateLimit-Remaining") == "0" {
		if reset, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			if d := time.Unix(reset, 0).Sub(now()); d > 0 {
				return d, true
			}
		}
		return 0, true
	}
	return 0, status == http.StatusTooManyRequests
}

// retryAfterHeader parses Retry-After, either seconds or an HTTP date.
func retryAfterHeader(h http.Header) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now()); d > 0 {
			return d
		}
	}
	return 0
}

// apiMessage extracts the human-readable message of a provider error body.
func apiMessage(body []byte) string {
	var m struct {
		Message any    `json:"message"`
		Error   string `json:"error"`
		Errors  []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if json.Unmarshal(body, &m) != nil {
		return ""
	}
	msg := m.Error
	switch v := m.Message.(type) {
	case string:
		msg = v
	case nil:
	default:
		// GitLab sometimes returns a structured message.
		if b, err := json.Marshal(v); err == nil {
			msg = string(b)
		}
	}
	for _, e := range m.Errors {
		if e.Message != "" {
			msg += "; " + e.Message
		}
	}
	const limit = 300
	if len(msg) > limit {
		msg = msg[:limit] + "..."
	}
	return msg
}

// ParseRepoURL splits a Git remote URL into its host and project path.
// It accepts https://host/group/repo(.git), ssh://git@host/group/repo.git and
// the scp-like git@host:group/repo.git.
func ParseRepoURL(raw string) (host, project string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", errors.New("scm: empty repository URL")
	}
	var path string
	if !strings.Contains(raw, "://") {
		// scp-like: [user@]host:path
		at := raw
		if i := strings.Index(at, "@"); i >= 0 {
			at = at[i+1:]
		}
		i := strings.Index(at, ":")
		if i <= 0 {
			return "", "", fmt.Errorf("scm: cannot parse repository URL %q", raw)
		}
		host, path = at[:i], at[i+1:]
	} else {
		u, perr := url.Parse(raw)
		if perr != nil || u.Hostname() == "" {
			return "", "", fmt.Errorf("scm: cannot parse repository URL %q", raw)
		}
		host, path = u.Hostname(), u.Path
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	if path == "" || !strings.Contains(path, "/") {
		return "", "", fmt.Errorf("scm: repository URL %q has no project path", raw)
	}
	return strings.ToLower(host), path, nil
}
