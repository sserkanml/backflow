package scm

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// recorded is one request seen by a fake provider.
type recorded struct {
	Method string
	Path   string // escaped path, as sent
	Query  string
	Header http.Header
	Body   map[string]any
}

type reply struct {
	Status int
	Body   string
}

// fakeServer answers "METHOD /escaped/path" with canned replies and records requests.
type fakeServer struct {
	*httptest.Server
	mu       sync.Mutex
	replies  map[string]reply
	requests []recorded
}

func newFakeServer(t *testing.T, replies map[string]reply) *fakeServer {
	t.Helper()
	f := &fakeServer{replies: replies}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := recorded{Method: r.Method, Path: r.URL.EscapedPath(), Query: r.URL.RawQuery, Header: r.Header}
		if b, _ := io.ReadAll(r.Body); len(b) > 0 {
			_ = json.Unmarshal(b, &rec.Body)
		}
		f.mu.Lock()
		f.requests = append(f.requests, rec)
		rp, ok := f.replies[r.Method+" "+rec.Path]
		f.mu.Unlock()
		if !ok {
			http.Error(w, `{"message":"no route"}`, http.StatusTeapot)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rp.Status)
		_, _ = io.WriteString(w, rp.Body)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeServer) find(method, path string) *recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.requests {
		if f.requests[i].Method == method && f.requests[i].Path == path {
			return &f.requests[i]
		}
	}
	return nil
}

func TestNewValidation(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{"no url", Config{Provider: "gitlab", Token: "t"}, "base URL"},
		{"no token", Config{Provider: "gitlab", BaseURL: "https://x"}, "token"},
		{"bad provider", Config{Provider: "gitea", BaseURL: "https://x", Token: "t"}, "unsupported provider"},
		{"bad CA", Config{Provider: "gitlab", BaseURL: "https://x", Token: "t", CACert: []byte("junk")}, "CA bundle"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
	for _, p := range []string{"gitlab", "GitHub"} {
		if _, err := New(Config{Provider: p, BaseURL: "https://x/", Token: "t"}); err != nil {
			t.Errorf("New(%s): %v", p, err)
		}
	}
}

func TestParseRepoURL(t *testing.T) {
	cases := []struct {
		in, host, project string
		wantErr           bool
	}{
		{"https://gitlab.com/sserkanml/backflow-demo.git", "gitlab.com", "sserkanml/backflow-demo", false},
		{"https://GitLab.com/a/b/c/", "gitlab.com", "a/b/c", false},
		{"https://token@github.com/o/r", "github.com", "o/r", false},
		{"git@github.com:o/r.git", "github.com", "o/r", false},
		{"ssh://git@gitlab.example.com:2222/g/s/r.git", "gitlab.example.com", "g/s/r", false},
		{"", "", "", true},
		{"https://gitlab.com/onlyone", "", "", true},
		{"nonsense", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			host, project, err := ParseRepoURL(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if host != tc.host || project != tc.project {
				t.Errorf("got %q %q, want %q %q", host, project, tc.host, tc.project)
			}
		})
	}
}

func TestStatusErrorMapping(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   error
		msg    string
	}{
		{401, `{"message":"401 Unauthorized"}`, ErrUnauthorized, "401 Unauthorized"},
		{403, `{"message":"Resource not accessible"}`, ErrForbidden, "Resource not accessible"},
		{404, `{"message":"404 Project Not Found"}`, ErrNotFound, "404 Project Not Found"},
		{409, `{"message":"Another open merge request already exists"}`, ErrConflict, "already exists"},
		{400, `{"error":"source_branch is invalid"}`, ErrInvalid, "source_branch is invalid"},
		{422, `{"message":"Validation Failed","errors":[{"message":"No commits between a and b"}]}`, ErrInvalid, "No commits between"},
		{429, `rate limited`, ErrUnavailable, ""},
		{502, ``, ErrUnavailable, ""},
		{404, `{"message":{"base":["x"]}}`, ErrNotFound, `"base"`},
	}
	for _, tc := range cases {
		err := statusError("GET", "/x", tc.status, http.Header{}, []byte(tc.body))
		if !errors.Is(err, tc.want) {
			t.Errorf("status %d: err = %v, want %v", tc.status, err, tc.want)
		}
		if tc.want == ErrUnauthorized && errors.Is(err, ErrForbidden) || tc.want == ErrForbidden && errors.Is(err, ErrUnauthorized) {
			t.Errorf("status %d: 401 and 403 must stay distinct", tc.status)
		}
		if tc.msg != "" && !strings.Contains(err.Error(), tc.msg) {
			t.Errorf("status %d: err = %q, want containing %q", tc.status, err, tc.msg)
		}
	}
}

func TestNetworkErrorIsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	p, err := New(Config{Provider: "gitlab", BaseURL: url, Token: "secret-token"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.GetMergeRequest(t.Context(), "g/p", 1)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if strings.Contains(err.Error(), "secret-token") {
		t.Errorf("error leaks the token: %v", err)
	}
}

func TestRateLimitMapping(t *testing.T) {
	fixed := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	old := now
	now = func() time.Time { return fixed }
	t.Cleanup(func() { now = old })

	hdr := func(kv ...string) http.Header {
		h := http.Header{}
		for i := 0; i < len(kv); i += 2 {
			h.Set(kv[i], kv[i+1])
		}
		return h
	}
	reset := strconv.FormatInt(fixed.Add(90*time.Second).Unix(), 10)
	past := strconv.FormatInt(fixed.Add(-time.Minute).Unix(), 10)
	cases := []struct {
		name   string
		status int
		header http.Header
		want   error
		wait   time.Duration
	}{
		{"github 403 Retry-After seconds", 403, hdr("Retry-After", "60"), ErrUnavailable, 60 * time.Second},
		{"github 403 remaining 0 uses reset", 403, hdr("X-RateLimit-Remaining", "0", "X-RateLimit-Reset", reset), ErrUnavailable, 90 * time.Second},
		{"github 403 remaining 0 reset in the past", 403, hdr("X-RateLimit-Remaining", "0", "X-RateLimit-Reset", past), ErrUnavailable, 0},
		{"github 403 remaining 0 no reset", 403, hdr("X-RateLimit-Remaining", "0"), ErrUnavailable, 0},
		{"retry-after wins over reset", 403, hdr("Retry-After", "10", "X-RateLimit-Remaining", "0", "X-RateLimit-Reset", reset), ErrUnavailable, 10 * time.Second},
		{"github 403 with quota left is forbidden", 403, hdr("X-RateLimit-Remaining", "4999", "X-RateLimit-Reset", reset), ErrForbidden, 0},
		{"plain 403 is forbidden", 403, hdr(), ErrForbidden, 0},
		{"gitlab 429 Retry-After", 429, hdr("Retry-After", "30"), ErrUnavailable, 30 * time.Second},
		{"429 Retry-After HTTP date", 429, hdr("Retry-After", fixed.Add(45*time.Second).Format(http.TimeFormat)), ErrUnavailable, 45 * time.Second},
		{"429 without headers", 429, hdr(), ErrUnavailable, 0},
		{"429 junk Retry-After", 429, hdr("Retry-After", "soon"), ErrUnavailable, 0},
		{"503 Retry-After", 503, hdr("Retry-After", "5"), ErrUnavailable, 5 * time.Second},
		{"401 never rate limited", 401, hdr("Retry-After", "5"), ErrUnauthorized, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := statusError("GET", "/x", tc.status, tc.header, nil)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if got := RetryAfter(err); got != tc.wait {
				t.Errorf("RetryAfter = %v, want %v", got, tc.wait)
			}
		})
	}
	if RetryAfter(errors.New("plain")) != 0 || RetryAfter(nil) != 0 {
		t.Error("RetryAfter of a foreign error must be 0")
	}
}

// The suggestion survives the wrapping done by callers and the full HTTP path.
func TestRetryAfterThroughHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "17")
		http.Error(w, `{"message":"slow down"}`, http.StatusTooManyRequests)
	}))
	defer srv.Close()
	for _, prov := range []string{"gitlab", "github"} {
		p, err := New(Config{Provider: prov, BaseURL: srv.URL, Token: "t"})
		if err != nil {
			t.Fatal(err)
		}
		_, err = p.GetMergeRequest(t.Context(), "o/r", 1)
		wrapped := fmt.Errorf("syncing: %w", err)
		if !errors.Is(wrapped, ErrUnavailable) || RetryAfter(wrapped) != 17*time.Second {
			t.Errorf("%s: err = %v, retry = %v", prov, wrapped, RetryAfter(wrapped))
		}
	}
}
