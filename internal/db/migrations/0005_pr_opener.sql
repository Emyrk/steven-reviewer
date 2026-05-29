-- 0005_pr_opener.sql
-- Add opener login to prs so the random-deck view can show who opened
-- each PR without a per-card GitHub round-trip after first fetch.

ALTER TABLE prs ADD COLUMN opener TEXT;
