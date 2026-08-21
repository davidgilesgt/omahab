package health

import "github.com/omahab/omahab/internal/store"

// Migrations returns the ordered SQLite migrations owned by the health
// controller. They store last emitted health snapshots to suppress storms
// and allow delta detection. Durable health history is SQLite-backed as per
// DESIGN §4.
func Migrations() []store.Migration {
	return []store.Migration{
		{
			Name: "health_001_snapshots",
			SQL: `
CREATE TABLE IF NOT EXISTS health_snapshots (
    id TEXT PRIMARY KEY,
    component TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('healthy','degraded','unhealthy','unknown')),
    message TEXT NOT NULL,
    detail TEXT NOT NULL DEFAULT '',
    checked_at TEXT NOT NULL
) STRICT;
CREATE INDEX IF NOT EXISTS idx_health_snapshots_component ON health_snapshots(component);
CREATE INDEX IF NOT EXISTS idx_health_snapshots_checked_at ON health_snapshots(checked_at);
`,
		},
		{
			Name: "health_002_emitted",
			SQL: `
CREATE TABLE IF NOT EXISTS health_emitted (
    component TEXT PRIMARY KEY,
    status TEXT NOT NULL,
    emitted_at TEXT NOT NULL
) STRICT;
`,
		},
	}
}
