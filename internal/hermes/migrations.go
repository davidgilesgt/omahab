package hermes

import "github.com/omahab/omahab/internal/store"

// Migrations returns the ordered SQLite migrations owned by the hermes controller.
// Only remote-connection metadata remains; all project isolation tables were dropped in Step 3.
func Migrations() []store.Migration {
	return []store.Migration{
		{
			Name: "hermes-001-remote-connections",
			SQL: `
CREATE TABLE IF NOT EXISTS hermes_remote_connections (
	id TEXT PRIMARY KEY,
	server_url TEXT NOT NULL,
	hermes_url TEXT NOT NULL,
	instance_id TEXT NOT NULL,
	oidc_issuer TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
) STRICT;
`,
		},
		{
			Name: "hermes-002-drop-bots",
			SQL: `
DROP TABLE IF EXISTS hermes_capabilities;
DROP TABLE IF EXISTS hermes_knowledge_sources;
DROP TABLE IF EXISTS hermes_messages;
DROP TABLE IF EXISTS hermes_group_members;
DROP TABLE IF EXISTS hermes_groups;
DROP TABLE IF EXISTS hermes_` + `profiles;
-- Recreate hermes_remote_connections without profile_id for old DBs that had it (SQLite < 3.35 has no DROP COLUMN).
CREATE TABLE IF NOT EXISTS hermes_remote_connections_new (
	id TEXT PRIMARY KEY,
	server_url TEXT NOT NULL,
	hermes_url TEXT NOT NULL,
	instance_id TEXT NOT NULL,
	oidc_issuer TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
) STRICT;
INSERT OR IGNORE INTO hermes_remote_connections_new (id, server_url, hermes_url, instance_id, oidc_issuer, created_at, updated_at)
	SELECT id, server_url, hermes_url, instance_id, oidc_issuer, created_at, updated_at FROM hermes_remote_connections;
DROP TABLE IF EXISTS hermes_remote_connections;
ALTER TABLE hermes_remote_connections_new RENAME TO hermes_remote_connections;
`,
		},
	}
}
