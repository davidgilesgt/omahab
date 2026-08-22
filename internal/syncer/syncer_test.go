package syncer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/knowledge"
	"github.com/omahab/omahab/internal/store"
)

func openTestDB(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(context.Background(), Migrations()...); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// also need knowledge migrations for bridge tests
	if err := st.Migrate(context.Background(), knowledge.Migrations()...); err != nil {
		t.Fatalf("migrate knowledge: %v", err)
	}
	return st
}

type fakeRegistrar struct {
	mu          sync.Mutex
	registers   []struct{ ID, Path string }
	unregisters []string
	err         error
}

func (f *fakeRegistrar) Register(_ context.Context, id, path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.registers = append(f.registers, struct{ ID, Path string }{id, path})
	return f.err
}
func (f *fakeRegistrar) Unregister(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unregisters = append(f.unregisters, id)
	return f.err
}
func (f *fakeRegistrar) countRegister() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.registers)
}
func (f *fakeRegistrar) countUnregister() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.unregisters)
}

type fakeClient struct {
	mu           sync.Mutex
	folderErrors map[string]string
	folderErr    error
	conns        map[string]ConnectionInfo
	connErr      error
	calls        []string
}

func (f *fakeClient) FolderErrors(_ context.Context, folder string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, folder)
	if f.folderErr != nil {
		return "", f.folderErr
	}
	if s, ok := f.folderErrors[folder]; ok {
		return s, nil
	}
	return "", nil
}
func (f *fakeClient) Connections(_ context.Context) (map[string]ConnectionInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.connErr != nil {
		return nil, f.connErr
	}
	out := make(map[string]ConnectionInfo, len(f.conns))
	for k, v := range f.conns {
		out[k] = v
	}
	return out, nil
}

type fakeSink struct {
	mu     sync.Mutex
	events []domain.Event
}

func (f *fakeSink) Emit(_ context.Context, ev domain.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, ev)
	return nil
}
func (f *fakeSink) types() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.events))
	for i, e := range f.events {
		out[i] = e.Type
	}
	return out
}
func (f *fakeSink) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.events)
}
func (f *fakeSink) find(typ string) []domain.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []domain.Event
	for _, e := range f.events {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

type fakeKnowledgeService struct {
	mu       sync.Mutex
	sources  []*knowledge.Source
	register func(kind, name, baseURL string) (*knowledge.Source, error)
}

func (f *fakeKnowledgeService) RegisterSource(_ context.Context, kind, name, baseURL string) (*knowledge.Source, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.register != nil {
		return f.register(kind, name, baseURL)
	}
	src := &knowledge.Source{ID: "ks_" + name, Kind: kind, Name: name, BaseURL: baseURL}
	f.sources = append(f.sources, src)
	return src, nil
}
func (f *fakeKnowledgeService) DeleteSource(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, s := range f.sources {
		if s.ID == id {
			f.sources = append(f.sources[:i], f.sources[i+1:]...)
			return nil
		}
	}
	return knowledgeNotFound(id)
}
func (f *fakeKnowledgeService) ListSources(_ context.Context) ([]*knowledge.Source, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*knowledge.Source, len(f.sources))
	copy(out, f.sources)
	return out, nil
}
func knowledgeNotFound(id string) error { return &fakeKnowErr{msg: "source " + id + " not found"} }

type fakeKnowErr struct{ msg string }

func (e *fakeKnowErr) Error() string { return e.msg }

