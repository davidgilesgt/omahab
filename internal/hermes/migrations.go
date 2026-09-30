package hermes

import "github.com/omahab/omahab/internal/store"

// Migrations returns the schema owned by the hermes controller: only
// remote-connection metadata remains; all project isolation tables were
// dropped in Step 3.
func Migrations() []store.Migration {
	return []store.Migration{
		{
			Name: "hermes-001-baseline",
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
	}
}
