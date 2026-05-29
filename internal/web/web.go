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
	mux.HandleFunc("GET /c/{id}", s.handleDetail)
	mux.HandleFunc("POST /c/{id}/triage", s.handleTriage)
	mux.HandleFunc("GET /api/comments", s.handleAPIList)

	static, _ := http.FS(assets), 0
	_ = static
	mux.Handle("GET /static/", http.FileServer(http.FS(assets)))
	return mux
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
	Key   string
	Value string
}

var triageDecisions = []decision{
	{"h", "hard"},
	{"s", "soft"},
	{"p", "personal"},
	{"t", "tradeoff"},
	{"c", "style"},
	{"r", "praise"},
	{"k", "skip"},
	{"n", "needs-thought"},
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
	allowed := map[string]bool{
		"hard": true, "soft": true, "personal": true, "tradeoff": true,
		"style": true, "praise": true, "skip": true, "needs-thought": true,
	}
	if !allowed[decision] {
		http.Error(w, "bad decision", 400)
		return
	}
	status := "routed"
	if decision == "skip" {
		status = "skipped"
	} else if decision == "needs-thought" {
		status = "needs-thought"
	}
	_, err := s.db.Exec(`
		UPDATE comments
		SET status = ?, decision = ?, note = NULLIF(?, ''),
		    triaged_at = datetime('now')
		WHERE id = ?`, status, decision, note, id)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	// Redirect to next pending in the same repo, or back to list.
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
