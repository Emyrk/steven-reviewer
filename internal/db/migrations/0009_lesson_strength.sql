-- 0009_lesson_strength.sql
-- Soft-signal scale: replace the binary accept/reject with a 5-point
-- gradient embedded in the status column. New values join the existing
-- ones (proposed, accepted, rejected, edited).
--
-- Mapping (synthesizer weights):
--   rejected   0    not a lesson
--   weak       0.3  low signal, keep for context
--   accepted   1.0  standard (legacy default)
--   strong     1.5  emphasize in the vault
--   canonical  2.0  foundational — must surface every compile
--
-- No CHECK constraint change needed — status was always TEXT and never
-- constrained. Handlers validate.

-- No schema change here; this migration only carries documentation +
-- backfill of an index that helps the synthesizer rank by strength.
CREATE INDEX IF NOT EXISTS idx_lessons_status_pr ON lessons(status, repo, pr_number);
