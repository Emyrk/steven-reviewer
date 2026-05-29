// Package hermes is a thin OpenAI-compatible HTTP client for the local
// Hermes API server (https://hermes-agent.nousresearch.com/docs). Only one
// endpoint is used: POST /chat/completions.
package hermes

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Client talks to the local Hermes API server.
type Client struct {
	URL    string // base, e.g. http://localhost:8642/v1
	Key    string
	Model  string
	HTTP   *http.Client
}

// New constructs a Client. timeout=0 means use the default (180s).
func New(url, key, model string, timeout time.Duration) *Client {
	if timeout == 0 {
		timeout = 180 * time.Second
	}
	if model == "" {
		model = "hermes-agent"
	}
	return &Client{
		URL:   strings.TrimRight(url, "/"),
		Key:   key,
		Model: model,
		HTTP:  &http.Client{Timeout: timeout},
	}
}

type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatReq struct {
	Model    string        `json:"model"`
	Messages []ChatMessage `json:"messages"`
	// Stream / temperature etc. intentionally omitted; Hermes uses its own
	// configured model/provider — the API server ignores most knobs.
}

type chatResp struct {
	Choices []struct {
		Message ChatMessage `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// Chat sends a system + user message and returns the assistant content.
func (c *Client) Chat(ctx context.Context, system, user string) (string, error) {
	if c.URL == "" {
		return "", fmt.Errorf("hermes: URL not configured")
	}
	if c.Key == "" {
		return "", fmt.Errorf("hermes: API key not configured (set hermes.key in config.yml or HERMES_API_KEY)")
	}
	body, _ := json.Marshal(chatReq{
		Model: c.Model,
		Messages: []ChatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
	})
	req, err := http.NewRequestWithContext(ctx, "POST", c.URL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("hermes POST: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("hermes HTTP %d: %s", resp.StatusCode, truncate(string(raw), 400))
	}
	var cr chatResp
	if err := json.Unmarshal(raw, &cr); err != nil {
		return "", fmt.Errorf("hermes decode: %w (body: %s)", err, truncate(string(raw), 200))
	}
	if cr.Error != nil {
		return "", fmt.Errorf("hermes error: %s", cr.Error.Message)
	}
	if len(cr.Choices) == 0 {
		return "", fmt.Errorf("hermes: no choices in response")
	}
	return cr.Choices[0].Message.Content, nil
}

// ExtractJSONArray pulls the first JSON array out of a model response. Hermes
// sometimes wraps JSON in ```json fences or prose; this is robust to both.
// Returns the raw JSON bytes (not parsed) so the caller can decode into the
// target shape.
var fenceRE = regexp.MustCompile("(?s)```(?:json)?\\s*(\\[.*?\\])\\s*```")

func ExtractJSONArray(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	// Direct array.
	if strings.HasPrefix(s, "[") {
		if end := lastBracket(s); end > 0 {
			return []byte(s[:end+1]), nil
		}
	}
	// Fenced code block.
	if m := fenceRE.FindStringSubmatch(s); len(m) == 2 {
		return []byte(m[1]), nil
	}
	// Naked array anywhere in the string.
	if i := strings.Index(s, "["); i >= 0 {
		tail := s[i:]
		if end := lastBracket(tail); end > 0 {
			return []byte(tail[:end+1]), nil
		}
	}
	return nil, fmt.Errorf("no JSON array found in response (first 200 chars: %s)", truncate(s, 200))
}

// lastBracket finds the matching ] for a leading [.
func lastBracket(s string) int {
	depth := 0
	inStr := false
	esc := false
	for i, r := range s {
		if esc {
			esc = false
			continue
		}
		if r == '\\' && inStr {
			esc = true
			continue
		}
		if r == '"' {
			inStr = !inStr
			continue
		}
		if inStr {
			continue
		}
		if r == '[' {
			depth++
		} else if r == ']' {
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
