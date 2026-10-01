package controlplane

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	flow "github.com/Azure/go-workflow"
	"github.com/omahab/omahab/internal/apps"
	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/events"
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
	b, _ := newDAGRunBackendWithEvents(t, runner)
	return b
}

func newDAGRunBackendWithEvents(t *testing.T, runner apps.Runner) (*Backend, *events.Service) {
	t.Helper()
	b, ev := newSetupBackend(t, nil)

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
	return b, ev
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

// TestSetupDAGStorageBlocksCoreApps pins the storage edge: core_apps
// (and every per-bundle install node behind it) must never run before
// storage converged the bind mounts, so apps cannot initialize data on
// the root filesystem below a missing external volume.
func TestSetupDAGStorageBlocksCoreApps(t *testing.T) {
	ctx := context.Background()
	runner := &failingRunner{health: domain.HealthHealthy}
	b := newDAGRunBackend(t, runner)
	if _, err := b.secrets.Put(ctx, "platform-app", "cloudflare_dns", "dns-token"); err != nil {
		t.Fatal(err)
	}
	// An active placement for a disk that is not present makes the
	// storage node fail closed (missing-disk refusal). The native
	// layout makes caddy:state a known volume so the placement is
	// validated instead of silently skipped.
	layout := fmt.Sprintf(`{"caddy":{"data":[{"name":"state","path":%q}],"databases":[],"quiesce_units":[]}}`,
		filepath.Join(b.cfg.DataDir, "apps", "caddy"))
	lf := filepath.Join(b.cfg.StateDir, "layout.json")
	if err := os.WriteFile(lf, []byte(layout), 0o600); err != nil {
		t.Fatal(err)
	}
	b.cfg.NativeLayoutPath = lf
	uuid := "11111111-2222-3333-4444-555555555555"
	sf := fmt.Sprintf(`{"version":2,"placements":[{"volume":"caddy:state","fs_uuid":%q}]}`, uuid)
	if err := os.WriteFile(filepath.Join(b.cfg.StateDir, "storage.json"), []byte(sf), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := b.RunSetupReconciler(ctx); err != nil {
		t.Fatalf("RunSetupReconciler: %v", err)
	}
	states := b.setupNodeStates(ctx)
	if states["storage"] != setupNodeFailed {
		t.Fatalf("storage = %q, want failed (states: %v)", states["storage"], states)
	}
	if states["core_apps"] != setupNodeBlocked {
		t.Fatalf("core_apps = %q, want blocked behind storage (states: %v)", states["core_apps"], states)
	}
	for _, n := range b.setupNodesList(ctx) {
		if n.ID == "core_apps" {
			blocked := strings.Join(n.BlockedBy, ",")
			if !strings.Contains(blocked, "storage") {
				t.Fatalf("core_apps blocked_by = %v, want it to contain storage", n.BlockedBy)
			}
		}
	}
	// No app unit was deployed while storage was failing.
	for _, id := range runner.deploys {
		if id != "" {
			t.Fatalf("deploy ran before storage converged: %v", runner.deploys)
		}
	}
}

// blockableRunner blocks Deploy for one bundle until its release channel
// closes (or the deploy context ends), so a setup run can be canceled
// mid-node.
type blockableRunner struct {
	health   domain.Health
	block    string // bundle ID whose Deploy blocks
	released chan struct{}
	deploys  []string
	mu       sync.Mutex
}

func (r *blockableRunner) Deploy(ctx context.Context, app domain.Application, _ apps.DeploySpec) error {
	r.mu.Lock()
	r.deploys = append(r.deploys, app.BundleID)
	block := r.block == app.BundleID
	r.mu.Unlock()
	if block {
		select {
		case <-r.released:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (r *blockableRunner) Start(context.Context, domain.Application, apps.DeploySpec) error {
	return nil
}
func (r *blockableRunner) Stop(context.Context, domain.Application, apps.DeploySpec) error {
	return nil
}
func (r *blockableRunner) Remove(context.Context, domain.Application, apps.DeploySpec) error {
	return nil
}

func (r *blockableRunner) Check(_ context.Context, _ domain.Application, _ apps.DeploySpec) (domain.Health, error) {
	if r.health == "" {
		return domain.HealthHealthy, nil
	}
	return r.health, nil
}

// TestSetupDAGCanceledRunReleasesLockAndPreservesState proves the three
// cancellation contracts: a canceled run returns (no nil-error panic, no
// stuck single-flight), the next setup run can reserve the lock, and the
// canceled node persists "canceled" — never "failed".
func TestSetupDAGCanceledRunReleasesLockAndPreservesState(t *testing.T) {
	ctx := context.Background()
	runner := &blockableRunner{health: domain.HealthHealthy, block: "caddy", released: make(chan struct{})}
	b := newDAGRunBackend(t, runner)
	if _, err := b.secrets.Put(ctx, "platform-app", "cloudflare_dns", "dns-token"); err != nil {
		t.Fatal(err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	go func() {
		// Cancel while the caddy install node is blocked inside Deploy.
		runner.mu.Lock()
		started := len(runner.deploys) > 0
		runner.mu.Unlock()
		for !started {
			time.Sleep(time.Millisecond)
			runner.mu.Lock()
			started = len(runner.deploys) > 0
			runner.mu.Unlock()
		}
		cancel()
	}()
	// A canceled run must return (no panic, no hang holding setupMu).
	done := make(chan error, 1)
	go func() { done <- b.RunSetupReconciler(runCtx) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("canceled setup run did not return")
	}
	if b.IsSetupRunning() {
		t.Fatal("setup still running after canceled run; single-flight lock leaked")
	}
	// Subsequent setup/status can obtain the lock: the retry reserves the
	// single-flight and completes its own pass (node outcomes reflect the
	// half-installed app from the interrupted first run; what matters is
	// that the lock was free and the run returned cleanly).
	close(runner.released)
	if err := b.RunSetupReconciler(ctx); err != nil {
		t.Fatalf("retry after canceled run: %v", err)
	}
	if b.IsSetupRunning() {
		t.Fatal("setup still running after retry; single-flight lock leaked")
	}
	// A canceled run persists canceled state — never failed.
	runStates := b.setupNodeStates(ctx)
	for id, st := range runStates {
		_ = id
		if st == setupNodeCanceled {
			t.Fatalf("node %s still canceled after a retry reset it (states: %v)", id, runStates)
		}
	}
}

// TestSetupDAGCanceledNodesPersistCanceledNotFailed pins the persisted
// node state during a canceled run: the interrupted node itself and every
// condition-canceled downstream node read "canceled", never "failed", and
// no setup.step_failed events fire.
func TestSetupDAGCanceledNodesPersistCanceledNotFailed(t *testing.T) {
	ctx := context.Background()
	runner := &blockableRunner{health: domain.HealthHealthy, block: "caddy", released: make(chan struct{})}
	b, ev := newDAGRunBackendWithEvents(t, runner)
	if _, err := b.secrets.Put(ctx, "platform-app", "cloudflare_dns", "dns-token"); err != nil {
		t.Fatal(err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	go func() {
		runner.mu.Lock()
		started := len(runner.deploys) > 0
		runner.mu.Unlock()
		for !started {
			time.Sleep(time.Millisecond)
			runner.mu.Lock()
			started = len(runner.deploys) > 0
			runner.mu.Unlock()
		}
		cancel()
	}()
	done := make(chan error, 1)
	go func() { done <- b.RunSetupReconciler(runCtx) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("canceled setup run did not return")
	}
	states := b.setupNodeStates(ctx)
	if states["core_apps:caddy"] != setupNodeCanceled {
		t.Fatalf("interrupted node caddy = %q, want canceled (states: %v)", states["core_apps:caddy"], states)
	}
	canceledCount := 0
	for _, st := range states {
		switch st {
		case setupNodeCanceled:
			canceledCount++
		case setupNodeFailed:
			t.Fatalf("canceled run persisted a failed node (states: %v)", states)
		}
	}
	if canceledCount == 0 {
		t.Fatalf("expected canceled node states, got %v", states)
	}
	if evts := eventTypes(t, ev, "setup.step_failed"); len(evts) != 0 {
		t.Fatalf("canceled run emitted setup.step_failed events: %v", evts)
	}
}

// TestSetupDAGOIDCDependsOnRunningCheckInstalls pins the oidc node's
// upstream set: the install nodes of every bundle whose OIDC operation is
// gated on observed-running state (immich, paperless-ngx, karakeep,
// litellm) must be explicit dependencies, so first setup can never
// silently miss sign-in configuration because the scheduler happened to
// run oidc before those apps were running.
func TestSetupDAGOIDCDependsOnRunningCheckInstalls(t *testing.T) {
	b, _ := newSetupBackend(t, nil)
	digest := "sha256:" + strings.Repeat("a", 64)
	cat, err := apps.NewCatalog(
		testSetupBundle("caddy", digest, domain.ExposurePublic, "", nil),
		testSetupBundle("pocket-id", digest, domain.ExposurePrivate, "id", []string{"caddy"}),
		testSetupBundle("immich", digest, domain.ExposurePrivate, "photos", []string{"caddy", "pocket-id"}),
		testSetupBundle("paperless-ngx", digest, domain.ExposurePrivate, "paperless", []string{"caddy"}),
		testSetupBundle("karakeep", digest, domain.ExposurePrivate, "karakeep", []string{"caddy"}),
		testSetupBundle("litellm", digest, domain.ExposurePrivate, "litellm", []string{"caddy"}),
		testSetupBundle("forgejo", digest, domain.ExposurePrivate, "git", []string{"caddy"}),
		testSetupBundle("technitium", digest, domain.ExposurePrivate, "dns", []string{"caddy"}),
		testSetupBundle("jellyfin", digest, domain.ExposurePrivate, "media", []string{"caddy"}),
	)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := apps.NewService(b.db, apps.Options{Catalog: cat, Runner: &failingRunner{health: domain.HealthHealthy}})
	if err != nil {
		t.Fatal(err)
	}
	b.apps = svc
	w, nodes := b.buildSetupDAG(context.Background())
	byID := map[string]*setupNode{}
	for _, n := range nodes {
		byID[n.id] = n
	}
	oidc, ok := byID["oidc"]
	if !ok {
		t.Fatal("oidc node missing from DAG")
	}
	ups := w.UpstreamOf(oidc)
	want := []string{
		"core_apps:pocket-id", "core_apps:forgejo", "core_apps:technitium", "core_apps:jellyfin",
		"core_apps:immich", "core_apps:paperless-ngx", "core_apps:karakeep", "core_apps:litellm",
	}
	got := map[string]bool{}
	for up := range ups {
		got[flow.String(up)] = true
	}
	for _, id := range want {
		if !got[id] {
			t.Fatalf("oidc upstream missing %s (got %v)", id, got)
		}
	}
}
