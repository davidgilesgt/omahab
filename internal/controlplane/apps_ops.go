package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/omahab/omahab/internal/apitypes"
	"github.com/omahab/omahab/internal/apps"
	"github.com/omahab/omahab/internal/domain"
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
	if err := validateInstanceDomain(domainName); err != nil {
		return domain.Instance{}, translateError(err)
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
	// Refresh exposure with new domain (best-effort; not-ready-yet defers
	// quietly instead of warning).
	if err := b.refreshExposure(ctx); err != nil {
		b.publishExposureRefreshIssue(ctx, "after UpdateInstance", err)
	}
	return saved, nil
}

// validateInstanceDomain mirrors internal/exposure validateHostname: lowercase
// DNS hostname with at least two labels. Single-label and malformed names
// fail with store.ErrValidation. (exposure.go is read-only here, so the
// rules are mirrored rather than imported.)
func validateInstanceDomain(name string) error {
	if len(name) > 253 {
		return store.Validationf("domain %q is longer than 253 characters", name)
	}
	if strings.ToLower(name) != name {
		return store.Validationf("domain %q must be lowercase", name)
	}
	labels := strings.Split(name, ".")
	if len(labels) < 2 {
		return store.Validationf("domain %q needs at least two labels", name)
	}
	for _, label := range labels {
		if label == "" {
			return store.Validationf("domain %q has an empty label", name)
		}
		if len(label) > 63 {
			return store.Validationf("domain label %q is longer than 63 characters", label)
		}
		for i := range len(label) {
			c := label[i]
			switch {
			case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			case c == '-' && i > 0 && i < len(label)-1:
			default:
				return store.Validationf("domain label %q contains invalid character %q", label, string(rune(c)))
			}
		}
	}
	return nil
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

// applicationLaunchURL computes the browser destination for one application copy.
// It is pure: no I/O, stored hostname/exposure untouched. Empty means no
// browser interface (no catalog app path), an unknown bundle, an invalid
// hostname, or a missing/sentinel domain without an explicit usable hostname.
// An explicit valid stored hostname wins; otherwise the bundle route resolves
// against the instance domain. Assembly is always https + hostname + app path;
// service loopback ports and example.com fallbacks are never used.
func applicationLaunchURL(app domain.Application, bundle apps.Bundle, domainName string) string {
	if bundle.AppPath == "" {
		return ""
	}
	hostname := ""
	if h := strings.ToLower(strings.TrimSpace(app.Hostname)); h != "" {
		if validateInstanceDomain(h) != nil {
			return ""
		}
		hostname = h
	} else {
		h, ok := resolveBundleHostname(bundle.Route, domainName)
		if !ok {
			return ""
		}
		h = strings.ToLower(strings.TrimSpace(h))
		if validateInstanceDomain(h) != nil {
			return ""
		}
		hostname = h
	}
	return (&url.URL{Scheme: "https", Host: hostname, Path: bundle.AppPath}).String()
}

// launchContext reads the instance domain and indexes the catalog once per
// call. Instance-read errors propagate through translateError; callers keep
// GET handlers read-only (response copies only, no stored-state writes).
func (b *Backend) launchContext(ctx context.Context) (map[string]apps.Bundle, string, error) {
	inst, err := b.store.Instance(ctx)
	if err != nil {
		return nil, "", translateError(err)
	}
	bundles := map[string]apps.Bundle{}
	if b.apps != nil {
		for _, bd := range b.apps.CatalogBundles() {
			bundles[bd.ID] = bd
		}
	}
	return bundles, strings.TrimSpace(inst.Domain), nil
}

func (b *Backend) decorateApplication(app domain.Application, bundles map[string]apps.Bundle, domainName string) domain.Application {
	bundle, ok := bundles[app.BundleID]
	if !ok {
		app.LaunchURL = ""
		return app
	}
	app.LaunchURL = applicationLaunchURL(app, bundle, domainName)
	return app
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
	bundles, domainName, err := b.launchContext(ctx)
	if err != nil {
		return nil, err
	}
	apps := make([]domain.Application, 0, len(list))
	for _, s := range list {
		apps = append(apps, b.decorateApplication(s.Application, bundles, domainName))
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
	bundles, domainName, err := b.launchContext(ctx)
	if err != nil {
		return domain.Application{}, err
	}
	return b.decorateApplication(st.Application, bundles, domainName), nil
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
	bundles, domainName, err := b.launchContext(ctx)
	if err != nil {
		return domain.Application{}, err
	}
	return b.decorateApplication(st.Application, bundles, domainName), nil
}

// RefreshApplicationHealth re-probes running unpaused applications through
// the apps service observer. It never starts, resumes, or clears maintenance.
func (b *Backend) RefreshApplicationHealth(ctx context.Context) error {
	if b.apps == nil {
		return nil
	}
	return b.apps.RefreshHealth(ctx)
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
		if err := b.checkExposureCeiling(ctx, "application", id, *req.Exposure); err != nil {
			return domain.Application{}, err
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
			bundles, domainName, lerr := b.launchContext(ctx)
			if lerr != nil {
				return domain.Application{}, lerr
			}
			return b.decorateApplication(st.Application, bundles, domainName), nil
		case "stopped":
			st, err := b.apps.Stop(ctx, id)
			if err != nil {
				return domain.Application{}, translateError(err)
			}
			bundles, domainName, lerr := b.launchContext(ctx)
			if lerr != nil {
				return domain.Application{}, lerr
			}
			return b.decorateApplication(st.Application, bundles, domainName), nil
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
	launch := func(app domain.Application) (domain.Application, error) {
		bundles, domainName, err := b.launchContext(ctx)
		if err != nil {
			return domain.Application{}, err
		}
		return b.decorateApplication(app, bundles, domainName), nil
	}
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "start":
		st, err := b.apps.Start(ctx, id)
		if err != nil {
			return domain.Application{}, translateError(err)
		}
		return launch(st.Application)
	case "stop":
		st, err := b.apps.Stop(ctx, id)
		if err != nil {
			return domain.Application{}, translateError(err)
		}
		return launch(st.Application)
	case "restart":
		// stop then start
		if _, err := b.apps.Stop(ctx, id); err != nil {
			return domain.Application{}, translateError(err)
		}
		st, err := b.apps.Start(ctx, id)
		if err != nil {
			return domain.Application{}, translateError(err)
		}
		return launch(st.Application)
	case "update":
		// requires digest param in action? For generic action we need digest; fail with validation
		return domain.Application{}, translateError(fmt.Errorf("%w: update requires digest; use PATCH", store.ErrValidation))
	case "rollback":
		st, err := b.apps.Rollback(ctx, id)
		if err != nil {
			return domain.Application{}, translateError(err)
		}
		return launch(st.Application)
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
		return launch(st.Application)
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
	if err := b.checkExposureCeiling(ctx, resourceType, id, exposure); err != nil {
		return apitypes.ExposureState{}, err
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

// exposureRank orders exposure for ceiling checks, mirroring install-time
// validation in apps/service.go. Empty ranks as private.
func exposureRank(e domain.Exposure) int {
	switch e {
	case "", domain.ExposurePrivate:
		return 0
	case domain.ExposureShared:
		return 1
	case domain.ExposurePublic:
		return 2
	default:
		return -1
	}
}

// exposureCeiling resolves the catalog bundle governing an exposure update
// target. It returns false when ownership cannot be proven: public exposure
// is a security boundary, so unresolvable service/application targets fail
// closed in checkExposureCeiling.
func (b *Backend) exposureCeiling(ctx context.Context, kind string, id domain.ID) (apps.Bundle, bool) {
	if b.apps == nil {
		return apps.Bundle{}, false
	}
	bundles := b.apps.CatalogBundles()
	if len(bundles) == 0 {
		return apps.Bundle{}, false
	}
	byID := func(id string) (apps.Bundle, bool) {
		for _, bd := range bundles {
			if bd.ID == id {
				return bd, true
			}
		}
		return apps.Bundle{}, false
	}
	// Application targets resolve through the installed app record only:
	// a missing app (or one from outside the catalog) proves nothing.
	if kind == "application" {
		st, err := b.apps.Status(ctx, id)
		if err != nil {
			return apps.Bundle{}, false
		}
		return byID(st.BundleID)
	}
	// Service and project rows resolve through the exposure record.
	return b.exposureRowBundle(ctx, bundles, byID, string(id))
}

// exposureRow reads one exposure_services row by service id or hostname.
func (b *Backend) exposureRow(ctx context.Context, key string) (hostname, upstream string, found bool) {
	if err := b.db.QueryRowContext(ctx,
		`SELECT hostname, upstream FROM exposure_services WHERE id = ? OR hostname = ?`,
		key, key).Scan(&hostname, &upstream); err != nil {
		return "", "", false
	}
	return hostname, upstream, true
}

// exposureRowBundle maps an exposure row to its owning catalog bundle. Only
// unambiguous signals count: bundle id, full hostname, a route label
// claimed by exactly one bundle, or an upstream claimed by exactly one
// bundle. Shared upstreams or routes stay unresolved rather than guessing
// an owner.
func (b *Backend) exposureRowBundle(ctx context.Context, bundles []apps.Bundle, byID func(string) (apps.Bundle, bool), key string) (apps.Bundle, bool) {
	if bd, ok := byID(key); ok {
		return bd, true
	}
	hostname, upstream, found := b.exposureRow(ctx, key)
	if !found {
		return apps.Bundle{}, false
	}
	if bd, ok := byID(hostname); ok {
		return bd, true
	}
	label := hostname
	if i := strings.Index(label, "."); i >= 0 {
		label = label[:i]
	}
	var owner apps.Bundle
	claims := 0
	for _, bd := range bundles {
		if label != "" && (label == bd.Route || label == bd.ID) {
			owner, claims = bd, claims+1
		}
	}
	if claims == 1 {
		return owner, true
	}
	if claims > 1 {
		return apps.Bundle{}, false
	}
	if upstream != "" {
		owner, claims = apps.Bundle{}, 0
		for _, bd := range bundles {
			up, uerr := bundleUpstream(bd)
			if uerr != nil || up == "" || up != upstream {
				continue
			}
			owner, claims = bd, claims+1
		}
		if claims == 1 {
			return owner, true
		}
	}
	return apps.Bundle{}, false
}

// unownedExposureRow reports whether key names a persisted exposure row that
// no catalog bundle claims by any signal: a proven user project row with no
// catalog ceiling. Any claim, even an ambiguous one, disqualifies it.
func (b *Backend) unownedExposureRow(ctx context.Context, key string) bool {
	hostname, upstream, found := b.exposureRow(ctx, key)
	if !found || b.apps == nil {
		return false
	}
	bundles := b.apps.CatalogBundles()
	byID := func(id string) (apps.Bundle, bool) {
		for _, bd := range bundles {
			if bd.ID == id {
				return bd, true
			}
		}
		return apps.Bundle{}, false
	}
	if _, ok := byID(key); ok {
		return false
	}
	if _, ok := byID(hostname); ok {
		return false
	}
	label := hostname
	if i := strings.Index(label, "."); i >= 0 {
		label = label[:i]
	}
	for _, bd := range bundles {
		if label != "" && (label == bd.Route || label == bd.ID) {
			return false
		}
		if upstream != "" {
			if up, uerr := bundleUpstream(bd); uerr == nil && up != "" && up == upstream {
				return false
			}
		}
	}
	return true
}

// checkExposureCeiling rejects exposure above the target's catalog
// max_exposure, matching install-time limits. Lowering to private is always
// safe and needs no ceiling; any other service/application escalation
// without a proven catalog owner fails closed. Project rows pass only with
// a proven owner (enforced) or as proven ownerless rows (no ceiling to
// enforce). It runs before any change.
func (b *Backend) checkExposureCeiling(ctx context.Context, kind string, id domain.ID, exposure domain.Exposure) error {
	if exposure == domain.ExposurePrivate {
		return nil
	}
	bd, ok := b.exposureCeiling(ctx, kind, id)
	if !ok {
		if kind == "project" && b.unownedExposureRow(ctx, string(id)) {
			return nil
		}
		return translateError(fmt.Errorf("%w: cannot verify catalog max_exposure for %s %q", store.ErrValidation, kind, string(id)))
	}
	if exposureRank(exposure) > exposureRank(bd.MaxExposure) {
		return translateError(fmt.Errorf("%w: bundle %q supports at most %q exposure", store.ErrValidation, bd.ID, bd.MaxExposure))
	}
	return nil
}

// Projects