func TestCreateShareWithAIRegistration(t *testing.T) {
	st := openTestDB(t)
	db := st.DB()
	root := filepath.Join(t.TempDir(), "sync")
	fr := &fakeRegistrar{}
	svc := New(db, root, fr)
	f, err := svc.Create(context.Background(), CreateInput{Name: "Notes", ServerPath: filepath.Join(root, "notes"), ShareWithAI: true})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if fr.countRegister() != 1 {
		t.Fatalf("expected 1 register, got %d", fr.countRegister())
	}
	fr.mu.Lock()
	gotID := fr.registers[0].ID
	fr.mu.Unlock()
	if gotID != string(f.ID) {
		t.Fatalf("register ID %q != folder ID %q", gotID, f.ID)
	}
	// Without share should not register
	fr2 := &fakeRegistrar{}
	svc2 := New(db, root, fr2)
	if _, err := svc2.Create(context.Background(), CreateInput{Name: "Other", ServerPath: filepath.Join(root, "other"), ShareWithAI: false}); err != nil {
		t.Fatalf("Create other: %v", err)
	}
	if fr2.countRegister() != 0 {
		t.Fatalf("expected 0 register, got %d", fr2.countRegister())
	}
}

func TestUpdateTogglesRegistration(t *testing.T) {
	st := openTestDB(t)
	db := st.DB()
	root := filepath.Join(t.TempDir(), "sync")
	fr := &fakeRegistrar{}
	svc := New(db, root, fr)
	f, err := svc.Create(context.Background(), CreateInput{Name: "Notes", ServerPath: filepath.Join(root, "notes"), ShareWithAI: false})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if fr.countRegister() != 0 {
		t.Fatalf("initial register count %d", fr.countRegister())
	}
	enable := true
	if _, err := svc.Update(context.Background(), string(f.ID), UpdateInput{ShareWithAI: &enable}); err != nil {
		t.Fatalf("Update enable: %v", err)
	}
	if fr.countRegister() != 1 {
		t.Fatalf("expected register on enable, got %d", fr.countRegister())
	}
	disable := false
	if _, err := svc.Update(context.Background(), string(f.ID), UpdateInput{ShareWithAI: &disable}); err != nil {
		t.Fatalf("Update disable: %v", err)
	}
	if fr.countUnregister() != 1 {
		t.Fatalf("expected unregister on disable, got %d", fr.countUnregister())
	}
	// No transition should not call again
	if _, err := svc.Update(context.Background(), string(f.ID), UpdateInput{ShareWithAI: &disable}); err != nil {
		t.Fatalf("Update no change: %v", err)
	}
	if fr.countRegister() != 1 || fr.countUnregister() != 1 {
		t.Fatalf("unexpected extra calls register %d unregister %d", fr.countRegister(), fr.countUnregister())
	}
}

func TestDeleteUnregistersWhenShared(t *testing.T) {
	st := openTestDB(t)
	db := st.DB()
	root := filepath.Join(t.TempDir(), "sync")
	fr := &fakeRegistrar{}
	svc := New(db, root, fr)
	f, _ := svc.Create(context.Background(), CreateInput{Name: "Notes", ServerPath: filepath.Join(root, "notes"), ShareWithAI: true})
	initial := fr.countRegister()
	if initial != 1 {
		t.Fatalf("register %d", initial)
	}
	if err := svc.Delete(context.Background(), string(f.ID)); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if fr.countUnregister() != 1 {
		t.Fatalf("expected unregister on delete, got %d", fr.countUnregister())
	}
}

func TestDeleteNoUnregisterWhenNotShared(t *testing.T) {
	st := openTestDB(t)
	db := st.DB()
	root := filepath.Join(t.TempDir(), "sync")
	fr := &fakeRegistrar{}
	svc := New(db, root, fr)
	f, _ := svc.Create(context.Background(), CreateInput{Name: "Notes", ServerPath: filepath.Join(root, "notes"), ShareWithAI: false})
	if err := svc.Delete(context.Background(), string(f.ID)); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if fr.countUnregister() != 0 {
		t.Fatalf("unexpected unregister %d", fr.countUnregister())
	}
}

