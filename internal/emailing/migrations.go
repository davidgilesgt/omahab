package emailing

import "github.com/omahab/omahab/internal/store"

// Migrations returns the ordered SQLite migrations owned by the emailing
// controller. They cover sender enrollment, replay nonces, messages, and
// quarantine, as required by acceptance.
func Migrations() []store.Migration {
	return []store.Migration{
		{
			Name: "emailing_001_senders",
			SQL: `
CREATE TABLE IF NOT EXISTS email_senders (
    id TEXT PRIMARY KEY,
    email TEXT NOT NULL COLLATE NOCASE,
    status TEXT NOT NULL CHECK (status IN ('pending','verified')),
    created_at TEXT NOT NULL,
    verified_at TEXT,
    UNIQUE(email)
) STRICT;
CREATE INDEX IF NOT EXISTS idx_email_senders_email ON email_senders(email);
`,
		},
		{
			Name: "emailing_002_nonces",
			SQL: `
CREATE TABLE IF NOT EXISTS email_nonces (
    nonce TEXT PRIMARY KEY,
    expires_at TEXT NOT NULL,
    created_at TEXT NOT NULL
) STRICT;
CREATE INDEX IF NOT EXISTS idx_email_nonces_expires ON email_nonces(expires_at);
`,
		},
		{
			Name: "emailing_003_messages",
			SQL: `
CREATE TABLE IF NOT EXISTS email_messages (
    id TEXT PRIMARY KEY,
    envelope_from TEXT NOT NULL,
    header_from TEXT NOT NULL,
    recipient TEXT NOT NULL,
    subject TEXT NOT NULL,
    authentication TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('received','quarantined','pending')),
    raw_size INTEGER NOT NULL,
    decoded_size INTEGER NOT NULL,
    received_at TEXT NOT NULL,
    created_at TEXT NOT NULL
) STRICT;
CREATE INDEX IF NOT EXISTS idx_email_messages_status ON email_messages(status);
CREATE INDEX IF NOT EXISTS idx_email_messages_received ON email_messages(received_at);
`,
		},
		{
			Name: "emailing_004_quarantine",
			SQL: `
CREATE TABLE IF NOT EXISTS email_quarantine (
    id TEXT PRIMARY KEY,
    message_id TEXT NOT NULL REFERENCES email_messages(id) ON DELETE CASCADE,
    reason TEXT NOT NULL,
    details TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
) STRICT;
CREATE INDEX IF NOT EXISTS idx_email_quarantine_message ON email_quarantine(message_id);
CREATE INDEX IF NOT EXISTS idx_email_quarantine_reason ON email_quarantine(reason);
`,
		},
	}
}
