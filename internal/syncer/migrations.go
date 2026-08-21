package syncer

import "github.com/omahab/omahab/internal/store"

// Migrations returns the SQLite migrations owned by the syncer controller.
func Migrations() []store.Migration {
	return []store.Migration{
		{
			Name: "syncer-001-sync_folders",
			SQL: `
CREATE TABLE IF NOT EXISTS sync_folders (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL UNIQUE,
	server_path TEXT NOT NULL UNIQUE,
	share_with_ai INTEGER NOT NULL DEFAULT 0,
	health TEXT NOT NULL DEFAULT 'unknown',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
`,
		},
		{
			Name: "syncer-002-sync_devices",
			SQL: `
CREATE TABLE IF NOT EXISTS sync_devices (
	id TEXT PRIMARY KEY,
	folder_id TEXT NOT NULL REFERENCES sync_folders(id) ON DELETE CASCADE,
	device_id TEXT NOT NULL,
	device_name TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	UNIQUE(folder_id, device_id)
);
CREATE INDEX IF NOT EXISTS idx_sync_devices_folder_id ON sync_devices(folder_id);
`,
		},
	}
}
