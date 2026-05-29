// Package web hosts the HTTP handlers for the steven-reviewer viewer.
package web

import (
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"

	"github.com/Emyrk/steven-reviewer/internal/model"
	"github.com/alecthomas/chroma/v2"
	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	gmhtml "github.com/yuin/goldmark/renderer/html"
)

//go:embed templates/*.html static/*
var assets embed.FS

// Server is the HTTP layer.
type Server struct {
	db   *sql.DB
	tmpl *template.Template
	md   goldmark.Markdown
}

// NewServer constructs a Server. The database must already be migrated.
func NewServer(db *sql.DB) (*Server, error) {
	funcs := template.FuncMap{
		"ucFirst": func(s string) string {
			if s == "" {
				return ""
			}
			return strings.ToUpper(s[:1]) + s[1:]
		},
		"shortBody": func(s string) string {
			s = strings.TrimSpace(s)
			if len(s) > 180 {
				return s[:177] + "..."
			}
			return s
		},
		"replaceAll": strings.ReplaceAll,
		"split":      strings.Split,
		"hasPrefix":  strings.HasPrefix,
		"list":       func(items ...string) []string { return items },
		"add":        func(a, b int) int { return a + b },
	}
	tmpl, err := template.New("").Funcs(funcs).ParseFS(assets, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	md := goldmark.New(
		goldmark.WithExtensions(extension.GFM),
		goldmark.WithParserOptions(parser.WithAutoHeadingID()),
		goldmark.WithRendererOptions(gmhtml.WithUnsafe()),
	)
	return &Server{db: db, tmpl: tmpl, md: md}, nil
}

// Routes returns the mux.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleList)
	mux.HandleFunc("GET /prs", s.handlePRList)
	mux.HandleFunc("GET /pr/{repo_owner}/{repo_name}/{number}", s.handlePRDetail)
	mux.HandleFunc("POST /pr/{repo_owner}/{repo_name}/{number}/tag", s.handlePRTag)
	mux.HandleFunc("GET /c/{id}", s.handleDetail)
	mux.HandleFunc("POST /c/{id}/triage", s.handleTriage)
	mux.HandleFunc("POST /c/{id}/context", s.handleContext)
	mux.HandleFunc("GET /help", s.handleHelp)
	mux.HandleFunc("GET /api/comments", s.handleAPIList)

	mux.Handle("GET /static/", http.FileServer(http.FS(assets)))
	return mux
}

func (s *Server) handleHelp(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{"Title": "help · steven-reviewer"}
	if err := s.tmpl.ExecuteTemplate(w, "help.html", data); err != nil {
		http.Error(w, err.Error(), 500)
	}
}

type listRow struct {
	ID          string
	Repo        string
	PRNumber    int
	CommentType string
	URL         string
	Body        string
	Status      string
	Decision    sql.NullString
	CreatedAt   string
	FilePath    sql.NullString
}

