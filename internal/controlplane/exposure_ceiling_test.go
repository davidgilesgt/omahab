package controlplane

import (
	"context"
	"errors"
	"testing"

	"github.com/omahab/omahab/internal/apitypes"
	"github.com/omahab/omahab/internal/apps"
	"github.com/omahab/omahab/internal/config"
	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/exposure"
	"github.com/omahab/omahab/internal/store"
)

// ceilingTestBackend wires a catalog pocket-id bundle capped at shared
// exposure with an exposure service holding its id.omahab.com record, so
// service/application/project update paths can be checked against the same
// install-time ceiling.
func ceilingTestBackend(t *testing.T) *Backend {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx, store.Migrations()...); err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx, apps.Migrations()...); err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx, exposure.Migrations()...); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SaveInstance(ctx, domain.Instance{Domain: "omahab.com", TailscaleIP: "100.75.94.122", AssistantName: "AI", AssistantSlug: "ai"}); err != nil {
		t.Fatal(err)
	}
	cat, err := apps.NewCatalog(apps.Bundle{
		ID:              "pocket-id",
		Name:            "Pocket ID",
		Port:            1411,
		DefaultExposure: domain.ExposurePrivate,
		MaxExposure:     domain.ExposureShared,
		HealthCheck:     apps.HealthCheck{Kind: apps.CheckNone},
		Default:         true,
		Route:           "id",
		Units:           []string{"pocket-id.service"},
	}, apps.Bundle{
		ID:              "twin-svc",
		Name:            "Twin",
		Port:            1411,
		DefaultExposure: domain.ExposurePrivate,
		MaxExposure:     domain.ExposureShared,
		HealthCheck:     apps.HealthCheck{Kind: apps.CheckNone},
		Default:         true,
		Route:           "twin",
		Units:           []string{"twin-svc.service"},
	})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := apps.NewService(st.DB(), apps.Options{Catalog: cat, Runner: storageTestRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	b := &Backend{cfg: config.Config{StateDir: t.TempDir()}, store: st, db: st.DB(), apps: svc}
	expSvc, err := exposure.New(st, exposure.Config{
		Domain:      "omahab.com",
		TailscaleIP: "100.75.94.122",
		TunnelDNS:   "tunnel.example.com",
	}, exposure.Clients{DNS: &memDNS{}, Edge: &memEdge{routes: map[string]exposure.Route{}}})
	if err != nil {
		t.Fatal(err)
	}
	b.exposure = expSvc
	return b
}

func seedPocketIDService(t *testing.T, b *Backend) exposure.ServiceRecord {
	t.Helper()
	rec, err := b.exposure.UpsertService(context.Background(), exposure.UpsertInput{
		Hostname: "id.omahab.com",
		Upstream: "http://127.0.0.1:1411",
		Exposure: domain.ExposurePrivate,
	})
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func exposureOf(t *testing.T, b *Backend, kind string, id domain.ID) domain.Exposure {
	t.Helper()
	st, err := b.GetExposure(context.Background(), kind, id)
	if err != nil {
		t.Fatal(err)
	}
	return st.Exposure
}

// Service exposure updates must reject exposure above the bundle catalog
// max_exposure, mirroring install limits; at-or-below-max still applies.
func TestUpdateExposureRejectsAboveCatalogMax(t *testing.T) {
	b := ceilingTestBackend(t)
	ctx := context.Background()
	rec := seedPocketIDService(t, b)

	if _, err := b.UpdateExposure(ctx, "service", domain.ID(rec.ID), domain.ExposurePublic); err == nil || !errors.Is(err, apitypes.ErrValidation) {
		t.Fatalf("public over shared max err = %v, want validation error", err)
	}
	if got := exposureOf(t, b, "service", domain.ID(rec.ID)); got != domain.ExposurePrivate {
		t.Fatalf("exposure = %q after rejected update, want private (no change)", got)
	}

	if _, err := b.UpdateExposure(ctx, "service", domain.ID(rec.ID), domain.ExposureShared); err != nil {
		t.Fatalf("shared at max: %v", err)
	}
	if got := exposureOf(t, b, "service", domain.ID(rec.ID)); got != domain.ExposureShared {
		t.Fatalf("exposure = %q, want shared", got)
	}
}

// The finding's route addresses the service by bundle id; the ceiling must
// hold there too, before any change is attempted.
func TestUpdateExposureRejectsAboveMaxByBundleID(t *testing.T) {
	b := ceilingTestBackend(t)
	ctx := context.Background()
	seedPocketIDService(t, b)

	if _, err := b.UpdateExposure(ctx, "service", "pocket-id", domain.ExposurePublic); err == nil || !errors.Is(err, apitypes.ErrValidation) {
		t.Fatalf("bundle-id public err = %v, want validation error", err)
	}
}

// Application exposure updates (both the exposure resource and the PATCH
// application path) share the same catalog ceiling as install.
func TestApplicationExposureUpdatesRespectCatalogMax(t *testing.T) {
	b := ceilingTestBackend(t)
	ctx := context.Background()
	seedPocketIDService(t, b)
	installed, err := b.apps.Install(ctx, apps.InstallRequest{BundleID: "pocket-id"})
	if err != nil {
		t.Fatalf("install pocket-id: %v", err)
	}
	appID := installed.Application.ID

	if _, err := b.UpdateExposure(ctx, "application", appID, domain.ExposurePublic); err == nil || !errors.Is(err, apitypes.ErrValidation) {
		t.Fatalf("application resource public err = %v, want validation error", err)
	}
	public := domain.ExposurePublic
	if _, err := b.UpdateApplication(ctx, appID, apitypes.UpdateApplicationRequest{Exposure: &public}); err == nil || !errors.Is(err, apitypes.ErrValidation) {
		t.Fatalf("application PATCH public err = %v, want validation error", err)
	}
	app, err := b.GetApplication(ctx, appID)
	if err != nil {
		t.Fatalf("get app: %v", err)
	}
	if app.Exposure == domain.ExposurePublic {
		t.Fatal("app exposure is public after rejected updates, want no change")
	}

	shared := domain.ExposureShared
	if _, err := b.UpdateApplication(ctx, appID, apitypes.UpdateApplicationRequest{Exposure: &shared}); err != nil {
		t.Fatalf("application PATCH shared at max: %v", err)
	}
}

// Project hostnames owned by no catalog bundle have no catalog ceiling, but
// a project update aimed at a bundle-owned hostname is still capped.
func TestProjectExposureCeilingOnlyForBundleOwnedHostnames(t *testing.T) {
	b := ceilingTestBackend(t)
	ctx := context.Background()
	pocket := seedPocketIDService(t, b)
	proj, err := b.exposure.UpsertService(ctx, exposure.UpsertInput{
		Hostname: "myproj.omahab.com",
		Upstream: "http://127.0.0.1:8080",
		Exposure: domain.ExposurePrivate,
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := b.UpdateExposure(ctx, "project", domain.ID(proj.ID), domain.ExposurePublic); err != nil {
		t.Fatalf("unowned project hostname public: %v", err)
	}
	if got := exposureOf(t, b, "project", domain.ID(proj.ID)); got != domain.ExposurePublic {
		t.Fatalf("project exposure = %q, want public", got)
	}
	if _, err := b.UpdateExposure(ctx, "project", domain.ID(pocket.ID), domain.ExposurePublic); err == nil || !errors.Is(err, apitypes.ErrValidation) {
		t.Fatalf("project update on bundle hostname public err = %v, want validation error", err)
	}
}

// Unresolvable targets fail closed: without a proven catalog owner, any
// escalation above private is rejected before any change.
func TestUpdateExposureUnresolvableTargetsFailClosed(t *testing.T) {
	b := ceilingTestBackend(t)
	ctx := context.Background()

	for _, kind := range []string{"service", "application", "project"} {
		if _, err := b.UpdateExposure(ctx, kind, "no-such-target", domain.ExposurePublic); err == nil || !errors.Is(err, apitypes.ErrValidation) {
			t.Fatalf("kind %s unknown public err = %v, want validation error", kind, err)
		}
	}
	// A missing application fails closed rather than surfacing 404 on escalation.
	if _, err := b.UpdateExposure(ctx, "application", "app_000000000000000000000000", domain.ExposureShared); err == nil || !errors.Is(err, apitypes.ErrValidation) {
		t.Fatalf("missing app shared err = %v, want validation error", err)
	}
	// Lowering to private stays open even when ownership is unprovable; a
	// missing row still reports not-found from the update itself.
	if _, err := b.UpdateExposure(ctx, "service", "no-such-target", domain.ExposurePrivate); err == nil {
		t.Fatal("private on missing row succeeded, want not-found")
	} else if !errors.Is(err, apitypes.ErrNotFound) {
		t.Fatalf("private on missing row err = %v, want not-found", err)
	}
}

// An upstream claimed by several bundles proves no single owner: escalation
// is rejected for every kind, including project rows over the same record.
func TestUpdateExposureAmbiguousUpstreamFailClosed(t *testing.T) {
	b := ceilingTestBackend(t)
	ctx := context.Background()
	rec, err := b.exposure.UpsertService(ctx, exposure.UpsertInput{
		Hostname: "other.omahab.com",
		Upstream: "http://127.0.0.1:1411",
		Exposure: domain.ExposurePrivate,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"service", "application", "project"} {
		if _, err := b.UpdateExposure(ctx, kind, domain.ID(rec.ID), domain.ExposurePublic); err == nil || !errors.Is(err, apitypes.ErrValidation) {
			t.Fatalf("kind %s ambiguous-upstream public err = %v, want validation error", kind, err)
		}
	}
	if got := exposureOf(t, b, "service", domain.ID(rec.ID)); got != domain.ExposurePrivate {
		t.Fatalf("exposure = %q after rejected updates, want private (no change)", got)
	}
}

// Without a catalog, ownership cannot be proven: only lowering to private
// still applies.
func TestUpdateExposureWithoutCatalogFailClosed(t *testing.T) {
	b := ceilingTestBackend(t)
	ctx := context.Background()
	rec := seedPocketIDService(t, b)
	b.apps = nil
	if _, err := b.UpdateExposure(ctx, "service", domain.ID(rec.ID), domain.ExposureShared); err == nil || !errors.Is(err, apitypes.ErrValidation) {
		t.Fatalf("catalogless shared err = %v, want validation error", err)
	}
	if _, err := b.UpdateExposure(ctx, "service", domain.ID(rec.ID), domain.ExposurePrivate); err != nil {
		t.Fatalf("catalogless private: %v", err)
	}
}
