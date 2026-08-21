package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/omahab/omahab/internal/domain"
)

// Migrations returns the core migrations owned by the store package: the
// singleton instance identity row (DESIGN.md §4.1, "installation and
// instance identity"). Controllers' own migrations stay in their packages and
// are passed to Migrate alongside or after these.
func Migrations() []Migration {
	return []Migration{
		{
			Name: "core-001-instance",
			SQL: `CREATE TABLE IF NOT EXISTS instance (
	singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
	id TEXT NOT NULL UNIQUE,
	domain TEXT NOT NULL,
	tailnet TEXT NOT NULL DEFAULT '',
	tailscale_ip TEXT NOT NULL DEFAULT '',
	assistant_name TEXT NOT NULL,
	assistant_slug TEXT NOT NULL,
	created_at TEXT NOT NULL
)`,
		},
	}
}

// SaveInstance creates or updates the single instance identity row and
// returns the instance as persisted. Blank ID and CreatedAt fields are
// filled in; the created timestamp of an existing row is preserved. Saving
// with an ID different from the existing one fails with ErrConflict: the
// instance identity is stable and must not be silently replaced.
func (s *Store) SaveInstance(ctx context.Context, in domain.Instance) (domain.Instance, error) {
	var zero domain.Instance
	if s == nil || s.db == nil {
		return zero, Validation("store is not opened")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return zero, fmt.Errorf("store: save instance: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var existingID, existingCreated string
	err = tx.QueryRowContext(ctx,
		`SELECT id, created_at FROM instance WHERE singleton = 1`,
	).Scan(&existingID, &existingCreated)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if in.ID == "" {
			in.ID = domain.ID(NewID())
		}
		if in.CreatedAt.IsZero() {
			in.CreatedAt = time.Now().UTC()
		}
		if err := in.Validate(); err != nil {
			return zero, Validationf("%s", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO instance
				(singleton, id, domain, tailnet, tailscale_ip, assistant_name, assistant_slug, created_at)
			VALUES (1, ?, ?, ?, ?, ?, ?, ?)`,
			string(in.ID), in.Domain, in.Tailnet, in.TailscaleIP,
			in.AssistantName, in.AssistantSlug, FormatTime(in.CreatedAt),
		); err != nil {
			return zero, fmt.Errorf("store: save instance: %w", Translate(err))
		}
	case err != nil:
		return zero, fmt.Errorf("store: save instance: %w", err)
	default:
		if in.ID == "" {
			in.ID = domain.ID(existingID)
		}
		if in.ID != domain.ID(existingID) {
			return zero, Conflictf("instance identity %q does not match existing %q", in.ID, existingID)
		}
		createdAt, err := ParseTime(existingCreated)
		if err != nil {
			return zero, fmt.Errorf("store: parse instance created_at: %w", err)
		}
		in.CreatedAt = createdAt
		if err := in.Validate(); err != nil {
			return zero, Validationf("%s", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE instance
			SET domain = ?, tailnet = ?, tailscale_ip = ?, assistant_name = ?, assistant_slug = ?
			WHERE singleton = 1`,
			in.Domain, in.Tailnet, in.TailscaleIP, in.AssistantName, in.AssistantSlug,
		); err != nil {
			return zero, fmt.Errorf("store: save instance: %w", Translate(err))
		}
	}

	if err := tx.Commit(); err != nil {
		return zero, fmt.Errorf("store: save instance: %w", err)
	}
	return in, nil
}

// Instance returns the instance identity. Before the first SaveInstance it
// fails with ErrNotFound.
func (s *Store) Instance(ctx context.Context) (domain.Instance, error) {
	var zero domain.Instance
	if s == nil || s.db == nil {
		return zero, Validation("store is not opened")
	}

	var id, dmn, tailnet, tailscaleIP, assistantName, assistantSlug, createdAt string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, domain, tailnet, tailscale_ip, assistant_name, assistant_slug, created_at
		FROM instance WHERE singleton = 1`,
	).Scan(&id, &dmn, &tailnet, &tailscaleIP, &assistantName, &assistantSlug, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return zero, NotFound("instance is not initialized")
	}
	if err != nil {
		return zero, fmt.Errorf("store: load instance: %w", err)
	}
	created, err := ParseTime(createdAt)
	if err != nil {
		return zero, fmt.Errorf("store: parse instance created_at: %w", err)
	}
	return domain.Instance{
		ID:            domain.ID(id),
		Domain:        dmn,
		Tailnet:       tailnet,
		TailscaleIP:   tailscaleIP,
		AssistantName: assistantName,
		AssistantSlug: assistantSlug,
		CreatedAt:     created,
	}, nil
}
