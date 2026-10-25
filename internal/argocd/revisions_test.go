package argocd

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
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

const hunter = "aHVudGVyMi1zZWNyZXQ="

func TestGetManifestsByRevisionsMasksSecrets(t *testing.T) {
	secret := `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"s"},"type":"Opaque","data":{"password":"` + hunter + `","user":"YWRtaW4="},"stringData":{"token":"plain-token"}}`
	cm := `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cm"},"data":{"password":"visible"}}`
	custom := `{"apiVersion":"example.com/v1","kind":"Secret","metadata":{"name":"x"},"data":{"k":"v"}}`
	body, _ := json.Marshal(map[string]any{"manifests": []string{secret, cm, custom}})
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) })
	resp, err := c.GetManifestsByRevisions(t.Context(), RevisionsQuery{AppName: "a", AppNamespace: "argocd", Revisions: []string{"x"}, SourcePositions: []int{1}})
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(resp.Manifests, "\n")
	for _, leak := range []string{hunter, "plain-token", "YWRtaW4="} {
		if strings.Contains(all, leak) {
			t.Errorf("the response still holds %q: %s", leak, all)
		}
	}
	dataOf := func(manifest string) map[string]string {
		var m struct {
			Data map[string]string `json:"data"`
		}
		if err := json.Unmarshal([]byte(manifest), &m); err != nil {
			t.Fatal(err)
		}
		return m.Data
	}
	got := dataOf(resp.Manifests[0])
	if !strings.HasPrefix(got["password"], "<hidden:") || got["password"] == got["user"] {
		t.Errorf("masked data = %v", got)
	}
	if resp.Manifests[1] != cm || resp.Manifests[2] != custom {
		t.Errorf("a ConfigMap and a Secret of another API group are not touched: %v", resp.Manifests[1:])
	}

	// The same value gives the same mask, another value another one.
	again, err := maskManifests([]string{secret, strings.ReplaceAll(secret, hunter, "b3RoZXI=")})
	if err != nil {
		t.Fatal(err)
	}
	a, b := dataOf(again[0]), dataOf(again[1])
	if a["password"] != got["password"] || a["password"] == b["password"] {
		t.Errorf("masks: %v %v %v", got, a, b)
	}
}

func TestGetManifestsByRevisionsErrorsNeverHoldManifestText(t *testing.T) {
	secretText := "hunter2-super-secret"
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{"a render error that quotes the value", http.StatusInternalServerError,
			"{\"message\":\"rpc error: code = Unknown desc = yaml: unmarshal errors:\\n  line 3: cannot unmarshal !!str `" + secretText + "` into int\"}"},
		{"quotes of every kind", http.StatusInternalServerError,
			`{"message":"error: password: \"` + secretText + `\" and '` + secretText + `' failed"}`},
		{"a manifest that is not JSON", http.StatusOK, `{"manifests":["kind: Secret\ndata:\n  password: ` + secretText + `"]}`},
		{"a body that is not JSON at all", http.StatusOK, `not json ` + secretText},
		{"a body of the wrong shape", http.StatusOK, `{"manifests":"` + secretText + `"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			})
			_, err := c.GetManifestsByRevisions(t.Context(), RevisionsQuery{AppName: "a", AppNamespace: "argocd", Revisions: []string{"x"}, SourcePositions: []int{1}})
			if err == nil {
				t.Fatal("no error")
			}
			if strings.Contains(err.Error(), secretText) {
				t.Errorf("the error holds the value: %v", err)
			}
		})
	}
}

func TestSanitizeMessage(t *testing.T) {
	long := strings.Repeat("x", 1000)
	tests := []struct{ in, want string }{
		{"permission denied: repositories, get, ghcr.io/x", "permission denied: repositories, get, ghcr.io/x"},
		{"first line\nsecond line with `quoted`", "first line"},
		{"failed on `helm template . --values x`: exit 1", `failed on "…": exit 1`},
		{`value "a" and 'b' and ` + "`c`", `value "…" and "…" and "…"`},
		{"  padded  \r\nrest", "padded"},
		{long, strings.Repeat("x", maxServerMessage) + "…"},
	}
	for _, tt := range tests {
		if got := sanitizeMessage(tt.in); got != tt.want {
			t.Errorf("sanitizeMessage(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
