package companion

import "github.com/omahab/omahab/internal/store"

// Migrations returns SQLite migrations owned by companion/environments.
// A single baseline creates the whole schema; Migrate is idempotent via
// schema_migrations.
func Migrations() []store.Migration {
	return []store.Migration{
		{
			Name: "environments-001-baseline",
			SQL: `
CREATE TABLE IF NOT EXISTS companion_devices (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	token_hash TEXT NOT NULL UNIQUE,
	token_prefix TEXT NOT NULL,
	allow_provider_oauth INTEGER NOT NULL DEFAULT 0,
	last_seen_at TEXT,
	revoked_at TEXT,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	hostname TEXT NOT NULL DEFAULT '',
	platform TEXT NOT NULL DEFAULT '',
	arch TEXT NOT NULL DEFAULT '',
	clientd_version TEXT NOT NULL DEFAULT '',
	shell TEXT NOT NULL DEFAULT '',
	env_revision INTEGER NOT NULL DEFAULT 0,
	env_variable_count INTEGER NOT NULL DEFAULT 0,
	backup_last_snapshot TEXT,
	forgejo_token_name TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_companion_devices_token_hash ON companion_devices(token_hash);

CREATE TABLE IF NOT EXISTS companion_enrollments (
	id TEXT PRIMARY KEY,
	code_hash TEXT NOT NULL UNIQUE,
	code_prefix TEXT NOT NULL DEFAULT '',
	expires_at TEXT NOT NULL,
	consumed_at TEXT,
	created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_companion_enrollments_code_hash ON companion_enrollments(code_hash);
CREATE INDEX IF NOT EXISTS idx_companion_enrollments_expires_at ON companion_enrollments(expires_at);

CREATE TABLE IF NOT EXISTS environment_meta (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	revision INTEGER NOT NULL DEFAULT 0,
	variable_count INTEGER NOT NULL DEFAULT 0,
	updated_at TEXT NOT NULL
);
INSERT OR IGNORE INTO environment_meta (id, revision, variable_count, updated_at) VALUES (1, 0, 0, '1970-01-01T00:00:00Z');

CREATE TABLE IF NOT EXISTS environment_variables (
	name TEXT PRIMARY KEY,
	secret_id TEXT,
	version INTEGER NOT NULL DEFAULT 1,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS device_environment_grants (
	device_id TEXT PRIMARY KEY,
	granted_at TEXT NOT NULL,
	created_at TEXT NOT NULL,
	FOREIGN KEY(device_id) REFERENCES companion_devices(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS device_revocation_pending (
	device_id TEXT NOT NULL,
	target TEXT NOT NULL,
	handle TEXT NOT NULL DEFAULT '',
	alias TEXT NOT NULL DEFAULT '',
	attempts INTEGER NOT NULL DEFAULT 0,
	last_error TEXT NOT NULL DEFAULT '',
	updated_at TEXT NOT NULL,
	PRIMARY KEY (device_id, target, handle)
);
CREATE INDEX IF NOT EXISTS idx_device_revocation_pending_device ON device_revocation_pending(device_id);
`,
		},
	}
}
