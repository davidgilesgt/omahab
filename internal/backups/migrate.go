package backups

import "github.com/omahab/omahab/internal/store"

// Migrations returns the schema migrations owned by the backup controller.
// Each migration is a single statement so it composes with any migration
// runner that prepares or transactionally applies SQL.
func Migrations() []store.Migration {
	return []store.Migration{
		{
			Name: "backups_0001_create_backup_repositories",
			SQL: `CREATE TABLE IF NOT EXISTS backup_repositories (
	id TEXT PRIMARY KEY,
	label TEXT NOT NULL,
	location TEXT NOT NULL,
	secret_id TEXT NOT NULL,
	secret_version INTEGER NOT NULL CHECK (secret_version >= 1),
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
)`,
		},
		{
			Name: "backups_0002_backup_repositories_location_key",
			SQL:  `CREATE UNIQUE INDEX IF NOT EXISTS backup_repositories_location ON backup_repositories (location)`,
		},
		{
			Name: "backups_0003_create_backup_runs",
			SQL: `CREATE TABLE IF NOT EXISTS backup_runs (
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
)`,
		},
		{
			// Database-level guarantee that at most one operation is
			// active at a time, across processes and restarts.
			Name: "backups_0004_backup_runs_single_active",
			SQL:  `CREATE UNIQUE INDEX IF NOT EXISTS backup_runs_single_active ON backup_runs (status) WHERE status = 'running'`,
		},
		{
			Name: "backups_0005_backup_runs_repo_started_idx",
			SQL:  `CREATE INDEX IF NOT EXISTS backup_runs_repo_started ON backup_runs (repository_id, started_at DESC)`,
		},
		{
			Name: "backups_0006_create_backup_snapshots",
			SQL: `CREATE TABLE IF NOT EXISTS backup_snapshots (
	repository_id TEXT NOT NULL REFERENCES backup_repositories (id),
	id TEXT NOT NULL,
	run_id TEXT NOT NULL REFERENCES backup_runs (id),
	created_at TEXT NOT NULL,
	paths TEXT NOT NULL,
	size_bytes INTEGER NOT NULL DEFAULT 0,
	file_count INTEGER NOT NULL DEFAULT 0,
	verified_at TEXT,
	PRIMARY KEY (repository_id, id)
)`,
		},
		{
			Name: "backups_0007_create_backup_hook_results",
			SQL: `CREATE TABLE IF NOT EXISTS backup_hook_results (
	id TEXT PRIMARY KEY,
	run_id TEXT NOT NULL REFERENCES backup_runs (id),
	application TEXT NOT NULL,
	hook TEXT NOT NULL CHECK (hook IN ('pre_backup','post_restore')),
	status TEXT NOT NULL CHECK (status IN ('ok','failed','skipped')),
	exit_code INTEGER,
	error TEXT,
	output TEXT,
	started_at TEXT NOT NULL,
	finished_at TEXT
)`,
		},
		{
			Name: "backups_0008_backup_hook_results_run_idx",
			SQL:  `CREATE INDEX IF NOT EXISTS backup_hook_results_run ON backup_hook_results (run_id)`,
		},
		{
			Name: "backups_0009_create_backup_verifications",
			SQL: `CREATE TABLE IF NOT EXISTS backup_verifications (
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
)`,
		},
		{
			Name: "backups_0010_backup_verifications_snapshot_idx",
			SQL:  `CREATE INDEX IF NOT EXISTS backup_verifications_snapshot ON backup_verifications (repository_id, snapshot_id)`,
		},
		{
			Name: "backups_0011_create_backup_health_state",
			SQL: `CREATE TABLE IF NOT EXISTS backup_health_state (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	health TEXT NOT NULL,
	last_backup_at TEXT,
	last_verified_at TEXT,
	evaluated_at TEXT NOT NULL
)`,
		},
	}
}
