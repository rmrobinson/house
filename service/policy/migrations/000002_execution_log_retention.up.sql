-- Supports SQLiteStore.AppendLog's retention prune (DELETE ... WHERE
-- started_at < ?), which would otherwise force a full table scan on every
-- insert as execution_logs grows unbounded.
CREATE INDEX IF NOT EXISTS idx_execution_logs_started_at ON execution_logs(started_at);
