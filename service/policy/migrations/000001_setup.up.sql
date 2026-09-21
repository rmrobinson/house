CREATE TABLE IF NOT EXISTS policies(
    id TEXT PRIMARY KEY,
    condition_expr TEXT NOT NULL,
    script TEXT NOT NULL,
    on_condition_false INTEGER NOT NULL,
    is_system INTEGER NOT NULL DEFAULT 0,
    created_at DATETIME NOT NULL,
    updated_at DATETIME NOT NULL
);

CREATE TABLE IF NOT EXISTS execution_logs(
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    policy_id TEXT NOT NULL REFERENCES policies(id),
    started_at DATETIME NOT NULL,
    ended_at DATETIME,
    status TEXT NOT NULL,
    error TEXT
);
