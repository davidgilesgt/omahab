package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/omahab/omahab/internal/apitypes"
	"github.com/omahab/omahab/internal/apps"
	"github.com/omahab/omahab/internal/bundles"
	"github.com/omahab/omahab/internal/domain"
)

// bundleOpsTestEnv is a backend with a curated catalog file and a real git
// custom-bundles repo (the bundles.Service runs real git in a temp dir), a
// tmpfs handoff dir, and injectable unit/HTTP probe seams.
type bundleOpsTestEnv struct {
	b          *Backend
	catalogDir string
	repoDir    string
	runtimeDir string
	catalog    string // current curated catalog JSON
	unitActive map[string]bool
	httpHealth map[string]domain.Health
}

const bundleOpsCuratedCatalog = `{"bundles":[{"id":"immich","name":"Immich","units":["immich.service"],"max_exposure":"shared","health_check":{"kind":"none"},"resources":{"memory_mb":128}}]}`

func validCustomBundleJSON(id string, port int) string {
	return `{"id":"` + id + `","name":"` + id + `","units":["` + id + `.service"],"port":` + strconv.Itoa(port) + `,"health_check":{"kind":"none","port":` + strconv.Itoa(port) + `,"path":"/"},"resources":{"memory_mb":128}}`
}

func newBundleOpsTestEnv(t *testing.T) *bundleOpsTestEnv {
	t.Helper()
	env := &bundleOpsTestEnv{
		catalogDir: t.TempDir(),
		repoDir:    filepath.Join(t.TempDir(), "bundles"),
		runtimeDir: t.TempDir(),
		unitActive: map[string]bool{},
		httpHealth: map[string]domain.Health{},
	}
	t.Setenv("OMAHAB_RUNTIME_DIR", env.runtimeDir)
	env.catalog = bundleOpsCuratedCatalog
	if err := os.WriteFile(filepath.Join(env.catalogDir, "catalog.json"), []byte(env.catalog), 0o644); err != nil {
		t.Fatal(err)
	}
	b, _ := newSetupBackend(t, nil)
	b.cfg.StateDir = t.TempDir()
	b.cfg.CatalogPath = filepath.Join(env.catalogDir, "catalog.json")
	b.cfg.CustomBundlesDir = env.repoDir
	env.b = b

	oldUnit, oldHTTP := probeUnitActiveFunc, probeHTTPHealthFunc
	probeUnitActiveFunc = func(ctx context.Context, unit string) (bool, error) {
		return env.unitActive[unit], nil
	}
	probeHTTPHealthFunc = func(ctx context.Context, hc apps.HealthCheck, port int) (domain.Health, error) {
		return env.httpHealth[strconv.Itoa(port)], nil
	}
	t.Cleanup(func() { probeUnitActiveFunc, probeHTTPHealthFunc = oldUnit, oldHTTP })
	return env
}

// commitCustomBundle adds a custom bundle through the repo service and
// commits it, so it appears in the HEAD view.
func (e *bundleOpsTestEnv) commitCustomBundle(t *testing.T, id string, port int, moduleNix string) {
	t.Helper()
	svc := e.b.bundlesSvc()
	if svc == nil {
		t.Fatal("custom bundles service is not configured")
	}
	if err := svc.EnsureRepo(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddFiles(context.Background(), []byte(validCustomBundleJSON(id, port)), []byte(moduleNix)); err != nil {
		t.Fatal(err)
	}
	// Mirror push failures are reported but not fatal (commit != "").
	if commit, err := svc.Commit(context.Background(), "test add "+id); err != nil && commit == "" {
		t.Fatal(err)
	}
}

// readHandoff decodes the current staging handoff, if any.
func (e *bundleOpsTestEnv) readHandoff(t *testing.T) (opID string, skip []string, exists bool) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(e.runtimeDir, "bundle-apply"))
	if err != nil {
		return "", nil, false
	}
	var h struct {
		OperationID string   `json:"operation_id"`
		SkipHealth  []string `json:"skip_health,omitempty"`
	}
	if err := json.Unmarshal(raw, &h); err != nil {
		t.Fatalf("parse handoff: %v", err)
	}
	return h.OperationID, h.SkipHealth, true
}

// --- ProbeBundleHealth ---

