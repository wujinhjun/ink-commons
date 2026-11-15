-- name: ListWork :many
SELECT id, payload FROM work_items ORDER BY id;
-- name: ListPushes :many
SELECT position, payload FROM pushes ORDER BY position;
-- name: ListRepositories :many
SELECT name, payload FROM repositories ORDER BY name;
-- name: PutWork :exec
INSERT INTO work_items(id, payload) VALUES (?, ?)
ON CONFLICT(id) DO UPDATE SET payload = excluded.payload;
-- name: PutPush :exec
INSERT INTO pushes(position, payload) VALUES (?, ?)
ON CONFLICT(position) DO UPDATE SET payload = excluded.payload;
-- name: PutRepository :exec
INSERT INTO repositories(name, payload) VALUES (?, ?)
ON CONFLICT(name) DO UPDATE SET payload = excluded.payload;
-- name: DeleteWork :exec
DELETE FROM work_items WHERE id = ?;
-- name: DeletePush :exec
DELETE FROM pushes WHERE position = ?;
-- name: DeleteRepository :exec
DELETE FROM repositories WHERE name = ?;
