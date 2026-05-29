// Package gh is a minimal GitHub REST client scoped to the queries the
// steven-reviewer ingester needs.
package gh

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const restBase = "https://api.github.com"

// Client wraps net/http with token auth. The token is never logged or
// returned in errors.
type Client struct {
	token string
	http  *http.Client
}

// New returns a Client that uses the given PAT.
func New(token string) *Client {
	return &Client{
		token: token,
		http:  &http.Client{Timeout: 30 * time.Second},
	}
}

// IssueComment is the unified shape we store. It covers PR/issue
// conversation comments (issue_comment) and inline PR review comments
// (review_comment). Review summary objects (type "review") are not yet
// fetched; see TODO in main.go.
type IssueComment struct {
	ID          string // GitHub REST node_id
	Repo        string // "owner/name"
	PRNumber    int
	CommentType string // issue_comment | review_comment
	URL         string
	Author      string
	Body        string
	DiffHunk    string // review_comment only
	FilePath    string // review_comment only
	CreatedAt   time.Time
}

// Viewer returns the authenticated user's login.
func (c *Client) Viewer(ctx context.Context) (string, error) {
	var out struct {
		Login string `json:"login"`
	}
	if err := c.get(ctx, "/user", nil, &out); err != nil {
		return "", err
	}
	return out.Login, nil
}

// FetchIssueComments paginates /repos/{owner}/{repo}/issues/comments,
// filters to author, and returns matching comments plus the most recent
// `updated_at` value seen (use as next `since`).
//
// since may be empty (== beginning of time).
func (c *Client) FetchIssueComments(ctx context.Context, repo, author, since string) ([]IssueComment, string, error) {
	return c.fetchComments(ctx, repo, author, since, "issues/comments", parseIssueComment)
}

// FetchReviewComments paginates /repos/{owner}/{repo}/pulls/comments,
// filters to author, and returns matching comments plus the most recent
// `updated_at` value seen.
func (c *Client) FetchReviewComments(ctx context.Context, repo, author, since string) ([]IssueComment, string, error) {
	return c.fetchComments(ctx, repo, author, since, "pulls/comments", parseReviewComment)
}

type rawComment struct {
	ID               int64     `json:"id"`
	NodeID           string    `json:"node_id"`
	URL              string    `json:"html_url"`
	IssueURL         string    `json:"issue_url"`
	PullRequestURL   string    `json:"pull_request_url"`
	Body             string    `json:"body"`
	User             struct{ Login string } `json:"user"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
	DiffHunk         string    `json:"diff_hunk"`
	Path             string    `json:"path"`
}

var numRE = regexp.MustCompile(`/(?:issues|pulls)/(\d+)$`)

func prNumberFromURL(u string) int {
	m := numRE.FindStringSubmatch(u)
	if len(m) < 2 {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

func parseIssueComment(repo string, r rawComment) IssueComment {
	return IssueComment{
		ID:          r.NodeID,
		Repo:        repo,
		PRNumber:    prNumberFromURL(r.IssueURL),
		CommentType: "issue_comment",
		URL:         r.URL,
		Author:      r.User.Login,
		Body:        r.Body,
		CreatedAt:   r.CreatedAt,
	}
}

func parseReviewComment(repo string, r rawComment) IssueComment {
	return IssueComment{
		ID:          r.NodeID,
		Repo:        repo,
		PRNumber:    prNumberFromURL(r.PullRequestURL),
		CommentType: "review_comment",
		URL:         r.URL,
		Author:      r.User.Login,
		Body:        r.Body,
		DiffHunk:    r.DiffHunk,
		FilePath:    r.Path,
		CreatedAt:   r.CreatedAt,
	}
}

func (c *Client) fetchComments(
	ctx context.Context,
	repo, author, since, endpoint string,
	parse func(repo string, r rawComment) IssueComment,
) ([]IssueComment, string, error) {
	var out []IssueComment
	maxSeen := since

	// Sort by updated asc so we can checkpoint as we go (resumable on crash).
	q := url.Values{}
	q.Set("per_page", "100")
	q.Set("sort", "updated")
	q.Set("direction", "asc")
	if since != "" {
		q.Set("since", since)
	}

	path := fmt.Sprintf("/repos/%s/%s?%s", repo, endpoint, q.Encode())

	for path != "" {
		var page []rawComment
		nextPath, err := c.getPaged(ctx, path, &page)
		if err != nil {
			return nil, "", fmt.Errorf("%s: %w", endpoint, err)
		}
		for _, r := range page {
			ts := r.UpdatedAt.Format(time.RFC3339)
			if ts > maxSeen {
				maxSeen = ts
			}
			if !strings.EqualFold(r.User.Login, author) {
				continue
			}
			out = append(out, parse(repo, r))
		}
		path = nextPath
	}
	return out, maxSeen, nil
}

// get performs GET <path> (path may include query). path must start with
// "/". Decodes JSON into out.
func (c *Client) get(ctx context.Context, path string, _ url.Values, out any) error {
	_, err := c.getPaged(ctx, path, out)
	return err
}

// getPaged is like get but also returns the next-page path parsed from
// the Link header, or "" if no next page.
func (c *Client) getPaged(ctx context.Context, path string, out any) (string, error) {
	full := restBase + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read: %w", err)
	}
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		// rate limited
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if secs, err := strconv.Atoi(ra); err == nil {
				select {
				case <-time.After(time.Duration(secs) * time.Second):
					return c.getPaged(ctx, path, out)
				case <-ctx.Done():
					return "", ctx.Err()
				}
			}
		}
		// rate limit by X-RateLimit-Reset
		if reset := resp.Header.Get("X-RateLimit-Reset"); reset != "" {
			if epoch, err := strconv.ParseInt(reset, 10, 64); err == nil {
				wait := time.Until(time.Unix(epoch, 0))
				if wait > 0 && wait < 15*time.Minute {
					select {
					case <-time.After(wait + time.Second):
						return c.getPaged(ctx, path, out)
					case <-ctx.Done():
						return "", ctx.Err()
					}
				}
			}
		}
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d on %s: %s", resp.StatusCode, path, truncate(string(body), 300))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return "", fmt.Errorf("decode: %w", err)
	}
	return nextPageFromLink(resp.Header.Get("Link")), nil
}

// nextPageFromLink parses an RFC 5988 Link header for rel="next" and
// returns the path component, or "" if absent.
func nextPageFromLink(link string) string {
	for _, part := range strings.Split(link, ",") {
		part = strings.TrimSpace(part)
		if !strings.Contains(part, `rel="next"`) {
			continue
		}
		// part looks like: <https://api.github.com/...>; rel="next"
		i := strings.Index(part, "<")
		j := strings.Index(part, ">")
		if i < 0 || j < 0 || j <= i {
			return ""
		}
		full := part[i+1 : j]
		if u, err := url.Parse(full); err == nil {
			out := u.Path
			if u.RawQuery != "" {
				out += "?" + u.RawQuery
			}
			return out
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
