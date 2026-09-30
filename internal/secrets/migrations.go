package secrets

import "github.com/omahab/omahab/internal/store"

// Migrations returns the schema owned by the secrets broker.
func Migrations() []store.Migration {
	return []store.Migration{
		{
			Name: "secrets-001-baseline",
			SQL: `
CREATE TABLE IF NOT EXISTS secrets (
	id TEXT PRIMARY KEY,
	scope TEXT NOT NULL,
	name TEXT NOT NULL,
	version INTEGER NOT NULL DEFAULT 1,
	nonce BLOB NOT NULL,
	ciphertext BLOB NOT NULL,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	UNIQUE(scope, name)
);
CREATE INDEX IF NOT EXISTS idx_secrets_scope_name ON secrets(scope, name);
`,
		},
	}
}
