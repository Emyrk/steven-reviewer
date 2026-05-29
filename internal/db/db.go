// Package db wraps sqlite for the ingestion store.
package db

import (
	"database/sql"
	"embed"
	"fmt"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Open opens (or creates) the sqlite database at path and applies any
// pending migrations.
func Open(path string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)", path)
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
	// Ensure schema_migrations exists before we try to read from it.
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
