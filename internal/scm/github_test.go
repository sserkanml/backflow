package scm

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

const ghPR = `{"number":12,"html_url":"https://github.example.com/o/r/pull/12","state":"open","merged":false,
"head":{"ref":"backflow/dp-1"},"base":{"ref":"main"}}`

// GitHub Enterprise layout: any base URL other than github.com uses /api/v3.
func newGitHubTest(t *testing.T, replies map[string]reply) (Provider, *fakeServer) {
	t.Helper()
	srv := newFakeServer(t, replies)
	p, err := New(Config{Provider: "github", BaseURL: srv.URL, Token: "ghp_x"})
	if err != nil {
		t.Fatal(err)
	}
	return p, srv
}

func TestGitHubAPIRoot(t *testing.T) {
	if g := newGitHub("https://github.com", "t", nil); g.base != "https://api.github.com" {
		t.Errorf("github.com base = %q", g.base)
	}
	if g := newGitHub("https://ghe.example.com", "t", nil); g.base != "https://ghe.example.com/api/v3" {
		t.Errorf("enterprise base = %q", g.base)
	}
	for _, in := range []string{"https://GitHub.com", "https://www.github.com", "https://WWW.GITHUB.COM/"} {
		if g := newGitHub(strings.TrimRight(in, "/"), "t", nil); g.base != "https://api.github.com" {
			t.Errorf("%s: base = %q", in, g.base)
		}
	}
}

func TestGitHubFindOpenMergeRequest(t *testing.T) {
	const path = "GET /api/v3/repos/o/r/pulls"
	t.Run("found", func(t *testing.T) {
		p, srv := newGitHubTest(t, map[string]reply{path: {200, "[" + ghPR + "]"}})
		mr, err := p.FindOpenMergeRequest(t.Context(), "o/r", "backflow/dp-1")
		if err != nil {
			t.Fatal(err)
		}
		want := &MergeRequest{Number: 12, URL: "https://github.example.com/o/r/pull/12",
			State: StateOpen, SourceBranch: "backflow/dp-1", TargetBranch: "main"}
		if !reflect.DeepEqual(mr, want) {
			t.Errorf("got %+v, want %+v", mr, want)
		}
		rec := srv.find("GET", "/api/v3/repos/o/r/pulls")
		if rec.Header.Get("Authorization") != "Bearer ghp_x" {
			t.Errorf("authorization = %q", rec.Header.Get("Authorization"))
		}
		if rec.Query != "head=o%3Abackflow%2Fdp-1&per_page=100&state=open" {
			t.Errorf("query = %q", rec.Query)
		}
	})
	t.Run("none", func(t *testing.T) {
		p, _ := newGitHubTest(t, map[string]reply{path: {200, "[]"}})
		mr, err := p.FindOpenMergeRequest(t.Context(), "o/r", "backflow/dp-1")
		if err != nil || mr != nil {
			t.Fatalf("got %v, %v; want nil, nil", mr, err)
		}
	})
	t.Run("errors", func(t *testing.T) {
		for status, want := range map[int]error{401: ErrUnauthorized, 403: ErrForbidden, 404: ErrNotFound, 503: ErrUnavailable} {
			p, _ := newGitHubTest(t, map[string]reply{path: {status, `{"message":"x"}`}})
			if _, err := p.FindOpenMergeRequest(t.Context(), "o/r", "b"); !errors.Is(err, want) {
				t.Errorf("status %d: err = %v, want %v", status, err, want)
			}
		}
	})
}

func TestGitHubFindMergeRequest(t *testing.T) {
	const path = "GET /api/v3/repos/o/r/pulls"
	pr := func(number, state string, merged bool) string {
		return `{"number":` + number + `,"html_url":"https://github.example.com/o/r/pull/` + number + `","state":"` + state +
			`","merged":` + map[bool]string{true: "true", false: "false"}[merged] +
			`,"merge_commit_sha":"mc","head":{"ref":"backflow/dp-1"},"base":{"ref":"main"}}`
	}
	tests := []struct {
		name   string
		body   string
		want   int64
		wState State
	}{
		{"none", `[]`, 0, ""},
		{"a closed one", "[" + pr("12", "closed", false) + "]", 12, StateClosed},
		{"a merged one", "[" + pr("12", "closed", true) + "]", 12, StateMerged},
		{"the newest of several", "[" + pr("14", "closed", false) + "," + pr("12", "closed", true) + "]", 14, StateClosed},
		{"an open one is preferred", "[" + pr("14", "closed", false) + "," + pr("12", "open", false) + "]", 12, StateOpen},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, srv := newGitHubTest(t, map[string]reply{path: {200, tt.body}})
			got, err := p.FindMergeRequest(t.Context(), "o/r", "backflow/dp-1")
			if err != nil {
				t.Fatal(err)
			}
			if tt.want == 0 {
				if got != nil {
					t.Fatalf("got %+v, want nil", got)
				}
			} else if got == nil || got.Number != tt.want || got.State != tt.wState {
				t.Fatalf("got %+v, want #%d %s", got, tt.want, tt.wState)
			}
			if q := srv.find("GET", "/api/v3/repos/o/r/pulls").Query; !strings.Contains(q, "state=all") {
				t.Errorf("query = %q, want every state", q)
			}
		})
	}
	t.Run("errors", func(t *testing.T) {
		p, _ := newGitHubTest(t, map[string]reply{path: {503, `{"message":"x"}`}})
		if _, err := p.FindMergeRequest(t.Context(), "o/r", "b"); !errors.Is(err, ErrUnavailable) {
			t.Errorf("err = %v", err)
		}
	})
}

