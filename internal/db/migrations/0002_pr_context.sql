-- 0002_pr_context.sql
-- PR metadata + full unified diffs, and linked-issue bodies, to give the
-- bot the context it needs to learn what TRIGGERED a comment.
--
-- Comments alone are "we should check capability not role"; without the
-- diff the bot can't learn what code Steven was looking at when he said it.

CREATE TABLE prs (
    repo                  TEXT NOT NULL,
    number                INTEGER NOT NULL,
    title                 TEXT,
    body                  TEXT,                   -- description, markdown
    state                 TEXT,                   -- open | closed
    merged                INTEGER NOT NULL DEFAULT 0,  -- 0|1
    base_ref              TEXT,
    head_sha              TEXT,
    additions             INTEGER,
    deletions             INTEGER,
    changed_files         INTEGER,
    diff_gz               BLOB,                   -- gzip-compressed unified diff
    diff_bytes_raw        INTEGER,                -- uncompressed length
    diff_truncated        INTEGER NOT NULL DEFAULT 0,  -- 0|1, true if GitHub capped it
    linked_issues         TEXT,                   -- JSON array of integers
    created_at            TEXT,
    closed_at             TEXT,
    merged_at             TEXT,
    fetched_at            TEXT NOT NULL,
    PRIMARY KEY (repo, number)
);

CREATE INDEX idx_prs_repo ON prs(repo);

CREATE TABLE issues (
    repo                  TEXT NOT NULL,
    number                INTEGER NOT NULL,
    title                 TEXT,
    body                  TEXT,
    state                 TEXT,
    closed_at             TEXT,
    fetched_at            TEXT NOT NULL,
    PRIMARY KEY (repo, number)
);
