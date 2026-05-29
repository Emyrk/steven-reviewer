# AGENTS

Conventions for any agent (Hermes, Claude Code, OpenCode, human) working in
this repo. Companion to the umbrella plan in `Emyrk/my-agent` at
`docs/plans/2026-05-29-steven-reviewer.md`.

## Identity

This repo is **`Emyrk/steven-reviewer`**. It is public. It hosts the
ingestion CLI, the walk-through tools, and general-purpose scripts for the
steven-reviewer project. **The vault and the persona artifacts live in
`Emyrk/my-agent`**, not here.

## Push hygiene

- Commit and push after every meaningful change. Match the `my-agent` rule:
  push-on-write.
- Remote is configured to use the `github-steven-reviewer` ssh alias which
  binds the deploy key at `~/.ssh/steven_reviewer_deploy`. If a `git push`
  fails with hostname resolution, that alias is missing from `~/.ssh/config`
  — see `Emyrk/my-agent` → `agent/skills/my-agent-repo-work/SKILL.md` Rule 1.
- Commits authored by an agent should set `user.name` to the runtime
  (`hermes`, `claude-code`, etc.) and `user.email` to `<runtime>@local`.

## Secrets

- GitHub PAT lives at `~/.config/steven-reviewer/gh-token` (chmod 600).
  **Never** commit, never echo, never include in any output.
- The repo `.gitignore` covers: `*.db`, `*.db-*`, `gh-token`, `.env*`,
  `.config/`, `bin/`. Verify before adding new secret-bearing paths.
- If you accidentally print or commit a secret, **say so immediately** and
  rotate. Don't try to quietly fix it.

## Code conventions

- **Language: Go** for the ingest CLI and any backend code. Module path
  `github.com/Emyrk/steven-reviewer`.
- Standard library first. Reach for dependencies only when they pay clear
  rent. Initial dependency set: `modernc.org/sqlite` (pure-Go sqlite,
  avoids CGO), `github.com/shurcooL/githubv4` (GraphQL) only if REST won't
  do, otherwise plain `net/http`.
- Idiomatic Go: lowercase package names, capitalized exports, errors
  wrapped with `fmt.Errorf("...: %w", err)`. No `interface{}` where a
  concrete type works.
- Tests beside the code (`foo_test.go`). Table-driven where it fits.
  `go test ./...` must pass before push.
- `gofmt -s` and `go vet ./...` on every commit. Pre-push:
  `go build ./... && go test ./... && go vet ./...`.

## Database conventions

- Single sqlite file `./ingest.db` (gitignored).
- All schema changes go through a versioned migration in `migrations/`
  named `NNNN_description.sql`. The CLI applies pending migrations on
  every run. **Never** edit a migration after it's been pushed; add a new
  one.
- Triage state on the `comments` table is the source of truth for what's
  been seen. The vault is the *output* of triage, not the state store.

## GitHub API conventions

- Read PAT once at startup from `~/.config/steven-reviewer/gh-token`,
  never log it, never include it in error messages (write a wrapper that
  redacts before logging the request).
- Respect rate limit headers. Back off on 403 with `Retry-After`. Fail
  loudly (not silently) on 401.
- Use the GraphQL API for paginated comment fetches (rest API requires
  3 round trips per PR; graphql does it in one).

## Walk-through conventions

When the walk-through CLI routes a comment into a vault file in
`my-agent`, the agent must:

1. Resolve the my-agent checkout path (default `~/agent`, configurable).
2. Run `agent-pull` first to avoid rebasing onto a stale tree.
3. Write or append the routed entry.
4. Append a line to `agent/vault/log.md`.
5. Use `vault-sync -m "<msg>"` to commit and push.
6. Record the resulting vault path in `comments.routed_to`.

Failure modes (vault-sync conflict, push reject) must not lose the
triage decision in sqlite. Triage first, sync second.

## Plan and decisions

- The umbrella plan lives in `my-agent`, not here. Don't fork it. Link
  back to it in PR descriptions and commit messages where relevant.
- If you make an architectural decision while working here that affects
  the umbrella plan, update the plan in `my-agent` in the same session.
  Cross-repo coordination is a forcing function for keeping the plan
  honest.

## When in doubt

- Ask Steven. Manual gating is the design.
- Don't post comments to coder/coder or any other repo without explicit
  confirmation per invocation. Posting is gated behind a Discord
  preview-and-react flow; never bypass that for "convenience."
