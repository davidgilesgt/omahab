-- schema.sql — sqlc view of the SQLite schema owned by package store.
-- The authoritative DDL lives in Go migrations (store.Migrations and the
-- ad-hoc schema_migrations creation in migrate.go). This file is a
-- sqlc-readable copy; keep it in sync with core-001-instance.
-- Regenerate with: sqlc generate

CREATE TABLE IF NOT EXISTS instance (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    id TEXT NOT NULL UNIQUE,
    domain TEXT NOT NULL,
    tailnet TEXT NOT NULL DEFAULT '',
    tailscale_ip TEXT NOT NULL DEFAULT '',
    assistant_name TEXT NOT NULL,
    assistant_slug TEXT NOT NULL,
    created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS schema_migrations (
    name TEXT PRIMARY KEY,
    applied_at TEXT NOT NULL
);
