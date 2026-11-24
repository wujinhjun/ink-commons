CREATE TABLE work_items (
    id TEXT PRIMARY KEY NOT NULL,
    repo TEXT NOT NULL,
    number INTEGER NOT NULL,
    revision INTEGER NOT NULL,
    poll_pending BOOLEAN NOT NULL,
    handoff_pending BOOLEAN NOT NULL,
    execution_pending BOOLEAN NOT NULL,
    blocked_notice_pending BOOLEAN NOT NULL,
    payload TEXT NOT NULL
);
CREATE INDEX work_poll_pending ON work_items(repo, number) WHERE poll_pending;
CREATE INDEX work_handoff_pending ON work_items(id) WHERE handoff_pending;
CREATE INDEX work_execution_pending ON work_items(id) WHERE execution_pending;
CREATE INDEX work_blocked_notice_pending ON work_items(id) WHERE blocked_notice_pending;

CREATE TABLE work_tasks (
    task_id TEXT PRIMARY KEY NOT NULL,
    work_id TEXT NOT NULL REFERENCES work_items(id) ON DELETE CASCADE
);
CREATE INDEX work_tasks_by_work ON work_tasks(work_id);
CREATE TABLE work_notifications (
    event_id TEXT PRIMARY KEY NOT NULL,
    work_id TEXT NOT NULL REFERENCES work_items(id) ON DELETE CASCADE
);
CREATE INDEX work_notifications_by_work ON work_notifications(work_id);

CREATE TABLE pushes (
    repo TEXT NOT NULL,
    ref TEXT NOT NULL,
    sha TEXT NOT NULL,
    route TEXT NOT NULL,
    observed_at INTEGER NOT NULL,
    PRIMARY KEY (repo, ref, sha, route)
);
CREATE INDEX pushes_by_age ON pushes(observed_at);

CREATE TABLE runtime_config (scope TEXT PRIMARY KEY, payload TEXT NOT NULL, revision INTEGER NOT NULL);

CREATE TABLE workspace_allocations (
	id TEXT PRIMARY KEY, request_id TEXT NOT NULL UNIQUE,
	common_dir TEXT NOT NULL, source TEXT NOT NULL, path TEXT NOT NULL,
	kind TEXT NOT NULL, preference TEXT NOT NULL,
	ref TEXT NOT NULL, branch TEXT NOT NULL, head TEXT NOT NULL, previous_head TEXT NOT NULL,
	state TEXT NOT NULL, owner_route TEXT NOT NULL, task_id TEXT NOT NULL,
	repository TEXT
);
CREATE UNIQUE INDEX workspace_active_path ON workspace_allocations(path)
WHERE state IN ('preparing', 'active');
