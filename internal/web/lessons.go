package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Emyrk/steven-reviewer/internal/hermes"
)

// LessonProposal is what the model returns. evidence is opaque JSON we
// pass through to the DB so the schema can evolve without the model
// caring.
type LessonProposal struct {
	Kind     string          `json:"kind"`     // pattern|antipattern|tradeoff|convention|gotcha
	Title    string          `json:"title"`
	Body     string          `json:"body"`
	Evidence json.RawMessage `json:"evidence,omitempty"`
}

// Lesson is the DB-row shape used by templates.
type Lesson struct {
	ID         int64
	Repo       string
	PRNumber   int
	Kind       string
	Title      string
	Body       string
	Evidence   string
	Status     string
	Source     string
	Model      string
	CreatedAt  string
	DecidedAt  string
}

var validKinds = map[string]bool{
	"pattern": true, "antipattern": true, "tradeoff": true,
	"convention": true, "gotcha": true,
}

const lessonSystemPrompt = `You are reviewing a GitHub pull request from the perspective of Steven Masley (Emyrk), a backend developer at Coder. Your job: extract 1-5 DURABLE LESSONS that should inform future code reviews. Output JSON only.

Each lesson must be one of these kinds:
- pattern:      a concrete coding/design pattern worth doing again
- antipattern:  something to avoid, with the reason
- tradeoff:     a choice with explicit pros and cons (why X over Y here)
- convention:   a team or codebase rule (naming, layout, error handling)
- gotcha:       a non-obvious failure mode or invariant

Rules:
1. Only propose lessons that are GENERALIZABLE — they should apply to other code, not just this PR. A bug fix is rarely a lesson; the underlying invariant might be.
2. Steven's voice: concise, opinionated, second-person ("prefer X" not "one might prefer X"). No hedging. No corporate tone.
3. If nothing notable, return [] — do not invent lessons to fill quota.
4. Cite specific evidence: comment IDs Steven wrote, or file paths from the diff. The "evidence" field is a JSON array of {"type": "comment", "id": <int>} or {"type": "file", "path": "..."} objects.
5. Do NOT call any tools. Read the provided context and respond with JSON only.

Output format — a JSON array, nothing else:
[
  {"kind": "pattern", "title": "...", "body": "...", "evidence": [...]},
  ...
]`

