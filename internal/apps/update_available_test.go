package apps

import (
	"context"
	"strings"
	"testing"

	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/store"
)

type recordingSink struct {
	events []domain.Event
}

func (r *recordingSink) Emit(_ context.Context, ev domain.Event) error {
	r.events = append(r.events, ev)
	return nil
}
func (r *recordingSink) Count() int { return len(r.events) }
func (r *recordingSink) Last() domain.Event {
	if len(r.events) == 0 {
		return domain.Event{}
	}
	return r.events[len(r.events)-1]
}
func (r *recordingSink) Reset() { r.events = nil }

type fakeRunner struct{}

func (f *fakeRunner) Deploy(_ context.Context, _ domain.Application, _ DeploySpec) error { return nil }
func (f *fakeRunner) Start(_ context.Context, _ domain.Application, _ DeploySpec) error  { return nil }
func (f *fakeRunner) Stop(_ context.Context, _ domain.Application, _ DeploySpec) error   { return nil }
func (f *fakeRunner) Remove(_ context.Context, _ domain.Application, _ DeploySpec) error { return nil }
func (f *fakeRunner) Check(_ context.Context, _ domain.Application, _ DeploySpec) (domain.Health, error) {
	return domain.HealthHealthy, nil
}

func testBundle(id, digest string) Bundle {
	return Bundle{
		ID:            id,
		Name:          id,
		Image:         "example.com/" + id,
		Digest:        digest,
		Architectures: []string{"amd64", "arm64"},
		Compose:       "services:\n  app:\n    image: {{.Image}}@{{.Digest}}\n",
		HealthCheck:   HealthCheck{Kind: CheckNone},
	}
}

func newTestService(t *testing.T, catalog *Catalog) (*Service, *recordingSink, *store.Store) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(context.Background(), Migrations()...); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	sink := &recordingSink{}
	svc, err := NewService(st.DB(), Options{Catalog: catalog, Runner: &fakeRunner{}, Events: sink})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return svc, sink, st
}

func TestServiceUpdateAvailableEmitsOnTransition(t *testing.T) {
	t.Parallel()
	digestA := "sha256:" + strings.Repeat("a", 64)
	digestB := "sha256:" + strings.Repeat("b", 64)
	digestC := "sha256:" + strings.Repeat("c", 64)

	catA, err := NewCatalog(testBundle("myapp", digestA))
	if err != nil {
		t.Fatalf("catA: %v", err)
	}
	svc, sink, _ := newTestService(t, catA)
	ctx := context.Background()

	// Install myapp with digestA
	_, err = svc.Install(ctx, InstallRequest{BundleID: "myapp"})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	sink.Reset()

	// No update yet: catalog digest equals installed
	if _, err := svc.CheckForUpdates(ctx); err != nil {
		t.Fatalf("CheckForUpdates: %v", err)
	}
	if sink.Count() != 0 {
		t.Fatalf("expected 0 events when digest matches, got %d", sink.Count())
	}

	// Update catalog to digestB
	catB, err := NewCatalog(testBundle("myapp", digestB))
	if err != nil {
		t.Fatalf("catB: %v", err)
	}
	if err := svc.SetCatalog(catB); err != nil {
		t.Fatalf("set catalog: %v", err)
	}

	// First check should emit
	list, err := svc.CheckForUpdates(ctx)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 app with update, got %d", len(list))
	}
	if sink.Count() != 1 {
		t.Fatalf("expected 1 event, got %d", sink.Count())
	}
	ev := sink.Last()
	if ev.Type != EventUpdateAvailable {
		t.Fatalf("type = %q, want %q", ev.Type, EventUpdateAvailable)
	}
	if ev.Data["old_digest"] != digestA || ev.Data["new_digest"] != digestB {
		t.Fatalf("payload old/new mismatch: %+v", ev.Data)
	}
	if ev.Data["bundle_id"] != "myapp" {
		t.Fatalf("bundle_id mismatch: %+v", ev.Data)
	}

	// Re-observation with same digest must not duplicate
	sink.Reset()
	if _, err := svc.CheckForUpdates(ctx); err != nil {
		t.Fatalf("second check: %v", err)
	}
	// list still 1 (update still available) but no new emit
	if sink.Count() != 0 {
		t.Fatalf("expected 0 duplicate events on re-observation, got %d", sink.Count())
	}

	// Update app to digestB, then no update available
	if _, err := svc.Update(ctx, list[0].ID, digestB); err != nil {
		t.Fatalf("update to B: %v", err)
	}
	sink.Reset()
	if _, err := svc.CheckForUpdates(ctx); err != nil {
		t.Fatalf("after update check: %v", err)
	}
	if sink.Count() != 0 {
		t.Fatalf("expected 0 after app updated to catalog digest, got %d", sink.Count())
	}

	// Catalog moves to C -> should emit again
	catC, _ := NewCatalog(testBundle("myapp", digestC))
	_ = svc.SetCatalog(catC)
	sink.Reset()
	if _, err := svc.CheckForUpdates(ctx); err != nil {
		t.Fatalf("check C: %v", err)
	}
	if sink.Count() != 1 {
		t.Fatalf("expected 1 event for C, got %d", sink.Count())
	}
	ev = sink.Last()
	if ev.Data["old_digest"] != digestB || ev.Data["new_digest"] != digestC {
		t.Fatalf("payload C mismatch: %+v", ev.Data)
	}

	// Duplicate C again -> no emit
	sink.Reset()
	if _, err := svc.CheckForUpdates(ctx); err != nil {
		t.Fatalf("dup C: %v", err)
	}
	if sink.Count() != 0 {
		t.Fatalf("expected 0 duplicate for C, got %d", sink.Count())
	}
}

