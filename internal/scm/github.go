package scm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

type github struct{ api }

func newGitHub(base, token string, c *http.Client) *github {
	return &github{api{
		base: GitHubAPIRoot(base),
		headers: map[string]string{
			"Authorization":        "Bearer " + token,
			"Accept":               "application/vnd.github+json",
			"X-GitHub-Api-Version": "2022-11-28",
		},
		http: c,
	}}
}

type githubPR struct {
	Number         int64  `json:"number"`
	HTMLURL        string `json:"html_url"`
	State          string `json:"state"`
	Merged         bool   `json:"merged"`
	MergeCommitSHA string `json:"merge_commit_sha"`
	Head           struct {
		Ref string `json:"ref"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

func (p githubPR) convert() *MergeRequest {
	out := &MergeRequest{
		Number: p.Number, URL: p.HTMLURL,
		SourceBranch: p.Head.Ref, TargetBranch: p.Base.Ref,
	}
	switch {
	case p.Merged:
		out.State = StateMerged
		out.MergeCommitSHA = p.MergeCommitSHA
	case p.State == "open":
		out.State = StateOpen
	default:
		out.State = StateClosed
	}
	return out
}

func githubRepo(project string) string { return "/repos/" + project }

func (g *github) prPath(project string, number int64) string {
	return githubRepo(project) + "/pulls/" + strconv.FormatInt(number, 10)
}

func (g *github) issuePath(project string, number int64) string {
	return githubRepo(project) + "/issues/" + strconv.FormatInt(number, 10)
}

func (g *github) FindOpenMergeRequest(ctx context.Context, project, sourceBranch string) (*MergeRequest, error) {
	owner, _, _ := strings.Cut(project, "/")
	var list []githubPR
	q := url.Values{"state": {"open"}, "head": {owner + ":" + sourceBranch}, "per_page": {"100"}}
	if err := g.do(ctx, http.MethodGet, githubRepo(project)+"/pulls", q, nil, &list); err != nil {
		return nil, err
	}
	for _, p := range list {
		if p.Head.Ref == sourceBranch {
			return p.convert(), nil
		}
	}
	return nil, nil
}

func (g *github) FindMergeRequest(ctx context.Context, project, sourceBranch string) (*MergeRequest, error) {
	owner, _, _ := strings.Cut(project, "/")
	var list []githubPR
	q := url.Values{"state": {"all"}, "head": {owner + ":" + sourceBranch}, "sort": {"created"}, "direction": {"desc"}, "per_page": {"100"}}
	if err := g.do(ctx, http.MethodGet, githubRepo(project)+"/pulls", q, nil, &list); err != nil {
		return nil, err
	}
	var found []*MergeRequest
	for _, p := range list {
		if p.Head.Ref == sourceBranch {
			found = append(found, p.convert())
		}
	}
	return preferOpen(found), nil
}

func (g *github) CreateMergeRequest(ctx context.Context, project string, req CreateRequest) (*MergeRequest, error) {
	body := map[string]any{
		"title": req.Title, "body": req.Body,
		"head": req.SourceBranch, "base": req.TargetBranch,
	}
	var created githubPR
	if err := g.do(ctx, http.MethodPost, githubRepo(project)+"/pulls", nil, body, &created); err != nil {
		// GitHub answers 422 when a pull request for the branches exists.
		if strings.Contains(err.Error(), "pull request already exists") {
			return nil, fmt.Errorf("%w: %v", ErrConflict, err)
		}
		return nil, err
	}
	mr := created.convert()

	// The pull request exists from here on. Optional metadata that fails is
	// reported as a warning so the caller does not retry and duplicate it.
	if len(req.Labels) > 0 {
		err := g.do(ctx, http.MethodPost, g.issuePath(project, mr.Number)+"/labels", nil,
			map[string]any{"labels": req.Labels}, nil)
		if err != nil {
			mr.Warnings = append(mr.Warnings, "labels not applied: "+err.Error())
		}
	}
	if len(req.Reviewers) > 0 {
		err := g.do(ctx, http.MethodPost, g.prPath(project, mr.Number)+"/requested_reviewers", nil,
			map[string]any{"reviewers": req.Reviewers}, nil)
		if err != nil {
			mr.Warnings = append(mr.Warnings, "reviewers not requested: "+err.Error())
		}
	}
	if req.Assignee != "" {
		err := g.do(ctx, http.MethodPost, g.issuePath(project, mr.Number)+"/assignees", nil,
			map[string]any{"assignees": []string{req.Assignee}}, nil)
		if err != nil {
			mr.Warnings = append(mr.Warnings, "assignee not set: "+err.Error())
		}
	}
	return mr, nil
}

func (g *github) GetMergeRequest(ctx context.Context, project string, number int64) (*MergeRequest, error) {
	var p githubPR
	if err := g.do(ctx, http.MethodGet, g.prPath(project, number), nil, nil, &p); err != nil {
		return nil, err
	}
	return p.convert(), nil
}

func (g *github) CloseMergeRequest(ctx context.Context, project string, number int64) error {
	return g.do(ctx, http.MethodPatch, g.prPath(project, number), nil, map[string]string{"state": "closed"}, nil)
}

func (g *github) CommentOnMergeRequest(ctx context.Context, project string, number int64, body string) error {
	return g.do(ctx, http.MethodPost, g.issuePath(project, number)+"/comments", nil, map[string]string{"body": body}, nil)
}

func (g *github) DeleteBranch(ctx context.Context, project, branch string) error {
	// Branch names may contain slashes, which are part of the ref path.
	err := g.do(ctx, http.MethodDelete, githubRepo(project)+"/git/refs/heads/"+escapePath(branch), nil, nil, nil)
	// GitHub answers 422 "Reference does not exist" for a branch that is gone.
	if errors.Is(err, ErrInvalid) && strings.Contains(strings.ToLower(err.Error()), "reference does not exist") {
		return fmt.Errorf("%w: %v", ErrNotFound, err)
	}
	return err
}

func (g *github) LookupUser(ctx context.Context, username string) (*User, error) {
	var u struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
	}
	if err := g.do(ctx, http.MethodGet, "/users/"+url.PathEscape(username), nil, nil, &u); err != nil {
		return nil, err
	}
	return &User{ID: u.ID, Username: u.Login}, nil
}

// escapePath escapes each segment of a slash-separated path.
func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

func isNotFound(err error) bool { return errors.Is(err, ErrNotFound) }
