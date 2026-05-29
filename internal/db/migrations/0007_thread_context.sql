-- 0007_thread_context.sql
-- Pull the *other side* of conversations: comments by non-Emyrk authors
-- on PRs/issues where Emyrk also commented. Stored as is_context=1 so
-- the viewer can render them inline for human reading (no triage forms)
-- without polluting decision queries.
--
-- thread_fetches tracks the last per-PR fetch so re-runs are cheap.

ALTER TABLE comments ADD COLUMN is_context INTEGER NOT NULL DEFAULT 0;
CREATE INDEX idx_comments_context ON comments(repo, pr_number, is_context);

CREATE TABLE thread_fetches (
    repo        TEXT NOT NULL,
    pr_number   INTEGER NOT NULL,
    fetched_at  TEXT NOT NULL,
    PRIMARY KEY (repo, pr_number)
);