func TestCheckConflictsDetectsFiles(t *testing.T) {
	st := openTestDB(t)
	db := st.DB()
	root := t.TempDir()
	svc := New(db, filepath.Join(root, "sync"), nil)
	sink := &fakeSink{}
	svc.SetEventSink(sink)
	svc.SetSyncthingClient(&fakeClient{folderErrors: map[string]string{}})
	dir := filepath.Join(root, "sync", "notes")
	_ = ensureDir(dir)
	f, err := svc.Create(context.Background(), CreateInput{Name: "Notes", ServerPath: dir, ShareWithAI: false})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	_ = f
	if err := svc.CheckConflicts(context.Background()); err != nil {
		t.Fatalf("CheckConflicts: %v", err)
	}
	if sink.count() != 0 {
		t.Fatalf("expected 0 events, got %d", sink.count())
	}
	conflictPath := filepath.Join(dir, "doc.sync-conflict-20240101.txt")
	if err := writeFile(conflictPath, "conflict"); err != nil {
		t.Fatalf("write conflict: %v", err)
	}
	sink2 := &fakeSink{}
	svc.SetEventSink(sink2)
	if err := svc.CheckConflicts(context.Background()); err != nil {
		t.Fatalf("CheckConflicts: %v", err)
	}
	if sink2.count() != 1 {
		t.Fatalf("expected 1 conflict event, got %d", sink2.count())
	}
	ev := sink2.events[0]
	if ev.Type != "syncthing.conflict" {
		t.Fatalf("type %q", ev.Type)
	}
	if ev.ResourceID != f.ID {
		t.Fatalf("resource %q != %q", ev.ResourceID, f.ID)
	}
	if !strings.Contains(strings.ToLower(ev.Data["details"].(string)), "conflict") {
		t.Fatalf("details missing conflict: %v", ev.Data)
	}
}

func TestCheckConflictsDetectsFolderErrorsViaClient(t *testing.T) {
	st := openTestDB(t)
	db := st.DB()
	root := t.TempDir()
	svc := New(db, filepath.Join(root, "sync"), nil)
	sink := &fakeSink{}
	svc.SetEventSink(sink)
	dir := filepath.Join(root, "sync", "proj")
	_ = ensureDir(dir)
	f, err := svc.Create(context.Background(), CreateInput{Name: "Proj", ServerPath: dir, ShareWithAI: false})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Client returns folder error for this folder ID
	fc := &fakeClient{folderErrors: map[string]string{string(f.ID): "folder marker missing", f.Name: "folder marker missing"}}
	svc.SetSyncthingClient(fc)
	if err := svc.CheckConflicts(context.Background()); err != nil {
		t.Fatalf("CheckConflicts: %v", err)
	}
	if sink.count() != 1 {
		t.Fatalf("expected 1 event, got %d", sink.count())
	}
	if sink.events[0].Type != "syncthing.conflict" {
		t.Fatalf("type %q", sink.events[0].Type)
	}
	// Healthy client -> no event
	sink2 := &fakeSink{}
	svc.SetEventSink(sink2)
	svc.SetSyncthingClient(&fakeClient{folderErrors: map[string]string{}})
	if err := svc.CheckConflicts(context.Background()); err != nil {
		t.Fatalf("CheckConflicts: %v", err)
	}
	if sink2.count() != 0 {
		t.Fatalf("expected 0 events for healthy folder, got %d", sink2.count())
	}
}

func TestCheckConflictsMultipleFolders(t *testing.T) {
	st := openTestDB(t)
	db := st.DB()
	root := t.TempDir()
	svc := New(db, filepath.Join(root, "sync"), nil)
	sink := &fakeSink{}
	svc.SetEventSink(sink)
	dirA := filepath.Join(root, "sync", "a")
	dirB := filepath.Join(root, "sync", "b")
	_ = ensureDir(dirA)
	_ = ensureDir(dirB)
	if _, err := svc.Create(context.Background(), CreateInput{Name: "A", ServerPath: dirA}); err != nil {
		t.Fatalf("create A: %v", err)
	}
	fB, _ := svc.Create(context.Background(), CreateInput{Name: "B", ServerPath: dirB})
	fc := &fakeClient{folderErrors: map[string]string{string(fB.ID): "error"}}
	svc.SetSyncthingClient(fc)
	if err := svc.CheckConflicts(context.Background()); err != nil {
		t.Fatalf("CheckConflicts: %v", err)
	}
	if sink.count() != 1 {
		t.Fatalf("expected 1 event, got %d", sink.count())
	}
	if sink.events[0].ResourceID != fB.ID {
		t.Fatalf("wrong folder %q", sink.events[0].ResourceID)
	}
}