// gatherPRContext builds the user message — PR meta + Steven's comments +
// context comments + already-accepted lessons (to avoid dupes).
func (s *Server) gatherPRContext(ctx context.Context, repo string, num int) (string, error) {
	var sb strings.Builder
	// PR meta
	var title, opener, state, createdAt string
	var adds, dels, files int
	_ = s.db.QueryRowContext(ctx, `
		SELECT COALESCE(title,''), COALESCE(opener,''), COALESCE(state,''),
		       COALESCE(created_at,''), COALESCE(additions,0),
		       COALESCE(deletions,0), COALESCE(changed_files,0)
		FROM prs WHERE repo = ? AND number = ?`, repo, num).
		Scan(&title, &opener, &state, &createdAt, &adds, &dels, &files)
	fmt.Fprintf(&sb, "# PR %s#%d\n", repo, num)
	if title != "" {
		fmt.Fprintf(&sb, "Title: %s\n", title)
	}
	if opener != "" {
		fmt.Fprintf(&sb, "Author: %s\n", opener)
	}
	if state != "" {
		fmt.Fprintf(&sb, "State: %s\n", state)
	}
	if adds+dels > 0 {
		fmt.Fprintf(&sb, "Diff: +%d -%d across %d files\n", adds, dels, files)
	}
	if createdAt != "" {
		fmt.Fprintf(&sb, "Opened: %s\n", createdAt)
	}
	sb.WriteString("\n")

	// Steven's comments + their decisions
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.id, COALESCE(c.file_path,''), COALESCE(c.body,''),
		       COALESCE(c.note,''), COALESCE(c.author,''), c.is_context
		FROM comments c
		WHERE c.repo = ? AND c.pr_number = ?
		ORDER BY c.created_at`, repo, num)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	type cmt struct {
		ID            string
		Path, Body    string
		Note, Author  string
		IsContext     int
	}
	var stevens, contexts []cmt
	for rows.Next() {
		var c cmt
		if err := rows.Scan(&c.ID, &c.Path, &c.Body, &c.Note, &c.Author, &c.IsContext); err != nil {
			return "", err
		}
		if c.IsContext == 1 {
			contexts = append(contexts, c)
		} else {
			stevens = append(stevens, c)
		}
	}

	// Per-comment tags (decisions)
	tagsByID := map[string][]string{}
	if trows, err := s.db.QueryContext(ctx, `
		SELECT ct.comment_id, ct.tag
		FROM comment_tags ct
		JOIN comments c ON c.id = ct.comment_id
		WHERE c.repo = ? AND c.pr_number = ?`, repo, num); err == nil {
		defer trows.Close()
		for trows.Next() {
			var id, tag string
			if trows.Scan(&id, &tag) == nil {
				tagsByID[id] = append(tagsByID[id], tag)
			}
		}
	}

	if len(stevens) > 0 {
		sb.WriteString("## Steven's comments\n\n")
		for _, c := range stevens {
			fmt.Fprintf(&sb, "### comment %s", c.ID)
			if c.Path != "" {
				fmt.Fprintf(&sb, " (on %s)", c.Path)
			}
			if tags := tagsByID[c.ID]; len(tags) > 0 {
				fmt.Fprintf(&sb, " [tags: %s]", strings.Join(tags, ", "))
			}
			sb.WriteString("\n")
			sb.WriteString(strings.TrimSpace(c.Body))
			sb.WriteString("\n")
			if c.Note != "" {
				fmt.Fprintf(&sb, "\n_Steven's later note:_ %s\n", c.Note)
			}
			sb.WriteString("\n")
		}
	}

	// Cap context comments to keep prompt size in check
	maxCtx := 30
	if len(contexts) > 0 {
		sb.WriteString("## Conversation context (others' comments)\n\n")
		n := len(contexts)
		if n > maxCtx {
			fmt.Fprintf(&sb, "_(showing %d of %d — truncated)_\n\n", maxCtx, n)
			contexts = contexts[:maxCtx]
		}
		for _, c := range contexts {
			fmt.Fprintf(&sb, "**%s** (comment %s", c.Author, c.ID)
			if c.Path != "" {
				fmt.Fprintf(&sb, " on %s", c.Path)
			}
			sb.WriteString("):\n")
			sb.WriteString(strings.TrimSpace(c.Body))
			sb.WriteString("\n\n")
		}
	}

	// PR-level tags (Steven's weights/done state)
	if prows, err := s.db.QueryContext(ctx, `SELECT tag FROM pr_tags WHERE repo = ? AND pr_number = ?`, repo, num); err == nil {
		defer prows.Close()
		var prTags []string
		for prows.Next() {
			var t string
			if prows.Scan(&t) == nil {
				prTags = append(prTags, t)
			}
		}
		if len(prTags) > 0 {
			fmt.Fprintf(&sb, "## PR-level tags from Steven\n%s\n\n", strings.Join(prTags, ", "))
		}
	}

	// Already-accepted lessons for this PR (avoid duplicates)
	if lrows, err := s.db.QueryContext(ctx, `
		SELECT kind, title FROM lessons
		WHERE repo = ? AND pr_number = ? AND status IN ('accepted','edited','strong','canonical','weak')`, repo, num); err == nil {
		defer lrows.Close()
		var existing []string
		for lrows.Next() {
			var k, t string
			if lrows.Scan(&k, &t) == nil {
				existing = append(existing, fmt.Sprintf("- [%s] %s", k, t))
			}
		}
		if len(existing) > 0 {
			sb.WriteString("## Already accepted from this PR — do NOT propose duplicates\n")
			sb.WriteString(strings.Join(existing, "\n"))
			sb.WriteString("\n\n")
		}
	}

	sb.WriteString("---\nNow propose lessons. JSON array only.\n")
	return sb.String(), nil
}

func (s *Server) handleLessonsPropose(w http.ResponseWriter, r *http.Request) {
	if s.hm == nil {
		http.Error(w, "Hermes API not configured (set hermes.url + hermes.key in config.yml)", http.StatusServiceUnavailable)
		return
	}
	repo := r.PathValue("repo_owner") + "/" + r.PathValue("repo_name")
	num, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		http.Error(w, "bad number", 400)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Minute)
	defer cancel()
	userMsg, err := s.gatherPRContext(ctx, repo, num)
	if err != nil {
		http.Error(w, "gather context: "+err.Error(), 500)
		return
	}
	raw, err := s.hm.Chat(ctx, lessonSystemPrompt, userMsg)
	if err != nil {
		http.Error(w, "hermes: "+err.Error(), 502)
		return
	}
	jsonBytes, err := extractJSONArray(raw)
	if err != nil {
		// Surface the raw model output for debugging.
		http.Error(w, fmt.Sprintf("parse model output: %v\n\nraw:\n%s", err, raw), 502)
		return
	}
	var proposals []LessonProposal
	if err := json.Unmarshal(jsonBytes, &proposals); err != nil {
		http.Error(w, fmt.Sprintf("decode proposals: %v\n\njson:\n%s", err, string(jsonBytes)), 502)
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	inserted := 0
	for _, p := range proposals {
		p.Kind = strings.ToLower(strings.TrimSpace(p.Kind))
		if !validKinds[p.Kind] {
			continue
		}
		p.Title = strings.TrimSpace(p.Title)
		p.Body = strings.TrimSpace(p.Body)
		if p.Title == "" || p.Body == "" {
			continue
		}
		ev := string(p.Evidence)
		if ev == "" || ev == "null" {
			ev = "[]"
		}
		_, err := s.db.ExecContext(ctx, `
			INSERT INTO lessons (repo, pr_number, kind, title, body, evidence, status, source, model, created_at)
			VALUES (?, ?, ?, ?, ?, ?, 'proposed', 'llm', ?, ?)`,
			repo, num, p.Kind, p.Title, p.Body, ev, s.hm.Model, now)
		if err == nil {
			inserted++
		}
	}
	// Return the lessons section HTML for HTMX swap.
	s.renderLessonsSection(w, r, repo, num)
	_ = inserted
}

// extractJSONArray re-exposes hermes.ExtractJSONArray without forcing
// callers to import the hermes package.
func extractJSONArray(s string) ([]byte, error) {
	return hermesExtract(s)
}

func hermesExtract(s string) ([]byte, error) {
	return hermes.ExtractJSONArray(s)
}

// handleLessonDecide accepts/rejects a proposed lesson.
//
//	form: status=accepted|rejected
func (s *Server) handleLessonDecide(w http.ResponseWriter, r *http.Request) {
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
	var repo string
	var num int
	if err := s.db.QueryRow(`SELECT repo, pr_number FROM lessons WHERE id = ?`, id).Scan(&repo, &num); err != nil {
		http.Error(w, "lesson not found", 404)
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := s.db.Exec(`UPDATE lessons SET status = ?, decided_at = ? WHERE id = ?`, status, now, id); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	s.renderLessonsSection(w, r, repo, num)
}

// handleLessonEdit lets Steven rewrite a lesson; sets status='edited'.
func (s *Server) handleLessonEdit(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", 400)
		return
	}
	title := strings.TrimSpace(r.FormValue("title"))
	body := strings.TrimSpace(r.FormValue("body"))
	kind := strings.ToLower(strings.TrimSpace(r.FormValue("kind")))
	if title == "" || body == "" {
		http.Error(w, "title and body required", 400)
		return
	}
	if kind != "" && !validKinds[kind] {
		http.Error(w, "invalid kind", 400)
		return
	}
	var repo string
	var num int
	if err := s.db.QueryRow(`SELECT repo, pr_number FROM lessons WHERE id = ?`, id).Scan(&repo, &num); err != nil {
		http.Error(w, "lesson not found", 404)
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	q := `UPDATE lessons SET title = ?, body = ?, status = 'edited', decided_at = ?`
	args := []any{title, body, now}
	if kind != "" {
		q += `, kind = ?`
		args = append(args, kind)
	}
	q += ` WHERE id = ?`
	args = append(args, id)
	if _, err := s.db.Exec(q, args...); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	s.renderLessonsSection(w, r, repo, num)
}

func (s *Server) handleLessonDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", 400)
		return
	}
	var repo string
	var num int
	if err := s.db.QueryRow(`SELECT repo, pr_number FROM lessons WHERE id = ?`, id).Scan(&repo, &num); err != nil {
		http.Error(w, "lesson not found", 404)
		return
	}
	if _, err := s.db.Exec(`DELETE FROM lessons WHERE id = ?`, id); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	s.renderLessonsSection(w, r, repo, num)
}

// renderLessonsSection writes the lessons section template (for HTMX swap).
func (s *Server) renderLessonsSection(w http.ResponseWriter, r *http.Request, repo string, num int) {
	lessons, err := s.loadLessons(repo, num)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	data := map[string]any{
		"Repo":     repo,
		"PRNumber": num,
		"Lessons":  lessons,
	}
	if err := s.tmpl.ExecuteTemplate(w, "lessons_section", data); err != nil {
		http.Error(w, err.Error(), 500)
	}
}

func (s *Server) loadLessons(repo string, num int) ([]Lesson, error) {
	rows, err := s.db.Query(`
		SELECT id, repo, pr_number, kind, title, body, evidence, status, source,
		       COALESCE(model,''), created_at, COALESCE(decided_at,'')
		FROM lessons WHERE repo = ? AND pr_number = ?
		ORDER BY CASE status
		           WHEN 'proposed'  THEN 0
		           WHEN 'canonical' THEN 1
		           WHEN 'strong'    THEN 2
		           WHEN 'accepted'  THEN 3
		           WHEN 'edited'    THEN 4
		           WHEN 'weak'      THEN 5
		           WHEN 'rejected'  THEN 6
		         END, id`, repo, num)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Lesson
	for rows.Next() {
		var l Lesson
		if err := rows.Scan(&l.ID, &l.Repo, &l.PRNumber, &l.Kind, &l.Title, &l.Body,
			&l.Evidence, &l.Status, &l.Source, &l.Model, &l.CreatedAt, &l.DecidedAt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, nil
}

// handleLessonsList renders /lessons — accepted+edited+proposed across all PRs.
func (s *Server) handleLessonsList(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	if status == "" {
		status = "accepted"
	}
	kind := r.URL.Query().Get("kind")
	q := `SELECT id, repo, pr_number, kind, title, body, evidence, status, source,
	             COALESCE(model,''), created_at, COALESCE(decided_at,'')
	      FROM lessons WHERE 1=1`
	var args []any
	if status != "all" {
		// Comma-separated: e.g. "accepted,edited"
		parts := strings.Split(status, ",")
		placeholders := strings.Repeat("?,", len(parts))
		placeholders = strings.TrimRight(placeholders, ",")
		q += ` AND status IN (` + placeholders + `)`
		for _, p := range parts {
			args = append(args, strings.TrimSpace(p))
		}
	}
	if kind != "" && kind != "all" {
		q += ` AND kind = ?`
		args = append(args, kind)
	}
	q += ` ORDER BY COALESCE(decided_at, created_at) DESC LIMIT 500`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer rows.Close()
	var out []Lesson
	for rows.Next() {
		var l Lesson
		if err := rows.Scan(&l.ID, &l.Repo, &l.PRNumber, &l.Kind, &l.Title, &l.Body,
			&l.Evidence, &l.Status, &l.Source, &l.Model, &l.CreatedAt, &l.DecidedAt); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		out = append(out, l)
	}
	// Stats per kind
	stats := map[string]int{}
	srows, _ := s.db.Query(`SELECT kind, COUNT(*) FROM lessons WHERE status IN ('accepted','edited','strong','canonical','weak') GROUP BY kind`)
	if srows != nil {
		for srows.Next() {
			var k string
			var n int
			if srows.Scan(&k, &n) == nil {
				stats[k] = n
			}
		}
		srows.Close()
	}
	data := map[string]any{
		"Title":        "lessons · steven-reviewer",
		"Lessons":      out,
		"StatusFilter": status,
		"KindFilter":   kind,
		"Stats":        stats,
		"Kinds":        []string{"pattern", "antipattern", "tradeoff", "convention", "gotcha"},
		"Statuses":     []string{"accepted,strong,canonical", "canonical", "strong", "accepted", "weak", "edited", "proposed", "rejected", "all"},
	}
	if err := s.tmpl.ExecuteTemplate(w, "lessons_list.html", data); err != nil {
		http.Error(w, err.Error(), 500)
	}
}

// sanity check that the lessons table exists; serve startup will hard-fail
// if migrations didn't run.
var _ = sql.ErrNoRows
