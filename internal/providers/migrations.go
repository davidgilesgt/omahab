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
		{
			Name: "providers-004-managed-metadata",
			SQL: `
PRAGMA foreign_keys=OFF;
CREATE TABLE provider_credentials_new (
	id TEXT PRIMARY KEY,
	provider TEXT NOT NULL,
	credential_type TEXT NOT NULL,
	display_name TEXT NOT NULL DEFAULT '',
	secret_id TEXT,
	managed_by TEXT NOT NULL CHECK(managed_by IN ('omahab','litellm')),
	external_ref TEXT CHECK(external_ref IS NULL OR external_ref IN ('chatgpt','xai_oauth')),
	entitlement TEXT NOT NULL DEFAULT 'unknown',
	entitlement_message TEXT NOT NULL DEFAULT '',
	expires_at TEXT,
	health TEXT NOT NULL DEFAULT 'unknown',
	last_checked_at TEXT,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	CHECK ((managed_by = 'omahab' AND secret_id IS NOT NULL AND external_ref IS NULL) OR (managed_by = 'litellm' AND secret_id IS NULL AND external_ref IN ('chatgpt','xai_oauth')))
);
INSERT INTO provider_credentials_new (id, provider, credential_type, display_name, secret_id, managed_by, external_ref, entitlement, entitlement_message, expires_at, health, last_checked_at, created_at, updated_at)
	SELECT id, provider, credential_type, display_name, secret_id, 'omahab', NULL, entitlement, entitlement_message, expires_at, health, last_checked_at, created_at, updated_at FROM provider_credentials;
DROP TABLE provider_credentials;
ALTER TABLE provider_credentials_new RENAME TO provider_credentials;
CREATE INDEX IF NOT EXISTS idx_provider_credentials_provider ON provider_credentials(provider);
CREATE INDEX IF NOT EXISTS idx_provider_credentials_secret_id ON provider_credentials(secret_id);
CREATE INDEX IF NOT EXISTS idx_provider_credentials_health ON provider_credentials(health);
CREATE INDEX IF NOT EXISTS idx_provider_credentials_managed_by ON provider_credentials(managed_by);
PRAGMA foreign_keys=ON;
`,
		},
		{
			Name: "providers-005-virtual-keys-extended",
			SQL: `
ALTER TABLE provider_virtual_keys ADD COLUMN gateway_key_id TEXT;
ALTER TABLE provider_virtual_keys ADD COLUMN owner_kind TEXT CHECK(owner_kind IN ('hermes','device','harness'));
ALTER TABLE provider_virtual_keys ADD COLUMN owner_id TEXT;
ALTER TABLE provider_virtual_keys ADD COLUMN rpm_limit INTEGER CHECK(rpm_limit IS NULL OR rpm_limit > 0);
ALTER TABLE provider_virtual_keys ADD COLUMN tpm_limit INTEGER CHECK(tpm_limit IS NULL OR tpm_limit > 0);
ALTER TABLE provider_virtual_keys ADD COLUMN concurrency_limit INTEGER CHECK(concurrency_limit IS NULL OR concurrency_limit > 0);
ALTER TABLE provider_virtual_keys ADD COLUMN budget_amount REAL CHECK(budget_amount IS NULL OR budget_amount > 0);
ALTER TABLE provider_virtual_keys ADD COLUMN budget_duration TEXT;
CREATE INDEX IF NOT EXISTS idx_provider_virtual_keys_gateway_key_id ON provider_virtual_keys(gateway_key_id);
CREATE INDEX IF NOT EXISTS idx_provider_virtual_keys_owner ON provider_virtual_keys(owner_kind, owner_id);
`,
		},
	}
}
