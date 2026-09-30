package syncer

import "github.com/omahab/omahab/internal/store"

// Migrations returns the schema owned by the syncer controller.
// syncthing_id is the folder ID on the wire shared with companions; it equals
// the server record ID unless explicitly set otherwise. companion_id links
// the enrolled Syncthing device ID back to the companion device that owns it,
// so revocation removes only that device's managed sharing.
func Migrations() []store.Migration {
	return []store.Migration{
		{
			Name: "syncer-001-baseline",
			SQL: `
CREATE TABLE IF NOT EXISTS sync_folders (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL UNIQUE,
	server_path TEXT NOT NULL UNIQUE,
	share_with_ai INTEGER NOT NULL DEFAULT 0,
	health TEXT NOT NULL DEFAULT 'unknown',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	syncthing_id TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS sync_devices (
	id TEXT PRIMARY KEY,
	folder_id TEXT NOT NULL REFERENCES sync_folders(id) ON DELETE CASCADE,
	device_id TEXT NOT NULL,
	device_name TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	companion_id TEXT NOT NULL DEFAULT '',
	UNIQUE(folder_id, device_id)
);
CREATE INDEX IF NOT EXISTS idx_sync_devices_folder_id ON sync_devices(folder_id);
`,
		},
	}
}
