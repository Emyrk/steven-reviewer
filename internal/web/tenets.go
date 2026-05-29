package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// TenetProposal is the model-returned shape for a global review principle.
type TenetProposal struct {
	Category  string          `json:"category"`
	Name      string          `json:"name"`
	Statement string          `json:"statement"`
	Rationale string          `json:"rationale"`
	Evidence  json.RawMessage `json:"evidence,omitempty"`
}

// Tenet is the DB-row shape used by templates.
type Tenet struct {
	ID        int64
	Category  string
	Name      string
	Statement string
	Rationale string
	Evidence  string
	Status    string
	Source    string
	Model     string
	CreatedAt string
	DecidedAt string
}

var validTenetCategories = map[string]bool{
	"backend": true, "database": true, "security": true, "api": true, "frontend": true,
	"operations": true, "testing": true, "architecture": true,
	"style": true, "process": true, "voice": true,
}

const tenetSystemPrompt = `You are distilling Steven Masley's code review taste into global review TENETS for steven-reviewer.

A tenet is not a one-off lesson. It is a stable principle the reviewer can apply across future PRs.

Return 5-12 tenets that are reinforced by the provided database excerpts. Output JSON only.

Allowed categories:
- backend
- database
- security
- api
- frontend
- operations
- testing
- architecture
- style
- process
- voice

Rules:
1. Propose only tenets supported by multiple lessons or strong canonical examples.
2. Keep each tenet concrete enough to review code against.
3. Name the principle, state the rule, then explain why Steven cares.
4. Do not preserve secrets, internal credentials, connection strings, customer names, or private Slack details.
5. Evidence must cite lesson IDs or comment IDs using objects like {"type":"lesson","id":123} or {"type":"comment","id":"..."}.
6. Avoid duplicates of existing accepted/canonical tenets.
7. Output a JSON array, nothing else.

Output format:
[
  {
    "category": "backend",
    "name": "Operation-specific errors",
    "statement": "Name the failed operation in every internal error response.",
    "rationale": "Generic 500s make production debugging depend on grep and luck.",
    "evidence": [{"type":"lesson","id":123}]
  }
]`

func normalizeTenetProposal(p TenetProposal) (TenetProposal, bool) {
	p.Category = strings.ToLower(strings.TrimSpace(p.Category))
	p.Name = strings.TrimSpace(p.Name)
	p.Statement = strings.TrimSpace(p.Statement)
	p.Rationale = strings.TrimSpace(p.Rationale)
	if !validTenetCategories[p.Category] || p.Name == "" || p.Statement == "" || p.Rationale == "" {
		return TenetProposal{}, false
	}
	if len(p.Evidence) == 0 || string(p.Evidence) == "null" {
		p.Evidence = json.RawMessage("[]")
	}
	return p, true
}

