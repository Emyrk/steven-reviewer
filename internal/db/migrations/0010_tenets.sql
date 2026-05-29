-- 0010_tenets.sql
-- Global review tenets distilled from accepted lessons and curated GitHub comments.
-- Tenets are proposed by the model, then rated/edited/rejected by Steven.

CREATE TABLE tenets (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    category    TEXT NOT NULL, -- backend | database | security | api | frontend | operations | testing | architecture | style | process | voice
    name        TEXT NOT NULL,
    statement   TEXT NOT NULL,
    rationale   TEXT NOT NULL,
    evidence    TEXT NOT NULL DEFAULT '[]',
    status      TEXT NOT NULL DEFAULT 'proposed', -- proposed | rejected | weak | accepted | strong | canonical | edited
    source      TEXT NOT NULL DEFAULT 'llm',
    model       TEXT,
    created_at  TEXT NOT NULL,
    decided_at  TEXT
);

CREATE INDEX idx_tenets_status ON tenets(status);
CREATE INDEX idx_tenets_category_status ON tenets(category, status);
