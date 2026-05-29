-- 0008_lessons.sql
-- Lessons learned from a PR — proposed by an LLM (via the Hermes API
-- server) or written manually, then accepted/rejected/edited by Steven.
-- Accepted lessons become vault entries in a later synthesis pass.

CREATE TABLE lessons (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    repo         TEXT NOT NULL,
    pr_number    INTEGER NOT NULL,
    kind         TEXT NOT NULL,    -- pattern | antipattern | tradeoff | convention | gotcha
    title        TEXT NOT NULL,    -- one-line "prefer X over Y in Z"
    body         TEXT NOT NULL,    -- 2-5 sentences in user's voice
    evidence     TEXT NOT NULL DEFAULT '[]',  -- JSON array of {type, id|file, line?}
    status       TEXT NOT NULL DEFAULT 'proposed', -- proposed | accepted | rejected | edited
    source       TEXT NOT NULL DEFAULT 'llm',      -- llm | manual
    model        TEXT,             -- model id that proposed it (audit)
    created_at   TEXT NOT NULL,
    decided_at   TEXT
);

CREATE INDEX idx_lessons_pr     ON lessons(repo, pr_number);
CREATE INDEX idx_lessons_status ON lessons(status);
CREATE INDEX idx_lessons_kind   ON lessons(kind, status);
