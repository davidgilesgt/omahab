package controlplane

import (
	"github.com/omahab/omahab/internal/apps"
	"github.com/omahab/omahab/internal/backups"
	"github.com/omahab/omahab/internal/emailing"
	"github.com/omahab/omahab/internal/companion"
	"github.com/omahab/omahab/internal/events"
	"github.com/omahab/omahab/internal/exposure"
	"github.com/omahab/omahab/internal/health"
	"github.com/omahab/omahab/internal/hermes"
	"github.com/omahab/omahab/internal/identity"
	"github.com/omahab/omahab/internal/integrations"
	"github.com/omahab/omahab/internal/knowledge"
	"github.com/omahab/omahab/internal/projects"
	"github.com/omahab/omahab/internal/providers"
	"github.com/omahab/omahab/internal/scm"
	"github.com/omahab/omahab/internal/secrets"
	"github.com/omahab/omahab/internal/store"
	"github.com/omahab/omahab/internal/syncer"
	"github.com/omahab/omahab/internal/workspaces"
)

// AllMigrations returns deterministic ordered migrations for the control-plane.
// Order is fixed and sorted by source to ensure idempotent startup.
func AllMigrations() []store.Migration {
	var out []store.Migration
	// core store first
	out = append(out, store.Migrations()...)
	// apps
	out = append(out, apps.Migrations()...)
	// projects
	out = append(out, projects.Migrations()...)
	// exposure
	out = append(out, exposure.Migrations()...)
	// secrets
	out = append(out, secrets.Migrations()...)
	// identity
	out = append(out, identity.Migrations()...)
	// backups
	out = append(out, backups.Migrations()...)
	// events
	out = append(out, events.Migrations()...)
	// health
	out = append(out, health.Migrations()...)
	// syncer
	out = append(out, syncer.Migrations()...)
	// workspaces
	out = append(out, workspaces.Migrations()...)
	// integrations
	out = append(out, integrations.Migrations()...)
	// providers
	out = append(out, providers.Migrations()...)
	// scm
	out = append(out, scm.Migrations()...)
	// hermes
	out = append(out, hermes.Migrations()...)
	// knowledge
	out = append(out, knowledge.Migrations()...)
	// emailing
	out = append(out, emailing.Migrations()...)
	// companion (devices + enrollments + env meta) — migration IDs keep environments-001 strings
	out = append(out, companion.Migrations()...)
	// controlplane glue (users, release tokens)
	out = append(out, glueMigrations()...)
	return out
}

func glueMigrations() []store.Migration {
	return []store.Migration{
		{
			Name: "controlplane-001-users",
			SQL: `
CREATE TABLE IF NOT EXISTS controlplane_users (
    id TEXT PRIMARY KEY,
    email TEXT NOT NULL COLLATE NOCASE,
    name TEXT NOT NULL,
    groups_json TEXT NOT NULL DEFAULT '[]',
    disabled INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE(email)
) STRICT;
CREATE INDEX IF NOT EXISTS idx_controlplane_users_email ON controlplane_users(email);
`,
		},
		{
			Name: "controlplane-002-release_tokens",
			SQL: `
CREATE TABLE IF NOT EXISTS project_release_tokens (
    project_id TEXT PRIMARY KEY,
    token_hash TEXT NOT NULL,
    token_prefix TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;
`,
		},
		{
			Name: "controlplane-003-email_listing_cache",
			SQL: `
-- email listing cache is covered by emailing migrations; this placeholder ensures deterministic naming for future extensions
CREATE TABLE IF NOT EXISTS controlplane_meta (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
) STRICT;
`,
		},
		{
			Name: "controlplane-004-pocket-user-id",
			SQL: `
ALTER TABLE controlplane_users ADD COLUMN pocket_user_id TEXT NOT NULL DEFAULT '';
`,
		},
	}
}
