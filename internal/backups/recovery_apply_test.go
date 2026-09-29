package backups

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	_ "modernc.org/sqlite"
)

// --- offline recovery executor tests (fake restic, temp dirs) ---

// fakeRecoveryRunner simulates a restic repository holding one staged
// tree per snapshot id.
type fakeRecoveryRunner struct {
	mu        sync.Mutex
	repos     []Repository
	creds     []Credentials
	snapList  []SnapshotListEntry
	snapshots map[string]string // snapshot id -> staging template dir
}

func (f *fakeRecoveryRunner) Init(_ context.Context, repo Repository, creds Credentials) error {
	return nil
}

func (f *fakeRecoveryRunner) Snapshots(_ context.Context, repo Repository, creds Credentials, latest int) ([]SnapshotListEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repos = append(f.repos, repo)
	f.creds = append(f.creds, creds)
	out := f.snapList
	if latest > 0 && len(out) > latest {
		out = out[:latest]
	}
	return out, nil
}

func (f *fakeRecoveryRunner) Backup(_ context.Context, _ Repository, _ Credentials, _ BackupRequest) (Snapshot, error) {
	return Snapshot{}, fmt.Errorf("backup not supported in recovery tests")
}

func (f *fakeRecoveryRunner) Check(_ context.Context, _ Repository, _ Credentials) error {
	return nil
}
func (f *fakeRecoveryRunner) Forget(_ context.Context, _ Repository, _ Credentials, _ ForgetRequest) (ForgetResult, error) {
	return ForgetResult{}, nil
}

func (f *fakeRecoveryRunner) Stats(_ context.Context, _ Repository, _ Credentials) (RepoStats, error) {
	return RepoStats{}, nil
}

func (f *fakeRecoveryRunner) Restore(_ context.Context, repo Repository, creds Credentials, snapshotID, target string) error {
	f.mu.Lock()
	src, ok := f.snapshots[snapshotID]
	f.mu.Unlock()
	if !ok {
		return fmt.Errorf("restic restore failed: snapshot %s not found", snapshotID)
	}
	return copyTree(context.Background(), src, target)
}

