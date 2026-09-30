package backups

import "github.com/omahab/omahab/internal/store"

// Migrations returns the schema migrations owned by the backup controller.
// A single baseline creates the whole backup schema; Migrate applies it
// idempotently via schema_migrations. The attached flag marks repositories
// that keep their exact source URL: the instance suffix is never appended
// at run time.
func Migrations() []store.Migration {
	return []store.Migration{
		{
			Name: "backups_0001_baseline",
			SQL: `CREATE TABLE IF NOT EXISTS backup_repositories (
	id TEXT PRIMARY KEY,
	label TEXT NOT NULL,
	location TEXT NOT NULL,
	secret_id TEXT NOT NULL,
	secret_version INTEGER NOT NULL CHECK (secret_version >= 1),
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	attached INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS backup_repositories_location ON backup_repositories (location);

CREATE TABLE IF NOT EXISTS backup_runs (
	id TEXT PRIMARY KEY,
	kind TEXT NOT NULL CHECK (kind IN ('backup','verify','restore')),
	repository_id TEXT NOT NULL REFERENCES backup_repositories (id),
	status TEXT NOT NULL CHECK (status IN ('running','completed','failed')),
	triggered_by TEXT NOT NULL,
	stage TEXT NOT NULL DEFAULT '',
	snapshot_id TEXT NOT NULL DEFAULT '',
	error TEXT,
	stats TEXT,
	started_at TEXT NOT NULL,
	finished_at TEXT
);
-- Database-level guarantee that at most one operation is active at a time,
-- across processes and restarts.
CREATE UNIQUE INDEX IF NOT EXISTS backup_runs_single_active ON backup_runs (status) WHERE status = 'running';
CREATE INDEX IF NOT EXISTS backup_runs_repo_started ON backup_runs (repository_id, started_at DESC);

CREATE TABLE IF NOT EXISTS backup_snapshots (
	repository_id TEXT NOT NULL REFERENCES backup_repositories (id),
	id TEXT NOT NULL,
	run_id TEXT NOT NULL REFERENCES backup_runs (id),
	created_at TEXT NOT NULL,
	paths TEXT NOT NULL,
	size_bytes INTEGER NOT NULL DEFAULT 0,
	file_count INTEGER NOT NULL DEFAULT 0,
	verified_at TEXT,
	PRIMARY KEY (repository_id, id)
);

CREATE TABLE IF NOT EXISTS backup_verifications (
	id TEXT PRIMARY KEY,
	run_id TEXT NOT NULL REFERENCES backup_runs (id),
	repository_id TEXT NOT NULL REFERENCES backup_repositories (id),
	snapshot_id TEXT NOT NULL,
	status TEXT NOT NULL CHECK (status IN ('running','passed','failed')),
	target TEXT NOT NULL,
	files_restored INTEGER NOT NULL DEFAULT 0,
	bytes_restored INTEGER NOT NULL DEFAULT 0,
	started_at TEXT NOT NULL,
	finished_at TEXT,
	cleaned_at TEXT,
	error TEXT,
	cleanup_error TEXT
);
CREATE INDEX IF NOT EXISTS backup_verifications_snapshot ON backup_verifications (repository_id, snapshot_id);

-- Isolated verification depth: manifest presence, SQLite integrity results
-- and disposable-database PostgreSQL restore results per verification.
-- data_verified (extraction plus database proof) stays distinct from
-- restore_tested (full app startup proof, recorded by live restores).
CREATE TABLE IF NOT EXISTS verification_depth (
	verification_id TEXT PRIMARY KEY REFERENCES backup_verifications (id) ON DELETE CASCADE,
	manifest_found INTEGER NOT NULL DEFAULT 0,
	sqlite_checked INTEGER NOT NULL DEFAULT 0,
	sqlite_ok INTEGER NOT NULL DEFAULT 0,
	pg_checked INTEGER NOT NULL DEFAULT 0,
	pg_ok INTEGER NOT NULL DEFAULT 0,
	pg_skipped TEXT NOT NULL DEFAULT '',
	checked_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS backup_health_state (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	health TEXT NOT NULL,
	last_backup_at TEXT,
	last_verified_at TEXT,
	evaluated_at TEXT NOT NULL
);

-- Retention state: the applied policy and the last outcome per repository,
-- so the weekly prune timer and post-backup passes stay idempotent and the
-- UI can show the policy read-only.
CREATE TABLE IF NOT EXISTS backup_retention_state (
	repository_id TEXT PRIMARY KEY REFERENCES backup_repositories(id) ON DELETE CASCADE,
	policy_keep_daily INTEGER NOT NULL,
	policy_keep_weekly INTEGER NOT NULL,
	policy_keep_monthly INTEGER NOT NULL,
	policy_keep_yearly INTEGER NOT NULL,
	applied_at TEXT NOT NULL,
	removed INTEGER NOT NULL DEFAULT 0,
	repo_size_bytes INTEGER NOT NULL DEFAULT 0,
	repo_snapshot_count INTEGER NOT NULL DEFAULT 0
)`,
		},
	}
}
