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
	return c.fetchCommentsPaged(ctx, repo, author, since, endpoint, parse, nil)
}

// PageHandler is called with each page of matched comments and the
// latest updated_at timestamp seen so far. It runs *inside* the fetch
// loop so the caller can checkpoint to durable storage between pages,
// making the overall fetch crash-safe.
//
// If a page handler returns an error, the fetch stops and returns it.
type PageHandler func(matches []IssueComment, maxSeen string) error

// FetchIssueCommentsPaged is like FetchIssueComments but invokes onPage
// after each successful page. Use it for large backfills.
func (c *Client) FetchIssueCommentsPaged(ctx context.Context, repo, author, since string, onPage PageHandler) ([]IssueComment, string, error) {
	return c.fetchCommentsPaged(ctx, repo, author, since, "issues/comments", parseIssueComment, onPage)
}

// FetchReviewCommentsPaged is the review_comment counterpart.
func (c *Client) FetchReviewCommentsPaged(ctx context.Context, repo, author, since string, onPage PageHandler) ([]IssueComment, string, error) {
	return c.fetchCommentsPaged(ctx, repo, author, since, "pulls/comments", parseReviewComment, onPage)
}

// PRMeta is a tiny subset of the PR object used by the random-deck view.
type PRMeta struct {
	Title        string
	Opener       string // user.login
	State        string // open | closed
	Merged       bool
	Additions    int
	Deletions    int
	ChangedFiles int
	CreatedAt    time.Time
}

// FetchPRMeta returns the minimum metadata needed to render a PR card.
// Uses /repos/{repo}/pulls/{n} which is one round-trip per PR. Cache via
// the prs table.
func (c *Client) FetchPRMeta(ctx context.Context, repo string, number int) (PRMeta, error) {
	var raw struct {
		Title        string    `json:"title"`
		State        string    `json:"state"`
		Merged       bool      `json:"merged"`
		Additions    int       `json:"additions"`
		Deletions    int       `json:"deletions"`
		ChangedFiles int       `json:"changed_files"`
		User         struct{ Login string } `json:"user"`
		CreatedAt    time.Time `json:"created_at"`
	}
	path := fmt.Sprintf("/repos/%s/pulls/%d", repo, number)
	if err := c.get(ctx, path, nil, &raw); err != nil {
		return PRMeta{}, err
	}
	return PRMeta{
		Title:        raw.Title,
		Opener:       raw.User.Login,
		State:        raw.State,
		Merged:       raw.Merged,
		Additions:    raw.Additions,
		Deletions:    raw.Deletions,
		ChangedFiles: raw.ChangedFiles,
		CreatedAt:    raw.CreatedAt,
	}, nil
}

// AuthoredPR is a row from the GH search-issues endpoint, scoped to PRs
// authored by a given user.
type AuthoredPR struct {
	Repo      string
	Number    int
	Title     string
	State     string // open | closed (search doesn't expose merged)
	CreatedAt time.Time
}

// FetchAuthoredPRs queries GitHub search for PRs in `repo` authored by
// `author`. Search caps at 1000 results; paginates 100 at a time. Sorts
// by created-desc so the newest are first.
func (c *Client) FetchAuthoredPRs(ctx context.Context, repo, author string) ([]AuthoredPR, error) {
	q := url.Values{}
	q.Set("q", fmt.Sprintf("repo:%s is:pr author:%s", repo, author))
	q.Set("per_page", "100")
	q.Set("sort", "created")
	q.Set("order", "desc")
	path := "/search/issues?" + q.Encode()

	var out []AuthoredPR
	prNumRE := regexp.MustCompile(`/pull/(\d+)$`)
	for path != "" {
		var page struct {
			Items []struct {
				Number      int       `json:"number"`
				Title       string    `json:"title"`
				State       string    `json:"state"`
				CreatedAt   time.Time `json:"created_at"`
				HTMLURL     string    `json:"html_url"`
				PullRequest *struct{} `json:"pull_request"`
			} `json:"items"`
		}
		next, err := c.getPaged(ctx, path, &page)
		if err != nil {
			return out, fmt.Errorf("search authored: %w", err)
		}
		for _, it := range page.Items {
			if it.PullRequest == nil {
				continue
			}
			n := it.Number
			if n == 0 {
				if m := prNumRE.FindStringSubmatch(it.HTMLURL); len(m) > 1 {
					n, _ = strconv.Atoi(m[1])
				}
			}
			out = append(out, AuthoredPR{
				Repo: repo, Number: n, Title: it.Title,
				State: it.State, CreatedAt: it.CreatedAt,
			})
		}
		path = next
	}
	return out, nil
}

