# steven-reviewer

Tooling for the **steven-reviewer** project: a PR/issue review bot that uses
Steven's voice and engineering taste, manually invoked from Discord against a
GitHub PR URL, that posts a review comment signed `— From steven's agent`.

This repo is the **ingestion + walk-through + general scripts** side. The bot
itself, the vault that holds the synthesized persona, and the Discord
invocation live in [Emyrk/my-agent](https://github.com/Emyrk/my-agent).

## Quick map

| Concern | Lives where |
|---|---|
| Persona vault (style, tradeoffs, heuristics, `review-persona.md`) | `Emyrk/my-agent` → `agent/vault/projects/steven-reviewer/` |
| Plan / phases / decisions | `Emyrk/my-agent` → `docs/plans/2026-05-29-steven-reviewer.md` |
| Discord invocation, persona compiler cron, bot prompt pipeline | Hermes runtime (config in `my-agent`) |
| GitHub PR/comment ingestion CLI | **this repo** → `cmd/ingest/` |
| Walk-through (triage) — CLI then web | **this repo** → `internal/walk/`, later `web/` |
| Local sqlite of ingested comments | **this repo** → `./ingest.db` (gitignored) |
| Misc one-off scripts | **this repo** → `scripts/` |

## Repo layout

```
cmd/ingest/          CLI entrypoint (subcommands: pull, walk, status, ...)
internal/
  gh/                GitHub API client (REST + GraphQL)
  db/                sqlite open + migrations + queries
  walk/              walk-through state machine
  config/            config.yml loading + defaults
migrations/          versioned sqlite DDL
docs/                schema notes, design docs
scripts/             general-purpose utility scripts
web/                 (Phase 3b) browser walk-through UI
```

## Quick start

Prerequisites:

- Go 1.22+
- A fine-grained GitHub PAT with `Contents: read`, `Issues: r/w`,
  `Pull requests: r/w` on the repos in `config.yml`. Path:
  `~/.config/steven-reviewer/gh-token` (chmod 600).

```sh
# build
go build -o bin/ingest ./cmd/ingest

# initial backfill (idempotent; can be re-run any time)
./bin/ingest pull

# triage what was pulled
./bin/ingest walk
```

See `AGENTS.md` for the conventions any agent (Hermes, Claude Code, OpenCode)
should follow when working in this repo.

## Status

**Scaffold phase.** See the plan in `my-agent` for current phase and pending
decisions. As of 2026-05-29: Phase 3 — ingestion CLI scaffolded, first
backfill pending.
