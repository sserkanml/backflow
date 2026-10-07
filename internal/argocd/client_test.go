package argocd

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func newTestClient(t *testing.T, h http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	c, err := New(Config{URL: srv.URL, Token: "tok", InsecureSkipTLSVerify: true, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return c, srv
}

func TestGetManagedResources(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/applications/demo-app/managed-resources" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("appNamespace"); got != "argocd" {
			t.Errorf("appNamespace = %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q", got)
		}
		_, _ = w.Write([]byte(`{"items":[
			{"kind":"ConfigMap","namespace":"demo","name":"demo-config",
			 "predictedLiveState":"{\"data\":{\"LOG_LEVEL\":\"info\"}}",
			 "normalizedLiveState":"{\"data\":{\"LOG_LEVEL\":\"debug\"}}"},
			{"group":"apps","kind":"Deployment","namespace":"demo","name":"demo",
			 "predictedLiveState":"","normalizedLiveState":"null"}]}`))
	})
	got, err := c.GetManagedResources(context.Background(), "demo-app", "argocd")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d", len(got))
	}
	if got[0].Kind != "ConfigMap" || got[0].Name != "demo-config" || got[0].Namespace != "demo" {
		t.Errorf("unexpected resource: %+v", got[0])
	}
	data := got[0].NormalizedLiveState["data"].(map[string]interface{})
	if data["LOG_LEVEL"] != "debug" {
		t.Errorf("normalized = %v", got[0].NormalizedLiveState)
	}
	if got[1].Group != "apps" || got[1].PredictedLiveState != nil || got[1].NormalizedLiveState != nil {
		t.Errorf("unexpected resource: %+v", got[1])
	}
}

func TestGetManagedResourcesErrors(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"unauthorized", http.StatusUnauthorized, `{}`, ErrUnauthorized},
		{"forbidden", http.StatusForbidden, `{}`, ErrUnauthorized},
		{"not found", http.StatusNotFound, `{}`, ErrNotFound},
		{"server error", http.StatusInternalServerError, `oops`, ErrUnreachable},
		{"bad json", http.StatusOK, `not json`, ErrUnreachable},
		{"bad state", http.StatusOK, `{"items":[{"kind":"X","predictedLiveState":"{"}]}`, ErrUnreachable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			})
			_, err := c.GetManagedResources(context.Background(), "a", "argocd")
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestUnreachable(t *testing.T) {
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	c, err := New(Config{URL: url, Token: "t", InsecureSkipTLSVerify: true, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetManagedResources(context.Background(), "a", "argocd"); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("err = %v", err)
	}
}

func TestTimeout(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()
	c, _ := New(Config{URL: srv.URL, Token: "t", InsecureSkipTLSVerify: true, Timeout: 100 * time.Millisecond})
	if _, err := c.GetManagedResources(context.Background(), "a", "argocd"); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("err = %v", err)
	}
}

func TestTLSVerification(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer srv.Close()

	// Untrusted certificate without CA / insecure flag fails.
	c, _ := New(Config{URL: srv.URL, Token: "t"})
	if _, err := c.GetManagedResources(context.Background(), "a", "argocd"); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("err = %v, want unreachable", err)
	}

	// Trusting the server's certificate through CACert works.
	pemBytes := pemOf(srv)
	c, err := New(Config{URL: srv.URL, Token: "t", CACert: pemBytes})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetManagedResources(context.Background(), "a", "argocd"); err != nil {
		t.Fatalf("with CA: %v", err)
	}
}

func TestNewValidation(t *testing.T) {
	if _, err := New(Config{Token: "t"}); err == nil {
		t.Error("missing URL accepted")
	}
	if _, err := New(Config{URL: "https://x"}); err == nil {
		t.Error("missing token accepted")
	}
	if _, err := New(Config{URL: "https://x", Token: "t", CACert: []byte("junk")}); err == nil {
		t.Error("invalid CA accepted")
	}
}
