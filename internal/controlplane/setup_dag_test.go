package controlplane

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/omahab/omahab/internal/apps"
	"github.com/omahab/omahab/internal/domain"
)

// failingRunner fails Deploy/Start for the given bundle IDs; everything else
// succeeds with healthy checks.
type failingRunner struct {
	health  domain.Health
	failIDs map[string]bool
	deploys []string
}

func (r *failingRunner) Deploy(_ context.Context, app domain.Application, _ apps.DeploySpec) error {
	r.deploys = append(r.deploys, app.BundleID)
	if r.failIDs[app.BundleID] {
		return fmt.Errorf("deploy %s failed", app.BundleID)
	}
	return nil
}
func (r *failingRunner) Start(_ context.Context, app domain.Application, _ apps.DeploySpec) error {
	if r.failIDs[app.BundleID] {
		return fmt.Errorf("start %s failed", app.BundleID)
	}
	return nil
}
func (r *failingRunner) Stop(context.Context, domain.Application, apps.DeploySpec) error { return nil }
func (r *failingRunner) Remove(context.Context, domain.Application, apps.DeploySpec) error {
	return nil
}
func (r *failingRunner) Check(_ context.Context, _ domain.Application, _ apps.DeploySpec) (domain.Health, error) {
	if r.health == "" {
		return domain.HealthHealthy, nil
	}
	return r.health, nil
}

// newDAGRunBackend wires a backend whose catalog exercises per-bundle DAG
// isolation: caddy; pocket-id (dep caddy); failme (dep caddy); fine (dep
// caddy); needs-failme (dep failme).
func newDAGRunBackend(t *testing.T, runner apps.Runner) *Backend {
	t.Helper()
	b, _ := newSetupBackend(t, nil)

	b.cfg.StateDir = t.TempDir()
	b.cfg.DataDir = t.TempDir()
	b.dockerNetwork = func(context.Context) error { return nil }
	b.httpsWait = time.Millisecond
	b.httpsInterval = time.Millisecond
	digest := "sha256:" + strings.Repeat("a", 64)
	cat, err := apps.NewCatalog(
		testSetupBundle("caddy", digest, domain.ExposurePublic, "", nil),
		testSetupBundle("pocket-id", digest, domain.ExposurePrivate, "id", []string{"caddy"}),
		testSetupBundle("failme", digest, domain.ExposurePrivate, "fail", []string{"caddy"}),
		testSetupBundle("fine", digest, domain.ExposurePrivate, "fine", []string{"caddy"}),
		testSetupBundle("needs-failme", digest, domain.ExposurePrivate, "need", []string{"failme"}),
	)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := apps.NewService(b.db, apps.Options{Catalog: cat, Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	b.apps = svc
	return b
}

// TestSetupDAGFailingAppDoesNotBlockUnrelated proves the per-bundle split: a
// failing app node fails, its dependents block, and unrelated apps still
// install.
func TestSetupDAGFailingAppDoesNotBlockUnrelated(t *testing.T) {
	ctx := context.Background()
	runner := &failingRunner{health: domain.HealthHealthy, failIDs: map[string]bool{"failme": true}}
	b := newDAGRunBackend(t, runner)
	if _, err := b.secrets.Put(ctx, "platform-app", "cloudflare_dns", "dns-token"); err != nil {
		t.Fatal(err)
	}
	if err := b.RunSetupReconciler(ctx); err != nil {
		t.Fatalf("RunSetupReconciler: %v", err)
	}
	states := b.setupNodeStates(ctx)
	if states["preconditions"] != setupNodeSucceeded {
		t.Fatalf("preconditions = %q, want succeeded (states: %v)", states["preconditions"], states)
	}
	if states["core_apps:failme"] != setupNodeFailed {
		t.Fatalf("failme state = %q, want failed (states: %v)", states["core_apps:failme"], states)
	}
	if states["core_apps:fine"] != setupNodeSucceeded {
		t.Fatalf("fine state = %q, want succeeded (unrelated app must not be blocked; states: %v)", states["core_apps:fine"], states)
	}
	if states["core_apps:needs-failme"] != setupNodeBlocked {
		t.Fatalf("needs-failme state = %q, want blocked (states: %v)", states["core_apps:needs-failme"], states)
	}
	var blockedBy []string
	for _, n := range b.setupNodesList(ctx) {
		if n.ID == "core_apps:needs-failme" {
			blockedBy = n.BlockedBy
		}
	}
	if len(blockedBy) == 0 || !strings.Contains(strings.Join(blockedBy, ","), "core_apps:failme") {
		t.Fatalf("needs-failme blocked_by = %v, want it to contain core_apps:failme", blockedBy)
	}
}

// TestSetupDAGWaitingPropagatesBlocked proves waiting (operator action
// needed) is a node outcome: preconditions waits and every downstream node
// is blocked with blocked_by = [preconditions], not a whole-run abort.
func TestSetupDAGWaitingPropagatesBlocked(t *testing.T) {
	ctx := context.Background()
	runner := &failingRunner{health: domain.HealthHealthy}
	b := newDAGRunBackend(t, runner)
	inst, err := b.store.Instance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Sentinel domain: the instance row requires a domain, so "not yet
	// enrolled" is represented by the placeholder value.
	inst.Domain = "example.com"
	if _, err := b.store.SaveInstance(ctx, inst); err != nil {
		t.Fatal(err)
	}
	if err := b.RunSetupReconciler(ctx); err != nil {
		t.Fatalf("RunSetupReconciler: %v", err)
	}
	states := b.setupNodeStates(ctx)
	if states["preconditions"] != setupNodeWaiting {
		t.Fatalf("preconditions = %q, want waiting (states: %v)", states["preconditions"], states)
	}
	for _, id := range []string{"tunnel", "secrets", "storage", "core_apps", "core_apps:caddy", "oidc", "exposure"} {
		if states[id] != setupNodeBlocked {
			t.Fatalf("node %s = %q, want blocked (states: %v)", id, states[id], states)
		}
	}
	for _, n := range b.setupNodesList(ctx) {
		if n.ID == "tunnel" {
			if len(n.BlockedBy) == 0 || !strings.Contains(strings.Join(n.BlockedBy, ","), "preconditions") {
				t.Fatalf("tunnel blocked_by = %v, want it to contain preconditions", n.BlockedBy)
			}
		}
	}
}
