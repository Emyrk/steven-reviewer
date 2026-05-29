-- 0004_multi_tags_and_pr_tags.sql
-- Three additions, all driven by walk-through ergonomic asks:
--
-- 1. A comment can carry multiple triage tags. Replace the
--    single-column `decision` with a many-to-many comment_tags table.
--    Keep `decision` for backwards compatibility; it's backfilled from
--    the first tag and the UI keeps it in sync (first tag wins).
--
-- 2. needs_more_context flag on `comments`: Steven hit "the related
--    code spans multiple files; this diff_hunk isn't enough." Marks the
--    comment so the future PR-diff enrichment knows to prioritize it.
--
-- 3. pr_tags table for whole-PR opinions ("this PR rocks", "canonical
--    refactor", etc.) separate from per-comment decisions.

CREATE TABLE comment_tags (
    comment_id TEXT NOT NULL,
    tag        TEXT NOT NULL,
    added_at   TEXT NOT NULL,
    PRIMARY KEY (comment_id, tag),
    FOREIGN KEY (comment_id) REFERENCES comments(id) ON DELETE CASCADE
);

CREATE INDEX idx_comment_tags_tag ON comment_tags(tag);

-- Backfill from existing single-decision rows.
INSERT INTO comment_tags (comment_id, tag, added_at)
SELECT id, decision, COALESCE(triaged_at, datetime('now'))
FROM comments
WHERE decision IS NOT NULL AND decision != '';

ALTER TABLE comments ADD COLUMN needs_more_context INTEGER NOT NULL DEFAULT 0;

CREATE TABLE pr_tags (
    repo       TEXT NOT NULL,
    pr_number  INTEGER NOT NULL,
    tag        TEXT NOT NULL,        -- e.g. 'rocks', 'canonical', 'reread', 'authored'
    note       TEXT,                 -- optional free text
    added_at   TEXT NOT NULL,
    PRIMARY KEY (repo, pr_number, tag)
);

CREATE INDEX idx_pr_tags_tag ON pr_tags(tag);
