-- Query-generation schema only. Runtime initialization remains in ledger.go.
CREATE TABLE executions (
 attempt_id TEXT PRIMARY KEY, task_id TEXT NOT NULL, number INTEGER NOT NULL,
 resource TEXT NOT NULL, request TEXT NOT NULL, receipt TEXT NOT NULL,
 owner TEXT NOT NULL DEFAULT '', UNIQUE(task_id, number));
CREATE TABLE sessions (task_id TEXT PRIMARY KEY, session TEXT NOT NULL);
