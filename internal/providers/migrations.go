package providers

import "github.com/omahab/omahab/internal/store"

// Migrations returns the schema owned by the provider credential broker.
func Migrations() []store.Migration {
	return []store.Migration{
		{
			Name: "providers-001-baseline",
			SQL: `
CREATE TABLE IF NOT EXISTS provider_credentials (
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
	plan TEXT NOT NULL DEFAULT '',
	authorized_coding_use INTEGER NOT NULL DEFAULT 0,
	authorized_at TEXT,
	CHECK ((managed_by = 'omahab' AND secret_id IS NOT NULL AND external_ref IS NULL) OR (managed_by = 'litellm' AND secret_id IS NULL AND external_ref IN ('chatgpt','xai_oauth')))
);
CREATE INDEX IF NOT EXISTS idx_provider_credentials_provider ON provider_credentials(provider);
CREATE INDEX IF NOT EXISTS idx_provider_credentials_secret_id ON provider_credentials(secret_id);
CREATE INDEX IF NOT EXISTS idx_provider_credentials_health ON provider_credentials(health);
CREATE INDEX IF NOT EXISTS idx_provider_credentials_managed_by ON provider_credentials(managed_by);

CREATE TABLE IF NOT EXISTS provider_aliases (
	name TEXT PRIMARY KEY,
	credential_id TEXT NOT NULL REFERENCES provider_credentials(id) ON DELETE RESTRICT,
	model TEXT NOT NULL,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_provider_aliases_credential_id ON provider_aliases(credential_id);

CREATE TABLE IF NOT EXISTS provider_virtual_keys (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	key_hash TEXT NOT NULL UNIQUE,
	key_prefix TEXT NOT NULL,
	scopes TEXT NOT NULL DEFAULT '',
	expires_at TEXT,
	revoked_at TEXT,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	gateway_key_id TEXT,
	owner_kind TEXT CHECK(owner_kind IN ('hermes','device','harness')),
	owner_id TEXT,
	rpm_limit INTEGER CHECK(rpm_limit IS NULL OR rpm_limit > 0),
	tpm_limit INTEGER CHECK(tpm_limit IS NULL OR tpm_limit > 0),
	concurrency_limit INTEGER CHECK(concurrency_limit IS NULL OR concurrency_limit > 0),
	budget_amount REAL CHECK(budget_amount IS NULL OR budget_amount > 0),
	budget_duration TEXT
);
CREATE INDEX IF NOT EXISTS idx_provider_virtual_keys_key_hash ON provider_virtual_keys(key_hash);
CREATE INDEX IF NOT EXISTS idx_provider_virtual_keys_name ON provider_virtual_keys(name);
CREATE INDEX IF NOT EXISTS idx_provider_virtual_keys_gateway_key_id ON provider_virtual_keys(gateway_key_id);
CREATE INDEX IF NOT EXISTS idx_provider_virtual_keys_owner ON provider_virtual_keys(owner_kind, owner_id);
`,
		},
	}
}
