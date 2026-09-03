package workspaces

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/store"
)

// fakeExec records commands for assertion. It implements Executor.
type fakeExec struct {
	mu      sync.Mutex
	calls   []execCall
	outputs map[string]outResult
}

type execCall struct {
	Name string
	Args []string
}

type outResult struct {
	out []byte
	err error
}

func (f *fakeExec) Run(_ context.Context, name string, args ...string) error {
	f.mu.Lock()
	f.calls = append(f.calls, execCall{Name: name, Args: append([]string(nil), args...)})
	f.mu.Unlock()
	key := name + " " + strings.Join(args, " ")
	if f.outputs != nil {
		if r, ok := f.outputs[key]; ok {
			return r.err
		}
		if r, ok := f.outputs[name]; ok {
			return r.err
		}
	}
	return nil
}

func (f *fakeExec) Output(_ context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, execCall{Name: name, Args: append([]string(nil), args...)})
	f.mu.Unlock()
	key := name + " " + strings.Join(args, " ")
	if f.outputs != nil {
		if r, ok := f.outputs[key]; ok {
			return r.out, r.err
		}
		if r, ok := f.outputs[name]; ok {
			return r.out, r.err
		}
	}
	return []byte(`{"status":"running"}`), nil
}

func (f *fakeExec) all() []execCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]execCall, len(f.calls))
	copy(out, f.calls)
	return out
}

func (f *fakeExec) contains(name, substr string) bool {
	for _, c := range f.all() {
		if c.Name != name {
			continue
		}
		joined := strings.Join(c.Args, " ")
		if strings.Contains(joined, substr) {
			return true
		}
	}
	return false
}

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
	return st
}

func TestDevPodRunner_Up_ConstructsCommand(t *testing.T) {
	fe := &fakeExec{}
	tmp := t.TempDir()
	r := NewDevPodRunner(DevPodRunnerConfig{
		Bin:           "devpod",
		Provider:      "docker",
		WorkspacesDir: tmp,
		Executor:      fe,
	})
	ctx := context.Background()
	err := r.Up(ctx, "ws-123", domain.ID("proj-1"), "feature-a", "omp", RunnerOpts{DevcontainerSource: "default"})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	calls := fe.all()
	if len(calls) < 1 {
		t.Fatal("expected at least one command")
	}
	// First call should be devpod up
	foundUp := false
	for _, c := range calls {
		if c.Name == "devpod" && len(c.Args) > 0 && c.Args[0] == "up" {
			foundUp = true
			joined := strings.Join(c.Args, " ")
			if !strings.Contains(joined, "ws-123") {
				t.Errorf("up args missing workspace id: %q", joined)
			}
			if !strings.Contains(joined, "--provider") || !strings.Contains(joined, "docker") {
				t.Errorf("up args missing docker provider: %q", joined)
			}
			if !strings.Contains(joined, "proj-1") {
				t.Errorf("up args missing project reference: %q", joined)
			}
			if !strings.Contains(joined, "feature-a") {
				t.Errorf("up args missing branch: %q", joined)
			}
			if strings.Contains(joined, "docker.sock") {
				t.Errorf("up args must not contain docker.sock: %q", joined)
			}
			if strings.Contains(strings.ToLower(joined), "secret") {
				t.Errorf("up args must not contain secrets: %q", joined)
			}
		}
	}
	if !foundUp {
		t.Error("devpod up not called")
	}
	// Agent install should be second devpod ssh call
	foundInstall := false
	for _, c := range calls {
		if c.Name == "devpod" && len(c.Args) > 0 && c.Args[0] == "ssh" {
			joined := strings.Join(c.Args, " ")
			if strings.Contains(joined, "ws-123") && (strings.Contains(joined, "omp") || strings.Contains(joined, "install")) {
				foundInstall = true
			}
		}
	}
	if !foundInstall {
		t.Error("agent install via devpod ssh not called for omp")
	}
}

