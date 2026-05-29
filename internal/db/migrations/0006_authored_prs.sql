-- 0006_authored_prs.sql
-- Mark PRs as authored-by-me so /prs/mine can show them in a separate
-- header without scanning comments.author. The opener column added in
-- 0005 already contains the login; this is just a convenience flag so
-- we can index it cheaply.

ALTER TABLE prs ADD COLUMN authored_by_me INTEGER NOT NULL DEFAULT 0;
CREATE INDEX idx_prs_authored ON prs(authored_by_me) WHERE authored_by_me = 1;