func (s *Server) gatherTenetContext(ctx context.Context) (string, error) {
	var sb strings.Builder
	sb.WriteString("# Accepted lessons\n\n")
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, status, kind, title, body, evidence
		FROM lessons
		WHERE status IN ('accepted','edited','strong','canonical','weak')
		ORDER BY CASE status
			WHEN 'canonical' THEN 0
			WHEN 'strong' THEN 1
			WHEN 'accepted' THEN 2
			WHEN 'edited' THEN 3
			WHEN 'weak' THEN 4
			ELSE 5
		END, id DESC
		LIMIT 250`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	lessonCount := 0
	for rows.Next() {
		lessonCount++
		var id int64
		var status, kind, title, body, evidence string
		if err := rows.Scan(&id, &status, &kind, &title, &body, &evidence); err != nil {
			return "", err
		}
		fmt.Fprintf(&sb, "## lesson %d [%s/%s]\n%s\n%s\nEvidence: %s\n\n", id, status, kind, title, body, evidence)
	}
	if lessonCount == 0 {
		sb.WriteString("No accepted lessons yet.\n\n")
	}

	sb.WriteString("# Whole GitHub corpus summary\n\n")
	if rrows, err := s.db.QueryContext(ctx, `
		SELECT repo, status, is_context, COUNT(*)
		FROM comments
		GROUP BY repo, status, is_context
		ORDER BY repo, status, is_context`); err == nil {
		defer rrows.Close()
		for rrows.Next() {
			var repo, status string
			var isContext, count int
			if rrows.Scan(&repo, &status, &isContext, &count) == nil {
				fmt.Fprintf(&sb, "- %s status=%s is_context=%d count=%d\n", repo, status, isContext, count)
			}
		}
		sb.WriteString("\n")
	}
	if prows, err := s.db.QueryContext(ctx, `
		SELECT repo, COUNT(*), COALESCE(SUM(authored_by_me),0)
		FROM prs
		GROUP BY repo
		ORDER BY repo`); err == nil {
		defer prows.Close()
		for prows.Next() {
			var repo string
			var total, authored int
			if prows.Scan(&repo, &total, &authored) == nil {
				fmt.Fprintf(&sb, "- %s prs=%d authored_by_me=%d\n", repo, total, authored)
			}
		}
		sb.WriteString("\n")
	}

	sb.WriteString("# Representative GitHub comments from Steven, curated and uncurated\n\n")
	crows, err := s.db.QueryContext(ctx, `
		WITH ranked AS (
			SELECT c.id, c.repo, c.pr_number, COALESCE(c.file_path,'') AS path, COALESCE(c.note,'') AS note, c.body,
			       COALESCE(GROUP_CONCAT(ct.tag, ','), '') AS tags,
			       ROW_NUMBER() OVER (
				       PARTITION BY
				       CASE
				           WHEN c.file_path LIKE 'coderd/database/%' THEN 'database'
				           WHEN c.file_path LIKE 'coderd/rbac/%' OR c.file_path LIKE '%auth%' THEN 'auth'
				           WHEN c.file_path LIKE 'site/%' THEN 'frontend'
				           WHEN c.file_path LIKE 'cli/%' THEN 'cli'
				           WHEN c.file_path LIKE '%test%' THEN 'testing'
				           WHEN c.file_path = '' THEN 'issue'
				           ELSE 'backend'
				       END
				       ORDER BY
				       CASE WHEN ct.tag IN ('hard','style','tradeoff','praise') THEN 0 ELSE 1 END,
				       c.created_at DESC
			   ) AS rn
			FROM comments c
			LEFT JOIN comment_tags ct ON ct.comment_id = c.id
			WHERE c.is_context = 0
			GROUP BY c.id
		)
		SELECT id, repo, pr_number, path, note, body, tags
		FROM ranked
		WHERE rn <= 28
		ORDER BY repo, pr_number DESC, id
		LIMIT 220`)
	if err != nil {
		return "", err
	}
	defer crows.Close()
	for crows.Next() {
		var id, repo, path, note, body, tags string
		var pr int
		if err := crows.Scan(&id, &repo, &pr, &path, &note, &body, &tags); err != nil {
			return "", err
		}
		fmt.Fprintf(&sb, "## comment %s", id)
		if tags != "" {
			fmt.Fprintf(&sb, " [%s]", tags)
		}
		fmt.Fprintf(&sb, " on %s#%d", repo, pr)
		if path != "" {
			fmt.Fprintf(&sb, " (%s)", path)
		}
		sb.WriteString("\n")
		if note != "" {
			fmt.Fprintf(&sb, "Steven note: %s\n", note)
		}
		fmt.Fprintf(&sb, "%s\n\n", truncateForPrompt(body, 900))
	}

	sb.WriteString("# Representative surrounding thread context from other reviewers\n\n")
	ctxRows, err := s.db.QueryContext(ctx, `
		SELECT id, repo, pr_number, author, COALESCE(file_path,''), body
		FROM comments
		WHERE is_context = 1
		ORDER BY created_at DESC
		LIMIT 80`)
	if err != nil {
		return "", err
	}
	defer ctxRows.Close()
	for ctxRows.Next() {
		var id, repo, author, path, body string
		var pr int
		if err := ctxRows.Scan(&id, &repo, &pr, &author, &path, &body); err != nil {
			return "", err
		}
		fmt.Fprintf(&sb, "## context %s by %s on %s#%d", id, author, repo, pr)
		if path != "" {
			fmt.Fprintf(&sb, " (%s)", path)
		}
		fmt.Fprintf(&sb, "\n%s\n\n", truncateForPrompt(body, 700))
	}

	sb.WriteString("# Existing tenets to avoid duplicating\n\n")
	trows, err := s.db.QueryContext(ctx, `
		SELECT id, status, category, name, statement
		FROM tenets
		WHERE status IN ('accepted','edited','strong','canonical','weak')
		ORDER BY id`)
	if err == nil {
		defer trows.Close()
		for trows.Next() {
			var id int64
			var status, category, name, statement string
			if trows.Scan(&id, &status, &category, &name, &statement) == nil {
				fmt.Fprintf(&sb, "- tenet %d [%s/%s] %s: %s\n", id, status, category, name, statement)
			}
		}
	}
	return sb.String(), nil
}

func truncateForPrompt(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func (s *Server) handleTenetsPropose(w http.ResponseWriter, r *http.Request) {
	if s.hm == nil {
		http.Error(w, "Hermes API not configured (set hermes.url + hermes.key in config.yml)", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Minute)
	defer cancel()
	userMsg, err := s.gatherTenetContext(ctx)
	if err != nil {
		http.Error(w, "gather tenet context: "+err.Error(), 500)
		return
	}
	raw, err := s.hm.Chat(ctx, tenetSystemPrompt, userMsg)
	if err != nil {
		http.Error(w, "hermes: "+err.Error(), 502)
		return
	}
	jsonBytes, err := extractJSONArray(raw)
	if err != nil {
		http.Error(w, fmt.Sprintf("parse model output: %v\n\nraw:\n%s", err, raw), 502)
		return
	}
	var proposals []TenetProposal
	if err := json.Unmarshal(jsonBytes, &proposals); err != nil {
		http.Error(w, fmt.Sprintf("decode tenet proposals: %v\n\njson:\n%s", err, string(jsonBytes)), 502)
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	for _, p := range proposals {
		p, ok := normalizeTenetProposal(p)
		if !ok {
			continue
		}
		_, _ = s.db.ExecContext(ctx, `
			INSERT INTO tenets (category, name, statement, rationale, evidence, status, source, model, created_at)
			VALUES (?, ?, ?, ?, ?, 'proposed', 'llm', ?, ?)`,
			p.Category, p.Name, p.Statement, p.Rationale, string(p.Evidence), s.hm.Model, now)
	}
	s.renderTenetsList(w, r)
}

func (s *Server) handleTenetDecide(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", 400)
		return
	}
	status := r.FormValue("status")
	switch status {
	case "rejected", "weak", "accepted", "strong", "canonical":
	default:
		http.Error(w, "status must be rejected|weak|accepted|strong|canonical", 400)
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := s.db.Exec(`UPDATE tenets SET status = ?, decided_at = ? WHERE id = ?`, status, now, id); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	s.renderTenetsList(w, r)
}

func (s *Server) handleTenetEdit(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", 400)
		return
	}
	category := strings.ToLower(strings.TrimSpace(r.FormValue("category")))
	name := strings.TrimSpace(r.FormValue("name"))
	statement := strings.TrimSpace(r.FormValue("statement"))
	rationale := strings.TrimSpace(r.FormValue("rationale"))
	if !validTenetCategories[category] || name == "" || statement == "" || rationale == "" {
		http.Error(w, "category, name, statement, and rationale required", 400)
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := s.db.Exec(`UPDATE tenets SET category = ?, name = ?, statement = ?, rationale = ?, status = 'edited', decided_at = ? WHERE id = ?`, category, name, statement, rationale, now, id); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	s.renderTenetsList(w, r)
}

func (s *Server) handleTenetDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", 400)
		return
	}
	if _, err := s.db.Exec(`DELETE FROM tenets WHERE id = ?`, id); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	s.renderTenetsList(w, r)
}

func (s *Server) handleTenetsList(w http.ResponseWriter, r *http.Request) {
	s.renderTenetsList(w, r)
}

func (s *Server) renderTenetsList(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	if status == "" {
		status = "proposed"
	}
	category := r.URL.Query().Get("category")
	tenets, stats, err := s.loadTenets(status, category)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	data := map[string]any{
		"Title":          "tenets · steven-reviewer",
		"Tenets":         tenets,
		"Stats":          stats,
		"StatusFilter":   status,
		"CategoryFilter": category,
		"Statuses":       []string{"proposed", "canonical", "strong", "accepted", "weak", "edited", "rejected", "all"},
		"Categories":     []string{"backend", "database", "security", "api", "frontend", "operations", "testing", "architecture", "style", "process", "voice"},
	}
	if r.Header.Get("HX-Request") == "true" {
		if err := s.tmpl.ExecuteTemplate(w, "tenets_section", data); err != nil {
			http.Error(w, err.Error(), 500)
		}
		return
	}
	if err := s.tmpl.ExecuteTemplate(w, "tenets.html", data); err != nil {
		http.Error(w, err.Error(), 500)
	}
}

func (s *Server) loadTenets(status, category string) ([]Tenet, map[string]int, error) {
	q := `SELECT id, category, name, statement, rationale, evidence, status, source,
	             COALESCE(model,''), created_at, COALESCE(decided_at,'')
	      FROM tenets WHERE 1=1`
	var args []any
	if status != "all" {
		q += ` AND status = ?`
		args = append(args, status)
	}
	if category != "" && category != "all" {
		q += ` AND category = ?`
		args = append(args, category)
	}
	q += ` ORDER BY CASE status
			WHEN 'proposed' THEN 0
			WHEN 'canonical' THEN 1
			WHEN 'strong' THEN 2
			WHEN 'accepted' THEN 3
			WHEN 'edited' THEN 4
			WHEN 'weak' THEN 5
			WHEN 'rejected' THEN 6
		END, id DESC LIMIT 500`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var out []Tenet
	for rows.Next() {
		var t Tenet
		if err := rows.Scan(&t.ID, &t.Category, &t.Name, &t.Statement, &t.Rationale, &t.Evidence, &t.Status, &t.Source, &t.Model, &t.CreatedAt, &t.DecidedAt); err != nil {
			return nil, nil, err
		}
		out = append(out, t)
	}
	stats := map[string]int{}
	srows, _ := s.db.Query(`SELECT category, COUNT(*) FROM tenets WHERE status IN ('accepted','edited','strong','canonical','weak') GROUP BY category`)
	if srows != nil {
		defer srows.Close()
		for srows.Next() {
			var k string
			var n int
			if srows.Scan(&k, &n) == nil {
				stats[k] = n
			}
		}
	}
	return out, stats, nil
}
