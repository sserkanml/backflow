package scm

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

type gitlab struct{ api }

func newGitLab(base, token string, c *http.Client) *gitlab {
	return &gitlab{api{
		base:    base + "/api/v4",
		headers: map[string]string{"PRIVATE-TOKEN": token},
		http:    c,
	}}
}

// gitlabMR is the subset of a GitLab merge request Backflow reads.
type gitlabMR struct {
	IID            int64  `json:"iid"`
	WebURL         string `json:"web_url"`
	State          string `json:"state"`
	SourceBranch   string `json:"source_branch"`
	TargetBranch   string `json:"target_branch"`
	SHA            string `json:"sha"`
	MergeCommitSHA string `json:"merge_commit_sha"`
	SquashCommit   string `json:"squash_commit_sha"`
}

func (m gitlabMR) convert() *MergeRequest {
	out := &MergeRequest{
		Number: m.IID, URL: m.WebURL,
		SourceBranch: m.SourceBranch, TargetBranch: m.TargetBranch,
	}
	switch m.State {
	case "opened":
		out.State = StateOpen
	case "merged":
		out.State = StateMerged
		// Merge commit, else the squash commit, else (fast-forward merge) the
		// head of the source branch, which is now the target's commit.
		for _, sha := range []string{m.MergeCommitSHA, m.SquashCommit, m.SHA} {
			if sha != "" {
				out.MergeCommitSHA = sha
				break
			}
		}
	default: // closed, locked
		out.State = StateClosed
	}
	return out
}

func gitlabProject(project string) string {
	return "/projects/" + url.PathEscape(project)
}

func (g *gitlab) mrPath(project string, number int64) string {
	return gitlabProject(project) + "/merge_requests/" + strconv.FormatInt(number, 10)
}

func (g *gitlab) FindOpenMergeRequest(ctx context.Context, project, sourceBranch string) (*MergeRequest, error) {
	var list []gitlabMR
	q := url.Values{"state": {"opened"}, "source_branch": {sourceBranch}, "per_page": {"100"}}
	if err := g.do(ctx, http.MethodGet, gitlabProject(project)+"/merge_requests", q, nil, &list); err != nil {
		return nil, err
	}
	for _, m := range list {
		if m.SourceBranch == sourceBranch {
			return m.convert(), nil
		}
	}
	return nil, nil
}

func (g *gitlab) CreateMergeRequest(ctx context.Context, project string, req CreateRequest) (*MergeRequest, error) {
	var warnings []string
	body := map[string]any{
		"source_branch":        req.SourceBranch,
		"target_branch":        req.TargetBranch,
		"title":                req.Title,
		"description":          req.Body,
		"remove_source_branch": false,
	}
	if len(req.Labels) > 0 {
		body["labels"] = strings.Join(req.Labels, ",")
	}
	var reviewerIDs []int64
	for _, name := range req.Reviewers {
		u, err := g.LookupUser(ctx, name)
		if err != nil {
			if isNotFound(err) {
				warnings = append(warnings, fmt.Sprintf("reviewer %q not found", name))
				continue
			}
			return nil, err
		}
		reviewerIDs = append(reviewerIDs, u.ID)
	}
	if len(reviewerIDs) > 0 {
		body["reviewer_ids"] = reviewerIDs
	}
	if req.Assignee != "" {
		u, err := g.LookupUser(ctx, req.Assignee)
		switch {
		case err == nil:
			body["assignee_id"] = u.ID
		case isNotFound(err):
			warnings = append(warnings, fmt.Sprintf("assignee %q not found", req.Assignee))
		default:
			return nil, err
		}
	}
	var created gitlabMR
	if err := g.do(ctx, http.MethodPost, gitlabProject(project)+"/merge_requests", nil, body, &created); err != nil {
		return nil, err
	}
	mr := created.convert()
	mr.Warnings = warnings
	return mr, nil
}

func (g *gitlab) GetMergeRequest(ctx context.Context, project string, number int64) (*MergeRequest, error) {
	var m gitlabMR
	if err := g.do(ctx, http.MethodGet, g.mrPath(project, number), nil, nil, &m); err != nil {
		return nil, err
	}
	return m.convert(), nil
}

func (g *gitlab) CloseMergeRequest(ctx context.Context, project string, number int64) error {
	return g.do(ctx, http.MethodPut, g.mrPath(project, number), nil, map[string]string{"state_event": "close"}, nil)
}

func (g *gitlab) CommentOnMergeRequest(ctx context.Context, project string, number int64, body string) error {
	return g.do(ctx, http.MethodPost, g.mrPath(project, number)+"/notes", nil, map[string]string{"body": body}, nil)
}

func (g *gitlab) DeleteBranch(ctx context.Context, project, branch string) error {
	return g.do(ctx, http.MethodDelete, gitlabProject(project)+"/repository/branches/"+url.PathEscape(branch), nil, nil, nil)
}

func (g *gitlab) LookupUser(ctx context.Context, username string) (*User, error) {
	var users []struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
	}
	if err := g.do(ctx, http.MethodGet, "/users", url.Values{"username": {username}}, nil, &users); err != nil {
		return nil, err
	}
	for _, u := range users {
		if strings.EqualFold(u.Username, username) {
			return &User{ID: u.ID, Username: u.Username}, nil
		}
	}
	return nil, fmt.Errorf("%w: user %q", ErrNotFound, username)
}