func TestCheckDeviceStalenessEmitsWhenStale(t *testing.T) {
	st := openTestDB(t)
	db := st.DB()
	root := t.TempDir()
	svc := New(db, filepath.Join(root, "sync"), nil)
	dir := filepath.Join(root, "sync", "notes")
	_ = ensureDir(dir)
	f, _ := svc.Create(context.Background(), CreateInput{Name: "Notes", ServerPath: dir})
	if _, err := svc.EnrollDevice(context.Background(), string(f.ID), "DEVICE123", "phone"); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	fixedNow := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	svc.SetNow(func() time.Time { return fixedNow })
	svc.SetStaleThreshold(DeviceStaleThreshold)
	sink := &fakeSink{}
	svc.SetEventSink(sink)
	staleTime := fixedNow.Add(-25 * time.Hour)
	svc.SetSyncthingClient(&fakeClient{conns: map[string]ConnectionInfo{"DEVICE123": {Connected: false, LastSeen: staleTime}}})
	if err := svc.CheckDeviceStaleness(context.Background()); err != nil {
		t.Fatalf("CheckDeviceStaleness: %v", err)
	}
	if sink.count() != 1 {
		t.Fatalf("expected 1 stale event, got %d", sink.count())
	}
	ev := sink.events[0]
	if ev.Type != "syncthing.device_stale" {
		t.Fatalf("type %q", ev.Type)
	}
	if string(ev.ResourceID) != "DEVICE123" {
		t.Fatalf("resource %q", ev.ResourceID)
	}
	if ev.Data["device_id"] != "DEVICE123" {
		t.Fatalf("data device_id %v", ev.Data["device_id"])
	}
}

func TestCheckDeviceStalenessNoEmitWhenRecent(t *testing.T) {
	st := openTestDB(t)
	db := st.DB()
	root := t.TempDir()
	svc := New(db, filepath.Join(root, "sync"), nil)
	dir := filepath.Join(root, "sync", "notes")
	_ = ensureDir(dir)
	f, _ := svc.Create(context.Background(), CreateInput{Name: "Notes", ServerPath: dir})
	svc.EnrollDevice(context.Background(), string(f.ID), "DEV1", "laptop")
	fixedNow := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	svc.SetNow(func() time.Time { return fixedNow })
	sink := &fakeSink{}
	svc.SetEventSink(sink)
	recent := fixedNow.Add(-1 * time.Hour)
	svc.SetSyncthingClient(&fakeClient{conns: map[string]ConnectionInfo{"DEV1": {Connected: false, LastSeen: recent}}})
	if err := svc.CheckDeviceStaleness(context.Background()); err != nil {
		t.Fatalf("CheckDeviceStaleness: %v", err)
	}
	if sink.count() != 0 {
		t.Fatalf("expected 0 events for recent, got %d", sink.count())
	}
}

func TestCheckDeviceStalenessNoEmitWhenConnected(t *testing.T) {
	st := openTestDB(t)
	db := st.DB()
	root := t.TempDir()
	svc := New(db, filepath.Join(root, "sync"), nil)
	dir := filepath.Join(root, "sync", "notes")
	_ = ensureDir(dir)
	f, _ := svc.Create(context.Background(), CreateInput{Name: "Notes", ServerPath: dir})
	svc.EnrollDevice(context.Background(), string(f.ID), "DEV2", "tablet")
	fixedNow := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	svc.SetNow(func() time.Time { return fixedNow })
	sink := &fakeSink{}
	svc.SetEventSink(sink)
	stale := fixedNow.Add(-100 * time.Hour)
	svc.SetSyncthingClient(&fakeClient{conns: map[string]ConnectionInfo{"DEV2": {Connected: true, LastSeen: stale}}})
	if err := svc.CheckDeviceStaleness(context.Background()); err != nil {
		t.Fatalf("CheckDeviceStaleness: %v", err)
	}
	if sink.count() != 0 {
		t.Fatalf("expected 0 for connected, got %d", sink.count())
	}
}

