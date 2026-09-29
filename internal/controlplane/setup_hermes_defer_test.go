package controlplane

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omahab/omahab/internal/apps"
	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/events"
)

// Operator-prerequisite gaps (memory model not configured) must defer
// Hermes memory — publish the operator-owned pending step — while Hermes
// itself still installs and its route is exposed, so ai.<domain> exists and
// the full setup reports success instead of failing every run.
func TestHermesDefersMemoryButInstallsWithoutModel(t *testing.T) {
	ctx := context.Background()
	digest := "sha256:" + strings.Repeat("a", 64)
	runner := &scriptedRunner{health: domain.HealthHealthy}
	b, ev := newSetupBackend(t, runner)
	stateRoot := t.TempDir()
	b.cfg.StateDir = filepath.Join(stateRoot, "state")
	b.cfg.DataDir = filepath.Join(stateRoot, "data")
	hermes := testSetupBundle("hermes", digest, domain.ExposurePrivate, "ai", []string{"litellm", "pocket-id", "hindsight"})
	hermes.Units = []string{"docker-hermes.service"}
	hb := testSetupBundle("hindsight", digest, domain.ExposurePrivate, "", []string{"litellm", "embedding-worker"})
	hb.Units = []string{"docker-hindsight.service"}
	cat, err := apps.NewCatalog(
		testSetupBundle("caddy", digest, domain.ExposurePublic, "", nil),
		testSetupBundle("litellm", digest, domain.ExposurePrivate, "models", nil),
		testSetupBundle("embedding-worker", digest, domain.ExposurePrivate, "", nil),
		testSetupBundle("pocket-id", digest, domain.ExposurePrivate, "id", []string{"caddy"}),
		hb,
		hermes,
	)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := apps.NewService(b.db, apps.Options{Catalog: cat, Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	b.apps = svc
	if err := b.ensureHindsightSecrets(ctx); err != nil {
		t.Fatal(err)
	}
	// No gateway configured: the operator has not set up any AI model.

	if err := b.ensureHermesDependentApp(ctx); err != nil {
		t.Fatalf("hermes dependent app must not hard-fail on missing memory model: %v", err)
	}
	// The operator step is visible as a pending event.
	list, _, lerr := ev.List(ctx, events.ListOptions{Limit: 10, Filter: events.ListFilter{Type: "setup.step_failed"}})
	if lerr != nil {
		t.Fatal(lerr)
	}
	found := false
	for _, e := range list {
		if string(e.ResourceID) == "setup:hermes_memory" {
			found = true
			if e.Data["owner"] != "operator" {
				t.Fatalf("owner = %v, want operator", e.Data["owner"])
			}
		}
	}
	if !found {
		t.Fatalf("no setup:hermes_memory pending event; got %d events", len(list))
	}
	// Hermes still installed: the exposure record for ai.<domain> exists.
	st, err := b.apps.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	installed := false
	for _, s := range st {
		if s.BundleID == "hermes" {
			installed = true
		}
	}
	if !installed {
		t.Fatal("hermes not installed after deferral")
	}
	_ = os.RemoveAll(stateRoot)
}
