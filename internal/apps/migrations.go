package apps

import "github.com/omahab/omahab/internal/store"

// Migrations returns the schema owned by the apps controller. The apps table
// holds desired and observed state plus the current digest; app_releases
// retains the current and previous rendered releases for rollback.
func Migrations() []store.Migration {
	return []store.Migration{{
		Name: "0001_apps_and_app_releases",
		SQL:  appsSchema,
	}}
}

const appsSchema = `
CREATE TABLE IF NOT EXISTS apps (
    id                  TEXT PRIMARY KEY,
    name                TEXT NOT NULL UNIQUE,
    bundle_id           TEXT NOT NULL,
    image               TEXT NOT NULL,
    digest              TEXT NOT NULL,
    hostname            TEXT NOT NULL DEFAULT '',
    exposure            TEXT NOT NULL DEFAULT 'private',
    health              TEXT NOT NULL DEFAULT 'unknown',
    desired_state       TEXT NOT NULL DEFAULT 'running',
    observed_state      TEXT NOT NULL DEFAULT 'absent',
    current_release_id  TEXT NOT NULL,
    previous_release_id TEXT NOT NULL DEFAULT '',
    installed_at        TEXT,
    updated_at          TEXT NOT NULL,
    last_error          TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_apps_bundle_id ON apps (bundle_id);

CREATE TABLE IF NOT EXISTS app_releases (
    id         TEXT PRIMARY KEY,
    app_id     TEXT NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
    digest     TEXT NOT NULL,
    compose    TEXT NOT NULL,
    created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_app_releases_app ON app_releases (app_id, created_at);
`
