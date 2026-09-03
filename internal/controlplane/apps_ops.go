package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/omahab/omahab/internal/apitypes"
	"github.com/omahab/omahab/internal/apps"
	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/events"
	"github.com/omahab/omahab/internal/health"
	"github.com/omahab/omahab/internal/store"
)

func (b *Backend) GetStatus(ctx context.Context) (domain.Status, error) {
	inst, err := b.store.Instance(ctx)
	if err != nil {
		return domain.Status{}, translateError(err)
	}
	healthVal := domain.HealthHealthy
	if b.health != nil {
		if rep, err := b.health.Check(ctx); err == nil && rep != nil {
			// derive health: if any check unhealthy -> unhealthy, degraded -> degraded
			for _, c := range rep.Checks {
				switch c.Status {
				case "unhealthy":
					healthVal = domain.HealthUnhealthy
				case "degraded":
					if healthVal != domain.HealthUnhealthy {
						healthVal = domain.HealthDegraded
					}
				}
			}
		}
	}
	return domain.Status{
		InstanceID: inst.ID,
		Version:    b.version,
		Health:     healthVal,
		StartedAt:  b.startedAt,
		Now:        time.Now().UTC(),
	}, nil
}

func (b *Backend) GetInstance(ctx context.Context) (domain.Instance, error) {
	inst, err := b.store.Instance(ctx)
	if err != nil {
		return domain.Instance{}, translateError(err)
	}
	return inst, nil
}

func (b *Backend) UpdateInstance(ctx context.Context, domainName string, assistantName string) (domain.Instance, error) {
	domainName = strings.TrimSpace(strings.ToLower(domainName))
	assistantName = strings.TrimSpace(assistantName)
	if domainName == "" {
		return domain.Instance{}, translateError(store.Validation("domain is required"))
	}
	inst, err := b.store.Instance(ctx)
	if err != nil {
		return domain.Instance{}, translateError(err)
	}
	inst.Domain = domainName
	if assistantName != "" {
		inst.AssistantName = assistantName
		// slug derived from name (lowercase, hyphenated)
		slug := strings.ToLower(strings.ReplaceAll(assistantName, " ", "-"))
		if slug != "" {
			inst.AssistantSlug = slug
		}
	}
	saved, err := b.store.SaveInstance(ctx, inst)
	if err != nil {
		return domain.Instance{}, translateError(err)
	}
	// Refresh exposure with new domain (best-effort, log on failure).
	if err := b.refreshExposure(ctx); err != nil {
		_, _ = b.events.Publish(ctx, events.PublishInput{
			Type:     "exposure.refresh_failed",
			Severity: "warning",
			Message:  "exposure refresh after UpdateInstance failed: " + err.Error(),
		})
	}
	return saved, nil
}

