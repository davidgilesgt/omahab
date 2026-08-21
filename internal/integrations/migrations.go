package integrations

import "github.com/omahab/omahab/internal/store"

// Migrations returns the SQLite migrations owned by the integrations controller.
func Migrations() []store.Migration {
	return []store.Migration{
		{
			Name: "integrations-001-ha_integrations",
			SQL: `
CREATE TABLE IF NOT EXISTS ha_integrations (
	id TEXT PRIMARY KEY,
	server_url TEXT NOT NULL,
	status TEXT NOT NULL DEFAULT 'connected',
	last_validated_at TEXT,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
`,
		},
	}
}
