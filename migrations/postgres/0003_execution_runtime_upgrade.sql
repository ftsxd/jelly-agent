-- Upgrade existing deployments, including those predating approvals.
-- Use the application database and schema/search_path.
-- Additive and repeatable: existing rows are never deleted.
BEGIN;
CREATE TABLE IF NOT EXISTS execution_approvals (
 id TEXT PRIMARY KEY, session_id TEXT NOT NULL, agent TEXT NOT NULL,
 invocation_id TEXT NOT NULL, call_id TEXT NOT NULL, request_json TEXT NOT NULL,
 config_hash TEXT NOT NULL, origin_json TEXT NOT NULL, reason TEXT NOT NULL,
 state TEXT NOT NULL, created_ms BIGINT NOT NULL, expires_ms BIGINT NOT NULL,
 resolved_ms BIGINT NOT NULL DEFAULT 0, resolved_by TEXT NOT NULL DEFAULT '',
 exec_id TEXT NOT NULL DEFAULT '', outcome TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS execution_approvals_session ON execution_approvals(session_id, created_ms);

CREATE TABLE IF NOT EXISTS execution_runs (
 exec_id TEXT PRIMARY KEY, session_id TEXT NOT NULL, agent TEXT NOT NULL,
 profile TEXT NOT NULL, approval_id TEXT NOT NULL, workspace_root TEXT NOT NULL,
 scope TEXT NOT NULL, owner TEXT NOT NULL, daemon TEXT NOT NULL,
 container_id TEXT NOT NULL DEFAULT '', state TEXT NOT NULL,
 created_ms BIGINT NOT NULL, deadline_ms BIGINT NOT NULL, ended_ms BIGINT NOT NULL DEFAULT 0,
 cleanup_state TEXT NOT NULL DEFAULT 'pending', cleanup_owner TEXT NOT NULL DEFAULT '',
 cleanup_until_ms BIGINT NOT NULL DEFAULT 0, cleanup_error TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS execution_runs_recovery ON execution_runs(scope, cleanup_state, deadline_ms);
COMMIT;
