package hermes

import "github.com/omahab/omahab/internal/store"

// Migrations returns the ordered SQLite migrations owned by the hermes controller.
func Migrations() []store.Migration {
	return []store.Migration{
		{
			Name: "hermes-001-profiles",
			SQL: `
CREATE TABLE IF NOT EXISTS hermes_profiles (
	id TEXT PRIMARY KEY,
	kind TEXT NOT NULL CHECK (kind IN ('default','project')),
	project_id TEXT,
	display_alias TEXT NOT NULL,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	UNIQUE(project_id)
) STRICT;

CREATE INDEX IF NOT EXISTS idx_hermes_profiles_kind ON hermes_profiles(kind);
CREATE INDEX IF NOT EXISTS idx_hermes_profiles_project_id ON hermes_profiles(project_id);
`,
		},
		{
			Name: "hermes-002-capabilities",
			SQL: `
CREATE TABLE IF NOT EXISTS hermes_capabilities (
	profile_id TEXT NOT NULL REFERENCES hermes_profiles(id) ON DELETE CASCADE,
	capability TEXT NOT NULL,
	granted_at TEXT NOT NULL,
	PRIMARY KEY (profile_id, capability)
) STRICT;

CREATE INDEX IF NOT EXISTS idx_hermes_capabilities_profile_id ON hermes_capabilities(profile_id);
CREATE INDEX IF NOT EXISTS idx_hermes_capabilities_capability ON hermes_capabilities(capability);
`,
		},
		{
			Name: "hermes-003-knowledge_sources",
			SQL: `
CREATE TABLE IF NOT EXISTS hermes_knowledge_sources (
	id TEXT PRIMARY KEY,
	profile_id TEXT NOT NULL REFERENCES hermes_profiles(id) ON DELETE CASCADE,
	source_type TEXT NOT NULL,
	resource_id TEXT NOT NULL,
	granted_at TEXT NOT NULL,
	UNIQUE(profile_id, source_type, resource_id)
) STRICT;

CREATE INDEX IF NOT EXISTS idx_hermes_knowledge_sources_profile_id ON hermes_knowledge_sources(profile_id);
CREATE INDEX IF NOT EXISTS idx_hermes_knowledge_sources_type ON hermes_knowledge_sources(source_type);
`,
		},
		{
			Name: "hermes-004-messages",
			SQL: `
CREATE TABLE IF NOT EXISTS hermes_messages (
	id TEXT PRIMARY KEY,
	from_profile_id TEXT NOT NULL REFERENCES hermes_profiles(id) ON DELETE CASCADE,
	to_profile_id TEXT NOT NULL REFERENCES hermes_profiles(id) ON DELETE CASCADE,
	kind TEXT NOT NULL CHECK (kind IN ('delegation','status_request','question','status','message','redirect','cancel')),
	body TEXT NOT NULL,
	created_at TEXT NOT NULL
) STRICT;

CREATE INDEX IF NOT EXISTS idx_hermes_messages_from ON hermes_messages(from_profile_id);
CREATE INDEX IF NOT EXISTS idx_hermes_messages_to ON hermes_messages(to_profile_id);
CREATE INDEX IF NOT EXISTS idx_hermes_messages_kind ON hermes_messages(kind);
`,
		},
		{
			Name: "hermes-005-groups",
			SQL: `
CREATE TABLE IF NOT EXISTS hermes_groups (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL UNIQUE,
	created_at TEXT NOT NULL
) STRICT;

CREATE TABLE IF NOT EXISTS hermes_group_members (
	group_id TEXT NOT NULL REFERENCES hermes_groups(id) ON DELETE CASCADE,
	profile_id TEXT NOT NULL REFERENCES hermes_profiles(id) ON DELETE CASCADE,
	added_at TEXT NOT NULL,
	PRIMARY KEY (group_id, profile_id)
) STRICT;

CREATE INDEX IF NOT EXISTS idx_hermes_group_members_profile_id ON hermes_group_members(profile_id);
CREATE INDEX IF NOT EXISTS idx_hermes_group_members_group_id ON hermes_group_members(group_id);
`,
		},
		{
			Name: "hermes-006-remote_connections",
			SQL: `
CREATE TABLE IF NOT EXISTS hermes_remote_connections (
	id TEXT PRIMARY KEY,
	profile_id TEXT NOT NULL REFERENCES hermes_profiles(id) ON DELETE CASCADE,
	server_url TEXT NOT NULL,
	hermes_url TEXT NOT NULL,
	instance_id TEXT NOT NULL,
	oidc_issuer TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	UNIQUE(profile_id)
) STRICT;

CREATE INDEX IF NOT EXISTS idx_hermes_remote_connections_profile_id ON hermes_remote_connections(profile_id);
`,
		},
	}
}
