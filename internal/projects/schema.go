package projects

import "github.com/omahab/omahab/internal/store"

// Migrations returns the schema owned by the projects controller. The UI and
// control API check this slice when assembling the global migration set.
func Migrations() []store.Migration {
	return []store.Migration{{
		Name: "0001_projects_releases",
		SQL: `
CREATE TABLE projects (
    id                    TEXT PRIMARY KEY,
    slug                  TEXT NOT NULL UNIQUE,
    name                  TEXT NOT NULL,
    repository_url        TEXT NOT NULL,
    image_base            TEXT NOT NULL,
    bot_profile_id        TEXT NOT NULL DEFAULT '',
    exposure              TEXT NOT NULL DEFAULT 'private',
    hostname              TEXT NOT NULL DEFAULT '',
    contract_port         INTEGER NOT NULL,
    contract_health_path  TEXT NOT NULL,
    contract_storage_path TEXT NOT NULL,
    deploying             INTEGER NOT NULL DEFAULT 0 CHECK (deploying IN (0, 1)),
    deploy_started_ns     INTEGER NOT NULL DEFAULT 0,
    created_at            TEXT NOT NULL,
    updated_at            TEXT NOT NULL
);

CREATE TABLE releases (
    id         TEXT PRIMARY KEY,
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    commit_sha TEXT NOT NULL,
    digest     TEXT NOT NULL,
    status     TEXT NOT NULL CHECK (status IN ('deploying', 'succeeded', 'failed')),
    active     INTEGER NOT NULL DEFAULT 0 CHECK (active IN (0, 1)),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE (project_id, digest),
    CHECK (length(digest) = 71 AND substr(digest, 1, 7) = 'sha256:'),
    CHECK (length(commit_sha) IN (40, 64))
);

CREATE UNIQUE INDEX releases_one_active ON releases(project_id) WHERE active = 1;
CREATE INDEX releases_project_recent ON releases(project_id, created_at DESC);

CREATE TRIGGER releases_identity_immutable
BEFORE UPDATE ON releases
FOR EACH ROW
WHEN OLD.project_id <> NEW.project_id
  OR OLD.commit_sha <> NEW.commit_sha
  OR OLD.digest <> NEW.digest
BEGIN
    SELECT RAISE(ABORT, 'release identity is immutable');
END;
`,
	}}
}