func TestCheckDeviceStalenessZeroLastSeenIsStale(t *testing.T) {
	st := openTestDB(t)
	db := st.DB()
	root := t.TempDir()
	svc := New(db, filepath.Join(root, "sync"), nil)
	dir := filepath.Join(root, "sync", "notes")
	_ = ensureDir(dir)
	f, _ := svc.Create(context.Background(), CreateInput{Name: "Notes", ServerPath: dir})
	svc.EnrollDevice(context.Background(), string(f.ID), "DEV3", "phone")
	svc.SetNow(func() time.Time { return time.Now().UTC() })
	sink := &fakeSink{}
	svc.SetEventSink(sink)
	svc.SetSyncthingClient(&fakeClient{conns: map[string]ConnectionInfo{"DEV3": {Connected: false, LastSeen: time.Time{}}}})
	if err := svc.CheckDeviceStaleness(context.Background()); err != nil {
		t.Fatalf("CheckDeviceStaleness: %v", err)
	}
	if sink.count() != 1 {
		t.Fatalf("zero lastSeen should be stale, got %d", sink.count())
	}
}

func TestPollSyncthing(t *testing.T) {
	st := openTestDB(t)
	db := st.DB()
	root := t.TempDir()
	svc := New(db, filepath.Join(root, "sync"), nil)
	dir := filepath.Join(root, "sync", "notes")
	_ = ensureDir(dir)
	f, _ := svc.Create(context.Background(), CreateInput{Name: "Notes", ServerPath: dir})
	svc.EnrollDevice(context.Background(), string(f.ID), "D1", "phone")
	// Make folder have conflict via file
	_ = writeFile(filepath.Join(dir, "a.sync-conflict-123.txt"), "x")
	fixedNow := time.Now().UTC()
	svc.SetNow(func() time.Time { return fixedNow })
	sink := &fakeSink{}
	svc.SetEventSink(sink)
	svc.SetSyncthingClient(&fakeClient{
		conns: map[string]ConnectionInfo{"D1": {Connected: false, LastSeen: fixedNow.Add(-48 * time.Hour)}},
	})
	if err := svc.PollSyncthing(context.Background()); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if sink.count() != 2 {
		t.Fatalf("expected 2 events (conflict+stale), got %d types %v", sink.count(), sink.types())
	}
}

