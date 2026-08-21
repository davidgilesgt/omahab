package identity

import "github.com/omahab/omahab/internal/store"

// Migrations returns the SQLite migrations owned by the identity controller.
func Migrations() []store.Migration {
	return []store.Migration{
		{
			Name: "identity-001-recoveries",
			SQL: `
CREATE TABLE IF NOT EXISTS identity_recoveries (
	id TEXT PRIMARY KEY,
	email TEXT NOT NULL,
	code TEXT NOT NULL,
	url TEXT NOT NULL,
	expires_at TEXT NOT NULL,
	used_at TEXT,
	created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_identity_recoveries_email ON identity_recoveries(email);
CREATE INDEX IF NOT EXISTS idx_identity_recoveries_expires_at ON identity_recoveries(expires_at);
`,
		},
		{
			Name: "identity-002-security_events",
			SQL: `
CREATE TABLE IF NOT EXISTS identity_security_events (
	id TEXT PRIMARY KEY,
	type TEXT NOT NULL,
	email TEXT NOT NULL,
	message TEXT NOT NULL,
	created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_identity_security_events_email ON identity_security_events(email);
CREATE INDEX IF NOT EXISTS idx_identity_security_events_created_at ON identity_security_events(created_at);
`,
		},
	}
}
