package syncer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/store"
)

var (
	errTestDown      = errTestSentinel("connection refused")
	errTestForbidden = errTestSentinel("403 forbidden")
)

type errTestSentinel string

func (e errTestSentinel) Error() string { return string(e) }

func (f *fakeSink) countType(typ string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, e := range f.events {
		if e.Type == typ {
			n++
		}
	}
	return n
}

func reconcileTestService(t *testing.T, client SyncthingClient, root string) (*Service, func()) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	cleanup := func() { _ = st.Close() }
	if err := st.Migrate(context.Background(), Migrations()...); err != nil {
		t.Fatal(err)
	}
	svc := New(st.DB(), root, NoopRegistrar{})
	svc.SetSyncthingClient(client)
	svc.SetEventSink(NoopEventSink{})
	return svc, cleanup
}

func mustReconcileFolder(t *testing.T, svc *Service, name, path string, share bool) *domain.SyncFolder {
	t.Helper()
	f, err := svc.Create(context.Background(), CreateInput{Name: name, ServerPath: path, ShareWithAI: share})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return f
}

// Managed folders converge to configured with enrolled device sharing.
func TestReconcileConfigures(t *testing.T) {
	root := t.TempDir()
	fc := &fakeClient{}
	svc, cleanup := reconcileTestService(t, fc, root)
	defer cleanup()
	dir := filepath.Join(root, "obsidian")
	f := mustReconcileFolder(t, svc, "obsidian", dir, true)
	if _, err := svc.EnrollDevice(context.Background(), string(f.ID), "DEV1", "laptop"); err != nil {
		t.Fatal(err)
	}
	results, err := svc.Reconcile(context.Background(), ReconcileInput{})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(results) != 1 || results[0].Status != "configured" {
		t.Fatalf("results = %+v", results)
	}
	fc.mu.Lock()
	if len(fc.ensureFolders) != 1 {
		n := len(fc.ensureFolders)
		fc.mu.Unlock()
		t.Fatalf("ensure calls = %d", n)
	}
	spec := fc.ensureFolders[0]
	devCount := len(fc.ensureDevices)
	var dev0 DeviceSpec
	if devCount > 0 {
		dev0 = fc.ensureDevices[0]
	}
	fc.mu.Unlock()
	if spec.ID != f.SyncthingID || spec.Path != dir || len(spec.DeviceIDs) != 1 || spec.DeviceIDs[0] != "DEV1" {
		t.Fatalf("spec = %+v", spec)
	}
	if devCount != 1 || dev0.ID != "DEV1" {
		t.Fatalf("devices = %+v", dev0)
	}
	// Repeat: same wire ID, no duplicates.
	if _, err := svc.Reconcile(context.Background(), ReconcileInput{}); err != nil {
		t.Fatal(err)
	}
	fc.mu.Lock()
	n := len(fc.ensureFolders)
	var second FolderSpec
	if n > 1 {
		second = fc.ensureFolders[1]
	}
	fc.mu.Unlock()
	if n != 2 || second.ID != spec.ID {
		t.Fatalf("repeat must converge on the same wire ID (calls=%d)", n)
	}
}

// Transient Syncthing downtime: pending status with last error, then converge.
func TestReconcilePendingThenConverges(t *testing.T) {
	root := t.TempDir()
	fc := &fakeClient{ensureFolderErr: errTestDown}
	svc, cleanup := reconcileTestService(t, fc, root)
	defer cleanup()
	dir := filepath.Join(root, "drops")
	f := mustReconcileFolder(t, svc, "drops", dir, false)
	results, err := svc.Reconcile(context.Background(), ReconcileInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != "pending" || results[0].Error == "" {
		t.Fatalf("results = %+v, want pending with last error", results)
	}
	got, err := svc.Get(context.Background(), string(f.ID))
	if err != nil {
		t.Fatal(err)
	}
	if got.Health == domain.HealthDegraded {
		t.Fatal("transient downtime must not degrade; it stays pending")
	}
	fc.mu.Lock()
	fc.ensureFolderErr = nil
	fc.mu.Unlock()
	results, err = svc.Reconcile(context.Background(), ReconcileInput{})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Status != "configured" {
		t.Fatalf("retry results = %+v", results)
	}
}

// Permission failures surface as degraded health plus an event.
func TestReconcileAuthErrorDegrades(t *testing.T) {
	root := t.TempDir()
	sink := &fakeSink{}
	fc := &fakeClient{ensureFolderErr: errTestForbidden}
	svc, cleanup := reconcileTestService(t, fc, root)
	defer cleanup()
	svc.SetEventSink(sink)
	dir := filepath.Join(root, "obsidian")
	mustReconcileFolder(t, svc, "obsidian", dir, true)
	results, err := svc.Reconcile(context.Background(), ReconcileInput{})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Status != "pending" {
		t.Fatalf("results = %+v", results)
	}
	if sink.countType("syncthing.reconcile_failed") == 0 {
		t.Fatal("auth failure must emit a reconcile event (Sync/Doctor surface)")
	}
}

// Same-path adoption for managed folders; ambiguous matches stay conflicts.
func TestReconcileAdoptsSamePath(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "drops")
	fc := &fakeClient{listedFolders: []FolderInfo{{ID: "old-managed-id", Path: dir}}}
	svc, cleanup := reconcileTestService(t, fc, root)
	defer cleanup()
	f := mustReconcileFolder(t, svc, "drops", dir, false)
	results, err := svc.Reconcile(context.Background(), ReconcileInput{})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].WireID != "old-managed-id" || !results[0].RescanReady {
		t.Fatalf("results = %+v, want adoption with rescan", results)
	}
	got, err := svc.Get(context.Background(), string(f.ID))
	if err != nil {
		t.Fatal(err)
	}
	if got.SyncthingID != "old-managed-id" {
		t.Fatalf("syncthing_id = %q", got.SyncthingID)
	}
	if string(got.ID) != string(f.ID) {
		t.Fatal("internal record ID must never change")
	}
}

