package scm

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

const glMR = `{"iid":7,"web_url":"https://gitlab.example.com/g/p/-/merge_requests/7","state":"opened",
"source_branch":"backflow/dp-1","target_branch":"main","sha":"headsha"}`

func newGitLabTest(t *testing.T, replies map[string]reply) (Provider, *fakeServer) {
	t.Helper()
	srv := newFakeServer(t, replies)
	p, err := New(Config{Provider: "gitlab", BaseURL: srv.URL, Token: "glpat-x"})
	if err != nil {
		t.Fatal(err)
	}
	return p, srv
}

func TestGitLabFindOpenMergeRequest(t *testing.T) {
	const path = "GET /api/v4/projects/g%2Fsub%2Fp/merge_requests"
	t.Run("found", func(t *testing.T) {
		p, srv := newGitLabTest(t, map[string]reply{path: {200, "[" + glMR + "]"}})
		mr, err := p.FindOpenMergeRequest(t.Context(), "g/sub/p", "backflow/dp-1")
		if err != nil {
			t.Fatal(err)
		}
		want := &MergeRequest{Number: 7, URL: "https://gitlab.example.com/g/p/-/merge_requests/7",
			State: StateOpen, SourceBranch: "backflow/dp-1", TargetBranch: "main"}
		if !reflect.DeepEqual(mr, want) {
			t.Errorf("got %+v, want %+v", mr, want)
		}
		rec := srv.find("GET", "/api/v4/projects/g%2Fsub%2Fp/merge_requests")
		if rec.Header.Get("PRIVATE-TOKEN") != "glpat-x" {
			t.Errorf("token header = %q", rec.Header.Get("PRIVATE-TOKEN"))
		}
		if rec.Query != "per_page=100&source_branch=backflow%2Fdp-1&state=opened" {
			t.Errorf("query = %q", rec.Query)
		}
	})
	t.Run("none", func(t *testing.T) {
		p, _ := newGitLabTest(t, map[string]reply{path: {200, "[]"}})
		mr, err := p.FindOpenMergeRequest(t.Context(), "g/sub/p", "backflow/dp-1")
		if err != nil || mr != nil {
			t.Fatalf("got %v, %v; want nil, nil", mr, err)
		}
	})
	t.Run("other branch ignored", func(t *testing.T) {
		p, _ := newGitLabTest(t, map[string]reply{path: {200, "[" + glMR + "]"}})
		mr, err := p.FindOpenMergeRequest(t.Context(), "g/sub/p", "backflow/other")
		if err != nil || mr != nil {
			t.Fatalf("got %v, %v; want nil, nil", mr, err)
		}
	})
	t.Run("errors", func(t *testing.T) {
		for status, want := range map[int]error{401: ErrUnauthorized, 403: ErrForbidden, 404: ErrNotFound, 500: ErrUnavailable} {
			p, _ := newGitLabTest(t, map[string]reply{path: {status, `{"message":"x"}`}})
			if _, err := p.FindOpenMergeRequest(t.Context(), "g/sub/p", "b"); !errors.Is(err, want) {
				t.Errorf("status %d: err = %v, want %v", status, err, want)
			}
		}
	})
}

