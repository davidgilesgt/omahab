-- query.sql — typed queries covering internal/store's existing operations.
-- Regenerate with: sqlc generate

-- name: GetInstance :one
SELECT id, domain, tailnet, tailscale_ip, assistant_name, assistant_slug, created_at
FROM instance
WHERE singleton = 1;

-- name: GetInstanceIdentity :one
SELECT id, created_at
FROM instance
WHERE singleton = 1;

-- name: CreateInstance :exec
INSERT INTO instance (singleton, id, domain, tailnet, tailscale_ip, assistant_name, assistant_slug, created_at)
VALUES (1, ?, ?, ?, ?, ?, ?, ?);

-- name: UpdateInstance :exec
UPDATE instance
SET domain = ?, tailnet = ?, tailscale_ip = ?, assistant_name = ?, assistant_slug = ?
WHERE singleton = 1;

-- name: CreateSchemaMigrations :exec
CREATE TABLE IF NOT EXISTS schema_migrations (
    name TEXT PRIMARY KEY,
    applied_at TEXT NOT NULL
);

-- name: CheckSchemaMigration :one
SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE name = ?);

-- name: InsertSchemaMigration :exec
INSERT INTO schema_migrations (name, applied_at) VALUES (?, ?);

-- name: ListSchemaMigrations :many
SELECT name, applied_at FROM schema_migrations ORDER BY name;