func TestCheckForUpdatesNoCatalogMatch(t *testing.T) {
	t.Parallel()
	digestA := "sha256:" + strings.Repeat("a", 64)
	cat, _ := NewCatalog(testBundle("myapp", digestA))
	svc, sink, _ := newTestService(t, cat)
	ctx := context.Background()

	if _, err := svc.Install(ctx, InstallRequest{BundleID: "myapp"}); err != nil {
		t.Fatalf("install: %v", err)
	}
	sink.Reset()

	// Catalog with different bundle ID -> no update
	otherCat, _ := NewCatalog(testBundle("other", digestA))
	_ = svc.SetCatalog(otherCat)
	if _, err := svc.CheckForUpdates(ctx); err != nil {
		t.Fatalf("check: %v", err)
	}
	if sink.Count() != 0 {
		t.Fatalf("expected 0 when bundle missing from catalog, got %d", sink.Count())
	}
}

type countingRunner struct {
	fakeRunner
	deploys int
}

func (c *countingRunner) Deploy(ctx context.Context, app domain.Application, spec DeploySpec) error {
	c.deploys++
	return c.fakeRunner.Deploy(ctx, app, spec)
}

func TestServiceUpdateSameDigestComposeChange(t *testing.T) {
	t.Parallel()
	digest := "sha256:" + strings.Repeat("a", 64)
	original := testBundle("caddy", digest)
	cat, err := NewCatalog(original)
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	runner := &countingRunner{}
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(context.Background(), Migrations()...); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	sink := &recordingSink{}
	svc, err := NewService(st.DB(), Options{Catalog: cat, Runner: runner, Events: sink})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	ctx := context.Background()
	installed, err := svc.Install(ctx, InstallRequest{BundleID: "caddy"})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if runner.deploys != 1 {
		t.Fatalf("install deploys = %d want 1", runner.deploys)
	}

	changed := original
	changed.Compose = "services:\n  app:\n    image: {{.Image}}@{{.Digest}}\n    ports:\n      - \"127.0.0.1:80:80\"\n"
	cat2, err := NewCatalog(changed)
	if err != nil {
		t.Fatalf("changed catalog: %v", err)
	}
	if err := svc.SetCatalog(cat2); err != nil {
		t.Fatalf("set catalog: %v", err)
	}
	updated, err := svc.Update(ctx, installed.ID, digest)
	if err != nil {
		t.Fatalf("update compose: %v", err)
	}
	if runner.deploys != 2 {
		t.Fatalf("compose change deploys = %d want 2", runner.deploys)
	}
	var prev string
	if err := st.DB().QueryRow(`SELECT previous_release_id FROM apps WHERE id = ?`, string(updated.ID)).Scan(&prev); err != nil {
		t.Fatalf("previous release: %v", err)
	}
	if prev == "" {
		t.Fatal("expected rollback history after compose change")
	}
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM app_releases WHERE app_id = ?`, string(updated.ID)).Scan(&n); err != nil {
		t.Fatalf("count releases: %v", err)
	}
	if n != 2 {
		t.Fatalf("releases = %d want 2", n)
	}

	again, err := svc.Update(ctx, installed.ID, digest)
	if err != nil {
		t.Fatalf("second update: %v", err)
	}
	if runner.deploys != 2 {
		t.Fatalf("idempotent update deploys = %d want 2", runner.deploys)
	}
	if again.ID != updated.ID {
		t.Fatalf("status id changed on no-op")
	}
}
