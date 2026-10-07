-- Supports SQLiteStore.LastLogs' per-policy MAX(id) grouping and GetLogs'
-- WHERE policy_id = ? filter, both of which would otherwise force a full
-- table scan.
CREATE INDEX IF NOT EXISTS idx_execution_logs_policy_id ON execution_logs(policy_id, id);
