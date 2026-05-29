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
	"flag"
	"fmt"
	"os"

	"github.com/Emyrk/steven-reviewer/internal/config"
	"github.com/Emyrk/steven-reviewer/internal/db"
	"github.com/Emyrk/steven-reviewer/internal/gh"
)

var usage = `usage: ingest <subcommand> [flags]

subcommands:
  pull    [repo]   pull PR/issue comments into ./ingest.db
  walk             triage pending comments into the my-agent vault
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
	case "walk":
		fs.Parse(args)
		exit(runWalk(*cfgPath, fs.Args()))
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
	_ = gh.New(token) // wire-up smoke

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

	for _, r := range repos {
		fmt.Printf("==> pull %s (author=%s)\n", r.Name, r.Author)
		// TODO: loop client.FetchCommentsByAuthor with cursor pagination,
		// upsert into comments table, advance cursor row. Wired in the
		// next commit alongside the real GraphQL query.
		fmt.Printf("    (fetch not yet implemented; see internal/gh/client.go TODO)\n")
	}
	return nil
}

func runWalk(cfgPath string, args []string) error {
	_ = cfgPath
	_ = args
	return fmt.Errorf("walk: not implemented yet (Phase 3, next commit)")
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