func TestGitLabFindMergeRequest(t *testing.T) {
	const path = "GET /api/v4/projects/g%2Fp/merge_requests"
	mr := func(iid, state string) string {
		return `{"iid":` + iid + `,"web_url":"https://gitlab.example.com/g/p/-/merge_requests/` + iid + `","state":"` + state +
			`","source_branch":"backflow/dp-1","target_branch":"main","merge_commit_sha":"mc"}`
	}
	tests := []struct {
		name   string
		body   string
		want   int64 // 0: none
		wState State
	}{
		{"none", `[]`, 0, ""},
		{"a closed one", "[" + mr("7", "closed") + "]", 7, StateClosed},
		{"a merged one", "[" + mr("7", "merged") + "]", 7, StateMerged},
		{"the newest of several closed ones", "[" + mr("9", "closed") + "," + mr("7", "closed") + "]", 9, StateClosed},
		{"an open one is preferred over a newer closed one", "[" + mr("9", "closed") + "," + mr("7", "opened") + "]", 7, StateOpen},
		{"locked counts as open", "[" + mr("7", "locked") + "]", 7, StateOpen},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, srv := newGitLabTest(t, map[string]reply{path: {200, tt.body}})
			got, err := p.FindMergeRequest(t.Context(), "g/p", "backflow/dp-1")
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
			if q := srv.find("GET", "/api/v4/projects/g%2Fp/merge_requests").Query; !strings.Contains(q, "state=all") {
				t.Errorf("query = %q, want every state", q)
			}
		})
	}
	t.Run("other branches are ignored", func(t *testing.T) {
		p, _ := newGitLabTest(t, map[string]reply{path: {200, "[" + mr("7", "closed") + "]"}})
		if got, err := p.FindMergeRequest(t.Context(), "g/p", "backflow/other"); err != nil || got != nil {
			t.Fatalf("got %v, %v", got, err)
		}
	})
	t.Run("errors", func(t *testing.T) {
		p, _ := newGitLabTest(t, map[string]reply{path: {500, `{"message":"x"}`}})
		if _, err := p.FindMergeRequest(t.Context(), "g/p", "b"); !errors.Is(err, ErrUnavailable) {
			t.Errorf("err = %v", err)
		}
	})
}

func TestGitLabCreateMergeRequest(t *testing.T) {
	routes := map[string]reply{
		"GET /api/v4/users":                          {200, `[{"id":42,"username":"alice"}]`},
		"POST /api/v4/projects/g%2Fp/merge_requests": {201, glMR},
	}
	p, srv := newGitLabTest(t, routes)
	mr, err := p.CreateMergeRequest(t.Context(), "g/p", CreateRequest{
		SourceBranch: "backflow/dp-1", TargetBranch: "main", Title: "t", Body: "b",
		Labels: []string{"backflow", "drift"}, Reviewers: []string{"alice"}, Assignee: "alice",
	})
	if err != nil {
		t.Fatal(err)
	}
	if mr.Number != 7 || mr.State != StateOpen || len(mr.Warnings) != 0 {
		t.Errorf("mr = %+v", mr)
	}
	body := srv.find("POST", "/api/v4/projects/g%2Fp/merge_requests").Body
	want := map[string]any{
		"source_branch": "backflow/dp-1", "target_branch": "main", "title": "t", "description": "b",
		"labels": "backflow,drift", "reviewer_ids": []any{42.0}, "assignee_id": 42.0,
		"remove_source_branch": false,
	}
	if !reflect.DeepEqual(body, want) {
		t.Errorf("body = %v, want %v", body, want)
	}
}

