-- 0001_initial.sql
-- Initial schema for the steven-reviewer ingestion database.

CREATE TABLE comments (
    id              TEXT PRIMARY KEY,    -- GitHub global node id
    repo            TEXT NOT NULL,       -- "owner/name"
    pr_number       INTEGER NOT NULL,
    comment_type    TEXT NOT NULL,       -- 'review' | 'review_comment' | 'issue_comment'
    url             TEXT NOT NULL,
    author          TEXT NOT NULL,
    body            TEXT NOT NULL,
    diff_hunk       TEXT,                -- for review_comment, the hunk under review
    file_path       TEXT,
    pr_title        TEXT,
    pr_state        TEXT,                -- 'open' | 'closed' | 'merged'
    created_at      TEXT NOT NULL,
    fetched_at      TEXT NOT NULL,

    -- Triage state. Filled by the walk-through, not the puller.
    status          TEXT NOT NULL DEFAULT 'pending',
                    -- 'pending' | 'skipped' | 'seen' | 'routed' | 'needs-thought'
    decision        TEXT,                -- 'hard'|'soft'|'personal'|'tradeoff'|'style'|'praise'|'skip'
    routed_to       TEXT,                -- vault path where it landed
    note            TEXT,                -- one-liner about why
    triaged_at      TEXT
);

CREATE INDEX idx_comments_status     ON comments(status);
CREATE INDEX idx_comments_repo_pr    ON comments(repo, pr_number);
CREATE INDEX idx_comments_created_at ON comments(created_at);

CREATE TABLE cursors (
    repo            TEXT NOT NULL,
    comment_type    TEXT NOT NULL,
    last_cursor     TEXT,                -- GraphQL endCursor; NULL on first run
    last_fetched_at TEXT NOT NULL,
    PRIMARY KEY (repo, comment_type)
);

-- schema_migrations is created by the migrator itself; do not re-declare here.
