package events

import "github.com/omahab/omahab/internal/store"

// Migrations returns the ordered SQLite migrations owned by the events
// inbox. They provide a normalized durable event table with idempotency,
// read state, and cursor ordering.
func Migrations() []store.Migration {
	return []store.Migration{
		{
			Name: "events_001_events",
			SQL: `
CREATE TABLE IF NOT EXISTS events (
    id TEXT PRIMARY KEY,
    type TEXT NOT NULL,
    severity TEXT NOT NULL,
    resource_id TEXT NOT NULL DEFAULT '',
    message TEXT NOT NULL,
    data TEXT NOT NULL DEFAULT '{}',
    idempotency_key TEXT,
    read_at TEXT,
    created_at TEXT NOT NULL
) STRICT;
CREATE INDEX IF NOT EXISTS idx_events_type ON events(type);
CREATE INDEX IF NOT EXISTS idx_events_severity ON events(severity);
CREATE INDEX IF NOT EXISTS idx_events_created_at ON events(created_at);
CREATE INDEX IF NOT EXISTS idx_events_read_at ON events(read_at);
CREATE UNIQUE INDEX IF NOT EXISTS idx_events_idempotency_key ON events(idempotency_key) WHERE idempotency_key IS NOT NULL AND idempotency_key != '';
`,
		},
	}
}