func TestGitHubCreateMergeRequest(t *testing.T) {
	p, srv := newGitHubTest(t, map[string]reply{
		"POST /api/v3/repos/o/r/pulls":                        {201, ghPR},
		"POST /api/v3/repos/o/r/issues/12/labels":             {200, `[]`},
		"POST /api/v3/repos/o/r/pulls/12/requested_reviewers": {201, `{}`},
		"POST /api/v3/repos/o/r/issues/12/assignees":          {201, `{}`},
	})
	mr, err := p.CreateMergeRequest(t.Context(), "o/r", CreateRequest{
		SourceBranch: "backflow/dp-1", TargetBranch: "main", Title: "t", Body: "b",
		Labels: []string{"backflow"}, Reviewers: []string{"alice"}, Assignee: "bob",
	})
	if err != nil {
		t.Fatal(err)
	}
	if mr.Number != 12 || len(mr.Warnings) != 0 {
		t.Errorf("mr = %+v", mr)
	}
	want := map[string]any{"title": "t", "body": "b", "head": "backflow/dp-1", "base": "main"}
	if got := srv.find("POST", "/api/v3/repos/o/r/pulls").Body; !reflect.DeepEqual(got, want) {
		t.Errorf("pull body = %v, want %v", got, want)
	}
	if got := srv.find("POST", "/api/v3/repos/o/r/issues/12/labels").Body["labels"]; !reflect.DeepEqual(got, []any{"backflow"}) {
		t.Errorf("labels = %v", got)
	}
	if got := srv.find("POST", "/api/v3/repos/o/r/pulls/12/requested_reviewers").Body["reviewers"]; !reflect.DeepEqual(got, []any{"alice"}) {
		t.Errorf("reviewers = %v", got)
	}
	if got := srv.find("POST", "/api/v3/repos/o/r/issues/12/assignees").Body["assignees"]; !reflect.DeepEqual(got, []any{"bob"}) {
		t.Errorf("assignees = %v", got)
	}
}

func TestGitHubCreateMetadataFailureIsAWarning(t *testing.T) {
	p, _ := newGitHubTest(t, map[string]reply{
		"POST /api/v3/repos/o/r/pulls":                        {201, ghPR},
		"POST /api/v3/repos/o/r/issues/12/labels":             {403, `{"message":"forbidden"}`},
		"POST /api/v3/repos/o/r/pulls/12/requested_reviewers": {422, `{"message":"not a collaborator"}`},
		"POST /api/v3/repos/o/r/issues/12/assignees":          {201, `{}`},
	})
	mr, err := p.CreateMergeRequest(t.Context(), "o/r", CreateRequest{
		SourceBranch: "b", TargetBranch: "main", Title: "t",
		Labels: []string{"x"}, Reviewers: []string{"ghost"}, Assignee: "bob",
	})
	if err != nil {
		t.Fatalf("pull request exists, so no error expected: %v", err)
	}
	if len(mr.Warnings) != 2 {
		t.Errorf("warnings = %v, want 2", mr.Warnings)
	}
}