func (b *Backend) GetDoctor(ctx context.Context) (*health.Report, error) {
	if b.health == nil {
		return nil, translateError(fmt.Errorf("%w: health not configured", ErrNotConfigured))
	}
	rep, err := b.health.Check(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	return rep, nil
}

// Applications

func (b *Backend) ListApplications(ctx context.Context, p apitypes.Pagination) ([]domain.Application, error) {
	if b.apps == nil {
		return nil, translateError(fmt.Errorf("%w: apps not configured", ErrNotConfigured))
	}
	list, err := b.apps.List(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	apps := make([]domain.Application, 0, len(list))
	for _, s := range list {
		apps = append(apps, s.Application)
	}
	// pagination
	return paginate(apps, p), nil
}

func (b *Backend) InstallApplication(ctx context.Context, req apitypes.InstallApplicationRequest) (domain.Application, error) {
	if b.apps == nil {
		return domain.Application{}, translateError(fmt.Errorf("%w: apps not configured", ErrNotConfigured))
	}
	st, err := b.apps.Install(ctx, apps.InstallRequest{
		BundleID: strings.TrimSpace(req.BundleID),
		Name:     strings.TrimSpace(req.Name),
		Hostname: strings.TrimSpace(req.Hostname),
		Exposure: req.Exposure,
	})
	if err != nil {
		return domain.Application{}, translateError(err)
	}
	return st.Application, nil
}

func (b *Backend) ListCatalog(ctx context.Context) ([]apitypes.CatalogBundle, error) {
	if b.apps == nil {
		return nil, translateError(fmt.Errorf("%w: apps not configured", ErrNotConfigured))
	}
	installed := map[string]bool{}
	if list, err := b.apps.List(ctx); err == nil {
		for _, s := range list {
			installed[s.BundleID] = true
		}
	}
	bundles := b.apps.CatalogBundles()
	out := make([]apitypes.CatalogBundle, 0, len(bundles))
	for _, bundle := range bundles {
		exposure := bundle.DefaultExposure
		if exposure == "" {
			exposure = domain.ExposurePrivate
		}
		maxExposure := bundle.MaxExposure
		if maxExposure == "" {
			maxExposure = domain.ExposurePrivate
		}
		out = append(out, apitypes.CatalogBundle{
			ID:              bundle.ID,
			Name:            bundle.Name,
			DefaultExposure: exposure,
			MaxExposure:     maxExposure,
			MemoryMB:        bundle.Resources.MemoryMB,
			Installed:       installed[bundle.ID],
		})
	}
	return out, nil
}

func (b *Backend) GetApplication(ctx context.Context, id domain.ID) (domain.Application, error) {
	if b.apps == nil {
		return domain.Application{}, translateError(fmt.Errorf("%w: apps not configured", ErrNotConfigured))
	}
	st, err := b.apps.Status(ctx, id)
	if err != nil {
		return domain.Application{}, translateError(err)
	}
	return st.Application, nil
}

func (b *Backend) UpdateApplication(ctx context.Context, id domain.ID, req apitypes.UpdateApplicationRequest) (domain.Application, error) {
	if b.apps == nil {
		return domain.Application{}, translateError(fmt.Errorf("%w: apps not configured", ErrNotConfigured))
	}
	// For exposure update, we directly update SQLite apps table's exposure if provided
	if req.Exposure != nil {
		if !req.Exposure.Valid() {
			return domain.Application{}, translateError(fmt.Errorf("%w: invalid exposure", store.ErrValidation))
		}
		_, err := b.db.ExecContext(ctx, `UPDATE apps SET exposure = ?, updated_at = ? WHERE id = ?`, string(*req.Exposure), store.FormatTime(time.Now().UTC()), string(id))
		if err != nil {
			return domain.Application{}, translateError(err)
		}
	}
	// DesiredState handling: map to Start/Stop
	if req.DesiredState != nil {
		switch strings.ToLower(strings.TrimSpace(*req.DesiredState)) {
		case "running":
			st, err := b.apps.Start(ctx, id)
			if err != nil {
				return domain.Application{}, translateError(err)
			}
			return st.Application, nil
		case "stopped":
			st, err := b.apps.Stop(ctx, id)
			if err != nil {
				return domain.Application{}, translateError(err)
			}
			return st.Application, nil
		default:
			return domain.Application{}, translateError(fmt.Errorf("%w: invalid desired_state %q", store.ErrValidation, *req.DesiredState))
		}
	}
	return b.GetApplication(ctx, id)
}

func (b *Backend) DoApplicationAction(ctx context.Context, id domain.ID, action string) (domain.Application, error) {
	if b.apps == nil {
		return domain.Application{}, translateError(fmt.Errorf("%w: apps not configured", ErrNotConfigured))
	}
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "start":
		st, err := b.apps.Start(ctx, id)
		if err != nil {
			return domain.Application{}, translateError(err)
		}
		return st.Application, nil
	case "stop":
		st, err := b.apps.Stop(ctx, id)
		if err != nil {
			return domain.Application{}, translateError(err)
		}
		return st.Application, nil
	case "restart":
		// stop then start
		if _, err := b.apps.Stop(ctx, id); err != nil {
			return domain.Application{}, translateError(err)
		}
		st, err := b.apps.Start(ctx, id)
		if err != nil {
			return domain.Application{}, translateError(err)
		}
		return st.Application, nil
	case "update":
		// requires digest param in action? For generic action we need digest; fail with validation
		return domain.Application{}, translateError(fmt.Errorf("%w: update requires digest; use PATCH", store.ErrValidation))
	case "rollback":
		st, err := b.apps.Rollback(ctx, id)
		if err != nil {
			return domain.Application{}, translateError(err)
		}
		return st.Application, nil
	case "uninstall":
		if err := b.apps.Uninstall(ctx, id); err != nil {
			return domain.Application{}, translateError(err)
		}
		return domain.Application{ID: id}, nil
	case "check_health", "health":
		st, err := b.apps.CheckHealth(ctx, id)
		if err != nil {
			return domain.Application{}, translateError(err)
		}
		return st.Application, nil
	default:
		return domain.Application{}, translateError(fmt.Errorf("%w: unknown action %q", store.ErrValidation, action))
	}
}

// Exposure

func (b *Backend) GetExposure(ctx context.Context, resourceType string, id domain.ID) (apitypes.ExposureState, error) {
	if b.getExposure() == nil {
		return apitypes.ExposureState{}, translateError(fmt.Errorf("%w: exposure not configured (Cloudflare credentials missing)", ErrNotConfigured))
	}
	// Try to map resourceType to exposure service; we treat id as service hostname or id
	// For simplicity, attempt to find by id as hostname
	// Query exposure_services table directly for metadata
	var hostname, expStr, updated string
	err := b.db.QueryRowContext(ctx, `SELECT hostname, exposure, updated_at FROM exposure_services WHERE id = ? OR hostname = ?`, string(id), string(id)).Scan(&hostname, &expStr, &updated)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return apitypes.ExposureState{}, translateError(fmt.Errorf("%w: exposure %q not found", store.ErrNotFound, id))
		}
		return apitypes.ExposureState{}, translateError(err)
	}
	t, _ := store.ParseTime(updated)
	return apitypes.ExposureState{
		ResourceType: resourceType,
		ResourceID:   id,
		Hostname:     hostname,
		Exposure:     domain.Exposure(expStr),
		UpdatedAt:    t,
	}, nil
}

