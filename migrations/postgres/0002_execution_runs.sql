-- Existing deployments: apply before starting this runtime version.
-- Resource authority must survive session removal and database migration.
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
