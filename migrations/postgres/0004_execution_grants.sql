-- Session grants: "don't ask again for this kind of command in this session".
-- Optional: without this table approvals still work one by one.
-- Use the application database and schema/search_path. Additive and repeatable.
BEGIN;
CREATE TABLE IF NOT EXISTS execution_grants (
 id TEXT PRIMARY KEY, session_id TEXT NOT NULL, agent TEXT NOT NULL, profile TEXT NOT NULL,
 class TEXT NOT NULL, config_hash TEXT NOT NULL, approval_id TEXT NOT NULL,
 created_by TEXT NOT NULL, created_ms BIGINT NOT NULL, expires_ms BIGINT NOT NULL,
 revoked_ms BIGINT NOT NULL DEFAULT 0, uses BIGINT NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS execution_grants_session ON execution_grants(session_id, class);
COMMIT;
