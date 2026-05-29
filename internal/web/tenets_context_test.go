package web

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestGatherTenetContextIncludesUncuratedGitHubCorpus(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	stmts := []string{
		`CREATE TABLE lessons (id INTEGER, status TEXT, kind TEXT, title TEXT, body TEXT, evidence TEXT)`,
		`CREATE TABLE comments (id TEXT, repo TEXT, pr_number INTEGER, file_path TEXT, note TEXT, body TEXT, status TEXT, is_context INTEGER, author TEXT, created_at TEXT)`,
		`CREATE TABLE comment_tags (comment_id TEXT, tag TEXT)`,
		`CREATE TABLE prs (repo TEXT, authored_by_me INTEGER)`,
		`CREATE TABLE tenets (id INTEGER, status TEXT, category TEXT, name TEXT, statement TEXT)`,
		`INSERT INTO lessons VALUES (1, 'accepted', 'review', 'Prefer explicit errors', 'Name errors by operation.', '[]')`,
		`INSERT INTO comments VALUES ('uncurated-db', 'coder/coder', 10, 'coderd/database/migrations/001.sql', '', 'Do not edit old migrations once shared.', 'pending', 0, 'Emyrk', '2026-01-02T00:00:00Z')`,
		`INSERT INTO comments VALUES ('context-other', 'coder/coder', 10, 'coderd/users.go', '', 'A teammate reply with surrounding context.', 'context', 1, 'teammate', '2026-01-03T00:00:00Z')`,
		`INSERT INTO prs VALUES ('coder/coder', 1)`,
		`INSERT INTO tenets VALUES (1, 'accepted', 'database', 'Migration history', 'Migrations are append-only.')`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}

	s := &Server{db: db}
	got, err := s.gatherTenetContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"# Whole GitHub corpus summary",
		"Representative GitHub comments from Steven, curated and uncurated",
		"uncurated-db",
		"Do not edit old migrations once shared.",
		"Representative surrounding thread context from other reviewers",
		"context-other by teammate",
		"coder/coder prs=1 authored_by_me=1",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("context missing %q:\n%s", want, got)
		}
	}
}
