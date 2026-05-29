-- 0003_synthesized.sql
-- Add a synthesized flag so vault propagation can happen in bulk later
-- instead of per-triage-click. A row is 'synthesized' when its triage
-- decision has been written into the my-agent vault.

ALTER TABLE comments ADD COLUMN synthesized INTEGER NOT NULL DEFAULT 0;
ALTER TABLE comments ADD COLUMN synthesized_at TEXT;

CREATE INDEX idx_comments_synth ON comments(decision, synthesized) WHERE decision IS NOT NULL;
