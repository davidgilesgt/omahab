package workspaces

import "github.com/omahab/omahab/internal/store"

// Migrations returns the SQLite migrations owned by the workspaces controller.
func Migrations() []store.Migration {
	return []store.Migration{
		{
			Name: "workspaces-001-workspaces",
			SQL: `
CREATE TABLE IF NOT EXISTS workspaces (
	id TEXT PRIMARY KEY,
	project_id TEXT NOT NULL,
	branch TEXT NOT NULL,
	agent TEXT NOT NULL DEFAULT '',
	devcontainer_source TEXT NOT NULL DEFAULT 'default',
	status TEXT NOT NULL DEFAULT 'pending',
	last_active_at TEXT NOT NULL,
	expires_at TEXT,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	UNIQUE(project_id, branch)
);
CREATE INDEX IF NOT EXISTS idx_workspaces_project_id ON workspaces(project_id);
CREATE INDEX IF NOT EXISTS idx_workspaces_status ON workspaces(status);
`,
		},
		{
			Name: "workspaces-002-capabilities",
			SQL: `
CREATE TABLE IF NOT EXISTS workspace_capabilities (
	id TEXT PRIMARY KEY,
	workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
	token_hash TEXT NOT NULL,
	expires_at TEXT NOT NULL,
	consumed_at TEXT,
	created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_workspace_capabilities_workspace_id ON workspace_capabilities(workspace_id);
CREATE INDEX IF NOT EXISTS idx_workspace_capabilities_token_hash ON workspace_capabilities(token_hash);
`,
		},
		{
			Name: "workspaces-003-title-instructions-gateway",
			SQL: `
ALTER TABLE workspaces ADD COLUMN title TEXT NOT NULL DEFAULT '';
ALTER TABLE workspaces ADD COLUMN instructions TEXT NOT NULL DEFAULT '';
ALTER TABLE workspaces ADD COLUMN gateway_key_id TEXT;
ALTER TABLE workspaces ADD COLUMN forgejo_token_name TEXT;
`,
		},
	}
}
