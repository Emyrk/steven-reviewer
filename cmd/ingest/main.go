// Command ingest is the steven-reviewer ingestion + walk-through CLI.
//
// Subcommands:
//
//	ingest pull    [repo]   pull PR/issue comments into ./ingest.db
//	ingest walk             triage pending comments into the my-agent vault
//	ingest status           summarize ingestion + triage state
//	ingest doctor           check config, token, and DB health
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Emyrk/steven-reviewer/internal/config"
	"github.com/Emyrk/steven-reviewer/internal/db"
	"github.com/Emyrk/steven-reviewer/internal/gh"
	"github.com/Emyrk/steven-reviewer/internal/web"
)

var usage = `usage: ingest <subcommand> [flags]

subcommands:
  pull    [repo]   pull PR/issue comments into ./ingest.db
  walk             triage pending comments into the my-agent vault (TODO)
  serve            launch the web viewer/triager
  status           summarize ingestion + triage state
  doctor           check config, token, and DB health

global flags:
  --config <path>  path to config.yml (default: ./config.yml)
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	sub := os.Args[1]
	args := os.Args[2:]

	fs := flag.NewFlagSet(sub, flag.ExitOnError)
	cfgPath := fs.String("config", "./config.yml", "path to config.yml")

	switch sub {
	case "pull":
		fs.Parse(args)
		exit(runPull(*cfgPath, fs.Args()))
	case "pull-authored":
		fs.Parse(args)
		exit(runPullAuthored(*cfgPath, fs.Args()))
	case "enrich-prs":
		force := fs.Bool("force", false, "refetch even if additions is already set")
		fs.Parse(args)
		exit(runEnrichPRs(*cfgPath, fs.Args(), *force))
	case "pull-threads":
		force := fs.Bool("force", false, "re-fetch even if already cached")
		fs.Parse(args)
		exit(runPullThreads(*cfgPath, fs.Args(), *force))
	case "walk":
		fs.Parse(args)
		exit(runWalk(*cfgPath, fs.Args()))
	case "serve":
		bind := fs.String("bind", "0.0.0.0:8080", "host:port to listen on")
		fs.Parse(args)
		exit(runServe(*cfgPath, *bind))
	case "status":
		fs.Parse(args)
		exit(runStatus(*cfgPath))
	case "doctor":
		fs.Parse(args)
		exit(runDoctor(*cfgPath))
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n\n", sub)
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
}

func exit(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func runDoctor(cfgPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	fmt.Printf("config:        %s\n", cfgPath)
	fmt.Printf("db_path:       %s\n", cfg.DBPath)
	fmt.Printf("my_agent_path: %s\n", cfg.MyAgentPath)
	fmt.Printf("repos:         %d configured\n", len(cfg.Repos))
	for _, r := range cfg.Repos {
		fmt.Printf("  - %s (tag=%s, author=%s)\n", r.Name, r.Tag, r.Author)
	}
	token, err := cfg.Token()
	if err != nil {
		return err
	}
	fmt.Printf("token:         %s (%d chars)\n", cfg.TokenPath, len(token))

	d, err := db.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer d.Close()
	var count int
	if err := d.QueryRow(`SELECT COUNT(*) FROM comments`).Scan(&count); err != nil {
		return err
	}
	fmt.Printf("db:            ok (%d comments)\n", count)

	client := gh.New(token)
	login, err := client.Viewer(context.Background())
	if err != nil {
		return fmt.Errorf("github viewer: %w", err)
	}
	fmt.Printf("github:        authenticated as %s\n", login)
	return nil
}

func runPull(cfgPath string, args []string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	token, err := cfg.Token()
	if err != nil {
		return err
	}
	client := gh.New(token)

	d, err := db.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer d.Close()

	// Filter to a single repo if specified.
	repos := cfg.Repos
	if len(args) > 0 {
		want := args[0]
		repos = nil
		for _, r := range cfg.Repos {
			if r.Name == want {
				repos = append(repos, r)
			}
		}
		if len(repos) == 0 {
			return fmt.Errorf("repo %q not in config", want)
		}
	}

	ctx := context.Background()
	for _, r := range repos {
		fmt.Printf("==> pull %s (author=%s)\n", r.Name, r.Author)
		for _, kind := range []struct {
			label string
			fn    func(context.Context, string, string, string, gh.PageHandler) ([]gh.IssueComment, string, error)
		}{
			{"issue_comment", client.FetchIssueCommentsPaged},
			{"review_comment", client.FetchReviewCommentsPaged},
		} {
			since, err := db.Cursor(d, r.Name, kind.label)
			if err != nil {
				return fmt.Errorf("cursor %s/%s: %w", r.Name, kind.label, err)
			}
			fmt.Printf("    %-15s since=%q ...\n", kind.label, since)
			var totalIns, totalUpd, totalMatched, pages int
			onPage := func(matches []gh.IssueComment, maxSeen string) error {
				pages++
				ins, upd, err := db.UpsertComments(d, matches)
				if err != nil {
					return err
				}
				totalIns += ins
				totalUpd += upd
				totalMatched += len(matches)
				if maxSeen != "" && maxSeen != since {
					if err := db.SaveCursor(d, r.Name, kind.label, maxSeen); err != nil {
						return err
					}
				}
				if pages%10 == 0 {
					fmt.Printf("        page %d: %d matched cumulative, cursor=%s\n", pages, totalMatched, maxSeen)
				}
				return nil
			}
			_, finalSince, err := kind.fn(ctx, r.Name, r.Author, since, onPage)
			if err != nil {
				fmt.Printf("        FAIL after page %d (cursor saved at %s): %v\n", pages, finalSince, err)
				return fmt.Errorf("fetch %s/%s: %w", r.Name, kind.label, err)
			}
			fmt.Printf("    %-15s done: %d pages, %d matched, %d new, %d updated, cursor=%s\n",
				kind.label, pages, totalMatched, totalIns, totalUpd, finalSince)
		}
	}
	return nil
}

func runWalk(cfgPath string, args []string) error {
	_ = cfgPath
	_ = args
	return fmt.Errorf("walk: use the web viewer (`ingest serve`) for now; CLI walk-through later")
}

func runServe(cfgPath, bind string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	d, err := db.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer d.Close()
	// Optional GH client for lazy PR-meta fetch in /prs/random.
	var ghc *gh.Client
	if tok, terr := cfg.Token(); terr == nil && tok != "" {
		ghc = gh.New(tok)
	} else {
		log.Printf("serve: no GH token (%v) — /prs/random will only show cached PR meta", terr)
	}
	srv, err := web.NewServer(d, ghc)
	if err != nil {
		return err
	}
	log.Printf("steven-reviewer viewer listening on http://%s", bind)
	return http.ListenAndServe(bind, srv.Routes())
}

func runStatus(cfgPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	d, err := db.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer d.Close()

	rows, err := d.Query(`
		SELECT repo, status, COUNT(*) AS n
		FROM comments
		GROUP BY repo, status
		ORDER BY repo, status`)
	if err != nil {
		return err
	}
	defer rows.Close()
	fmt.Printf("%-30s %-15s %s\n", "repo", "status", "count")
	for rows.Next() {
		var repo, status string
		var n int
		if err := rows.Scan(&repo, &status, &n); err != nil {
			return err
		}
		fmt.Printf("%-30s %-15s %d\n", repo, status, n)
	}
	return nil
}

// runPullAuthored fetches the list of PRs authored by the configured
// author for each repo, and upserts them into the prs table with
// authored_by_me=1. Idempotent: re-running just refreshes title/state.
func runPullAuthored(cfgPath string, args []string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	token, err := cfg.Token()
	if err != nil {
		return err
	}
	client := gh.New(token)
	d, err := db.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer d.Close()

	repos := cfg.Repos
	if len(args) > 0 {
		want := args[0]
		repos = nil
		for _, r := range cfg.Repos {
			if r.Name == want {
				repos = append(repos, r)
			}
		}
		if len(repos) == 0 {
			return fmt.Errorf("repo %q not in config", want)
		}
	}

	ctx := context.Background()
	for _, r := range repos {
		fmt.Printf("==> pull-authored %s (author=%s)\n", r.Name, r.Author)
		prs, err := client.FetchAuthoredPRs(ctx, r.Name, r.Author)
		if err != nil {
			return fmt.Errorf("%s: %w", r.Name, err)
		}
		now := time.Now().UTC().Format(time.RFC3339)
		var inserted, updated int
		for _, p := range prs {
			res, err := d.Exec(`INSERT INTO prs (repo, number, title, opener, state, created_at, authored_by_me, fetched_at)
			                    VALUES (?, ?, ?, ?, ?, ?, 1, ?)
			                    ON CONFLICT(repo, number) DO UPDATE SET
			                      title = excluded.title,
			                      opener = COALESCE(NULLIF(prs.opener, ''), excluded.opener),
			                      state = excluded.state,
			                      created_at = COALESCE(NULLIF(prs.created_at, ''), excluded.created_at),
			                      authored_by_me = 1,
			                      fetched_at = excluded.fetched_at`,
				p.Repo, p.Number, p.Title, r.Author, p.State,
				p.CreatedAt.Format(time.RFC3339), now)
			if err != nil {
				return fmt.Errorf("upsert %s#%d: %w", p.Repo, p.Number, err)
			}
			if n, _ := res.RowsAffected(); n > 0 {
				// SQLite ON CONFLICT counts as 1 affected row whether insert or update;
				// distinguish by checking if it was already there.
				var existed int
				_ = d.QueryRow(`SELECT 1 FROM prs WHERE repo=? AND number=? AND fetched_at < ?`,
					p.Repo, p.Number, now).Scan(&existed)
				if existed == 1 {
					updated++
				} else {
					inserted++
				}
			}
		}
		fmt.Printf("    %d PRs (%d new, %d updated)\n", len(prs), inserted, updated)
	}
	return nil
}

// runPullThreads walks every PR where the user has authored at least
// one comment and fetches the other-author comments on that PR. Stores
// them with is_context=1. Idempotent + resumable: thread_fetches tracks
// which PRs we already pulled.
func runPullThreads(cfgPath string, args []string, force bool) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	token, err := cfg.Token()
	if err != nil {
		return err
	}
	client := gh.New(token)
	d, err := db.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer d.Close()

	repoFilter := ""
	if len(args) > 0 {
		repoFilter = args[0]
		found := false
		for _, r := range cfg.Repos {
			if r.Name == repoFilter {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("repo %q not in config", repoFilter)
		}
	}

	// Pre-build a repo→author map so we exclude the right login per repo.
	authorBy := map[string]string{}
	for _, r := range cfg.Repos {
		authorBy[r.Name] = r.Author
	}

	prs, err := db.DistinctPRsWithAuthorComments(d, repoFilter)
	if err != nil {
		return fmt.Errorf("list prs: %w", err)
	}
	fmt.Printf("==> pull-threads: %d PRs to check (force=%v)\n", len(prs), force)

	ctx := context.Background()
	var totalIns, totalSkip, fetched, cached int
	t0 := time.Now()
	for i, p := range prs {
		if !force && db.ThreadFetchedAt(d, p.Repo, p.Number) != "" {
			cached++
			continue
		}
		fmt.Printf("    [%d/%d] %s#%d ...", i+1, len(prs), p.Repo, p.Number)
		comments, err := client.FetchPRThread(ctx, p.Repo, p.Number, authorBy[p.Repo])
		if err != nil {
			fmt.Printf(" ERR: %v\n", err)
			continue
		}
		ins, skip, err := db.UpsertContextComments(d, comments)
		if err != nil {
			fmt.Printf(" upsert ERR: %v\n", err)
			continue
		}
		if err := db.MarkThreadFetched(d, p.Repo, p.Number); err != nil {
			log.Printf("    %s#%d: mark error: %v", p.Repo, p.Number, err)
		}
		totalIns += ins
		totalSkip += skip
		fetched++
		fmt.Printf(" %d new, %d existed\n", ins, skip)
		// Progress every 50 PRs.
		if fetched%50 == 0 {
			elapsed := time.Since(t0).Round(time.Second)
			rate := float64(fetched) / elapsed.Seconds()
			remaining := time.Duration(float64(len(prs)-i-1)/rate) * time.Second
			fmt.Printf("    %d/%d (%s elapsed, ~%s remaining) — %d new context comments\n",
				i+1, len(prs), elapsed, remaining.Round(time.Second), totalIns)
		}
	}
	fmt.Printf("==> done: fetched %d PRs (%d cached), %d new context comments, %d existed\n",
		fetched, cached, totalIns, totalSkip)
	return nil
}

// runEnrichPRs walks every distinct (repo, pr_number) seen in comments
// (including authored_by_me PRs) and fetches diff size via
// FetchPRMeta. Stores additions/deletions/changed_files in the prs
// table so the viewer can filter by diff size without hitting GH.
// Idempotent: skips rows where additions is already set unless --force.
func runEnrichPRs(cfgPath string, args []string, force bool) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	token, err := cfg.Token()
	if err != nil {
		return err
	}
	client := gh.New(token)
	d, err := db.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer d.Close()

	repoFilter := ""
	if len(args) > 0 {
		repoFilter = args[0]
	}

	// Build candidate set: every (repo, pr_number) we have a comment for,
	// PLUS every authored_by_me row in prs. UNION dedup-friendly.
	q := `
		SELECT DISTINCT repo, pr_number FROM comments
		WHERE pr_number > 0` + ifNonEmpty(repoFilter, ` AND repo = ?`) + `
		UNION
		SELECT repo, number FROM prs
		WHERE authored_by_me = 1` + ifNonEmpty(repoFilter, ` AND repo = ?`)
	var qargs []any
	if repoFilter != "" {
		qargs = append(qargs, repoFilter, repoFilter)
	}
	rows, err := d.Query(q, qargs...)
	if err != nil {
		return err
	}
	type rk struct {
		Repo   string
		Number int
	}
	var candidates []rk
	for rows.Next() {
		var k rk
		if err := rows.Scan(&k.Repo, &k.Number); err != nil {
			rows.Close()
			return err
		}
		candidates = append(candidates, k)
	}
	rows.Close()

	fmt.Printf("==> enrich-prs: %d candidate PRs (force=%v)\n", len(candidates), force)

	ctx := context.Background()
	var fetched, cached, errs int
	t0 := time.Now()
	for i, k := range candidates {
		if !force {
			var addsRaw sql.NullInt64
			_ = d.QueryRow(`SELECT additions FROM prs WHERE repo = ? AND number = ?`, k.Repo, k.Number).Scan(&addsRaw)
			if addsRaw.Valid {
				cached++
				continue
			}
		}
		m, err := client.FetchPRMeta(ctx, k.Repo, k.Number)
		if err != nil {
			errs++
			if errs <= 5 || errs%50 == 0 {
				fmt.Printf("    [%d/%d] %s#%d ERR: %v\n", i+1, len(candidates), k.Repo, k.Number, err)
			}
			// Tombstone 404s so they don't show as "not yet enriched" forever.
			if strings.Contains(err.Error(), "HTTP 404") {
				_, _ = d.Exec(`INSERT INTO prs (repo, number, state, fetched_at)
				               VALUES (?, ?, 'deleted', ?)
				               ON CONFLICT(repo, number) DO UPDATE SET
				                 state='deleted', fetched_at=excluded.fetched_at`,
					k.Repo, k.Number, time.Now().UTC().Format(time.RFC3339))
			}
			continue
		}
		mergedInt := 0
		if m.Merged {
			mergedInt = 1
		}
		_, err = d.Exec(`INSERT INTO prs (repo, number, title, opener, state, merged, additions, deletions, changed_files, created_at, fetched_at)
		                 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		                 ON CONFLICT(repo, number) DO UPDATE SET
		                   title=COALESCE(NULLIF(excluded.title, ''), prs.title),
		                   opener=COALESCE(NULLIF(excluded.opener, ''), prs.opener),
		                   state=COALESCE(NULLIF(excluded.state, ''), prs.state),
		                   merged=excluded.merged,
		                   additions=excluded.additions,
		                   deletions=excluded.deletions,
		                   changed_files=excluded.changed_files,
		                   created_at=COALESCE(NULLIF(excluded.created_at, ''), prs.created_at),
		                   fetched_at=excluded.fetched_at`,
			k.Repo, k.Number, m.Title, m.Opener, m.State, mergedInt,
			m.Additions, m.Deletions, m.ChangedFiles,
			m.CreatedAt.Format(time.RFC3339), time.Now().UTC().Format(time.RFC3339))
		if err != nil {
			errs++
			fmt.Printf("    upsert %s#%d ERR: %v\n", k.Repo, k.Number, err)
			continue
		}
		fetched++
		if fetched%50 == 0 {
			elapsed := time.Since(t0).Round(time.Second)
			rate := float64(fetched) / time.Since(t0).Seconds()
			eta := time.Duration(float64(len(candidates)-i-1)/rate) * time.Second
			fmt.Printf("    [%d/%d] fetched=%d cached=%d errs=%d elapsed=%s eta=%s\n",
				i+1, len(candidates), fetched, cached, errs, elapsed, eta.Round(time.Second))
		}
	}
	fmt.Printf("==> done: fetched=%d cached=%d errs=%d in %s\n", fetched, cached, errs, time.Since(t0).Round(time.Second))
	return nil
}

func ifNonEmpty(s, clause string) string {
	if s == "" {
		return ""
	}
	return clause
}
