package providers

import "github.com/omahab/omahab/internal/store"

// Migrations returns the SQLite migrations owned by the provider credential broker.
func Migrations() []store.Migration {
	return []store.Migration{
		{
			Name: "providers-001-credentials",
			SQL: `
CREATE TABLE IF NOT EXISTS provider_credentials (
	id TEXT PRIMARY KEY,
	provider TEXT NOT NULL,
	credential_type TEXT NOT NULL,
	display_name TEXT NOT NULL DEFAULT '',
	secret_id TEXT NOT NULL,
	entitlement TEXT NOT NULL DEFAULT 'unknown',
	entitlement_message TEXT NOT NULL DEFAULT '',
	expires_at TEXT,
	health TEXT NOT NULL DEFAULT 'unknown',
	last_checked_at TEXT,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_provider_credentials_provider ON provider_credentials(provider);
CREATE INDEX IF NOT EXISTS idx_provider_credentials_secret_id ON provider_credentials(secret_id);
CREATE INDEX IF NOT EXISTS idx_provider_credentials_health ON provider_credentials(health);
`,
		},
		{
			Name: "providers-002-aliases",
			SQL: `
CREATE TABLE IF NOT EXISTS provider_aliases (
	name TEXT PRIMARY KEY,
	credential_id TEXT NOT NULL REFERENCES provider_credentials(id) ON DELETE RESTRICT,
	model TEXT NOT NULL,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_provider_aliases_credential_id ON provider_aliases(credential_id);
`,
		},
		{
			Name: "providers-003-virtual_keys",
			SQL: `
CREATE TABLE IF NOT EXISTS provider_virtual_keys (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	key_hash TEXT NOT NULL UNIQUE,
	key_prefix TEXT NOT NULL,
	scopes TEXT NOT NULL DEFAULT '',
	expires_at TEXT,
	revoked_at TEXT,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_provider_virtual_keys_key_hash ON provider_virtual_keys(key_hash);
CREATE INDEX IF NOT EXISTS idx_provider_virtual_keys_name ON provider_virtual_keys(name);
`,
		},
	}
}