// The probe must observe the currently configured services with no
// application row: active units + a healthy HTTP check pass even though the
// apps database has no application for the bundle.
func TestProbeBundleHealthFreshNoAppRow(t *testing.T) {
	env := newBundleOpsTestEnv(t)
	env.commitCustomBundle(t, "myapp", 20500, "{ config, ... }: { }\n")
	// Re-point the catalog at an HTTP health check for the probe.
	env.writeCustomBundleWithHTTPCheck(t, "myapp", 20500)

	env.unitActive["myapp.service"] = true
	env.httpHealth["20500"] = domain.HealthHealthy
	if err := env.b.ProbeBundleHealth(context.Background(), "myapp", ""); err != nil {
		t.Fatalf("fresh probe with active units and healthy HTTP: %v", err)
	}
}

// A stopped unit fails the probe immediately, even when the HTTP endpoint
// still answers (units are checked first).
func TestProbeBundleHealthStoppedUnitFails(t *testing.T) {
	env := newBundleOpsTestEnv(t)
	env.commitCustomBundle(t, "myapp", 20500, "{ config, ... }: { }\n")
	env.writeCustomBundleWithHTTPCheck(t, "myapp", 20500)

	env.unitActive["myapp.service"] = false
	env.httpHealth["20500"] = domain.HealthHealthy
	err := env.b.ProbeBundleHealth(context.Background(), "myapp", "")
	if err == nil || !strings.Contains(err.Error(), "not active") {
		t.Fatalf("stopped unit must fail the probe, got %v", err)
	}
}

// A broken HTTP endpoint fails the probe even though every unit is active.
func TestProbeBundleHealthBrokenHTTPFails(t *testing.T) {
	env := newBundleOpsTestEnv(t)
	env.commitCustomBundle(t, "myapp", 20500, "{ config, ... }: { }\n")
	env.writeCustomBundleWithHTTPCheck(t, "myapp", 20500)

	env.unitActive["myapp.service"] = true
	env.httpHealth["20500"] = domain.HealthUnhealthy
	err := env.b.ProbeBundleHealth(context.Background(), "myapp", "")
	if err == nil || !strings.Contains(err.Error(), "health check") {
		t.Fatalf("unhealthy endpoint must fail the probe, got %v", err)
	}
}

// Unknown bundles 404; empty ids are a validation error.
func TestProbeBundleHealthUnknownAndEmpty(t *testing.T) {
	env := newBundleOpsTestEnv(t)
	err := env.b.ProbeBundleHealth(context.Background(), "nosuch", "")
	if err == nil || !errors.Is(err, apitypes.ErrNotFound) {
		t.Fatalf("unknown bundle must be ErrNotFound, got %v", err)
	}
	err = env.b.ProbeBundleHealth(context.Background(), " ", "")
	if err == nil || !errors.Is(err, apitypes.ErrValidation) {
		t.Fatalf("empty id must be ErrValidation, got %v", err)
	}
}

// A bundle whose committed health check is kind none only gates on units.
func TestProbeBundleHealthKindNoneUnitsOnly(t *testing.T) {
	env := newBundleOpsTestEnv(t)
	env.commitCustomBundle(t, "plain", 20501, "{ config, ... }: { }\n")
	env.unitActive["plain.service"] = true
	if err := env.b.ProbeBundleHealth(context.Background(), "plain", ""); err != nil {
		t.Fatalf("units-only bundle must pass with active unit: %v", err)
	}
	env.unitActive["plain.service"] = false
	if err := env.b.ProbeBundleHealth(context.Background(), "plain", ""); err == nil {
		t.Fatal("units-only bundle must fail when the unit is stopped")
	}
}

