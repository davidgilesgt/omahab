package identity

import "github.com/omahab/omahab/internal/store"

// Migrations returns the schema owned by the identity controller.
func Migrations() []store.Migration {
	return []store.Migration{
		{
			Name: "identity-001-baseline",
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

CREATE TABLE IF NOT EXISTS identity_security_events (
	id TEXT PRIMARY KEY,
	type TEXT NOT NULL,
	email TEXT NOT NULL,
	message TEXT NOT NULL,
	created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_identity_security_events_email ON identity_security_events(email);
CREATE INDEX IF NOT EXISTS idx_identity_security_events_created_at ON identity_security_events(created_at);

-- Single-active enrollment invitations: every IssueUserEnrollment supersedes
-- prior rows for the user so only the latest link is current. revoked tracks
-- whether the previous Pocket ID one-time token was confirmed consumed via
-- ConsumeOneTimeToken; note records why an old link may remain valid.
CREATE TABLE IF NOT EXISTS identity_invitations (
	id TEXT PRIMARY KEY,
	user_id TEXT NOT NULL,
	pocket_user_id TEXT NOT NULL DEFAULT '',
	code TEXT NOT NULL,
	url TEXT NOT NULL DEFAULT '',
	expires_at TEXT NOT NULL,
	superseded_at TEXT,
	revoked INTEGER NOT NULL DEFAULT 0,
	note TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_identity_invitations_user_id ON identity_invitations(user_id);
`,
		},
	}
}