func (s *Server) queryList(repoFilter, statusFilter string, limit int) ([]listRow, error) {
	q := `SELECT id, repo, pr_number, comment_type, url, body, status, decision, created_at, file_path
	      FROM comments WHERE 1=1`
	var args []any
	if repoFilter != "" && repoFilter != "all" {
		q += " AND repo = ?"
		args = append(args, repoFilter)
	}
	if statusFilter != "" && statusFilter != "all" {
		q += " AND status = ?"
		args = append(args, statusFilter)
	}
	q += " ORDER BY created_at DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []listRow
	for rows.Next() {
		var r listRow
		if err := rows.Scan(&r.ID, &r.Repo, &r.PRNumber, &r.CommentType, &r.URL, &r.Body,
			&r.Status, &r.Decision, &r.CreatedAt, &r.FilePath); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

type repoCount struct {
	Repo  string
	Total int
}

type statusCount struct {
	Status string
	Count  int
}

func (s *Server) queryRepoCounts() ([]repoCount, error) {
	rows, err := s.db.Query(`SELECT repo, COUNT(*) FROM comments GROUP BY repo ORDER BY repo`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []repoCount
	for rows.Next() {
		var r repoCount
		if err := rows.Scan(&r.Repo, &r.Total); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

func (s *Server) queryStatusCounts(repoFilter string) ([]statusCount, error) {
	q := `SELECT status, COUNT(*) FROM comments`
	var args []any
	if repoFilter != "" && repoFilter != "all" {
		q += " WHERE repo = ?"
		args = append(args, repoFilter)
	}
	q += " GROUP BY status ORDER BY status"
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []statusCount
	for rows.Next() {
		var sc statusCount
		if err := rows.Scan(&sc.Status, &sc.Count); err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, nil
}

// decision is one row in the triage button grid.
type decision struct {
	Key  string
	Value string
	Desc string
}

var triageDecisions = []decision{
	{"h", "hard", "blocking rule, always flag"},
	{"s", "soft", "preference, mention if relevant"},
	{"p", "personal", "my taste, won't push"},
	{"t", "tradeoff", "explains the why; no rule"},
	{"c", "style", "concrete code pattern"},
	{"r", "praise", "good pattern, voice sample"},
	{"k", "skip", "noise / not training data"},
	{"n", "needs-thought", "come back later"},
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	repoFilter := r.URL.Query().Get("repo")
	statusFilter := r.URL.Query().Get("status")
	if statusFilter == "" {
		statusFilter = "pending"
	}
	limit := 100

	rows, err := s.queryList(repoFilter, statusFilter, limit)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	repos, _ := s.queryRepoCounts()
	statuses, _ := s.queryStatusCounts(repoFilter)

	data := map[string]any{
		"Title":        "list · steven-reviewer",
		"Rows":         rows,
		"Repos":        repos,
		"Statuses":     statuses,
		"RepoFilter":   repoFilter,
		"StatusFilter": statusFilter,
		"Count":        len(rows),
		"Limit":        limit,
	}
	if err := s.tmpl.ExecuteTemplate(w, "list.html", data); err != nil {
		http.Error(w, err.Error(), 500)
	}
}

func (s *Server) handleDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var c model.Comment
	err := s.db.QueryRow(`
		SELECT id, repo, pr_number, comment_type, url, author, body,
		       COALESCE(diff_hunk, ''), COALESCE(file_path, ''),
		       COALESCE(pr_title, ''), created_at, status,
		       COALESCE(decision, ''), COALESCE(routed_to, ''), COALESCE(note, '')
		FROM comments WHERE id = ?`, id,
	).Scan(&c.ID, &c.Repo, &c.PRNumber, &c.CommentType, &c.URL, &c.Author, &c.Body,
		&c.DiffHunk, &c.FilePath, &c.PRTitle, &c.CreatedAt, &c.Status,
		&c.Decision, &c.RoutedTo, &c.Note)
	if err == sql.ErrNoRows {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	// Prev/next in current pending stream (within same repo).
	prev, next := s.adjacent(c.ID, c.Repo, c.CreatedAt)

	// Render body markdown.
	var buf strings.Builder
	if err := s.md.Convert([]byte(c.Body), &buf); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	bodyHTML := template.HTML(buf.String())

	// Highlight diff hunk.
	diffHTML := template.HTML("")
	if c.DiffHunk != "" {
		diffHTML = template.HTML(highlightDiff(c.DiffHunk))
	}

	data := map[string]any{
		"Title":     c.Repo + "#" + strconv.Itoa(c.PRNumber) + " · steven-reviewer",
		"C":         c,
		"BodyHTML":  bodyHTML,
		"DiffHTML":  diffHTML,
		"Prev":      prev,
		"Next":      next,
		"Decisions": triageDecisions,
	}
	if err := s.tmpl.ExecuteTemplate(w, "detail.html", data); err != nil {
		http.Error(w, err.Error(), 500)
	}
}

func (s *Server) adjacent(id, repo, createdAt string) (prevID, nextID string) {
	_ = s.db.QueryRow(`
		SELECT id FROM comments
		WHERE repo = ? AND status = 'pending' AND created_at < ?
		ORDER BY created_at DESC LIMIT 1`, repo, createdAt).Scan(&prevID)
	_ = s.db.QueryRow(`
		SELECT id FROM comments
		WHERE repo = ? AND status = 'pending' AND created_at > ?
		ORDER BY created_at ASC LIMIT 1`, repo, createdAt).Scan(&nextID)
	return prevID, nextID
}

func (s *Server) handleTriage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	decision := r.FormValue("decision")
	note := r.FormValue("note")
	redirectTo := r.FormValue("redirect_to")
	allowed := map[string]bool{
		"hard": true, "soft": true, "personal": true, "tradeoff": true,
		"style": true, "praise": true, "skip": true, "needs-thought": true,
	}
	if !allowed[decision] {
		http.Error(w, "bad decision", 400)
		return
	}
	// Toggle in comment_tags. If the tag already exists, remove it.
	// Otherwise add it.
	var existing int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM comment_tags WHERE comment_id = ? AND tag = ?`, id, decision).Scan(&existing)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if existing > 0 {
		if _, err := s.db.Exec(`DELETE FROM comment_tags WHERE comment_id = ? AND tag = ?`, id, decision); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
	} else {
		if _, err := s.db.Exec(`INSERT INTO comment_tags (comment_id, tag, added_at) VALUES (?, ?, datetime('now'))`, id, decision); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
	}
	// Recompute status + canonical decision from the tag set.
	if err := s.recomputeStatus(id, note); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	if redirectTo != "" && strings.HasPrefix(redirectTo, "/") {
		http.Redirect(w, r, redirectTo, http.StatusSeeOther)
		return
	}
	var repo, createdAt string
	_ = s.db.QueryRow(`SELECT repo, created_at FROM comments WHERE id = ?`, id).Scan(&repo, &createdAt)
	var nextID string
	_ = s.db.QueryRow(`
		SELECT id FROM comments
		WHERE repo = ? AND status = 'pending' AND created_at > ?
		ORDER BY created_at ASC LIMIT 1`, repo, createdAt).Scan(&nextID)
	if nextID != "" {
		http.Redirect(w, r, "/c/"+nextID, http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/?repo="+repo+"&status=pending", http.StatusSeeOther)
}

// recomputeStatus derives status + decision from the current comment_tags
// set. Priority: any decision tag -> routed; only skip -> skipped; only
// needs-thought -> needs-thought; no tags -> pending. The single
// `decision` column gets the first non-skip/non-needs-thought tag for
// backwards compatibility.
func (s *Server) recomputeStatus(id, note string) error {
	rows, err := s.db.Query(`SELECT tag FROM comment_tags WHERE comment_id = ? ORDER BY added_at`, id)
	if err != nil {
		return err
	}
	var tags []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			rows.Close()
			return err
		}
		tags = append(tags, t)
	}
	rows.Close()

	var status, decision string
	switch {
	case len(tags) == 0:
		status = "pending"
	case len(tags) == 1 && tags[0] == "skip":
		status = "skipped"
		decision = "skip"
	case len(tags) == 1 && tags[0] == "needs-thought":
		status = "needs-thought"
		decision = "needs-thought"
	default:
		status = "routed"
		for _, t := range tags {
			if t != "skip" && t != "needs-thought" {
				decision = t
				break
			}
		}
		if decision == "" {
			decision = tags[0]
		}
	}
	var nullDec any = decision
	if decision == "" {
		nullDec = nil
	}
	noteUpdate := ""
	if note != "" {
		noteUpdate = ", note = ?"
	}
	q := `UPDATE comments SET status = ?, decision = ?, triaged_at = datetime('now')` + noteUpdate + ` WHERE id = ?`
	args := []any{status, nullDec}
	if note != "" {
		args = append(args, note)
	}
	args = append(args, id)
	_, err = s.db.Exec(q, args...)
	return err
}

func (s *Server) handleContext(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	redirectTo := r.FormValue("redirect_to")
	// Toggle the flag.
	var cur int
	_ = s.db.QueryRow(`SELECT needs_more_context FROM comments WHERE id = ?`, id).Scan(&cur)
	next := 1 - cur
	if _, err := s.db.Exec(`UPDATE comments SET needs_more_context = ? WHERE id = ?`, next, id); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if redirectTo != "" && strings.HasPrefix(redirectTo, "/") {
		http.Redirect(w, r, redirectTo, http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/c/"+id, http.StatusSeeOther)
}

func (s *Server) handlePRTag(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("repo_owner")
	name := r.PathValue("repo_name")
	repo := owner + "/" + name
	numStr := r.PathValue("number")
	num, err := strconv.Atoi(numStr)
	if err != nil {
		http.Error(w, "bad pr number", 400)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	tag := strings.TrimSpace(r.FormValue("tag"))
	note := strings.TrimSpace(r.FormValue("note"))
	remove := r.FormValue("remove")
	if tag == "" {
		http.Error(w, "tag required", 400)
		return
	}
	if remove != "" {
		_, _ = s.db.Exec(`DELETE FROM pr_tags WHERE repo = ? AND pr_number = ? AND tag = ?`, repo, num, tag)
	} else {
		// weight:* tags are mutually exclusive — clicking a new weight clears
		// any prior weight on the same PR. Same for review:* (review-quality).
		for _, prefix := range []string{"weight:", "review:"} {
			if strings.HasPrefix(tag, prefix) {
				_, _ = s.db.Exec(`DELETE FROM pr_tags WHERE repo = ? AND pr_number = ? AND tag LIKE ? AND tag != ?`,
					repo, num, prefix+"%", tag)
			}
		}
		_, err = s.db.Exec(`
			INSERT INTO pr_tags (repo, pr_number, tag, note, added_at)
			VALUES (?, ?, ?, NULLIF(?, ''), datetime('now'))
			ON CONFLICT(repo, pr_number, tag) DO UPDATE SET note = excluded.note, added_at = datetime('now')`,
			repo, num, tag, note)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
	}
	http.Redirect(w, r, fmt.Sprintf("/pr/%s/%s/%d", owner, name, num), http.StatusSeeOther)
}

func (s *Server) handleAPIList(w http.ResponseWriter, r *http.Request) {
	repoFilter := r.URL.Query().Get("repo")
	statusFilter := r.URL.Query().Get("status")
	if statusFilter == "" {
		statusFilter = "pending"
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.queryList(repoFilter, statusFilter, limit)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rows)
}

// --- PR-grouped views ---

type prGroup struct {
	Repo        string
	PRNumber    int
	Total       int
	Pending     int
	Routed      int
	Skipped     int
	LastComment string
	Types       string // comma-separated set
	Weight      string // "skip" | "low" | "high" | "canonical" | "" (normal/unset)
}

func (s *Server) queryPRGroups(repoFilter, statusFilter string, limit int) ([]prGroup, error) {
	// Group by PR with counts per status. Apply repo filter; apply status
	// filter as a HAVING-ish predicate (PR appears only if it has at least
	// one comment in that status, unless status=all).
	where := "1=1"
	var args []any
	if repoFilter != "" && repoFilter != "all" {
		where += " AND repo = ?"
		args = append(args, repoFilter)
	}
	q := fmt.Sprintf(`
		SELECT repo, pr_number,
		       COUNT(*) AS total,
		       SUM(CASE WHEN status='pending' THEN 1 ELSE 0 END) AS pending,
		       SUM(CASE WHEN status='routed' THEN 1 ELSE 0 END) AS routed,
		       SUM(CASE WHEN status='skipped' THEN 1 ELSE 0 END) AS skipped,
		       MAX(created_at) AS last_at,
		       GROUP_CONCAT(DISTINCT comment_type) AS types
		FROM comments
		WHERE %s
		GROUP BY repo, pr_number`, where)
	if statusFilter == "pending" {
		q += " HAVING pending > 0"
	} else if statusFilter == "routed" {
		q += " HAVING routed > 0"
	} else if statusFilter == "skipped" {
		q += " HAVING skipped > 0"
	}
	q += " ORDER BY last_at DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []prGroup
	for rows.Next() {
		var g prGroup
		if err := rows.Scan(&g.Repo, &g.PRNumber, &g.Total, &g.Pending, &g.Routed, &g.Skipped, &g.LastComment, &g.Types); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, nil
}

func (s *Server) handlePRList(w http.ResponseWriter, r *http.Request) {
	repoFilter := r.URL.Query().Get("repo")
	statusFilter := r.URL.Query().Get("status")
	if statusFilter == "" {
		statusFilter = "pending"
	}
	limit := 200
	groups, err := s.queryPRGroups(repoFilter, statusFilter, limit)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	// Attach weight tag per PR.
	if len(groups) > 0 {
		weights := map[string]string{}
		wrows, werr := s.db.Query(`SELECT repo, pr_number, tag FROM pr_tags WHERE tag LIKE 'weight:%'`)
		if werr == nil {
			for wrows.Next() {
				var r string
				var n int
				var t string
				if wrows.Scan(&r, &n, &t) == nil {
					weights[fmt.Sprintf("%s#%d", r, n)] = strings.TrimPrefix(t, "weight:")
				}
			}
			wrows.Close()
		}
		for i := range groups {
			groups[i].Weight = weights[fmt.Sprintf("%s#%d", groups[i].Repo, groups[i].PRNumber)]
		}
	}
	repos, _ := s.queryRepoCounts()
	data := map[string]any{
		"Title":         "PRs · steven-reviewer",
		"Groups":        groups,
		"Repos":         repos,
		"RepoFilter":    repoFilter,
		"StatusFilter":  statusFilter,
		"StatusOptions": []string{"pending", "routed", "skipped", "all"},
		"Count":         len(groups),
		"Limit":         limit,
	}
	if err := s.tmpl.ExecuteTemplate(w, "pr_list.html", data); err != nil {
		http.Error(w, err.Error(), 500)
	}
}

type prDetailComment struct {
	model.Comment
	BodyHTML         template.HTML
	DiffHTML         template.HTML
	Tags             []string
	NeedsMoreContext bool
}

func (s *Server) handlePRDetail(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("repo_owner")
	name := r.PathValue("repo_name")
	repo := owner + "/" + name
	numStr := r.PathValue("number")
	num, err := strconv.Atoi(numStr)
	if err != nil {
		http.Error(w, "bad pr number", 400)
		return
	}
	rows, err := s.db.Query(`
		SELECT id, repo, pr_number, comment_type, url, author, body,
		       COALESCE(diff_hunk, ''), COALESCE(file_path, ''),
		       COALESCE(pr_title, ''), created_at, status,
		       COALESCE(decision, ''), COALESCE(routed_to, ''), COALESCE(note, ''),
		       needs_more_context
		FROM comments
		WHERE repo = ? AND pr_number = ?
		ORDER BY created_at ASC`, repo, num)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer rows.Close()
	var comments []prDetailComment
	for rows.Next() {
		var c model.Comment
		var nmc int
		if err := rows.Scan(&c.ID, &c.Repo, &c.PRNumber, &c.CommentType, &c.URL, &c.Author, &c.Body,
			&c.DiffHunk, &c.FilePath, &c.PRTitle, &c.CreatedAt, &c.Status,
			&c.Decision, &c.RoutedTo, &c.Note, &nmc); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		var bbuf strings.Builder
		_ = s.md.Convert([]byte(c.Body), &bbuf)
		var dhtml template.HTML
		if c.DiffHunk != "" {
			dhtml = template.HTML(highlightDiff(c.DiffHunk))
		}
		comments = append(comments, prDetailComment{
			Comment:          c,
			BodyHTML:         template.HTML(bbuf.String()),
			DiffHTML:         dhtml,
			NeedsMoreContext: nmc == 1,
		})
	}
	if len(comments) == 0 {
		http.NotFound(w, r)
		return
	}
	// Load tags per comment (one query, then bucketed).
	tagMap := map[string][]string{}
	tagRows, err := s.db.Query(`
		SELECT ct.comment_id, ct.tag
		FROM comment_tags ct
		JOIN comments c ON c.id = ct.comment_id
		WHERE c.repo = ? AND c.pr_number = ?
		ORDER BY ct.added_at`, repo, num)
	if err == nil {
		for tagRows.Next() {
			var cid, tag string
			if err := tagRows.Scan(&cid, &tag); err == nil {
				tagMap[cid] = append(tagMap[cid], tag)
			}
		}
		tagRows.Close()
	}
	for i := range comments {
		comments[i].Tags = tagMap[comments[i].ID]
	}
	// Load PR-level tags.
	type prTag struct {
		Tag  string
		Note string
	}
	var prTags []prTag
	ptRows, err := s.db.Query(`SELECT tag, COALESCE(note, '') FROM pr_tags WHERE repo = ? AND pr_number = ? ORDER BY added_at`, repo, num)
	if err == nil {
		for ptRows.Next() {
			var t prTag
			if err := ptRows.Scan(&t.Tag, &t.Note); err == nil {
				prTags = append(prTags, t)
			}
		}
		ptRows.Close()
	}

	prURL := fmt.Sprintf("https://github.com/%s/pull/%d", repo, num)
	for _, c := range comments {
		if strings.Contains(c.URL, "/issues/") {
			prURL = fmt.Sprintf("https://github.com/%s/issues/%d", repo, num)
			break
		}
	}
	data := map[string]any{
		"Title":     fmt.Sprintf("%s#%d · steven-reviewer", repo, num),
		"Repo":      repo,
		"PRNumber":  num,
		"PRURL":     prURL,
		"Comments":  comments,
		"Decisions": triageDecisions,
		"PRTags":    prTags,
	}
	if err := s.tmpl.ExecuteTemplate(w, "pr_detail.html", data); err != nil {
		http.Error(w, err.Error(), 500)
	}
}

func highlightDiff(src string) string {
	lex := lexers.Get("diff")
	if lex == nil {
		lex = lexers.Fallback
	}
	style := styles.Get("github-dark")
	if style == nil {
		style = styles.Fallback
	}
	formatter := chromahtml.New(
		chromahtml.WithClasses(false),
		chromahtml.TabWidth(2),
	)
	iter, err := lex.Tokenise(nil, src)
	if err != nil {
		return "<pre>" + template.HTMLEscapeString(src) + "</pre>"
	}
	var buf strings.Builder
	if err := formatter.Format(&buf, style, iter); err != nil {
		return "<pre>" + template.HTMLEscapeString(src) + "</pre>"
	}
	return buf.String()
}

var _ chroma.Style // keep import for clarity