func (b *Backend) ListExposure(ctx context.Context) ([]apitypes.ExposureState, error) {
	if b.getExposure() == nil {
		// Without Cloudflare, still return empty list (metadata only)
		return []apitypes.ExposureState{}, nil
	}
	rows, err := b.db.QueryContext(ctx, `SELECT id, hostname, exposure, updated_at FROM exposure_services ORDER BY hostname`)
	if err != nil {
		return nil, translateError(err)
	}
	defer rows.Close()
	var out []apitypes.ExposureState
	for rows.Next() {
		var id, hostname, expStr, updated string
		if err := rows.Scan(&id, &hostname, &expStr, &updated); err != nil {
			return nil, translateError(err)
		}
		t, _ := store.ParseTime(updated)
		out = append(out, apitypes.ExposureState{
			ResourceType: "service",
			ResourceID:   domain.ID(id),
			Hostname:     hostname,
			Exposure:     domain.Exposure(expStr),
			UpdatedAt:    t,
		})
	}
	return out, nil
}

func (b *Backend) UpdateExposure(ctx context.Context, resourceType string, id domain.ID, exposure domain.Exposure) (apitypes.ExposureState, error) {
	if b.getExposure() == nil {
		return apitypes.ExposureState{}, translateError(fmt.Errorf("%w: exposure not configured (Cloudflare credentials missing)", ErrNotConfigured))
	}
	if !exposure.Valid() {
		return apitypes.ExposureState{}, translateError(fmt.Errorf("%w: invalid exposure", store.ErrValidation))
	}
	// Update exposure_services directly; if not exists, create via exposure service? For now update row
	res, err := b.db.ExecContext(ctx, `UPDATE exposure_services SET exposure = ?, updated_at = ?, revision = revision + 1 WHERE id = ?`, string(exposure), store.FormatTime(time.Now().UTC()), string(id))
	if err != nil {
		return apitypes.ExposureState{}, translateError(err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return apitypes.ExposureState{}, translateError(fmt.Errorf("%w: exposure %q not found", store.ErrNotFound, id))
	}
	return b.GetExposure(ctx, resourceType, id)
}

// Projects
