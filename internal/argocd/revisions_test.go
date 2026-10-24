package argocd

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"testing"
)

func TestGetManifestsByRevisions(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/applications/ms-app/manifests" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		q := r.URL.Query()
		if got := q["revisions"]; !reflect.DeepEqual(got, []string{"6.7.1", "abc"}) {
			t.Errorf("revisions = %v", got)
		}
		if got := q["sourcePositions"]; !reflect.DeepEqual(got, []string{"1", "2"}) {
			t.Errorf("sourcePositions = %v", got)
		}
		if q.Get("appNamespace") != "argocd" || r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("query %v, auth %q", q, r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`{"manifests":["{\"kind\":\"Service\"}","{\"kind\":\"Deployment\"}"],"sourceType":"Helm"}`))
	})
	resp, err := c.GetManifestsByRevisions(t.Context(), RevisionsQuery{
		AppName: "ms-app", AppNamespace: "argocd", Revisions: []string{"6.7.1", "abc"}, SourcePositions: []int{1, 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Manifests) != 2 || resp.SourceType != "Helm" {
		t.Errorf("resp = %+v", resp)
	}

	t.Run("a mismatch of revisions and positions is refused before any request", func(t *testing.T) {
		if _, err := c.GetManifestsByRevisions(t.Context(), RevisionsQuery{AppName: "a", Revisions: []string{"x"}}); err == nil {
			t.Error("no error")
		}
	})
}

func TestGetManifestsByRevisionsErrors(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"denied", http.StatusForbidden, `{"message":"permission denied"}`, ErrUnauthorized},
		{"no such application", http.StatusNotFound, `{"message":"not found"}`, ErrNotFound},
		{"the render fails", http.StatusInternalServerError, `{"message":"failed to checkout revision abc"}`, ErrRenderFailed},
		{"an Argo CD without the parameters", http.StatusBadRequest, `{"message":"unknown field \"sourcePositions\""}`, ErrUnsupported},
		{"unimplemented", http.StatusNotImplemented, `{"code":12,"message":"unimplemented"}`, ErrUnsupported},
		{"unavailable", http.StatusServiceUnavailable, `{"message":"down"}`, ErrUnreachable},
		{"a bad request that is not about the parameters", http.StatusBadRequest, `{"message":"invalid revision"}`, ErrRenderFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			})
			_, err := c.GetManifestsByRevisions(t.Context(), RevisionsQuery{
				AppName: "a", AppNamespace: "argocd", Revisions: []string{"x"}, SourcePositions: []int{1},
			})
			if !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestGetHelmDetails(t *testing.T) {
	source := map[string]any{
		"repoURL": "ghcr.io/stefanprodan/charts", "chart": "podinfo", "targetRevision": "6.7.1",
		"helm": map[string]any{"valueFiles": []any{"$values/env/values.yaml"}},
	}
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.EscapedPath() != "/api/v1/repositories/ghcr.io%2Fstefanprodan%2Fcharts/appdetails" {
			t.Errorf("%s %s", r.Method, r.URL.EscapedPath())
		}
		var got map[string]any
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got["source"], source) || got["appName"] != "ms-app" || got["appNamespace"] != "argocd" ||
			got["appProject"] != "default" || got["sourceIndex"] != float64(0) {
			t.Errorf("body = %s", b)
		}
		_, _ = w.Write([]byte(`{"type":"Helm","helm":{"values":"replicaCount: 1\n","valueFiles":["values.yaml","charts/redis/values.yaml"]}}`))
	})
	got, err := c.GetHelmDetails(t.Context(), "ms-app", "argocd", "default", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.Values != "replicaCount: 1\n" || !reflect.DeepEqual(got.ValueFiles, []string{"values.yaml", "charts/redis/values.yaml"}) {
		t.Errorf("details = %+v", got)
	}

	for name, tt := range map[string]struct {
		status int
		body   string
		want   error
	}{
		"missing repositories get": {http.StatusForbidden, `{"message":"permission denied: repositories, get, ghcr.io/x"}`, ErrUnauthorized},
		"the chart is not there":   {http.StatusInternalServerError, `{"message":"chart not found"}`, ErrAppDetailsFailed},
		"not a Helm source":        {http.StatusOK, `{"type":"Directory"}`, ErrAppDetailsFailed},
	} {
		t.Run(name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			})
			if _, err := c.GetHelmDetails(t.Context(), "a", "argocd", "default", source, 0); !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
		})
	}
}
