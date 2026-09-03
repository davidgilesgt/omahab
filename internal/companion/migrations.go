package companion

import "github.com/omahab/omahab/internal/store"

// Migrations returns SQLite migrations owned by companion/environments.
// Order is fixed; Migrate is idempotent via schema_migrations.
func Migrations() []store.Migration {
	return []store.Migration{
		{
			Name: "environments-001-companion_devices",
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
	updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_companion_devices_token_hash ON companion_devices(token_hash);
`,
		},
		{
			Name: "environments-002-companion_enrollments",
			SQL: `
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
`,
		},
		{
			Name: "environments-003-environment_meta",
			SQL: `
CREATE TABLE IF NOT EXISTS environment_meta (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	revision INTEGER NOT NULL DEFAULT 0,
	variable_count INTEGER NOT NULL DEFAULT 0,
	updated_at TEXT NOT NULL
);
INSERT OR IGNORE INTO environment_meta (id, revision, variable_count, updated_at) VALUES (1, 0, 0, '1970-01-01T00:00:00Z');
`,
		},
		{
			Name: "environments-004-environment_variables",
			SQL: `
CREATE TABLE IF NOT EXISTS environment_variables (
	name TEXT PRIMARY KEY,
	secret_id TEXT,
	version INTEGER NOT NULL DEFAULT 1,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
`,
		},
		{
			Name: "environments-005-device_environment_grants",
			SQL: `
CREATE TABLE IF NOT EXISTS device_environment_grants (
	device_id TEXT PRIMARY KEY,
	granted_at TEXT NOT NULL,
	created_at TEXT NOT NULL,
	FOREIGN KEY(device_id) REFERENCES companion_devices(id) ON DELETE CASCADE
);
`,
		},
		{
			Name: "environments-006-companion_device_identity",
			SQL: `
ALTER TABLE companion_devices ADD COLUMN hostname TEXT NOT NULL DEFAULT '';
ALTER TABLE companion_devices ADD COLUMN platform TEXT NOT NULL DEFAULT '';
ALTER TABLE companion_devices ADD COLUMN arch TEXT NOT NULL DEFAULT '';
ALTER TABLE companion_devices ADD COLUMN clientd_version TEXT NOT NULL DEFAULT '';
ALTER TABLE companion_devices ADD COLUMN shell TEXT NOT NULL DEFAULT '';
ALTER TABLE companion_devices ADD COLUMN env_revision INTEGER NOT NULL DEFAULT 0;
ALTER TABLE companion_devices ADD COLUMN env_variable_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE companion_devices ADD COLUMN backup_last_snapshot TEXT;
ALTER TABLE companion_devices ADD COLUMN forgejo_token_name TEXT NOT NULL DEFAULT '';
`,
		},
	}
}
