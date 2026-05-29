// Package db wraps sqlite for the ingestion store.
package db

import (
	"database/sql"
	"embed"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Emyrk/steven-reviewer/internal/gh"
	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Open opens (or creates) the sqlite database at path and applies any
// pending migrations.
func Open(path string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(30000)&_pragma=synchronous(NORMAL)", path)
	d, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if err := d.Ping(); err != nil {
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	if err := migrate(d); err != nil {
		d.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return d, nil
}

func migrate(d *sql.DB) error {
	_, err := d.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		name       TEXT NOT NULL,
		applied_at TEXT NOT NULL
	)`)
	if err != nil {
		return err
	}

	applied := map[int]bool{}
	rows, err := d.Query(`SELECT version FROM schema_migrations`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return err
		}
		applied[v] = true
	}
	rows.Close()

	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read embedded migrations: %w", err)
	}

	type pending struct {
		version int
		name    string
		body    []byte
	}
	var ps []pending
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		parts := strings.SplitN(e.Name(), "_", 2)
		if len(parts) != 2 {
			return fmt.Errorf("migration %s: bad filename", e.Name())
		}
		var v int
		if _, err := fmt.Sscanf(parts[0], "%d", &v); err != nil {
			return fmt.Errorf("migration %s: parse version: %w", e.Name(), err)
		}
		if applied[v] {
			continue
		}
		body, err := migrationsFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return err
		}
		ps = append(ps, pending{v, e.Name(), body})
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].version < ps[j].version })

	for _, p := range ps {
		tx, err := d.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(p.body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply %s: %w", p.name, err)
		}
		if _, err := tx.Exec(
			`INSERT INTO schema_migrations(version, name, applied_at) VALUES (?, ?, ?)`,
			p.version, p.name, time.Now().UTC().Format(time.RFC3339),
		); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// UpsertComments inserts or updates each comment. Returns counts of
// inserted vs updated rows.
//
// If a row already exists with the same id and same body, it's a no-op.
// If body changed, the row is updated and triage state (status, decision,
// routed_to, note, triaged_at) is reset to pending so the change goes
// back through the walk-through.
func UpsertComments(d *sql.DB, comments []gh.IssueComment) (inserted, updated int, err error) {
	now := time.Now().UTC().Format(time.RFC3339)

	tx, err := d.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()

	getStmt, err := tx.Prepare(`SELECT body FROM comments WHERE id = ?`)
	if err != nil {
		return 0, 0, err
	}
	defer getStmt.Close()

	insStmt, err := tx.Prepare(`
		INSERT INTO comments (
			id, repo, pr_number, comment_type, url, author, body,
			diff_hunk, file_path, created_at, fetched_at, status
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending')`)
	if err != nil {
		return 0, 0, err
	}
	defer insStmt.Close()

	updStmt, err := tx.Prepare(`
		UPDATE comments
		SET body = ?, diff_hunk = ?, file_path = ?, fetched_at = ?,
		    status = 'pending', decision = NULL, routed_to = NULL,
		    note = NULL, triaged_at = NULL
		WHERE id = ?`)
	if err != nil {
		return 0, 0, err
	}
	defer updStmt.Close()

	for _, c := range comments {
		var existing string
		err := getStmt.QueryRow(c.ID).Scan(&existing)
		switch err {
		case sql.ErrNoRows:
			if _, err := insStmt.Exec(
				c.ID, c.Repo, c.PRNumber, c.CommentType, c.URL, c.Author, c.Body,
				nullable(c.DiffHunk), nullable(c.FilePath),
				c.CreatedAt.UTC().Format(time.RFC3339), now,
			); err != nil {
				return 0, 0, fmt.Errorf("insert %s: %w", c.ID, err)
			}
			inserted++
		case nil:
			if existing == c.Body {
				continue
			}
			if _, err := updStmt.Exec(
				c.Body, nullable(c.DiffHunk), nullable(c.FilePath), now, c.ID,
			); err != nil {
				return 0, 0, fmt.Errorf("update %s: %w", c.ID, err)
			}
			updated++
		default:
			return 0, 0, fmt.Errorf("lookup %s: %w", c.ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return inserted, updated, nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Cursor returns the saved cursor for (repo, commentType), or "" if none.
func Cursor(d *sql.DB, repo, commentType string) (string, error) {
	var c sql.NullString
	err := d.QueryRow(
		`SELECT last_cursor FROM cursors WHERE repo = ? AND comment_type = ?`,
		repo, commentType,
	).Scan(&c)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return c.String, nil
}

// SaveCursor upserts the cursor row for (repo, commentType).
func SaveCursor(d *sql.DB, repo, commentType, cursor string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := d.Exec(`
		INSERT INTO cursors(repo, comment_type, last_cursor, last_fetched_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(repo, comment_type) DO UPDATE SET
			last_cursor = excluded.last_cursor,
			last_fetched_at = excluded.last_fetched_at`,
		repo, commentType, cursor, now)
	return err
}

// UpsertContextComments inserts (or no-op-skips) comments marked as
// thread context. Unlike UpsertComments, these never get triage state —
// they're read-only context for the comments you authored.
func UpsertContextComments(d *sql.DB, comments []gh.IssueComment) (inserted, skipped int, err error) {
	now := time.Now().UTC().Format(time.RFC3339)

	tx, err := d.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()

	// INSERT OR IGNORE so we can blindly re-run pull-threads without
	// hammering already-stored rows. status='context' makes it filterable.
	insStmt, err := tx.Prepare(`
		INSERT OR IGNORE INTO comments (
			id, repo, pr_number, comment_type, url, author, body,
			diff_hunk, file_path, created_at, fetched_at, status, is_context
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'context', 1)`)
	if err != nil {
		return 0, 0, err
	}
	defer insStmt.Close()

	for _, c := range comments {
		res, err := insStmt.Exec(
			c.ID, c.Repo, c.PRNumber, c.CommentType, c.URL, c.Author, c.Body,
			nullable(c.DiffHunk), nullable(c.FilePath),
			c.CreatedAt.UTC().Format(time.RFC3339), now,
		)
		if err != nil {
			return 0, 0, fmt.Errorf("insert ctx %s: %w", c.ID, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			inserted++
		} else {
			skipped++
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return inserted, skipped, nil
}

// MarkThreadFetched records that this PR's thread has been pulled.
func MarkThreadFetched(d *sql.DB, repo string, number int) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := d.Exec(`INSERT INTO thread_fetches (repo, pr_number, fetched_at)
	                  VALUES (?, ?, ?)
	                  ON CONFLICT(repo, pr_number) DO UPDATE SET fetched_at=excluded.fetched_at`,
		repo, number, now)
	return err
}

// DistinctPRsWithAuthorComments lists (repo, number) where we have at
// least one comment authored by user (is_context=0). Used as the work
// list for pull-threads.
func DistinctPRsWithAuthorComments(d *sql.DB, repo string) ([]struct {
	Repo   string
	Number int
}, error) {
	q := `SELECT DISTINCT repo, pr_number FROM comments
	      WHERE is_context = 0 AND pr_number > 0`
	var args []any
	if repo != "" {
		q += " AND repo = ?"
		args = append(args, repo)
	}
	q += " ORDER BY repo, pr_number"
	rows, err := d.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []struct {
		Repo   string
		Number int
	}
	for rows.Next() {
		var r struct {
			Repo   string
			Number int
		}
		if err := rows.Scan(&r.Repo, &r.Number); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// ThreadFetchedAt returns the fetched_at timestamp for a PR, or "".
func ThreadFetchedAt(d *sql.DB, repo string, number int) string {
	var ts string
	_ = d.QueryRow(`SELECT fetched_at FROM thread_fetches WHERE repo=? AND pr_number=?`,
		repo, number).Scan(&ts)
	return ts
}