func TestKnowledgeRegistrarBridgeGatedOnShare(t *testing.T) {
	st := openTestDB(t)
	db := st.DB()
	// Test bridge directly
	fk := &fakeKnowledgeService{}
	bridge := NewKnowledgeRegistrar(fk)
	root := filepath.Join(t.TempDir(), "sync")
	svc := New(db, root, bridge)
	// Create without share -> no register
	if _, err := svc.Create(context.Background(), CreateInput{Name: "Notes", ServerPath: filepath.Join(root, "notes"), ShareWithAI: false}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	fk.mu.Lock()
	if len(fk.sources) != 0 {
		t.Fatalf("expected 0 knowledge sources, got %d", len(fk.sources))
	}
	fk.mu.Unlock()
	// Create with share -> register kind notes
	f, err := svc.Create(context.Background(), CreateInput{Name: "Shared", ServerPath: filepath.Join(root, "shared"), ShareWithAI: true})
	if err != nil {
		t.Fatalf("Create shared: %v", err)
	}
	fk.mu.Lock()
	if len(fk.sources) != 1 {
		t.Fatalf("expected 1 source, got %d", len(fk.sources))
	}
	src := fk.sources[0]
	fk.mu.Unlock()
	if src.Kind != "notes" {
		t.Fatalf("kind %q != notes", src.Kind)
	}
	if src.Name != string(f.ID) {
		t.Fatalf("name %q != folder ID %q", src.Name, f.ID)
	}
	if src.BaseURL != f.ServerPath {
		t.Fatalf("baseURL %q != %q", src.BaseURL, f.ServerPath)
	}
}

func TestKnowledgeRegistrarBridgeUnregister(t *testing.T) {
	fk := &fakeKnowledgeService{}
	bridge := NewKnowledgeRegistrar(fk)
	ctx := context.Background()
	// Register
	src, _ := fk.RegisterSource(ctx, "notes", "SRC123", "/srv/omahab/sync/notes")
	_ = src
	if err := bridge.Unregister(ctx, "SRC123"); err != nil {
		t.Fatalf("Unregister: %v", err)
	}
	fk.mu.Lock()
	if len(fk.sources) != 0 {
		t.Fatalf("expected 0 after unregister, got %d", len(fk.sources))
	}
	fk.mu.Unlock()
}

func TestKnowledgeRegistrarBridgeConflictIdempotent(t *testing.T) {
	fk := &fakeKnowledgeService{
		register: func(kind, name, baseURL string) (*knowledge.Source, error) {
			return nil, &fakeKnowErr{msg: "conflict: source already exists"}
		},
	}
	bridge := NewKnowledgeRegistrar(fk)
	if err := bridge.Register(context.Background(), "ID123", "/path"); err != nil {
		t.Fatalf("expected conflict treated as nil, got %v", err)
	}
}

func TestDeviceStaleThresholdConstant(t *testing.T) {
	if DeviceStaleThreshold == 0 {
		t.Fatal("DeviceStaleThreshold must be non-zero")
	}
	if DeviceStaleThreshold < time.Hour || DeviceStaleThreshold > 30*24*time.Hour {
		t.Fatalf("threshold %v out of sensible range", DeviceStaleThreshold)
	}
}

func TestHTTPClientFolderErrors(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/folder/errors", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("folder") == "F1" {
			_, _ = w.Write([]byte(`{"errors":[{"path":"a.txt","error":"marker missing"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"errors":[]}`))
	})
	// also need /rest/db/status fallback but not hit
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := NewHTTPClientWithHTTP(srv.URL, "key", srv.Client())
	s, err := c.FolderErrors(context.Background(), "F1")
	if err != nil {
		t.Fatalf("FolderErrors: %v", err)
	}
	if !strings.Contains(s, "marker") {
		t.Fatalf("expected marker error, got %q", s)
	}
	s2, err := c.FolderErrors(context.Background(), "OTHER")
	if err != nil {
		t.Fatalf("FolderErrors other: %v", err)
	}
	if s2 != "" {
		t.Fatalf("expected empty for healthy, got %q", s2)
	}
}

func TestHTTPClientConnections(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/stats/device", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"DEV1": map[string]any{"lastSeen": now.Format(time.RFC3339Nano)},
		})
	})
	mux.HandleFunc("/rest/system/connections", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"connections": map[string]any{
				"DEV1": map[string]any{"connected": true},
			},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := NewHTTPClientWithHTTP(srv.URL, "", srv.Client())
	conns, err := c.Connections(context.Background())
	if err != nil {
		t.Fatalf("Connections: %v", err)
	}
	ci, ok := conns["DEV1"]
	if !ok {
		t.Fatalf("missing DEV1")
	}
	if !ci.Connected {
		t.Fatalf("expected connected")
	}
	if !ci.LastSeen.Equal(now) {
		t.Fatalf("lastSeen %v != %v", ci.LastSeen, now)
	}
}

func ensureDir(p string) error {
	return os.MkdirAll(p, 0o755)
}

func writeFile(p, content string) error {
	if err := ensureDir(filepath.Dir(p)); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(content), 0o644)
}