// Ambiguous same-path matches are not adopted.
func TestReconcileAmbiguousStaysConflict(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "drops")
	fc := &fakeClient{listedFolders: []FolderInfo{{ID: "a", Path: dir}, {ID: "b", Path: dir}}}
	svc, cleanup := reconcileTestService(t, fc, root)
	defer cleanup()
	f := mustReconcileFolder(t, svc, "drops", dir, false)
	results, err := svc.Reconcile(context.Background(), ReconcileInput{})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].WireID == "a" || results[0].WireID == "b" {
		t.Fatalf("ambiguous match must not adopt: %+v", results)
	}
	if results[0].WireID != string(f.ID) {
		t.Fatalf("results = %+v, want server-ID folder", results)
	}
}

// Absent selected mounts are never recreated on root.
func TestReconcileSkipsUnavailableDisk(t *testing.T) {
	root := t.TempDir()
	fc := &fakeClient{}
	svc, cleanup := reconcileTestService(t, fc, root)
	defer cleanup()
	dir := filepath.Join(root, "obsidian")
	mustReconcileFolder(t, svc, "obsidian", dir, true)
	results, err := svc.Reconcile(context.Background(), ReconcileInput{Unavailable: []string{dir}})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Status != "pending" || !strings.Contains(results[0].Error, "disk") {
		t.Fatalf("results = %+v, want disk-pending", results)
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if len(fc.ensureFolders) != 0 {
		t.Fatal("must not touch Syncthing config for absent mounts")
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("must not recreate folders on an absent mount")
	}
}

// Maintenance pauses reconciliation wholesale.
func TestReconcilePaused(t *testing.T) {
	root := t.TempDir()
	fc := &fakeClient{}
	svc, cleanup := reconcileTestService(t, fc, root)
	defer cleanup()
	mustReconcileFolder(t, svc, "obsidian", filepath.Join(root, "obsidian"), true)
	results, err := svc.Reconcile(context.Background(), ReconcileInput{Paused: true})
	if err != nil || len(results) != 0 {
		t.Fatalf("results = %+v err = %v, want skip", results, err)
	}
}

// Companion link + revoke narrows to that device's managed sharing.
func TestCompanionLinkAndRevoke(t *testing.T) {
	root := t.TempDir()
	fc := &fakeClient{}
	svc, cleanup := reconcileTestService(t, fc, root)
	defer cleanup()
	ctx := context.Background()
	f := mustReconcileFolder(t, svc, "drops", filepath.Join(root, "drops"), false)
	if _, err := svc.EnrollDevice(ctx, string(f.ID), "DEV-A", "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.EnrollDevice(ctx, string(f.ID), "DEV-B", "b"); err != nil {
		t.Fatal(err)
	}
	if err := svc.LinkCompanionDevice(ctx, string(f.ID), "DEV-A", "comp-1"); err != nil {
		t.Fatal(err)
	}
	folders, err := svc.FoldersForCompanion(ctx, "comp-1")
	if err != nil || len(folders) != 1 {
		t.Fatalf("folders = %+v err = %v", folders, err)
	}
	affected, err := svc.RemoveCompanionDevice(ctx, "comp-1")
	if err != nil || len(affected) != 1 {
		t.Fatalf("affected = %v err = %v", affected, err)
	}
	remaining, err := svc.ListDevices(ctx, string(f.ID))
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].DeviceID != "DEV-B" {
		t.Fatalf("remaining = %+v, want only DEV-B", remaining)
	}
	// Reconcile now shares only with DEV-B.
	if _, err := svc.Reconcile(ctx, ReconcileInput{}); err != nil {
		t.Fatal(err)
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	last := fc.ensureFolders[len(fc.ensureFolders)-1]
	if len(last.DeviceIDs) != 1 || last.DeviceIDs[0] != "DEV-B" {
		t.Fatalf("sharing = %+v, want only DEV-B", last.DeviceIDs)
	}
	if len(fc.removedDevices) != 0 {
		t.Fatalf("global device entries must stay; only folder sharing changes: %v", fc.removedDevices)
	}
}
