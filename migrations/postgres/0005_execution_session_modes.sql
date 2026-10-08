-- Per-session "ask for every command" switch.
-- Optional: without this table the switch is not offered; everything else works.
-- Use the application database and schema/search_path. Additive and repeatable.
BEGIN;
CREATE TABLE IF NOT EXISTS execution_session_modes (
 session_id TEXT PRIMARY KEY, strict BIGINT NOT NULL DEFAULT 0,
 updated_ms BIGINT NOT NULL, updated_by TEXT NOT NULL DEFAULT ''
);
COMMIT;
