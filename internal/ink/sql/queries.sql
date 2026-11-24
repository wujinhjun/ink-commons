-- name: LoadWork :one
SELECT revision, payload FROM work_items WHERE id = ?;
-- name: LoadTaskWork :one
SELECT w.revision, w.payload FROM work_tasks t JOIN work_items w ON w.id = t.work_id
WHERE t.task_id = ?;
-- name: LoadNotificationWork :one
SELECT w.revision, w.payload FROM work_notifications n JOIN work_items w ON w.id = n.work_id
WHERE n.event_id = ?;
-- name: ListWork :many
SELECT revision, payload FROM work_items ORDER BY repo, number;

-- name: ListPollPending :many
SELECT number FROM work_items WHERE repo = ? AND poll_pending ORDER BY number;
-- name: ListHandoffPending :many
SELECT id FROM work_items WHERE handoff_pending ORDER BY id;
-- name: ListExecutionPending :many
SELECT id FROM work_items WHERE execution_pending ORDER BY id;
-- name: ListBlockedNoticePending :many
SELECT id FROM work_items WHERE blocked_notice_pending ORDER BY id;

-- name: SaveWork :execrows
INSERT INTO work_items(id, repo, number, revision, poll_pending, handoff_pending, execution_pending, blocked_notice_pending, payload)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET revision = excluded.revision, poll_pending = excluded.poll_pending,
    handoff_pending = excluded.handoff_pending, execution_pending = excluded.execution_pending,
    blocked_notice_pending = excluded.blocked_notice_pending, payload = excluded.payload
WHERE work_items.revision = excluded.revision - 1 AND work_items.payload <> excluded.payload;

-- name: DeleteWorkTasks :exec
DELETE FROM work_tasks WHERE work_id = ?;
-- name: PutWorkTask :exec
INSERT INTO work_tasks(task_id, work_id) VALUES (?, ?);
-- name: DeleteWorkNotifications :exec
DELETE FROM work_notifications WHERE work_id = ?;
-- name: PutWorkNotification :exec
INSERT INTO work_notifications(event_id, work_id) VALUES (?, ?);

-- name: PutPush :exec
INSERT INTO pushes(repo, ref, sha, route, observed_at) VALUES (?, ?, ?, ?, ?)
ON CONFLICT(repo, ref, sha, route) DO UPDATE SET observed_at = excluded.observed_at;
-- name: PrunePushes :exec
DELETE FROM pushes WHERE observed_at <= ?;
-- name: MatchPushRoutes :many
SELECT route FROM pushes WHERE repo = ? AND ref = ? AND sha = ? AND observed_at > ?;
-- name: ListPushes :many
SELECT repo, ref, sha, route, observed_at FROM pushes ORDER BY observed_at, repo, ref, sha, route;

-- name: ListRuntimeConfig :many
SELECT scope, payload, revision FROM runtime_config ORDER BY scope;
-- name: PutRuntimeConfig :exec
INSERT INTO runtime_config(scope, payload, revision) VALUES (?, ?, ?)
ON CONFLICT(scope) DO UPDATE SET payload = excluded.payload, revision = excluded.revision;
