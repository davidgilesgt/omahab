package knowledge

import "github.com/omahab/omahab/internal/store"

// Migrations returns the schema owned by the knowledge controller:
// sources, permissions, index generations, jobs/chunks, consents, and settings.
func Migrations() []store.Migration {
	return []store.Migration{
		{
			Name: "knowledge-001-baseline",
			SQL: `
CREATE TABLE IF NOT EXISTS knowledge_sources (
	id TEXT PRIMARY KEY,
	kind TEXT NOT NULL CHECK (kind IN ('paperless','karakeep','notes')),
	name TEXT NOT NULL UNIQUE,
	base_url TEXT NOT NULL,
	health TEXT NOT NULL DEFAULT 'unknown',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
) STRICT;
CREATE INDEX IF NOT EXISTS idx_knowledge_sources_kind ON knowledge_sources(kind);

CREATE TABLE IF NOT EXISTS knowledge_source_permissions (
	id TEXT PRIMARY KEY,
	source_id TEXT NOT NULL REFERENCES knowledge_sources(id) ON DELETE CASCADE,
	principal TEXT NOT NULL,
	permission TEXT NOT NULL DEFAULT 'read',
	granted_at TEXT NOT NULL,
	UNIQUE(source_id, principal)
) STRICT;
CREATE INDEX IF NOT EXISTS idx_knowledge_permissions_source ON knowledge_source_permissions(source_id);
CREATE INDEX IF NOT EXISTS idx_knowledge_permissions_principal ON knowledge_source_permissions(principal);

CREATE TABLE IF NOT EXISTS knowledge_index_generations (
	id TEXT PRIMARY KEY,
	source_id TEXT NOT NULL REFERENCES knowledge_sources(id) ON DELETE CASCADE,
	model_alias TEXT NOT NULL,
	model_id TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL CHECK (status IN ('pending','building','active','failed','superseded')),
	checksum TEXT NOT NULL DEFAULT '',
	failure_reason TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	activated_at TEXT,
	updated_at TEXT NOT NULL
) STRICT;
CREATE INDEX IF NOT EXISTS idx_knowledge_generations_source ON knowledge_index_generations(source_id);
CREATE INDEX IF NOT EXISTS idx_knowledge_generations_alias ON knowledge_index_generations(model_alias);
CREATE INDEX IF NOT EXISTS idx_knowledge_generations_status ON knowledge_index_generations(status);

CREATE TABLE IF NOT EXISTS knowledge_index_jobs (
	id TEXT PRIMARY KEY,
	generation_id TEXT NOT NULL REFERENCES knowledge_index_generations(id) ON DELETE CASCADE,
	source_id TEXT NOT NULL REFERENCES knowledge_sources(id) ON DELETE CASCADE,
	model_alias TEXT NOT NULL,
	status TEXT NOT NULL CHECK (status IN ('pending','running','succeeded','failed')),
	attempts INTEGER NOT NULL DEFAULT 0,
	error TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	started_at TEXT,
	finished_at TEXT,
	updated_at TEXT NOT NULL
) STRICT;
CREATE INDEX IF NOT EXISTS idx_knowledge_jobs_generation ON knowledge_index_jobs(generation_id);
CREATE INDEX IF NOT EXISTS idx_knowledge_jobs_source ON knowledge_index_jobs(source_id);
CREATE INDEX IF NOT EXISTS idx_knowledge_jobs_status ON knowledge_index_jobs(status);

CREATE TABLE IF NOT EXISTS knowledge_index_chunks (
	id TEXT PRIMARY KEY,
	generation_id TEXT NOT NULL REFERENCES knowledge_index_generations(id) ON DELETE CASCADE,
	source_document_id TEXT NOT NULL,
	chunk_index INTEGER NOT NULL,
	content_checksum TEXT NOT NULL,
	vector BLOB,
	created_at TEXT NOT NULL,
	UNIQUE(generation_id, source_document_id, chunk_index)
) STRICT;
CREATE INDEX IF NOT EXISTS idx_knowledge_chunks_generation ON knowledge_index_chunks(generation_id);
CREATE INDEX IF NOT EXISTS idx_knowledge_chunks_document ON knowledge_index_chunks(source_document_id);

CREATE TABLE IF NOT EXISTS knowledge_consents (
	id TEXT PRIMARY KEY,
	principal TEXT NOT NULL,
	provider TEXT NOT NULL,
	scope TEXT NOT NULL,
	granted INTEGER NOT NULL DEFAULT 1,
	granted_at TEXT NOT NULL,
	revoked_at TEXT,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	UNIQUE(principal, provider, scope)
) STRICT;
CREATE INDEX IF NOT EXISTS idx_knowledge_consents_principal ON knowledge_consents(principal);
CREATE INDEX IF NOT EXISTS idx_knowledge_consents_provider ON knowledge_consents(provider);

CREATE TABLE IF NOT EXISTS knowledge_settings (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL,
	updated_at TEXT NOT NULL
) STRICT;
`,
		},
	}
}
