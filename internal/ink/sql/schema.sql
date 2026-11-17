-- Query-generation schema only. Runtime migrations remain in store.go.
CREATE TABLE work_items (id TEXT PRIMARY KEY, payload TEXT NOT NULL);
CREATE TABLE pushes (position INTEGER PRIMARY KEY, payload TEXT NOT NULL);
CREATE TABLE repositories (name TEXT PRIMARY KEY, payload TEXT NOT NULL);

CREATE TABLE runtime_config (scope TEXT PRIMARY KEY, payload TEXT NOT NULL, revision INTEGER NOT NULL);
