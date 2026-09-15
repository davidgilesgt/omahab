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
		{
			// Wire ID for Syncthing configuration: server record IDs stay
			// stable (foreign keys unchanged); syncthing_id is the folder
			// ID on the wire shared with companions. Backfilled to the
			// server record ID for existing folders.
			Name: "syncer-003-folders-syncthing-id",
			SQL:  `ALTER TABLE sync_folders ADD COLUMN syncthing_id TEXT NOT NULL DEFAULT ''`,
		},
		{
			Name: "syncer-004-folders-syncthing-id-backfill",
			SQL:  `UPDATE sync_folders SET syncthing_id = id WHERE syncthing_id = ''`,
		},
		{
			// Companion linkage for revocation: the enrolled Syncthing
			// device ID maps back to the companion device that owns it, so
			// revocation removes only that device's managed sharing.
			Name: "syncer-005-devices-companion-id",
			SQL:  `ALTER TABLE sync_devices ADD COLUMN companion_id TEXT NOT NULL DEFAULT ''`,
		},
	}
}