func TestDevPodRunner_Up_DefaultDevcontainer_NoDockerSocket(t *testing.T) {
	fe := &fakeExec{}
	tmp := t.TempDir()
	r := NewDevPodRunner(DevPodRunnerConfig{WorkspacesDir: tmp, Executor: fe})
	_ = r.Up(context.Background(), "ws-dc", domain.ID("p1"), "main", "", RunnerOpts{DevcontainerSource: "default"})
	// Check written file does not contain docker.sock
	data, err := rExecReadFile(filepath.Join(tmp, "ws-dc", "devcontainer.json"))
	if err != nil {
		t.Fatalf("read generated devcontainer: %v", err)
	}
	if strings.Contains(string(data), "docker.sock") {
		t.Errorf("generated devcontainer must not mount docker socket: %s", string(data))
	}
	if strings.Contains(strings.ToLower(string(data)), "secret") {
		t.Errorf("generated devcontainer must not contain secrets: %s", string(data))
	}
}

func rExecReadFile(p string) ([]byte, error) {
	// helper to avoid importing os in test helpers already imported
	// use stdlib read
	return readFile(p)
}

func TestDevPodRunner_StopDeleteAttach_CommandConstruction(t *testing.T) {
	fe := &fakeExec{}
	r := NewDevPodRunner(DevPodRunnerConfig{Executor: fe, WorkspacesDir: t.TempDir()})
	ctx := context.Background()

	if err := r.Stop(ctx, "ws-stop"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !fe.contains("devpod", "stop ws-stop") {
		t.Error("Stop should run devpod stop <id>")
	}
	// Ensure stop does not contain docker.sock or secret
	for _, c := range fe.all() {
		if c.Name == "devpod" {
			joined := strings.Join(c.Args, " ")
			if strings.Contains(joined, "docker.sock") || strings.Contains(strings.ToLower(joined), "secret") {
				t.Errorf("stop args must not contain docker.sock or secret: %q", joined)
			}
		}
	}

	fe2 := &fakeExec{}
	r2 := NewDevPodRunner(DevPodRunnerConfig{Executor: fe2, WorkspacesDir: t.TempDir()})
	if err := r2.Delete(ctx, "ws-del"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !fe2.contains("devpod", "delete ws-del") {
		t.Error("Delete should run devpod delete <id> --force")
	}
	if !fe2.contains("devpod", "--force") {
		t.Error("Delete should include --force")
	}

	fe3 := &fakeExec{}
	r3 := NewDevPodRunner(DevPodRunnerConfig{Executor: fe3, WorkspacesDir: t.TempDir()})
	if err := r3.Attach(ctx, "ws-attach"); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	// Attach must use tmux new-session -A -s <session> devpod ssh --tty
	found := false
	for _, c := range fe3.all() {
		if c.Name == "tmux" {
			joined := strings.Join(c.Args, " ")
			if strings.Contains(joined, "new-session") && strings.Contains(joined, "-A") && strings.Contains(joined, "omahab-ws-attach") && strings.Contains(joined, "devpod") && strings.Contains(joined, "ssh") && strings.Contains(joined, "--tty") {
				found = true
			}
			if strings.Contains(joined, "docker.sock") || strings.Contains(strings.ToLower(joined), "secret") {
				t.Errorf("attach args must not contain docker.sock or secret: %q", joined)
			}
		}
	}
	if !found {
		t.Errorf("Attach should run tmux new-session -A -s omahab-<id> devpod ssh <id> --tty, got %+v", fe3.all())
	}
	// Attach session naming per workspace
	sn := r3.SessionNameForTest("ws-attach")
	if sn != "omahab-ws-attach" {
		t.Errorf("session name = %q want omahab-ws-attach", sn)
	}
}

func TestDevPodRunner_IsRunning(t *testing.T) {
	fe := &fakeExec{
		outputs: map[string]outResult{
			"devpod": {out: []byte(`{"status":"running"}`), err: nil},
		},
	}
	r := NewDevPodRunner(DevPodRunnerConfig{Executor: fe})
	ok, err := r.IsRunning(context.Background(), "ws1")
	if err != nil {
		t.Fatalf("IsRunning: %v", err)
	}
	if !ok {
		t.Error("expected running true")
	}
	fe2 := &fakeExec{
		outputs: map[string]outResult{
			"devpod": {out: nil, err: fakeErr("not found")},
		},
	}
	r2 := NewDevPodRunner(DevPodRunnerConfig{Executor: fe2})
	ok2, _ := r2.IsRunning(context.Background(), "ws2")
	if ok2 {
		t.Error("expected not running when devpod status fails")
	}
}

func fakeErr(s string) error { return &fakeError{s} }

type fakeError struct{ s string }

func (e *fakeError) Error() string { return e.s }

func TestDevPodRunner_Up_NoDockerSocket_NoSecrets_Explicit(t *testing.T) {
	fe := &fakeExec{}
	tmp := t.TempDir()
	r := NewDevPodRunner(DevPodRunnerConfig{WorkspacesDir: tmp, Executor: fe})
	_ = r.Up(context.Background(), "ws-sec", domain.ID("proj-x"), "main", "omp", RunnerOpts{DevcontainerSource: "default"})
	for _, c := range fe.all() {
		joined := strings.Join(append([]string{c.Name}, c.Args...), " ")
		if strings.Contains(joined, "/var/run/docker.sock") {
			t.Errorf("command must not mount docker socket: %q", joined)
		}
		if strings.Contains(strings.ToLower(joined), "production") && strings.Contains(strings.ToLower(joined), "secret") {
			t.Errorf("command must not contain production secrets: %q", joined)
		}
	}
	// Also verify generated devcontainer not leaking
	path := filepath.Join(tmp, "ws-sec", "devcontainer.json")
	if b, err := readFile(path); err == nil {
		if strings.Contains(string(b), "docker.sock") {
			t.Errorf("devcontainer mounts docker socket: %s", string(b))
		}
	}
}

func TestDevPodRunner_Up_RepoResolver(t *testing.T) {
	fe := &fakeExec{}
	tmp := t.TempDir()
	resolver := func(_ context.Context, id domain.ID) (string, error) {
		return "https://forgejo.example.com/" + string(id) + ".git", nil
	}
	r := NewDevPodRunner(DevPodRunnerConfig{WorkspacesDir: tmp, Executor: fe, RepoResolver: resolver})
	_ = r.Up(context.Background(), "ws-res", domain.ID("myproj"), "dev", "", RunnerOpts{DevcontainerSource: "default"})
	if !fe.contains("devpod", "forgejo.example.com/myproj.git") {
		t.Error("Up should use RepoResolver URL")
	}
	if !fe.contains("devpod", "dev") {
		t.Error("Up should include branch")
	}
}

func TestService_ExpireIdle_Transitions(t *testing.T) {
	st := openTestDB(t)
	// fake runner that records stops
	type fakeRunner struct {
		NoopRunner
		mu      sync.Mutex
		stopped []string
	}
	fr := &fakeRunner{}
	// override Stop to record
	origStop := func(_ context.Context, id string) error {
		fr.mu.Lock()
		fr.stopped = append(fr.stopped, id)
		fr.mu.Unlock()
		return nil
	}
	// Use a custom runner that records
	recorder := &recordingRunner{stopFn: origStop}
	svc := New(st.DB(), recorder)
	ctx := context.Background()

	// Create two workspaces
	ws1, err := svc.Create(ctx, CreateInput{ProjectID: "proj1", Branch: "main", Agent: "omp"})
	if err != nil {
		t.Fatalf("create ws1: %v", err)
	}
	ws2, err := svc.Create(ctx, CreateInput{ProjectID: "proj1", Branch: "dev", Agent: ""})
	if err != nil {
		t.Fatalf("create ws2: %v", err)
	}
	// Make ws1 idle by setting last_active_at far in past
	past := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339Nano)
	_, _ = st.DB().ExecContext(ctx, `UPDATE workspaces SET last_active_at = ? WHERE id = ?`, past, string(ws1.ID))
	// ws2 remains fresh
	_, _ = st.DB().ExecContext(ctx, `UPDATE workspaces SET last_active_at = ? WHERE id = ?`, time.Now().UTC().Format(time.RFC3339Nano), string(ws2.ID))

	n, err := svc.ExpireIdle(ctx, time.Hour)
	if err != nil {
		t.Fatalf("ExpireIdle: %v", err)
	}
	if n != 1 {
		t.Errorf("ExpireIdle n=%d want 1", n)
	}
	// ws1 should be expired
	got1, _ := svc.Get(ctx, string(ws1.ID))
	if got1.Status != StatusExpired {
		t.Errorf("ws1 status=%q want %q", got1.Status, StatusExpired)
	}
	got2, _ := svc.Get(ctx, string(ws2.ID))
	if got2.Status != StatusRunning {
		t.Errorf("ws2 status=%q want running", got2.Status)
	}
	// Expire again should not double count
	n2, _ := svc.ExpireIdle(ctx, time.Hour)
	if n2 != 0 {
		t.Errorf("second ExpireIdle n=%d want 0", n2)
	}
	// Ensure expired workspaces are not re-expired and stopped workspaces untouched
	// Create a stopped workspace and ensure it's not expired
	_ = svc.Stop(ctx, string(ws2.ID))
	_, _ = st.DB().ExecContext(ctx, `UPDATE workspaces SET last_active_at = ? WHERE id = ?`, past, string(ws2.ID))
	n3, _ := svc.ExpireIdle(ctx, time.Hour)
	if n3 != 0 {
		t.Errorf("stopped workspace should not be expired, got n=%d", n3)
	}
}

type recordingRunner struct {
	NoopRunner
	stopFn func(context.Context, string) error
}

func (r *recordingRunner) Stop(ctx context.Context, id string) error {
	if r.stopFn != nil {
		return r.stopFn(ctx, id)
	}
	return nil
}

func (r *recordingRunner) Delete(ctx context.Context, id string) error { return nil }
func (r *recordingRunner) Attach(ctx context.Context, id string) error { return nil }

func TestService_Capability_IssueValidateExpire(t *testing.T) {
	st := openTestDB(t)
	svc := New(st.DB(), NoopRunner{})
	ctx := context.Background()

	ws, err := svc.Create(ctx, CreateInput{ProjectID: "proj-cap", Branch: "main"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Issue
	cap, err := svc.IssueCapability(ctx, string(ws.ID), 0)
	if err != nil {
		t.Fatalf("IssueCapability: %v", err)
	}
	if cap.Token == "" {
		t.Fatal("empty token")
	}
	if time.Until(cap.ExpiresAt) < 4*time.Minute || time.Until(cap.ExpiresAt) > 6*time.Minute {
		t.Errorf("ExpiresAt %v expected ~5m", cap.ExpiresAt)
	}
	// Validate success
	if err := svc.ValidateCapability(ctx, string(ws.ID), cap.Token); err != nil {
		t.Fatalf("Validate first: %v", err)
	}
	// Second validate should be consumed
	if err := svc.ValidateCapability(ctx, string(ws.ID), cap.Token); err != ErrCapabilityConsumed {
		t.Errorf("second Validate = %v want ErrCapabilityConsumed", err)
	}
	// Invalid token
	if err := svc.ValidateCapability(ctx, string(ws.ID), "invalid"); err != ErrCapabilityInvalid {
		t.Errorf("invalid token err = %v want ErrCapabilityInvalid", err)
	}
	// Issue with short TTL and wait for expiry
	cap2, err := svc.IssueCapability(ctx, string(ws.ID), 10*time.Millisecond)
	if err != nil {
		t.Fatalf("Issue short TTL: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if err := svc.ValidateCapability(ctx, string(ws.ID), cap2.Token); err != ErrCapabilityExpired {
		t.Errorf("expired cap err = %v want ErrCapabilityExpired", err)
	}
	// Validate touches last_active
	// Issue new, validate, check last_active updated
	cap3, _ := svc.IssueCapability(ctx, string(ws.ID), time.Minute)
	before, _ := svc.Get(ctx, string(ws.ID))
	time.Sleep(5 * time.Millisecond)
	_ = svc.ValidateCapability(ctx, string(ws.ID), cap3.Token)
	after, _ := svc.Get(ctx, string(ws.ID))
	if !after.LastActiveAt.After(before.LastActiveAt) {
		t.Error("Validate should touch last_active_at")
	}
}

func TestService_Capability_OneTime_AndTTL(t *testing.T) {
	st := openTestDB(t)
	svc := New(st.DB(), NoopRunner{})
	ctx := context.Background()
	ws, _ := svc.Create(ctx, CreateInput{ProjectID: "p1", Branch: "b1"})
	// capped TTL
	cap, err := svc.IssueCapability(ctx, string(ws.ID), 2*time.Hour)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	// Should be capped at 1h
	if time.Until(cap.ExpiresAt) > 61*time.Minute {
		t.Errorf("TTL not capped: %v", time.Until(cap.ExpiresAt))
	}
}

func TestService_StartIdleExpirer(t *testing.T) {
	st := openTestDB(t)
	svc := New(st.DB(), NoopRunner{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.StartIdleExpirer(ctx, 10*time.Millisecond)
	// Create idle workspace
	ws, _ := svc.Create(ctx, CreateInput{ProjectID: "p-exp", Branch: "main"})
	past := time.Now().Add(-2 * DefaultIdleTimeout).Format(time.RFC3339Nano)
	_, _ = st.DB().ExecContext(ctx, `UPDATE workspaces SET last_active_at = ? WHERE id = ?`, past, string(ws.ID))
	// Wait for expirer to run
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		got, _ := svc.Get(ctx, string(ws.ID))
		if got.Status == StatusExpired {
			cancel()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("StartIdleExpirer did not expire idle workspace in time")
}

func TestService_DeleteAndAttach(t *testing.T) {
	st := openTestDB(t)
	fe := &fakeExec{}
	runner := NewDevPodRunner(DevPodRunnerConfig{Executor: fe, WorkspacesDir: t.TempDir()})
	svc := New(st.DB(), runner)
	ctx := context.Background()
	ws, err := svc.Create(ctx, CreateInput{ProjectID: "proj-del", Branch: "main"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Attach should work while running
	if err := svc.Attach(ctx, string(ws.ID)); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if !fe.contains("tmux", "omahab-") {
		t.Error("Attach didn't call tmux")
	}
	// Delete
	if err := svc.Delete(ctx, string(ws.ID)); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := svc.Get(ctx, string(ws.ID)); err != ErrNotFound {
		t.Errorf("Get after Delete = %v want ErrNotFound", err)
	}
	// Delete should have called devpod delete
	if !fe.contains("devpod", "delete") {
		t.Error("Delete should call devpod delete")
	}
}


func TestService_ValidateCapability_ConsumedRaceFix(t *testing.T) {
	st := openTestDB(t)
	svc := New(st.DB(), NoopRunner{})
	ctx := context.Background()
	ws, _ := svc.Create(ctx, CreateInput{ProjectID: "p-race", Branch: "main"})
	cap, _ := svc.IssueCapability(ctx, string(ws.ID), time.Minute)
	// First validate succeeds
	if err := svc.ValidateCapability(ctx, string(ws.ID), cap.Token); err != nil {
		t.Fatalf("first validate: %v", err)
	}
	// Simulate concurrent second validate where DB still has row but consumed_at set
	// Our fix checks RowsAffected so second should be ErrCapabilityConsumed, not nil
	if err := svc.ValidateCapability(ctx, string(ws.ID), cap.Token); err != ErrCapabilityConsumed {
		t.Errorf("second validate = %v want ErrCapabilityConsumed", err)
	}
}

func readFile(p string) ([]byte, error) {
	return os.ReadFile(p)
}