func TestGitLabCreateSkipsUnknownUsers(t *testing.T) {
	p, srv := newGitLabTest(t, map[string]reply{
		"GET /api/v4/users":                          {200, `[]`},
		"POST /api/v4/projects/g%2Fp/merge_requests": {201, glMR},
	})
	mr, err := p.CreateMergeRequest(t.Context(), "g/p", CreateRequest{
		SourceBranch: "b", TargetBranch: "main", Title: "t", Reviewers: []string{"ghost"}, Assignee: "nobody",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(mr.Warnings) != 2 {
		t.Errorf("warnings = %v, want 2", mr.Warnings)
	}
	body := srv.find("POST", "/api/v4/projects/g%2Fp/merge_requests").Body
	if _, ok := body["reviewer_ids"]; ok {
		t.Errorf("reviewer_ids sent for unknown user: %v", body)
	}
	if _, ok := body["assignee_id"]; ok {
		t.Errorf("assignee_id sent for unknown user: %v", body)
	}
}

func TestGitLabCreateErrors(t *testing.T) {
	const path = "POST /api/v4/projects/g%2Fp/merge_requests"
	cases := map[string]struct {
		status int
		want   error
	}{
		"conflict":     {409, ErrConflict},
		"unauthorized": {401, ErrUnauthorized},
		"forbidden":    {403, ErrForbidden},
		"no project":   {404, ErrNotFound},
		"bad branch":   {400, ErrInvalid},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			p, _ := newGitLabTest(t, map[string]reply{path: {tc.status, `{"message":"x"}`}})
			_, err := p.CreateMergeRequest(t.Context(), "g/p", CreateRequest{SourceBranch: "b", TargetBranch: "main", Title: "t"})
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestGitLabGetMergeRequestStates(t *testing.T) {
	cases := []struct {
		name, body string
		state      State
		sha        string
	}{
		{"open", `{"iid":7,"state":"opened"}`, StateOpen, ""},
		{"merged with merge commit", `{"iid":7,"state":"merged","merge_commit_sha":"mc","squash_commit_sha":"sq","sha":"h"}`, StateMerged, "mc"},
		{"merged squash", `{"iid":7,"state":"merged","merge_commit_sha":null,"squash_commit_sha":"sq","sha":"h"}`, StateMerged, "sq"},
		{"merged fast-forward", `{"iid":7,"state":"merged","sha":"h"}`, StateMerged, "h"},
		{"closed", `{"iid":7,"state":"closed","sha":"h"}`, StateClosed, ""},
		// A merge in progress: neither closed nor merged yet.
		{"locked while merging", `{"iid":7,"state":"locked","sha":"h"}`, StateOpen, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := newGitLabTest(t, map[string]reply{"GET /api/v4/projects/g%2Fp/merge_requests/7": {200, tc.body}})
			mr, err := p.GetMergeRequest(t.Context(), "g/p", 7)
			if err != nil {
				t.Fatal(err)
			}
			if mr.State != tc.state || mr.MergeCommitSHA != tc.sha {
				t.Errorf("got state %q sha %q, want %q %q", mr.State, mr.MergeCommitSHA, tc.state, tc.sha)
			}
		})
	}
	p, _ := newGitLabTest(t, nil)
	if _, err := p.GetMergeRequest(t.Context(), "g/p", 7); err == nil {
		t.Error("unexpected success on unknown route")
	}
}

func TestGitLabGetMergeRequestNotFound(t *testing.T) {
	p, _ := newGitLabTest(t, map[string]reply{"GET /api/v4/projects/g%2Fp/merge_requests/9": {404, `{"message":"404 Not found"}`}})
	if _, err := p.GetMergeRequest(t.Context(), "g/p", 9); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestGitLabCloseCommentDelete(t *testing.T) {
	p, srv := newGitLabTest(t, map[string]reply{
		"PUT /api/v4/projects/g%2Fp/merge_requests/7":                       {200, glMR},
		"POST /api/v4/projects/g%2Fp/merge_requests/7/notes":                {201, `{}`},
		"DELETE /api/v4/projects/g%2Fp/repository/branches/backflow%2Fdp-1": {204, ``},
	})
	ctx := t.Context()
	if err := p.CloseMergeRequest(ctx, "g/p", 7); err != nil {
		t.Fatal(err)
	}
	if err := p.CommentOnMergeRequest(ctx, "g/p", 7, "reverted"); err != nil {
		t.Fatal(err)
	}
	if err := p.DeleteBranch(ctx, "g/p", "backflow/dp-1"); err != nil {
		t.Fatal(err)
	}
	if got := srv.find("PUT", "/api/v4/projects/g%2Fp/merge_requests/7").Body["state_event"]; got != "close" {
		t.Errorf("state_event = %v", got)
	}
	if got := srv.find("POST", "/api/v4/projects/g%2Fp/merge_requests/7/notes").Body["body"]; got != "reverted" {
		t.Errorf("note body = %v", got)
	}
}

func TestGitLabDeleteBranchNotFound(t *testing.T) {
	p, _ := newGitLabTest(t, map[string]reply{
		"DELETE /api/v4/projects/g%2Fp/repository/branches/gone": {404, `{"message":"404 Branch Not Found"}`},
	})
	if err := p.DeleteBranch(t.Context(), "g/p", "gone"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestGitLabLookupUser(t *testing.T) {
	p, _ := newGitLabTest(t, map[string]reply{"GET /api/v4/users": {200, `[{"id":5,"username":"Bob"}]`}})
	u, err := p.LookupUser(t.Context(), "bob")
	if err != nil || u.ID != 5 || u.Username != "Bob" {
		t.Fatalf("got %+v, %v", u, err)
	}
	if _, err := p.LookupUser(t.Context(), "carol"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}
