-- 0002_readiness_probe: a single-row table that /readyz updates on every
-- check, so readiness is proven by a committed write rather than assumed from
-- an open file handle.
CREATE TABLE readiness_probe (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    checked_at TEXT    NOT NULL
) STRICT;
