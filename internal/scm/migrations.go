package scm

import "github.com/omahab/omahab/internal/store"

// Migrations returns the SQLite migrations owned by the SCM controller.
func Migrations() []store.Migration {
	return []store.Migration{
		{
			Name: "scm-001-repositories",
			SQL: `
CREATE TABLE IF NOT EXISTS scm_repositories (
	id TEXT PRIMARY KEY,
	project_id TEXT NOT NULL UNIQUE,
	owner TEXT NOT NULL,
	name TEXT NOT NULL,
	clone_url TEXT NOT NULL DEFAULT '',
	default_branch TEXT NOT NULL DEFAULT 'master',
	forgejo_remote_id INTEGER NOT NULL DEFAULT 0,
	private INTEGER NOT NULL DEFAULT 1,
	actions_disabled INTEGER NOT NULL DEFAULT 0,
	desired_state TEXT NOT NULL DEFAULT 'provisioned',
	observed_state TEXT NOT NULL DEFAULT 'pending',
	observed_detail TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	UNIQUE(owner, name)
);
CREATE INDEX IF NOT EXISTS idx_scm_repositories_project_id ON scm_repositories(project_id);
`,
		},
		{
			Name: "scm-002-ci_repos",
			SQL: `
CREATE TABLE IF NOT EXISTS scm_ci_repos (
	id TEXT PRIMARY KEY,
	repository_id TEXT NOT NULL UNIQUE REFERENCES scm_repositories(id) ON DELETE CASCADE,
	woodpecker_repo_id INTEGER NOT NULL DEFAULT 0,
	forgejo_remote_id INTEGER NOT NULL DEFAULT 0,
	pipeline_path TEXT NOT NULL DEFAULT '.woodpecker.yaml',
	enabled INTEGER NOT NULL DEFAULT 1,
	trusted INTEGER NOT NULL DEFAULT 0,
	desired_state TEXT NOT NULL DEFAULT 'enabled',
	observed_state TEXT NOT NULL DEFAULT 'pending',
	observed_detail TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_scm_ci_repos_woodpecker_repo_id ON scm_ci_repos(woodpecker_repo_id);
`,
		},
		{
			Name: "scm-003-ci_runs",
			SQL: `
CREATE TABLE IF NOT EXISTS scm_ci_runs (
	id TEXT PRIMARY KEY,
	repository_id TEXT NOT NULL REFERENCES scm_repositories(id) ON DELETE CASCADE,
	run_number INTEGER NOT NULL,
	woodpecker_run_id INTEGER NOT NULL DEFAULT 0,
	status TEXT NOT NULL DEFAULT 'pending',
	branch TEXT NOT NULL DEFAULT '',
	commit_sha TEXT NOT NULL DEFAULT '',
	event TEXT NOT NULL DEFAULT '',
	message TEXT NOT NULL DEFAULT '',
	author TEXT NOT NULL DEFAULT '',
	started_at TEXT,
	finished_at TEXT,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	UNIQUE(repository_id, run_number)
);
CREATE INDEX IF NOT EXISTS idx_scm_ci_runs_repository_id ON scm_ci_runs(repository_id);
CREATE INDEX IF NOT EXISTS idx_scm_ci_runs_status ON scm_ci_runs(status);
`,
		},
		{
			Name: "scm-004-mirrors",
			SQL: `
CREATE TABLE IF NOT EXISTS scm_mirrors (
	id TEXT PRIMARY KEY,
	repository_id TEXT NOT NULL UNIQUE REFERENCES scm_repositories(id) ON DELETE CASCADE,
	remote_url TEXT NOT NULL,
	remote_name TEXT NOT NULL DEFAULT 'github',
	credential_secret_ref TEXT NOT NULL DEFAULT '',
	interval_seconds INTEGER NOT NULL DEFAULT 0,
	lfs_enabled INTEGER NOT NULL DEFAULT 0,
	desired_state TEXT NOT NULL DEFAULT 'configured',
	observed_state TEXT NOT NULL DEFAULT 'pending',
	observed_detail TEXT NOT NULL DEFAULT '',
	last_synced_at TEXT,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_scm_mirrors_repository_id ON scm_mirrors(repository_id);
`,
		},
	}
}
