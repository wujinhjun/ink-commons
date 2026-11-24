CREATE TABLE IF NOT EXISTS executions (
 attempt_id TEXT PRIMARY KEY, task_id TEXT NOT NULL, number INTEGER NOT NULL,
 resource TEXT NOT NULL, request TEXT NOT NULL, receipt TEXT NOT NULL,
 owner TEXT NOT NULL DEFAULT '', UNIQUE(task_id, number));
CREATE TABLE IF NOT EXISTS sessions (task_id TEXT PRIMARY KEY, session TEXT NOT NULL);
