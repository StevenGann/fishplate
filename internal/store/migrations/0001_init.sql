-- 0001_init: the migration ledger. Nothing else belongs in this file.
--
-- One row per applied migration. checksum is the SHA-256 of the migration
-- file as embedded in the binary that applied it; a mismatch on a later start
-- means an applied migration was edited, and the runner refuses to continue.
CREATE TABLE schema_migrations (
    version    INTEGER PRIMARY KEY,
    name       TEXT    NOT NULL,
    checksum   TEXT    NOT NULL,
    applied_at TEXT    NOT NULL
) STRICT;