// A probe pinned to revision A must keep using A's service metadata
// after commit B changes the bundle's units and health check: B's
// metadata must never affect the gate observing the generation built
// from A. Malformed revisions are rejected without probing anything.
func TestProbeBundleHealthPinnedRevisionIgnoresLaterCommit(t *testing.T) {
	env := newBundleOpsTestEnv(t)
	env.commitCustomBundle(t, "pinned", 20510, "{ config, ... }: { }\n")

	svc := env.b.bundlesSvc()
	ctx := context.Background()
	revA, err := svc.RevParseHead(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Commit B moves the unit and the HTTP health-check port.
	revB := `{"id":"pinned","name":"pinned","units":["pinned-v2.service"],"port":20511,"health_check":{"kind":"http","port":20511,"path":"/health"},"resources":{"memory_mb":128}}`
	if err := os.WriteFile(filepath.Join(env.repoDir, "pinned", "bundle.json"), []byte(revB), 0o644); err != nil {
		t.Fatal(err)
	}
	if commit, err := svc.Commit(ctx, "test revision B"); err != nil && commit == "" {
		t.Fatal(err)
	}

	// B is current HEAD: probing current state observes B's unit.
	env.unitActive["pinned-v2.service"] = true
	env.unitActive["pinned.service"] = false
	env.httpHealth["20511"] = domain.HealthHealthy
	if err := env.b.ProbeBundleHealth(ctx, "pinned", ""); err != nil {
		t.Fatalf("current-revision probe must use B's metadata: %v", err)
	}

	// Pinned to A: B's unit state and port must be ignored; A's unit
	// (inactive here) fails the probe even though B's is healthy.
	err = env.b.ProbeBundleHealth(ctx, "pinned", revA)
	if err == nil {
		t.Fatal("probe pinned to A must fail while A's unit is inactive")
	}
	env.unitActive["pinned.service"] = true
	if err := env.b.ProbeBundleHealth(ctx, "pinned", revA); err != nil {
		t.Fatalf("probe pinned to A must pass on A's active unit (kind none at A): %v", err)
	}

	// Malformed revisions are validation errors, never probes.
	for _, bad := range []string{"deadbeef", revA + "0", strings.ToUpper(revA[:39]) + "Z"} {
		err := env.b.ProbeBundleHealth(ctx, "pinned", bad)
		if err == nil || !errors.Is(err, apitypes.ErrValidation) {
			t.Fatalf("revision %q must be rejected as validation error, got %v", bad, err)
		}
	}
}

// writeCustomBundleWithHTTPCheck rewrites the committed bundle.json to use
// an HTTP health check on the given port (used to exercise the HTTP leg).
func (e *bundleOpsTestEnv) writeCustomBundleWithHTTPCheck(t *testing.T, id string, port int) {
	t.Helper()
	svc := e.b.bundlesSvc()
	body := `{"id":"` + id + `","name":"` + id + `","units":["` + id + `.service"],"port":` + strconv.Itoa(port) +
		`,"health_check":{"kind":"http","port":` + strconv.Itoa(port) + `,"path":"/health"},"resources":{"memory_mb":128}}`
	if _, err := svc.AddFiles(context.Background(), []byte(body), []byte("{ config, ... }: { }\n")); err != nil {
		// AddFiles refuses to overwrite; write through the worktree and commit.
		if err := os.MkdirAll(filepath.Join(e.repoDir, id), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(e.repoDir, id, "bundle.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if commit, err := svc.Commit(context.Background(), "test http check "+id); err != nil && commit == "" {
		t.Fatal(err)
	}
}

// --- enable validation ---

// Enabling a disabled bundle whose bundle.json collides (unit clash with
// the curated catalog) must be rejected before the marker is removed: the
// committed state stays valid and the bundle stays disabled.
func TestEnableCollidingBundleRejectedAtomically(t *testing.T) {
	env := newBundleOpsTestEnv(t)
	// A custom bundle whose unit collides with curated immich.service.
	svc := env.b.bundlesSvc()
	if err := svc.EnsureRepo(context.Background()); err != nil {
		t.Fatal(err)
	}
	collide := `{"id":"clash","name":"clash","units":["immich.service"],"port":20502,"health_check":{"kind":"none","port":20502,"path":"/"},"resources":{"memory_mb":128}}`
	if _, err := svc.AddFiles(context.Background(), []byte(collide), []byte("{ config, ... }: { }\n")); err != nil {
		t.Fatal(err)
	}
	if commit, err := svc.Commit(context.Background(), "test add clash"); err != nil && commit == "" {
		t.Fatal(err)
	}
	// Disable it through the API (valid today: disabling never merges).
	if err := env.b.SetCustomBundleDisabled(context.Background(), "clash", true); err != nil {
		t.Fatalf("disable: %v", err)
	}

	err := env.b.SetCustomBundleDisabled(context.Background(), "clash", false)
	if err == nil || !errors.Is(err, apitypes.ErrValidation) {
		t.Fatalf("colliding enable must be rejected as validation, got %v", err)
	}
	// Atomicity: the marker is still present in the committed view and the
	// merged catalog still excludes the bundle.
	st, err := svc.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range st.Bundles {
		if item.ID == "clash" && !item.Disabled {
			t.Fatal("failed enable must leave the bundle disabled")
		}
	}
	merged, err := bundles.LoadMerged(context.Background(), env.b.cfg.CatalogPath, env.b.cfg.CustomBundlesDir)
	if err != nil {
		t.Fatalf("merged catalog must still load after the rejected enable: %v", err)
	}
	if _, ok := merged.Get("clash"); ok {
		t.Fatal("failed enable must not put the colliding bundle into the merged catalog")
	}
}

// Enabling a valid disabled bundle still succeeds.
func TestEnableValidDisabledBundleSucceeds(t *testing.T) {
	env := newBundleOpsTestEnv(t)
	env.commitCustomBundle(t, "good", 20503, "{ config, ... }: { }\n")
	if err := env.b.SetCustomBundleDisabled(context.Background(), "good", true); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if err := env.b.SetCustomBundleDisabled(context.Background(), "good", false); err != nil {
		t.Fatalf("valid enable must succeed: %v", err)
	}
}

// --- apply staging ---

// A staged apply reserves the slot: a second concurrent stage is rejected
// with a conflict before it can replace the handoff.
func TestStageBundleApplyUniqueReservation(t *testing.T) {
	env := newBundleOpsTestEnv(t)
	env.commitCustomBundle(t, "myapp", 20500, "{ config, ... }: { }\n")

	op1, err := env.b.stageBundleApply(context.Background(), nil)
	if err != nil {
		t.Fatalf("first stage: %v", err)
	}
	opID1, _, ok := env.readHandoff(t)
	if !ok || opID1 != op1.ID {
		t.Fatalf("handoff must hold the first operation id, got %q", opID1)
	}

	_, err = env.b.stageBundleApply(context.Background(), nil)
	if err == nil || !errors.Is(err, apitypes.ErrConflict) {
		t.Fatalf("concurrent stage must conflict, got %v", err)
	}
	// The first operation's handoff survives the rejected second stage.
	opID1b, _, ok := env.readHandoff(t)
	if !ok || opID1b != op1.ID {
		t.Fatalf("rejected stage must not replace the handoff, got %q", opID1b)
	}

	// startStagedBundleApply for a foreign operation id is rejected.
	foreign := op1
	foreign.ID = "op_not_staged"
	if err := env.b.startStagedBundleApply(foreign); err == nil {
		t.Fatal("start for a non-staged operation must be rejected")
	}

	// Releasing via outcome convergence frees the slot.
	writeBundleApplyOutcomeForTest(t, env, op1.ID, OpCompleted)
	if _, err := env.b.GetBundleApplyStatus(context.Background(), op1.ID); err != nil {
		t.Fatal(err)
	}
	op2, err := env.b.stageBundleApply(context.Background(), nil)
	if err != nil {
		t.Fatalf("stage after released reservation: %v", err)
	}
	if op2.ID == op1.ID {
		t.Fatal("second stage must create a new operation")
	}
}

// The staged handoff carries the trusted skip_health list, validated
// against the merged catalog.
func TestStageBundleApplySkipHealthHandoff(t *testing.T) {
	env := newBundleOpsTestEnv(t)
	env.commitCustomBundle(t, "myapp", 20500, "{ config, ... }: { }\n")
	env.commitCustomBundle(t, "other", 20504, "{ config, ... }: { }\n")

	op, err := env.b.stageBundleApply(context.Background(), []string{" myapp ", "", "other", "myapp"})
	if err != nil {
		t.Fatalf("stage with skips: %v", err)
	}
	opID, skip, ok := env.readHandoff(t)
	if !ok || opID != op.ID {
		t.Fatalf("handoff missing or wrong id: %q", opID)
	}
	if len(skip) != 2 || skip[0] != "myapp" || skip[1] != "other" {
		t.Fatalf("handoff skip_health = %v, want [myapp other]", skip)
	}
	// The journal records the skip list as the affected scope.
	j, err := readBundleApplyJournal(env.b.cfg.StateDir, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(j.AffectedApp) != 2 || j.AffectedApp[0] != "myapp" || j.AffectedApp[1] != "other" {
		t.Fatalf("journal affected = %v, want [myapp other]", j.AffectedApp)
	}

	// Unknown skip ids are rejected before anything is staged.
	if _, err := env.b.stageBundleApply(context.Background(), []string{"ghost"}); err == nil || !errors.Is(err, apitypes.ErrValidation) {
		t.Fatalf("unknown skip id must be validation error, got %v", err)
	}
}

// Staging with an invalid committed catalog is rejected and reserves
// nothing.
func TestStageBundleApplyRejectsBrokenCommittedCatalog(t *testing.T) {
	env := newBundleOpsTestEnv(t)
	env.commitCustomBundle(t, "myapp", 20500, "{ config, ... }: { }\n")
	// Corrupt the committed catalog through the worktree + commit.
	svc := env.b.bundlesSvc()
	broken := `{"id":"broken","name":"b","units":["immich.service"],"port":20505,"health_check":{"kind":"none"},"resources":{"memory_mb":128}}`
	if err := os.MkdirAll(filepath.Join(env.repoDir, "broken"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(env.repoDir, "broken", "bundle.json"), []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(env.repoDir, "broken", "module.nix"), []byte("{ config, ... }: { }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if commit, err := svc.Commit(context.Background(), "test broken"); err != nil && commit == "" {
		t.Fatal(err)
	}
	if _, err := env.b.stageBundleApply(context.Background(), nil); err == nil {
		t.Fatal("stage over a colliding committed catalog must fail")
	}
	if _, _, ok := env.readHandoff(t); ok {
		t.Fatal("failed stage must not leave a handoff")
	}
	if res := env.b.holdBundleApply(); res != nil {
		t.Fatalf("failed stage must not reserve, got %+v", res)
	}
}

// --- restart reconciliation ---

// After a restart (fresh Backend), a leftover handoff with a running
// journal is adopted: new stages conflict until the outcome converges.
func TestReconcileAdoptsLiveStagedApply(t *testing.T) {
	env := newBundleOpsTestEnv(t)
	env.commitCustomBundle(t, "myapp", 20500, "{ config, ... }: { }\n")
	op, err := env.b.stageBundleApply(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}

	// Simulate the restart: a fresh backend over the same state.
	b2, _ := newSetupBackend(t, nil)
	b2.cfg = env.b.cfg
	b2.reconcileBundleApplyReservation()

	if _, err := b2.stageBundleApply(context.Background(), nil); err == nil || !errors.Is(err, apitypes.ErrConflict) {
		t.Fatalf("stage after restart must conflict with the live apply, got %v", err)
	}
	// The outcome converges and releases.
	writeBundleApplyOutcomeForTest(t, env, op.ID, OpCompleted)
	if _, err := b2.GetBundleApplyStatus(context.Background(), op.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := b2.stageBundleApply(context.Background(), nil); err != nil {
		t.Fatalf("stage after converged outcome: %v", err)
	}
}

// A phantom handoff (no journal behind it) is removed at startup and does
// not block new applies.
func TestReconcileRemovesPhantomHandoff(t *testing.T) {
	env := newBundleOpsTestEnv(t)
	env.commitCustomBundle(t, "myapp", 20500, "{ config, ... }: { }\n")
	if err := writeBundleApplyHandoff("op_ghost", nil); err != nil {
		t.Fatal(err)
	}
	b2, _ := newSetupBackend(t, nil)
	b2.cfg = env.b.cfg
	b2.reconcileBundleApplyReservation()
	if _, _, ok := env.readHandoff(t); ok {
		t.Fatal("phantom handoff must be removed at startup")
	}
	if _, err := b2.stageBundleApply(context.Background(), nil); err != nil {
		t.Fatalf("phantom handoff must not block staging: %v", err)
	}
}

// A terminal outcome on a restart is not adopted: only running applies
// hold the reservation.
func TestReconcileIgnoresTerminalJournals(t *testing.T) {
	env := newBundleOpsTestEnv(t)
	env.commitCustomBundle(t, "myapp", 20500, "{ config, ... }: { }\n")
	op, err := env.b.stageBundleApply(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	writeBundleApplyOutcomeForTest(t, env, op.ID, OpFailed)
	// Consume the outcome so the journal is terminal on disk.
	if _, err := env.b.GetBundleApplyStatus(context.Background(), op.ID); err != nil {
		t.Fatal(err)
	}
	// A leftover handoff (unit never consumed it) with a terminal journal.
	if err := writeBundleApplyHandoff(op.ID, nil); err != nil {
		t.Fatal(err)
	}
	b2, _ := newSetupBackend(t, nil)
	b2.cfg = env.b.cfg
	b2.reconcileBundleApplyReservation()
	if _, err := b2.stageBundleApply(context.Background(), nil); err != nil {
		t.Fatalf("terminal journal must not block staging: %v", err)
	}
}

// writeBundleApplyOutcomeForTest records the CLI outcome marker exactly as
// the oneshot unit run does.
func writeBundleApplyOutcomeForTest(t *testing.T, env *bundleOpsTestEnv, opID, status string) {
	t.Helper()
	dir := filepath.Join(operationsDir(env.b.cfg.StateDir, bundleApplyJournalKind), opID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.MarshalIndent(map[string]string{"status": status, "detail": ""}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "outcome.json"), append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}