func (c *Client) fetchCommentsPaged(
	ctx context.Context,
	repo, author, since, endpoint string,
	parse func(repo string, r rawComment) IssueComment,
	onPage PageHandler,
) ([]IssueComment, string, error) {
	var out []IssueComment
	maxSeen := since

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
			return out, maxSeen, fmt.Errorf("%s: %w", endpoint, err)
		}
		var pageMatches []IssueComment
		for _, r := range page {
			ts := r.UpdatedAt.Format(time.RFC3339)
			if ts > maxSeen {
				maxSeen = ts
			}
			if !strings.EqualFold(r.User.Login, author) {
				continue
			}
			m := parse(repo, r)
			pageMatches = append(pageMatches, m)
			out = append(out, m)
		}
		if onPage != nil {
			if err := onPage(pageMatches, maxSeen); err != nil {
				return out, maxSeen, fmt.Errorf("page handler: %w", err)
			}
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
// the Link header, or "" if no next page. Retries 5xx and 502 errors
// with exponential backoff.
func (c *Client) getPaged(ctx context.Context, path string, out any) (string, error) {
	const maxAttempts = 5
	backoff := 2 * time.Second
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		next, retry, err := c.getPagedOnce(ctx, path, out)
		if err == nil {
			return next, nil
		}
		lastErr = err
		if !retry {
			return "", err
		}
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return "", ctx.Err()
		}
		backoff *= 2
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
	return "", fmt.Errorf("after %d attempts: %w", maxAttempts, lastErr)
}

func (c *Client) getPagedOnce(ctx context.Context, path string, out any) (next string, retry bool, err error) {
	full := restBase + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return "", false, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", true, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", true, fmt.Errorf("read: %w", err)
	}
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		// GitHub returns 403 for BOTH rate-limit exhaustion AND token
		// permission denial. The reset/limit headers are populated in
		// both cases, so the only reliable signal is X-RateLimit-Remaining.
		// If it's not 0, this is a permission error — fail fast, don't sleep.
		remaining := resp.Header.Get("X-RateLimit-Remaining")
		isRateLimit := resp.StatusCode == http.StatusTooManyRequests || remaining == "0"
		if !isRateLimit {
			return "", false, fmt.Errorf("HTTP %d on %s (permission denied, remaining=%s): %s",
				resp.StatusCode, path, remaining, truncate(string(body), 300))
		}
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if secs, err := strconv.Atoi(ra); err == nil {
				select {
				case <-time.After(time.Duration(secs) * time.Second):
					return c.getPagedOnce(ctx, path, out)
				case <-ctx.Done():
					return "", false, ctx.Err()
				}
			}
		}
		if reset := resp.Header.Get("X-RateLimit-Reset"); reset != "" {
			if epoch, err := strconv.ParseInt(reset, 10, 64); err == nil {
				wait := time.Until(time.Unix(epoch, 0))
				if wait > 0 && wait < 15*time.Minute {
					select {
					case <-time.After(wait + time.Second):
						return c.getPagedOnce(ctx, path, out)
					case <-ctx.Done():
						return "", false, ctx.Err()
					}
				}
			}
		}
	}
	if resp.StatusCode >= 500 && resp.StatusCode < 600 {
		return "", true, fmt.Errorf("HTTP %d on %s: %s", resp.StatusCode, path, truncate(string(body), 200))
	}
	if resp.StatusCode != http.StatusOK {
		return "", false, fmt.Errorf("HTTP %d on %s: %s", resp.StatusCode, path, truncate(string(body), 300))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return "", false, fmt.Errorf("decode: %w", err)
	}
	return nextPageFromLink(resp.Header.Get("Link")), false, nil
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

// FetchPRThread returns every issue_comment and review_comment on a
// single PR, regardless of author. Used to gather context for the
// conversations Emyrk participated in.
//
// excludeAuthor is the login whose comments should be SKIPPED (we
// already have those from the main pull). Pass "" to keep everyone.
func (c *Client) FetchPRThread(ctx context.Context, repo string, number int, excludeAuthor string) ([]IssueComment, error) {
	var out []IssueComment

	// Issue-level comments on PR.
	issuePath := fmt.Sprintf("/repos/%s/issues/%d/comments?per_page=100", repo, number)
	for issuePath != "" {
		var page []rawComment
		next, err := c.getPaged(ctx, issuePath, &page)
		if err != nil {
			return out, fmt.Errorf("issue thread: %w", err)
		}
		for _, r := range page {
			if excludeAuthor != "" && strings.EqualFold(r.User.Login, excludeAuthor) {
				continue
			}
			m := parseIssueComment(repo, r)
			m.PRNumber = number
			out = append(out, m)
		}
		issuePath = next
	}

	// Review (inline) comments on PR.
	reviewPath := fmt.Sprintf("/repos/%s/pulls/%d/comments?per_page=100", repo, number)
	for reviewPath != "" {
		var page []rawComment
		next, err := c.getPaged(ctx, reviewPath, &page)
		if err != nil {
			return out, fmt.Errorf("review thread: %w", err)
		}
		for _, r := range page {
			if excludeAuthor != "" && strings.EqualFold(r.User.Login, excludeAuthor) {
				continue
			}
			m := parseReviewComment(repo, r)
			m.PRNumber = number
			out = append(out, m)
		}
		reviewPath = next
	}

	return out, nil
}
