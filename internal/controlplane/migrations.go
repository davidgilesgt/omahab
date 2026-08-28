package controlplane

import (
	"github.com/omahab/omahab/internal/apps"
	"github.com/omahab/omahab/internal/backups"
	"github.com/omahab/omahab/internal/emailing"
	"github.com/omahab/omahab/internal/events"
	"github.com/omahab/omahab/internal/health"
	"github.com/omahab/omahab/internal/hermes"
	"github.com/omahab/omahab/internal/identity"
	"github.com/omahab/omahab/internal/installer"
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
	// exposure (static copy of store.go migrations - Service.Migrations requires instance)
	out = append(out, exposureMigrations()...)
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
	// installer
	out = append(out, installer.Migrations()...)
	// controlplane glue (users, release tokens)
	out = append(out, glueMigrations()...)
	return out
}

func exposureMigrations() []store.Migration {
	return []store.Migration{
		{
			Name: "0001_exposure_services_and_acks",
			SQL: `
CREATE TABLE IF NOT EXISTS exposure_services (
    id            TEXT PRIMARY KEY,
    hostname      TEXT NOT NULL UNIQUE,
    home_anchor   TEXT NOT NULL,
    upstream      TEXT NOT NULL,
    tunnel_origin TEXT NOT NULL,
    exposure      TEXT NOT NULL,
    app_auth      TEXT NOT NULL,
    revision      INTEGER NOT NULL,
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS exposure_public_acks (
    service_id TEXT PRIMARY KEY,
    ack_id     TEXT NOT NULL,
    revision   INTEGER NOT NULL,
    created_at TEXT NOT NULL
);
`,
		},
		{
			Name: "0002_exposure_plans",
			SQL: `
CREATE TABLE IF NOT EXISTS exposure_plans (
    id               TEXT PRIMARY KEY,
    service_id       TEXT NOT NULL,
    service_revision INTEGER NOT NULL,
    kind             TEXT NOT NULL,
    hostname         TEXT NOT NULL,
    from_exposure    TEXT NOT NULL,
    to_exposure      TEXT NOT NULL,
    steps            TEXT NOT NULL,
    warnings         TEXT NOT NULL,
    requires_ack     INTEGER NOT NULL,
    status           TEXT NOT NULL,
    results          TEXT NOT NULL,
    last_error       TEXT NOT NULL,
    created_at       TEXT NOT NULL,
    applied_at       TEXT
);
CREATE INDEX IF NOT EXISTS exposure_plans_by_service ON exposure_plans (service_id, created_at);
`,
		},
		{
			Name: "0003_exposure_observations",
			SQL: `
CREATE TABLE IF NOT EXISTS exposure_observations (
    service_id     TEXT PRIMARY KEY,
    observed_at    TEXT NOT NULL,
    vanity_dns     TEXT NOT NULL,
    anchor_dns     TEXT NOT NULL,
    tunnel_ingress TEXT NOT NULL,
    access_app     TEXT NOT NULL,
    edge_route     TEXT NOT NULL,
    reconciled     INTEGER NOT NULL,
    drift          TEXT NOT NULL,
    last_error     TEXT NOT NULL,
    plan_id        TEXT NOT NULL
);
`,
		},
	}
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
