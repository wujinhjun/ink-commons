-- name: ReadExecution :one
SELECT request, receipt, owner FROM executions WHERE attempt_id = ?;
-- name: SaveExecution :exec
UPDATE executions SET receipt = ?, owner = ? WHERE attempt_id = ?;
-- name: ReserveWriter :exec
UPDATE executions SET owner = owner WHERE 0;
-- name: InsertExecution :exec
INSERT INTO executions(attempt_id, task_id, number, resource, request, receipt) VALUES (?, ?, ?, ?, ?, ?);
-- name: HighestAttempt :one
SELECT CAST(COALESCE(MAX(number), 0) AS INTEGER) FROM executions WHERE task_id = ?;
-- name: ResourceReceipts :many
SELECT receipt FROM executions WHERE resource = ?;
-- name: ReadSession :one
SELECT session FROM sessions WHERE task_id = ?;
-- name: ReserveSession :exec
INSERT INTO sessions(task_id, session) VALUES (?, '');
-- name: SetSession :exec
UPDATE sessions SET session = ? WHERE task_id = ?;