func TestGitHubCreateErrors(t *testing.T) {
	const path = "POST /api/v3/repos/o/r/pulls"
	cases := map[string]struct {
		status int
		body   string
		want   error
	}{
		"already exists": {422, `{"message":"Validation Failed","errors":[{"message":"A pull request already exists for o:b."}]}`, ErrConflict},
		"no commits":     {422, `{"message":"Validation Failed","errors":[{"message":"No commits between main and b"}]}`, ErrInvalid},
		"unauthorized":   {401, `{"message":"Bad credentials"}`, ErrUnauthorized},
		"forbidden":      {403, `{"message":"Resource not accessible by personal access token"}`, ErrForbidden},
		"no repo":        {404, `{"message":"Not Found"}`, ErrNotFound},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			p, _ := newGitHubTest(t, map[string]reply{path: {tc.status, tc.body}})
			_, err := p.CreateMergeRequest(t.Context(), "o/r", CreateRequest{SourceBranch: "b", TargetBranch: "main", Title: "t"})
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestGitHubGetMergeRequestStates(t *testing.T) {
	cases := []struct {
		name, body string
		state      State
		sha        string
	}{
		{"open", `{"number":12,"state":"open","merged":false}`, StateOpen, ""},
		{"merged", `{"number":12,"state":"closed","merged":true,"merge_commit_sha":"mc"}`, StateMerged, "mc"},
		{"closed", `{"number":12,"state":"closed","merged":false,"merge_commit_sha":"stale"}`, StateClosed, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := newGitHubTest(t, map[string]reply{"GET /api/v3/repos/o/r/pulls/12": {200, tc.body}})
			mr, err := p.GetMergeRequest(t.Context(), "o/r", 12)
			if err != nil {
				t.Fatal(err)
			}
			if mr.State != tc.state || mr.MergeCommitSHA != tc.sha {
				t.Errorf("got state %q sha %q, want %q %q", mr.State, mr.MergeCommitSHA, tc.state, tc.sha)
			}
		})
	}
	p, _ := newGitHubTest(t, map[string]reply{"GET /api/v3/repos/o/r/pulls/99": {404, `{"message":"Not Found"}`}})
	if _, err := p.GetMergeRequest(t.Context(), "o/r", 99); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestGitHubCloseCommentDelete(t *testing.T) {
	p, srv := newGitHubTest(t, map[string]reply{
		"PATCH /api/v3/repos/o/r/pulls/12":                      {200, ghPR},
		"POST /api/v3/repos/o/r/issues/12/comments":             {201, `{}`},
		"DELETE /api/v3/repos/o/r/git/refs/heads/backflow/dp-1": {204, ``},
	})
	ctx := t.Context()
	if err := p.CloseMergeRequest(ctx, "o/r", 12); err != nil {
		t.Fatal(err)
	}
	if err := p.CommentOnMergeRequest(ctx, "o/r", 12, "reverted"); err != nil {
		t.Fatal(err)
	}
	if err := p.DeleteBranch(ctx, "o/r", "backflow/dp-1"); err != nil {
		t.Fatal(err)
	}
	if got := srv.find("PATCH", "/api/v3/repos/o/r/pulls/12").Body["state"]; got != "closed" {
		t.Errorf("state = %v", got)
	}
	if got := srv.find("POST", "/api/v3/repos/o/r/issues/12/comments").Body["body"]; got != "reverted" {
		t.Errorf("comment = %v", got)
	}
}

func TestGitHubDeleteBranchErrors(t *testing.T) {
	p, _ := newGitHubTest(t, map[string]reply{
		"DELETE /api/v3/repos/o/r/git/refs/heads/gone": {404, `{"message":"Not Found"}`},
		"DELETE /api/v3/repos/o/r/git/refs/heads/deny": {403, `{"message":"Resource not accessible"}`},
		"DELETE /api/v3/repos/o/r/git/refs/heads/nope": {422, `{"message":"Reference does not exist"}`},
		"DELETE /api/v3/repos/o/r/git/refs/heads/bad":  {422, `{"message":"Something else is wrong"}`},
	})
	if err := p.DeleteBranch(t.Context(), "o/r", "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("422 reference does not exist: err = %v, want ErrNotFound", err)
	}
	if err := p.DeleteBranch(t.Context(), "o/r", "bad"); !errors.Is(err, ErrInvalid) || errors.Is(err, ErrNotFound) {
		t.Errorf("other 422: err = %v, want ErrInvalid", err)
	}
	if err := p.DeleteBranch(t.Context(), "o/r", "gone"); !errors.Is(err, ErrNotFound) {
		t.Errorf("gone: err = %v", err)
	}
	if err := p.DeleteBranch(t.Context(), "o/r", "deny"); !errors.Is(err, ErrForbidden) {
		t.Errorf("deny: err = %v", err)
	}
}

func TestGitHubLookupUser(t *testing.T) {
	p, _ := newGitHubTest(t, map[string]reply{
		"GET /api/v3/users/bob":   {200, `{"id":5,"login":"bob"}`},
		"GET /api/v3/users/carol": {404, `{"message":"Not Found"}`},
	})
	u, err := p.LookupUser(t.Context(), "bob")
	if err != nil || u.ID != 5 || u.Username != "bob" {
		t.Fatalf("got %+v, %v", u, err)
	}
	if _, err = p.LookupUser(t.Context(), "carol"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}