// writeServerConfigSnapshot builds a staging template that mirrors the
// real snapshot layout: state files under <staging>/<stateDir>/ plus a
// manifest with the server-config scope.
func writeServerConfigSnapshot(t *testing.T, template, stateDir string, m SnapshotManifest) {
	t.Helper()
	base := filepath.Join(template, stateDir)
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	// A real SQLite control.db so ValidateStaging integrity passes.
	db, err := sql.Open("sqlite", "file:"+filepath.Join(base, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE t (id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if err := os.WriteFile(filepath.Join(base, "recovery.kit"), []byte(`{"version":1,"fingerprint":"aabbccdd","master_wrapped":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "storage.json"), []byte(`{"version":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base, "appenv"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "appenv", "immich.env"), []byte("K=v\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := WriteManifest(template, m); err != nil {
		t.Fatal(err)
	}
}

func recoveryTestManifest(fp string) SnapshotManifest {
	return SnapshotManifest{
		FormatVersion:       1,
		InstanceID:          "inst-1",
		Domain:              "omahab.com",
		ReleaseRef:          "v1",
		Scopes:              []string{ScopeServerConfig},
		RecoveryFingerprint: fp,
	}
}

func TestListRecoverySnapshotsMarksServerConfig(t *testing.T) {
	stateDir := "/var/lib/omahab"
	runner := &fakeRecoveryRunner{snapList: []SnapshotListEntry{
		{ID: "apponly", Time: "2026-09-20T00:00:00Z", Hostname: "h", Paths: []string{"/srv/omahab/apps/immich"}},
		{ID: "full", Time: "2026-09-26T00:00:00Z", Hostname: "h", Paths: []string{"/var/lib/omahab", "/srv/omahab/apps"}},
	}}
	snaps, err := ListRecoverySnapshots(context.Background(), runner, RecoverySource{Location: "sftp://u@h:23/omahab", Password: "pw"}, stateDir, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(snaps) != 2 {
		t.Fatalf("snaps = %d, want 2", len(snaps))
	}
	if !snaps[0].ServerCfg || snaps[0].ID != "full" {
		t.Fatalf("newest first + server-config mark failed: %+v", snaps[0])
	}
	if snaps[1].ServerCfg {
		t.Fatal("app-only snapshot must not be marked server-config")
	}
	// Credentials never ride on the repository URL or args surface: the
	// fake records what the runner saw.
	if runner.creds[0].Password != "pw" {
		t.Fatal("password must reach the runner env only")
	}
	if runner.repos[0].Attached != true {
		t.Fatal("recovery source must use the exact location (attached semantics)")
	}
}

func TestListRecoverySnapshotsRequiresLocationAndPassword(t *testing.T) {
	runner := &fakeRecoveryRunner{}
	if _, err := ListRecoverySnapshots(context.Background(), runner, RecoverySource{Location: "", Password: "pw"}, "/var/lib/omahab", 10); err == nil {
		t.Fatal("empty location must be refused")
	}
	if _, err := ListRecoverySnapshots(context.Background(), runner, RecoverySource{Location: "/srv/omahab/backups/repos/r"}, "/var/lib/omahab", 10); err == nil {
		t.Fatal("missing password must be refused")
	}
}

func TestPickRecoverySnapshot(t *testing.T) {
	snaps := []RecoverySnapshot{
		{ID: "older", ServerCfg: true},
		{ID: "newer", ServerCfg: true},
		{ID: "app", ServerCfg: false},
	}
	got, err := PickRecoverySnapshot(snaps, "")
	if err != nil {
		t.Fatalf("pick latest: %v", err)
	}
	// List order is newest-first; pick keeps it.
	if got.ID != "older" {
		t.Fatalf("pick returned %q, want first server-config entry", got.ID)
	}
	if _, err := PickRecoverySnapshot(snaps, "app"); err == nil {
		t.Fatal("explicit app-only snapshot must be refused")
	}
	if _, err := PickRecoverySnapshot(snaps, "missing"); err == nil {
		t.Fatal("unknown snapshot must be refused")
	}
	if _, err := PickRecoverySnapshot([]RecoverySnapshot{{ID: "app"}}, ""); err == nil {
		t.Fatal("no server-config snapshot must be refused")
	}
}

func TestServerConfigManifestRefusesLegacyAndWrongScopes(t *testing.T) {
	dir := t.TempDir()
	if _, err := ServerConfigManifest(dir); err == nil {
		t.Fatal("legacy snapshot without manifest must be refused")
	}
	if err := WriteManifest(dir, SnapshotManifest{FormatVersion: 1, InstanceID: "i", Scopes: []string{AppScope("immich")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := ServerConfigManifest(dir); err == nil {
		t.Fatal("manifest without server-config scope must be refused")
	}
	if err := WriteManifest(dir, recoveryTestManifest("")); err != nil {
		t.Fatal(err)
	}
	if _, err := ServerConfigManifest(dir); err != nil {
		t.Fatalf("valid manifest refused: %v", err)
	}
}

func TestCheckRecoveryFingerprint(t *testing.T) {
	m := recoveryTestManifest("aabbccdd")
	if err := CheckRecoveryFingerprint(m, "aabbccdd"); err != nil {
		t.Fatalf("matching fingerprint refused: %v", err)
	}
	if err := CheckRecoveryFingerprint(m, "AABBCCDD"); err != nil {
		t.Fatalf("case-insensitive match refused: %v", err)
	}
	err := CheckRecoveryFingerprint(m, "00000000")
	if err == nil {
		t.Fatal("mismatched fingerprint must be refused")
	}
	// Older snapshots without a recorded fingerprint skip rather than
	// guess a mismatch.
	if err := CheckRecoveryFingerprint(recoveryTestManifest(""), "aabbccdd"); err != nil {
		t.Fatalf("empty manifest fingerprint must skip: %v", err)
	}
	if err := CheckRecoveryFingerprint(recoveryTestManifest("aabbccdd"), ""); err != nil {
		t.Fatalf("empty phrase fingerprint must skip: %v", err)
	}
}

func TestPlanServerConfigSwapMapsStateFiles(t *testing.T) {
	staging := t.TempDir()
	stateDir := t.TempDir()
	writeServerConfigSnapshot(t, staging, stateDir, recoveryTestManifest(""))
	plan, err := PlanServerConfigSwap(staging, stateDir, recoveryTestManifest(""))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	dests := map[string]bool{}
	for _, f := range plan.files {
		dests[filepath.Base(f.dest)] = true
	}
	if !dests["control.db"] || !dests["recovery.kit"] || !dests["storage.json"] {
		t.Fatalf("plan missing core state files: %v", dests)
	}
	found := false
	for _, f := range plan.files {
		if strings_HasSuffixDir(f.dest, "appenv") {
			found = true
		}
	}
	if !found {
		t.Fatal("plan must include the appenv configuration subtree")
	}
	if plan.unwrap == nil || filepath.Base(plan.unwrap.dest) != "master.key" {
		t.Fatal("plan must unwrap master.key from the staged kit")
	}
}

func strings_HasSuffixDir(path, name string) bool {
	return filepath.Base(path) == name
}

func TestPlanServerConfigSwapRefusesMissingDBAndKit(t *testing.T) {
	staging := t.TempDir()
	stateDir := t.TempDir()
	// Manifest present, but no control.db and no kit.
	if err := WriteManifest(staging, recoveryTestManifest("")); err != nil {
		t.Fatal(err)
	}
	if _, err := PlanServerConfigSwap(staging, stateDir, recoveryTestManifest("")); err == nil {
		t.Fatal("missing control.db must be refused")
	}
	base := filepath.Join(staging, stateDir)
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "control.db"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := PlanServerConfigSwap(staging, stateDir, recoveryTestManifest("")); err == nil {
		t.Fatal("missing recovery.kit must be refused (master key unrecoverable)")
	}
}

func TestApplyServerConfigSwapAndRollback(t *testing.T) {
	ctx := context.Background()
	staging := t.TempDir()
	stateDir := t.TempDir()
	manifest := recoveryTestManifest("")

	// The staging template mirrors the extracted snapshot layout.
	template := t.TempDir()
	writeServerConfigSnapshot(t, template, stateDir, manifest)
	runner := &fakeRecoveryRunner{snapshots: map[string]string{"snapfull": template}}
	if err := StageRecoverySnapshot(ctx, runner, RecoverySource{Location: "sftp://u@h:23/omahab", Password: "pw"}, "snapfull", staging); err != nil {
		t.Fatalf("stage: %v", err)
	}
	if _, err := ValidateStaging(ctx, staging); err != nil {
		t.Fatalf("validate staged: %v", err)
	}
	plan, err := PlanServerConfigSwap(staging, stateDir, manifest)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	// Pre-existing live state must survive as the rollback copy.
	live := filepath.Join(stateDir, "control.db")
	if err := os.WriteFile(live, []byte("old-live-db"), 0o600); err != nil {
		t.Fatal(err)
	}
	master := make([]byte, 32)
	for i := range master {
		master[i] = byte(i)
	}
	rollback, cleanup, err := ApplyServerConfigSwap(ctx, plan, master)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	defer cleanup()
	got, err := os.ReadFile(live)
	if err != nil {
		t.Fatalf("read swapped control.db: %v", err)
	}
	if string(got) == "old-live-db" {
		t.Fatal("control.db was not swapped in")
	}
	km, err := os.ReadFile(filepath.Join(stateDir, "master.key"))
	if err != nil {
		t.Fatalf("read master.key: %v", err)
	}
	if len(km) != 32 {
		t.Fatalf("master.key length = %d, want 32", len(km))
	}
	if string(km) != string(master) {
		t.Fatal("master.key does not match the unwrapped key")
	}
	perm, _ := os.Stat(filepath.Join(stateDir, "master.key"))
	if perm.Mode().Perm() != 0o600 {
		t.Fatalf("master.key mode = %o, want 0600", perm.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(stateDir, "appenv", "immich.env")); err != nil {
		t.Fatalf("appenv subtree not restored: %v", err)
	}

	// Rollback restores the pre-swap bytes.
	if err := rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	got, err = os.ReadFile(live)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "old-live-db" {
		t.Fatalf("rollback left %q, want the preserved live db", string(got))
	}
	if _, err := os.Stat(filepath.Join(stateDir, "master.key")); !os.IsNotExist(err) {
		t.Fatalf("rollback must remove the swapped master.key: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "appenv", "immich.env")); !os.IsNotExist(err) {
		t.Fatal("rollback must remove the swapped appenv subtree")
	}
}

func TestApplyServerConfigSwapFailureRollsBack(t *testing.T) {
	ctx := context.Background()
	staging := t.TempDir()
	stateDir := t.TempDir()
	manifest := recoveryTestManifest("")
	template := t.TempDir()
	writeServerConfigSnapshot(t, template, stateDir, manifest)
	runner := &fakeRecoveryRunner{snapshots: map[string]string{"snapfull": template}}
	if err := StageRecoverySnapshot(ctx, runner, RecoverySource{Location: "sftp://u@h:23/omahab", Password: "pw"}, "snapfull", staging); err != nil {
		t.Fatalf("stage: %v", err)
	}
	plan, err := PlanServerConfigSwap(staging, stateDir, manifest)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	// Sabotage the last planned source after planning: earlier files in
	// the plan swap successfully, then the missing source fails the copy
	// and everything applied so far must roll back.
	last := plan.files[len(plan.files)-1]
	if err := os.RemoveAll(last.source); err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(stateDir, "storage.json")
	if err := os.WriteFile(live, []byte("live-storage"), 0o600); err != nil {
		t.Fatal(err)
	}
	rollback, cleanup, err := ApplyServerConfigSwap(ctx, plan, make([]byte, 32))
	if err == nil {
		t.Fatal("expected swap failure from a missing staged file")
	}
	defer cleanup()
	if err := rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	got, err := os.ReadFile(live)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "live-storage" {
		t.Fatalf("rollback left %q, want preserved live storage.json", string(got))
	}
}

func TestApplyServerConfigSwapRefusesBadMasterKey(t *testing.T) {
	ctx := context.Background()
	staging := t.TempDir()
	stateDir := t.TempDir()
	manifest := recoveryTestManifest("")
	template := t.TempDir()
	writeServerConfigSnapshot(t, template, stateDir, manifest)
	runner := &fakeRecoveryRunner{snapshots: map[string]string{"s": template}}
	if err := StageRecoverySnapshot(ctx, runner, RecoverySource{Location: "sftp://u@h:23/omahab", Password: "pw"}, "s", staging); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanServerConfigSwap(staging, stateDir, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ApplyServerConfigSwap(ctx, plan, make([]byte, 16)); err == nil {
		t.Fatal("non-32-byte master key must be refused")
	}
}

func TestWriteMasterKeyAtomicAndPermissioned(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")
	if err := writeMasterKey(path, make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 0600", fi.Mode().Perm())
	}
	if _, err := os.Stat(path + ".recovery-swap"); !os.IsNotExist(err) {
		t.Fatal("temp file must not survive the atomic rename")
	}
	if err := writeMasterKey(path, make([]byte, 8)); err == nil {
		t.Fatal("wrong-size key must be refused")
	}
}

func TestRecoverySourceNeverInArgs(t *testing.T) {
	// The recovery source password must only ever travel through the
	// runner environment contract. The fake asserts what the runner
	// observed; the location itself must be the exact repository URL.
	runner := &fakeRecoveryRunner{snapList: []SnapshotListEntry{{ID: "s", Time: "2026-09-26T00:00:00Z"}}}
	src := RecoverySource{Location: "sftp://u123@u123.your-storagebox.de:23/omahab/inst", Password: "hex64password"}
	if _, err := ListRecoverySnapshots(context.Background(), runner, src, "/var/lib/omahab", 10); err != nil {
		t.Fatal(err)
	}
	if runner.repos[0].Location != src.Location {
		t.Fatalf("location mutated: %q", runner.repos[0].Location)
	}
	if runner.creds[0].Password != src.Password {
		t.Fatal("password must reach the runner unmodified")
	}
}
