// Package gh is a minimal GitHub GraphQL client scoped to the queries
// the steven-reviewer ingester needs.
package gh

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const endpoint = "https://api.github.com/graphql"

// Client is a thin GraphQL caller. It does NOT log the token under any
// circumstance; the wrapper around `do` redacts the Authorization header
// before any error formatting.
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

type graphQLRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

type graphQLError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

type graphQLResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []graphQLError  `json:"errors"`
}

// do POSTs a GraphQL query and decodes data into out. Errors never include
// the token.
func (c *Client) do(ctx context.Context, query string, vars map[string]any, out any) error {
	body, err := json.Marshal(graphQLRequest{Query: query, Variables: vars})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("graphql request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read graphql response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("graphql HTTP %d: %s", resp.StatusCode, truncate(string(raw), 500))
	}
	var gr graphQLResponse
	if err := json.Unmarshal(raw, &gr); err != nil {
		return fmt.Errorf("decode graphql response: %w", err)
	}
	if len(gr.Errors) > 0 {
		msgs := make([]string, len(gr.Errors))
		for i, e := range gr.Errors {
			msgs[i] = e.Message
		}
		return fmt.Errorf("graphql errors: %s", strings.Join(msgs, "; "))
	}
	return json.Unmarshal(gr.Data, out)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// Viewer returns the authenticated user's login. Smoke test for the token.
func (c *Client) Viewer(ctx context.Context) (string, error) {
	var out struct {
		Viewer struct {
			Login string `json:"login"`
		} `json:"viewer"`
	}
	if err := c.do(ctx, `query { viewer { login } }`, nil, &out); err != nil {
		return "", err
	}
	return out.Viewer.Login, nil
}

// IssueComment is the unified shape we store. It covers PR review
// summary comments (review), inline PR comments (review_comment), and
// issue/PR conversation comments (issue_comment).
type IssueComment struct {
	ID          string // GraphQL global node id
	Repo        string // "owner/name"
	PRNumber    int
	CommentType string // review | review_comment | issue_comment
	URL         string
	Author      string
	Body        string
	DiffHunk    string // review_comment only
	FilePath    string // review_comment only
	PRTitle     string
	PRState     string // OPEN | CLOSED | MERGED (we lowercase on insert)
	CreatedAt   time.Time
}

// QueryByAuthorOpts bounds a single fetch.
type QueryByAuthorOpts struct {
	Repo   string // "owner/name"
	Author string
	After  string // cursor; empty for first page
	Limit  int    // per-page; max 100
}

// FetchCommentsByAuthor is a placeholder for the real GraphQL query
// (issue_comment, review, review_comment in parallel). For Phase 3 v0
// we'll wire this up in a follow-up commit; the structure is here so
// the rest of the CLI can compile and the pull command has a clear seam.
func (c *Client) FetchCommentsByAuthor(ctx context.Context, opts QueryByAuthorOpts) (comments []IssueComment, nextCursor string, err error) {
	// TODO(steven-reviewer): implement multi-type comment fetch.
	// Sketch:
	//   - search { type: ISSUE, query: "repo:<repo> commenter:<author>" }
	//     to enumerate PRs/issues the author touched.
	//   - then per node, pull `comments(first: 100)`, `reviews(...)`,
	//     `reviews { comments(...) }` filtered to the author.
	//   - merge into IssueComment slice, return next page cursor.
	return nil, "", fmt.Errorf("FetchCommentsByAuthor: not implemented yet (see TODO)")
}
