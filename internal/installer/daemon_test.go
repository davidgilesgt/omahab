package installer

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func newDaemonService(t *testing.T, probes Probes) *Service {
	t.Helper()
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := EnsureMigrations(ctx, db); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	return NewService(db, probes)
}

func TestDaemonHappyPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var sysCalls [][]string
	var order []string
	polls := 0
	token := "s3cr3t-tok-xyz-123"

	var wrotePath string
	var wroteData []byte
	var wrotePerm uint32

	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	nowCalls := 0
	nowFn := func() time.Time {
		v := start.Add(time.Duration(nowCalls*2) * time.Second)
		nowCalls++
		return v
	}

	probes := Probes{
		Systemctl: func(ctx context.Context, args ...string) (string, error) {
			cp := append([]string(nil), args...)
			sysCalls = append(sysCalls, cp)
			order = append(order, strings.Join(args, " "))
			return "", nil
		},
		HTTPSGet: func(ctx context.Context, url string) (int, []byte, error) {
			if url != DaemonHealthURL {
				t.Fatalf("unexpected url %q want %q", url, DaemonHealthURL)
			}
			order = append(order, "poll")
			polls++
			if polls == 1 {
				return 500, []byte("not ready"), nil
			}
			return 200, []byte(`{"status":"up"}`), nil
		},
		ReadFile: func(path string) ([]byte, error) {
			switch path {
			case "/var/lib/omahab/api.token":
				return []byte(token + "\n"), nil
			case "/etc/omahab/backup.env":
				return nil, errors.New("not found")
			default:
				return nil, errors.New("unexpected path " + path)
			}
		},
		WriteFile: func(path string, data []byte, perm uint32) error {
			wrotePath = path
			wroteData = append([]byte(nil), data...)
			wrotePerm = perm
			return nil
		},
		Now: nowFn,
	}

	svc := newDaemonService(t, probes)
	res := svc.runDaemonStep(ctx, InstallOptions{})
	if res.Step != StepDaemon {
		t.Fatalf("step = %q want %q", res.Step, StepDaemon)
	}
	if res.Status != JournalCompleted {
		t.Fatalf("status = %q want %q error %q", res.Status, JournalCompleted, res.Error)
	}
	if res.Error != "" {
		t.Fatalf("unexpected error %q", res.Error)
	}

	wantSys := [][]string{{"enable", "omahabd"}, {"restart", "omahabd"}}
	if !reflect.DeepEqual(sysCalls, wantSys) {
		t.Fatalf("systemctl calls = %v want %v", sysCalls, wantSys)
	}
	if polls != 2 {
		t.Fatalf("polls = %d want 2", polls)
	}
	// assert restart before poll in combined order
	restartIdx := -1
	pollIdx := -1
	for i, o := range order {
		if o == "restart omahabd" && restartIdx == -1 {
			restartIdx = i
		}
		if o == "poll" && pollIdx == -1 {
			pollIdx = i
		}
	}
	if restartIdx == -1 || pollIdx == -1 {
		t.Fatalf("order missing restart or poll: %v", order)
	}
	if restartIdx > pollIdx {
		t.Fatalf("restart should be before poll, order %v", order)
	}
	if wrotePath != "/etc/omahab/backup.env" {
		t.Fatalf("wrotePath = %q want /etc/omahab/backup.env", wrotePath)
	}
	if wrotePerm != 0o600 {
		t.Fatalf("perm = %o want 600", wrotePerm)
	}
	wantContent := "OMAHAB_SERVER=http://127.0.0.1:8484\nOMAHAB_TOKEN=" + token + "\n"
	if string(wroteData) != wantContent {
		t.Fatalf("backup.env = %q want %q", string(wroteData), wantContent)
	}
	if strings.Contains(res.Error, token) {
		t.Fatalf("RunResult leaked token")
	}
}

func TestDaemonTimeout(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	sysCalls := 0
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	nowCalls := 0
	nowFn := func() time.Time {
		if nowCalls == 0 {
			nowCalls++
			return start
		}
		// cross deadline instantly
		return start.Add(121 * time.Second)
	}

	probes := Probes{
		Systemctl: func(ctx context.Context, args ...string) (string, error) {
			sysCalls++
			return "", nil
		},
		HTTPSGet: func(ctx context.Context, url string) (int, []byte, error) {
			return 500, []byte("not ready"), nil
		},
		Now: nowFn,
		// ReadFile not needed, but provide dummy to avoid nil check after timeout
		ReadFile: func(path string) ([]byte, error) {
			return nil, errors.New("should not be called after timeout")
		},
	}

	svc := newDaemonService(t, probes)
	res := svc.runDaemonStep(ctx, InstallOptions{})
	if res.Step != StepDaemon {
		t.Fatalf("step %q", res.Step)
	}
	if res.Status != JournalFailed {
		t.Fatalf("status %q want failed", res.Status)
	}
	if !strings.Contains(res.Error, "journalctl -u omahabd -n 50 --no-pager") {
		t.Fatalf("error %q should contain journalctl hint", res.Error)
	}
	if sysCalls != 2 {
		t.Fatalf("expected 2 systemctl calls (enable+restart) before timeout, got %d", sysCalls)
	}
}

func TestDaemonTokenMissing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	secret := "super-secret-should-not-leak-xyz"
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	nowFn := func() time.Time { return start }

	var wrote bool
	probes := Probes{
		Systemctl: func(ctx context.Context, args ...string) (string, error) { return "", nil },
		HTTPSGet: func(ctx context.Context, url string) (int, []byte, error) {
			return 200, []byte(`{"status":"up"}`), nil
		},
		Now: nowFn,
		ReadFile: func(path string) ([]byte, error) {
			switch path {
			case "/var/lib/omahab/api.token":
				return nil, errors.New("not found")
			case "/etc/omahab/backup.env":
				return nil, errors.New("not found")
			default:
				return nil, errors.New("unexpected " + path)
			}
		},
		WriteFile: func(path string, data []byte, perm uint32) error {
			wrote = true
			return nil
		},
	}

	svc := newDaemonService(t, probes)
	res := svc.runDaemonStep(ctx, InstallOptions{})
	if res.Status != JournalFailed {
		t.Fatalf("status %q want failed", res.Status)
	}
	if !strings.Contains(res.Error, "omahabd did not create the token") {
		t.Fatalf("error %q should mention omahabd did not create the token", res.Error)
	}
	if !strings.Contains(res.Error, "journalctl") {
		t.Fatalf("error %q should contain journalctl hint", res.Error)
	}
	if strings.Contains(res.Error, secret) {
		t.Fatalf("error leaked token %q", res.Error)
	}
	if wrote {
		t.Fatalf("should not have written backup.env when token missing")
	}
	// also ensure generic secret not leaked
	if strings.Contains(res.Error, "super-secret") {
		t.Fatalf("error leaked secret")
	}
}

func TestDaemonTokenEmpty(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	probes := Probes{
		Systemctl: func(ctx context.Context, args ...string) (string, error) { return "", nil },
		HTTPSGet:  func(ctx context.Context, url string) (int, []byte, error) { return 200, nil, nil },
		Now:       func() time.Time { return start },
		ReadFile: func(path string) ([]byte, error) {
			if path == "/var/lib/omahab/api.token" {
				return []byte("   \n"), nil
			}
			return nil, errors.New("not found")
		},
	}
	svc := newDaemonService(t, probes)
	res := svc.runDaemonStep(ctx, InstallOptions{})
	if res.Status != JournalFailed {
		t.Fatalf("status %q want failed for empty token", res.Status)
	}
	if !strings.Contains(res.Error, "omahabd did not create the token") {
		t.Fatalf("empty token error %q should contain remediation hint", res.Error)
	}
}

func TestDaemonNeverLeaksTokenOnWriteError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	token := "my-secret-token-12345-leaky"
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	probes := Probes{
		Systemctl: func(ctx context.Context, args ...string) (string, error) { return "", nil },
		HTTPSGet:  func(ctx context.Context, url string) (int, []byte, error) { return 200, []byte(`{"status":"up"}`), nil },
		Now:       func() time.Time { return start },
		ReadFile: func(path string) ([]byte, error) {
			switch path {
			case "/var/lib/omahab/api.token":
				return []byte(token), nil
			case "/etc/omahab/backup.env":
				return nil, errors.New("not found")
			default:
				return nil, errors.New("unexpected")
			}
		},
		WriteFile: func(path string, data []byte, perm uint32) error {
			// ensure we are not logging token via error
			if strings.Contains("write failed", token) {
				t.Fatal("test setup leaked")
			}
			return errors.New("write failed: disk full")
		},
	}
	svc := newDaemonService(t, probes)
	res := svc.runDaemonStep(ctx, InstallOptions{})
	if res.Status != JournalFailed {
		t.Fatalf("status %q want failed", res.Status)
	}
	if strings.Contains(res.Error, token) {
		t.Fatalf("error leaked token: %q", res.Error)
	}
	if !strings.Contains(res.Error, "write failed") {
		t.Fatalf("error %q should contain write failure", res.Error)
	}
}

func TestDaemonIdempotentSecondRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	token := "idempotent-token-xyz"
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	nowFn := func() time.Time { return start }

	existing := map[string][]byte{}
	var writeCount int
	var lastPerm uint32

	probes := Probes{
		Systemctl: func(ctx context.Context, args ...string) (string, error) { return "", nil },
		HTTPSGet:  func(ctx context.Context, url string) (int, []byte, error) { return 200, []byte(`{"status":"up"}`), nil },
		Now:       nowFn,
		ReadFile: func(path string) ([]byte, error) {
			switch path {
			case "/var/lib/omahab/api.token":
				return []byte(token + "\n"), nil
			case "/etc/omahab/backup.env":
				if data, ok := existing[path]; ok {
					return append([]byte(nil), data...), nil
				}
				return nil, errors.New("not found")
			default:
				return nil, errors.New("unexpected " + path)
			}
		},
		WriteFile: func(path string, data []byte, perm uint32) error {
			if path != "/etc/omahab/backup.env" {
				t.Fatalf("unexpected write path %q", path)
			}
			writeCount++
			lastPerm = perm
			existing[path] = append([]byte(nil), data...)
			return nil
		},
	}

	svc := newDaemonService(t, probes)
	res1 := svc.runDaemonStep(ctx, InstallOptions{})
	if res1.Status != JournalCompleted {
		t.Fatalf("first run status %q want completed, err %q", res1.Status, res1.Error)
	}
	if writeCount != 1 {
		t.Fatalf("first run writeCount = %d want 1", writeCount)
	}
	if lastPerm != 0o600 {
		t.Fatalf("first run perm %o want 600", lastPerm)
	}
	wantContent := "OMAHAB_SERVER=http://127.0.0.1:8484\nOMAHAB_TOKEN=" + token + "\n"
	if string(existing["/etc/omahab/backup.env"]) != wantContent {
		t.Fatalf("first run content %q want %q", string(existing["/etc/omahab/backup.env"]), wantContent)
	}

	// second run with same token and already-existing identical file should skip write
	res2 := svc.runDaemonStep(ctx, InstallOptions{})
	if res2.Status != JournalCompleted {
		t.Fatalf("second run status %q want completed, err %q", res2.Status, res2.Error)
	}
	if writeCount != 1 {
		t.Fatalf("second run should not rewrite: writeCount = %d want 1", writeCount)
	}
}

func TestDaemonIdempotentRewritesWhenDifferent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	token1 := "token-one"
	token2 := "token-two"
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	existing := map[string][]byte{}
	writeCount := 0
	probes := Probes{
		Systemctl: func(ctx context.Context, args ...string) (string, error) { return "", nil },
		HTTPSGet:  func(ctx context.Context, url string) (int, []byte, error) { return 200, nil, nil },
		Now:       func() time.Time { return start },
		ReadFile: func(path string) ([]byte, error) {
			if path == "/var/lib/omahab/api.token" {
				// Return token2 after first write to simulate rotation
				if writeCount == 0 {
					return []byte(token1), nil
				}
				return []byte(token2), nil
			}
			if path == "/etc/omahab/backup.env" {
				if data, ok := existing[path]; ok {
					return append([]byte(nil), data...), nil
				}
				return nil, errors.New("not found")
			}
			return nil, errors.New("unexpected")
		},
		WriteFile: func(path string, data []byte, perm uint32) error {
			writeCount++
			existing[path] = append([]byte(nil), data...)
			return nil
		},
	}
	svc := newDaemonService(t, probes)
	res1 := svc.runDaemonStep(ctx, InstallOptions{})
	if res1.Status != JournalCompleted {
		t.Fatalf("first %q", res1.Error)
	}
	if writeCount != 1 {
		t.Fatalf("want 1 write after first, got %d", writeCount)
	}
	res2 := svc.runDaemonStep(ctx, InstallOptions{})
	if res2.Status != JournalCompleted {
		t.Fatalf("second %q", res2.Error)
	}
	if writeCount != 2 {
		t.Fatalf("different token should rewrite: writeCount %d want 2", writeCount)
	}
	want := "OMAHAB_SERVER=http://127.0.0.1:8484\nOMAHAB_TOKEN=" + token2 + "\n"
	if string(existing["/etc/omahab/backup.env"]) != want {
		t.Fatalf("rewritten content %q want %q", string(existing["/etc/omahab/backup.env"]), want)
	}
}

func TestDaemonRollback(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var sysCalls [][]string
	var removed string
	probes := Probes{
		Systemctl: func(ctx context.Context, args ...string) (string, error) {
			sysCalls = append(sysCalls, append([]string(nil), args...))
			return "", nil
		},
		RemoveFile: func(path string) error {
			removed = path
			return nil
		},
	}
	if err := RollbackDaemon(ctx, probes); err != nil {
		t.Fatalf("RollbackDaemon error = %v", err)
	}
	want := [][]string{{"stop", "omahabd"}, {"disable", "omahabd"}}
	if !reflect.DeepEqual(sysCalls, want) {
		t.Fatalf("sysCalls = %v want %v", sysCalls, want)
	}
	if removed != "/etc/omahab/backup.env" {
		t.Fatalf("removed = %q want /etc/omahab/backup.env", removed)
	}
}

func TestDaemonRollbackBestEffort(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var sysCalls [][]string
	probes := Probes{
		Systemctl: func(ctx context.Context, args ...string) (string, error) {
			sysCalls = append(sysCalls, append([]string(nil), args...))
			if len(args) > 0 && args[0] == "stop" {
				return "", errors.New("stop failed")
			}
			return "", nil
		},
		RemoveFile: func(path string) error {
			return errors.New("remove failed")
		},
	}
	if err := RollbackDaemon(ctx, probes); err != nil {
		t.Fatalf("best-effort rollback should return nil, got %v", err)
	}
	// both systemctl calls should still happen even though first failed
	if len(sysCalls) != 2 {
		t.Fatalf("expected 2 systemctl calls, got %d: %v", len(sysCalls), sysCalls)
	}
	if !reflect.DeepEqual(sysCalls[0], []string{"stop", "omahabd"}) {
		t.Fatalf("first = %v", sysCalls[0])
	}
	if !reflect.DeepEqual(sysCalls[1], []string{"disable", "omahabd"}) {
		t.Fatalf("second = %v", sysCalls[1])
	}
}

func TestDaemonRollbackNilProbes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic with nil probes: %v", r)
		}
	}()
	if err := RollbackDaemon(ctx, Probes{}); err != nil {
		t.Fatalf("nil probes rollback should return nil, got %v", err)
	}
	if err := RollbackDaemon(ctx, Probes{Systemctl: nil, RemoveFile: nil}); err != nil {
		t.Fatalf("nil funcs rollback should return nil, got %v", err)
	}
}

func TestDaemonNilProbes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	svc := newDaemonService(t, Probes{})
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic with nil probes: %v", r)
		}
	}()
	res := svc.runDaemonStep(ctx, InstallOptions{})
	if res.Step != StepDaemon {
		t.Fatalf("step = %q", res.Step)
	}
	if res.Status != JournalFailed {
		t.Fatalf("status = %q want failed", res.Status)
	}
	if res.Error == "" {
		t.Fatal("expected error for nil probes")
	}

	var zero Service
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("panic with zero Service: %v", r)
			}
		}()
		r2 := zero.runDaemonStep(ctx, InstallOptions{})
		if r2.Status != JournalFailed {
			t.Fatalf("zero svc status %q want failed", r2.Status)
		}
	}()
}

func TestDaemonSystemctlEnableFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var sysCalls [][]string
	probes := Probes{
		Systemctl: func(ctx context.Context, args ...string) (string, error) {
			sysCalls = append(sysCalls, append([]string(nil), args...))
			if len(args) == 2 && args[0] == "enable" && args[1] == "omahabd" {
				return "", errors.New("enable failed")
			}
			return "", nil
		},
		Now: func() time.Time { return start },
	}
	svc := newDaemonService(t, probes)
	res := svc.runDaemonStep(ctx, InstallOptions{})
	if res.Status != JournalFailed {
		t.Fatalf("status %q want failed", res.Status)
	}
	if !strings.Contains(res.Error, "enable") {
		t.Fatalf("error %q should mention enable", res.Error)
	}
	if len(sysCalls) != 1 {
		t.Fatalf("should stop after enable failure, calls = %v", sysCalls)
	}
}

func TestDaemonSystemctlRestartFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var sysCalls [][]string
	probes := Probes{
		Systemctl: func(ctx context.Context, args ...string) (string, error) {
			sysCalls = append(sysCalls, append([]string(nil), args...))
			if len(args) == 2 && args[0] == "restart" && args[1] == "omahabd" {
				return "", errors.New("restart failed")
			}
			return "", nil
		},
		Now: func() time.Time { return start },
	}
	svc := newDaemonService(t, probes)
	res := svc.runDaemonStep(ctx, InstallOptions{})
	if res.Status != JournalFailed {
		t.Fatalf("status %q", res.Status)
	}
	if len(sysCalls) != 2 {
		t.Fatalf("expected enable+restart, got %v", sysCalls)
	}
}

func TestDaemonContextCancelled(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// ctx already cancelled
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	probes := Probes{
		Systemctl: func(ctx context.Context, args ...string) (string, error) { return "", nil },
		HTTPSGet: func(ctx context.Context, url string) (int, []byte, error) {
			// Should not be reached if ctx cancelled before poll, but if it is, return not ready
			return 500, nil, nil
		},
		Now: func() time.Time { return start },
		ReadFile: func(path string) ([]byte, error) {
			return nil, errors.New("should not reach")
		},
	}
	svc := newDaemonService(t, probes)
	res := svc.runDaemonStep(ctx, InstallOptions{})
	if res.Status != JournalFailed {
		t.Fatalf("cancelled ctx should yield failed, got %q", res.Status)
	}
	if !strings.Contains(strings.ToLower(res.Error), "cancel") && !strings.Contains(res.Error, "journalctl") {
		// waitHealthy returns cancelled error; allow either
		t.Fatalf("cancelled error %q should mention cancel or journalctl", res.Error)
	}
}

func TestDaemonHealthURLConstant(t *testing.T) {
	t.Parallel()
	if DaemonHealthURL != "http://127.0.0.1:8484/up" {
		t.Fatalf("DaemonHealthURL = %q want http://127.0.0.1:8484/up", DaemonHealthURL)
	}
}

func TestDaemonWaitHealthyNilProbe(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	err := waitHealthy(ctx, Probes{})
	if err == nil {
		t.Fatal("expected error for nil HTTPSGet")
	}
	if !strings.Contains(err.Error(), "https get") {
		t.Fatalf("error %q should mention https get", err.Error())
	}
}

func TestDaemonWaitHealthyHonorsContext(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ctx, cancel := context.WithCancel(context.Background())
	// HTTPSGet cancels the context on first call to simulate cancellation during poll
	probes := Probes{
		HTTPSGet: func(ctx context.Context, url string) (int, []byte, error) {
			cancel()
			// also check ctx
			select {
			case <-ctx.Done():
				return 0, nil, ctx.Err()
			default:
			}
			return 500, nil, nil
		},
		Now: func() time.Time { return start },
	}
	// Need to ensure waitHealthy checks ctx between polls. Since we fake Now that never reaches deadline, it should eventually notice cancellation.
	// Make Now advance slowly so deadline not hit for a few polls, but context cancelled will be noticed.
	calls := 0
	probes.Now = func() time.Time {
		calls++
		// keep time before deadline for first few calls, then still before deadline
		return start.Add(time.Duration(calls) * time.Second)
	}
	err := waitHealthy(ctx, probes)
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "cancel") {
		t.Fatalf("error %q should mention cancel", err.Error())
	}
}
func TestDaemonProvisionsUserTokenForOmahab(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	token := "provisions-token-omahab-xyz-999"
	home := "/home/omahab"
	tokenPath := home + "/.config/omahab/token"
	parentDir := home + "/.config"
	configDir := home + "/.config/omahab"
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	var mkdirCalls []struct {
		path string
		perm uint32
	}
	var chownCalls []struct {
		path     string
		uid, gid int
	}
	var chmodCalls []struct {
		path string
		perm uint32
	}
	var writes []struct {
		path string
		data []byte
		perm uint32
	}

	probes := Probes{
		Systemctl: func(ctx context.Context, args ...string) (string, error) { return "", nil },
		HTTPSGet:  func(ctx context.Context, url string) (int, []byte, error) { return 200, []byte(`{"status":"up"}`), nil },
		Now:       func() time.Time { return start },
		ReadFile: func(path string) ([]byte, error) {
			switch path {
			case "/var/lib/omahab/api.token":
				return []byte(token + "\n"), nil
			case "/etc/omahab/backup.env":
				return nil, errors.New("not found")
			case tokenPath:
				return nil, errors.New("not found")
			default:
				return nil, errors.New("unexpected " + path)
			}
		},
		WriteFile: func(path string, data []byte, perm uint32) error {
			writes = append(writes, struct {
				path string
				data []byte
				perm uint32
			}{path, append([]byte(nil), data...), perm})
			return nil
		},
		MkdirAll: func(path string, perm uint32) error {
			mkdirCalls = append(mkdirCalls, struct {
				path string
				perm uint32
			}{path, perm})
			return nil
		},
		StatFile: func(path string) (bool, uint32, error) {
			return false, 0, errors.New("not found")
		},
		FileOwner: func(path string) (int, int, error) {
			return 0, 0, errors.New("not found")
		},
		LookupUser: func(name string) (int, int, string, error) {
			if name == "omahab" {
				return 1000, 1000, home, nil
			}
			return 0, 0, "", errors.New("unknown user " + name)
		},
		Chown: func(path string, uid, gid int) error {
			chownCalls = append(chownCalls, struct {
				path     string
				uid, gid int
			}{path, uid, gid})
			return nil
		},
		Chmod: func(path string, perm uint32) error {
			chmodCalls = append(chmodCalls, struct {
				path string
				perm uint32
			}{path, perm})
			return nil
		},
	}
	svc := newDaemonService(t, probes)
	res := svc.runDaemonStep(ctx, InstallOptions{TargetUser: "omahab"})
	if res.Status != JournalCompleted {
		t.Fatalf("status %q want completed, err %q", res.Status, res.Error)
	}
	if strings.Contains(res.Error, token) {
		t.Fatalf("error leaked token")
	}
	// backup.env written
	foundBackup := false
	foundToken := false
	for _, w := range writes {
		if w.path == "/etc/omahab/backup.env" {
			foundBackup = true
			if w.perm != 0o600 {
				t.Fatalf("backup.env perm %o want 600", w.perm)
			}
			want := "OMAHAB_SERVER=http://127.0.0.1:8484\nOMAHAB_TOKEN=" + token + "\n"
			if string(w.data) != want {
				t.Fatalf("backup.env content %q want %q", string(w.data), want)
			}
		}
		if w.path == tokenPath {
			foundToken = true
			if w.perm != 0o600 {
				t.Fatalf("token perm %o want 600", w.perm)
			}
			if string(w.data) != token+"\n" {
				t.Fatalf("token file content %q want %q", string(w.data), token+"\n")
			}
		}
	}
	if !foundBackup {
		t.Fatalf("backup.env not written, writes %v", writes)
	}
	if !foundToken {
		t.Fatalf("token file not written, writes %v", writes)
	}
	// dirs created with 0700
	foundParent := false
	foundConfig := false
	for _, m := range mkdirCalls {
		if m.path == parentDir && m.perm == 0o700 {
			foundParent = true
		}
		if m.path == configDir && m.perm == 0o700 {
			foundConfig = true
		}
		if m.path == "/var/lib/omahab" {
			t.Fatalf("should not weaken /var/lib, got mkdir %q", m.path)
		}
	}
	if !foundParent {
		t.Fatalf("parent .config not mkdir 0700, calls %v", mkdirCalls)
	}
	if !foundConfig {
		t.Fatalf("config dir not mkdir 0700, calls %v", mkdirCalls)
	}
	// chown for dirs and token
	hasChown := func(path string, uid, gid int) bool {
		for _, c := range chownCalls {
			if c.path == path && c.uid == uid && c.gid == gid {
				return true
			}
		}
		return false
	}
	if !hasChown(parentDir, 1000, 1000) {
		t.Fatalf("parent dir not chowned to 1000:1000, calls %v", chownCalls)
	}
	if !hasChown(configDir, 1000, 1000) {
		t.Fatalf("config dir not chowned, calls %v", chownCalls)
	}
	if !hasChown(tokenPath, 1000, 1000) {
		t.Fatalf("token file not chowned, calls %v", chownCalls)
	}
	// chmod for dirs/token
	hasChmod := func(path string, perm uint32) bool {
		for _, c := range chmodCalls {
			if c.path == path && c.perm == perm {
				return true
			}
		}
		return false
	}
	if !hasChmod(parentDir, 0o700) {
		t.Fatalf("parent dir not chmod 0700, calls %v", chmodCalls)
	}
	if !hasChmod(configDir, 0o700) {
		t.Fatalf("config dir not chmod 0700, calls %v", chmodCalls)
	}
	if !hasChmod(tokenPath, 0o600) {
		t.Fatalf("token file not chmod 0600, calls %v", chmodCalls)
	}
	if strings.Contains(res.Error, token) {
		t.Fatalf("leaked token in error")
	}
}

func TestDaemonProvisionsUserTokenIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	token := "idempotent-token-abc"
	home := "/home/omahab"
	tokenPath := home + "/.config/omahab/token"
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	files := map[string][]byte{}
	var writes []string
	var mkdirs []string

	probes := Probes{
		Systemctl: func(ctx context.Context, args ...string) (string, error) { return "", nil },
		HTTPSGet:  func(ctx context.Context, url string) (int, []byte, error) { return 200, nil, nil },
		Now:       func() time.Time { return start },
		ReadFile: func(path string) ([]byte, error) {
			switch path {
			case "/var/lib/omahab/api.token":
				return []byte(token), nil
			default:
				if data, ok := files[path]; ok {
					return append([]byte(nil), data...), nil
				}
				return nil, errors.New("not found")
			}
		},
		WriteFile: func(path string, data []byte, perm uint32) error {
			if perm != 0o600 {
				t.Fatalf("write perm %o want 600 for %q", perm, path)
			}
			writes = append(writes, path)
			files[path] = append([]byte(nil), data...)
			return nil
		},
		MkdirAll: func(path string, perm uint32) error {
			mkdirs = append(mkdirs, path)
			if perm != 0o700 && path != "/etc/omahab" {
				// allow backup.env parent not checked; only token dirs must be 0700
				// token dirs
				if path == home+"/.config" || path == home+"/.config/omahab" {
					t.Fatalf("mkdir perm %o want 700 for %q", perm, path)
				}
			}
			return nil
		},
		StatFile: func(path string) (bool, uint32, error) {
			if _, ok := files[path]; ok {
				// token file exists with 0600
				if path == tokenPath {
					return false, 0o600, nil
				}
				return false, 0o600, nil
			}
			return false, 0, errors.New("not found")
		},
		FileOwner: func(path string) (int, int, error) {
			if _, ok := files[path]; ok {
				// token owned by target
				return 1000, 1000, nil
			}
			return 0, 0, errors.New("not found")
		},
		LookupUser: func(name string) (int, int, string, error) {
			return 1000, 1000, home, nil
		},
		Chown: func(path string, uid, gid int) error { return nil },
		Chmod: func(path string, perm uint32) error { return nil },
	}
	svc := newDaemonService(t, probes)
	res1 := svc.runDaemonStep(ctx, InstallOptions{TargetUser: "omahab"})
	if res1.Status != JournalCompleted {
		t.Fatalf("first run %q", res1.Error)
	}
	if strings.Contains(res1.Error, token) {
		t.Fatalf("leaked token")
	}
	// count writes for backup.env and token
	backupWrites := 0
	tokenWrites := 0
	for _, p := range writes {
		if p == "/etc/omahab/backup.env" {
			backupWrites++
		}
		if p == tokenPath {
			tokenWrites++
		}
	}
	if backupWrites != 1 {
		t.Fatalf("backup writes %d want 1", backupWrites)
	}
	if tokenWrites != 1 {
		t.Fatalf("token writes %d want 1", tokenWrites)
	}
	// second run same token should be idempotent: no additional writes
	writes = nil
	mkdirs = nil
	res2 := svc.runDaemonStep(ctx, InstallOptions{TargetUser: "omahab"})
	if res2.Status != JournalCompleted {
		t.Fatalf("second run %q", res2.Error)
	}
	if len(writes) != 0 {
		t.Fatalf("second run should be idempotent, writes %v", writes)
	}
	if strings.Contains(res2.Error, token) {
		t.Fatalf("leaked token second run")
	}
}

func TestDaemonProvisionsUserTokenRewritesWhenChanged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	token1 := "token-one-aaa"
	token2 := "token-two-bbb"
	home := "/home/omahab"
	tokenPath := home + "/.config/omahab/token"
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	files := map[string][]byte{}
	writeCount := 0
	probes := Probes{
		Systemctl: func(ctx context.Context, args ...string) (string, error) { return "", nil },
		HTTPSGet:  func(ctx context.Context, url string) (int, []byte, error) { return 200, nil, nil },
		Now:       func() time.Time { return start },
		ReadFile: func(path string) ([]byte, error) {
			switch path {
			case "/var/lib/omahab/api.token":
				if writeCount == 0 {
					return []byte(token1), nil
				}
				return []byte(token2), nil
			default:
				if data, ok := files[path]; ok {
					return append([]byte(nil), data...), nil
				}
				return nil, errors.New("not found")
			}
		},
		WriteFile: func(path string, data []byte, perm uint32) error {
			writeCount++
			// actually track per file; but writeCount global counts both files
			files[path] = append([]byte(nil), data...)
			return nil
		},
		MkdirAll: func(path string, perm uint32) error { return nil },
		StatFile: func(path string) (bool, uint32, error) {
			if _, ok := files[path]; ok {
				return false, 0o600, nil
			}
			return false, 0, errors.New("not found")
		},
		FileOwner: func(path string) (int, int, error) {
			if _, ok := files[path]; ok {
				return 1000, 1000, nil
			}
			return 0, 0, errors.New("not found")
		},
		LookupUser: func(name string) (int, int, string, error) { return 1000, 1000, home, nil },
		Chown:      func(path string, uid, gid int) error { return nil },
		Chmod:      func(path string, perm uint32) error { return nil },
	}
	svc := newDaemonService(t, probes)
	res1 := svc.runDaemonStep(ctx, InstallOptions{TargetUser: "omahab"})
	if res1.Status != JournalCompleted {
		t.Fatalf("first %q", res1.Error)
	}
	if string(files[tokenPath]) != token1+"\n" {
		t.Fatalf("first token %q want %q", string(files[tokenPath]), token1+"\n")
	}
	// capture writeCount after first
	afterFirst := writeCount
	res2 := svc.runDaemonStep(ctx, InstallOptions{TargetUser: "omahab"})
	if res2.Status != JournalCompleted {
		t.Fatalf("second %q", res2.Error)
	}
	if writeCount <= afterFirst {
		t.Fatalf("changed token should rewrite, writeCount %d afterFirst %d", writeCount, afterFirst)
	}
	if string(files[tokenPath]) != token2+"\n" {
		t.Fatalf("rewritten token %q want %q", string(files[tokenPath]), token2+"\n")
	}
	wantBackup := "OMAHAB_SERVER=http://127.0.0.1:8484\nOMAHAB_TOKEN=" + token2 + "\n"
	if string(files["/etc/omahab/backup.env"]) != wantBackup {
		t.Fatalf("backup.env %q want %q", string(files["/etc/omahab/backup.env"]), wantBackup)
	}
}

func TestDaemonProvisionsUserTokenEvenWhenBackupEnvUpToDate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	token := "token-up-to-date-xyz"
	home := "/home/omahab"
	tokenPath := home + "/.config/omahab/token"
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	backupContent := "OMAHAB_SERVER=http://127.0.0.1:8484\nOMAHAB_TOKEN=" + token + "\n"
	files := map[string][]byte{
		"/etc/omahab/backup.env": []byte(backupContent),
	}
	var writes []string
	probes := Probes{
		Systemctl: func(ctx context.Context, args ...string) (string, error) { return "", nil },
		HTTPSGet:  func(ctx context.Context, url string) (int, []byte, error) { return 200, nil, nil },
		Now:       func() time.Time { return start },
		ReadFile: func(path string) ([]byte, error) {
			switch path {
			case "/var/lib/omahab/api.token":
				return []byte(token), nil
			default:
				if d, ok := files[path]; ok {
					return append([]byte(nil), d...), nil
				}
				return nil, errors.New("not found")
			}
		},
		WriteFile: func(path string, data []byte, perm uint32) error {
			writes = append(writes, path)
			files[path] = append([]byte(nil), data...)
			return nil
		},
		MkdirAll: func(path string, perm uint32) error { return nil },
		StatFile: func(path string) (bool, uint32, error) {
			if _, ok := files[path]; ok {
				return false, 0o600, nil
			}
			return false, 0, errors.New("not found")
		},
		FileOwner: func(path string) (int, int, error) {
			if _, ok := files[path]; ok {
				return 1000, 1000, nil
			}
			return 0, 0, errors.New("not found")
		},
		LookupUser: func(name string) (int, int, string, error) { return 1000, 1000, home, nil },
		Chown:      func(path string, uid, gid int) error { return nil },
		Chmod:      func(path string, perm uint32) error { return nil },
	}
	svc := newDaemonService(t, probes)
	res := svc.runDaemonStep(ctx, InstallOptions{TargetUser: "omahab"})
	if res.Status != JournalCompleted {
		t.Fatalf("status %q want completed, err %q", res.Status, res.Error)
	}
	// backup.env should not be rewritten (already up to date)
	for _, p := range writes {
		if p == "/etc/omahab/backup.env" {
			t.Fatalf("backup.env should not rewrite when up to date, writes %v", writes)
		}
	}
	foundToken := false
	for _, p := range writes {
		if p == tokenPath {
			foundToken = true
		}
	}
	if !foundToken {
		t.Fatalf("token should be provisioned even when backup.env up to date, writes %v", writes)
	}
	if files[tokenPath] == nil || string(files[tokenPath]) != token+"\n" {
		t.Fatalf("token content %q", string(files[tokenPath]))
	}
}

func TestDaemonUserTokenFailureDoesNotLeak(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	token := "super-secret-token-should-not-leak-9999"
	home := "/home/omahab"
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name  string
		probe Probes
	}{
		{
			name: "lookup fails",
			probe: Probes{
				Systemctl: func(ctx context.Context, args ...string) (string, error) { return "", nil },
				HTTPSGet:  func(ctx context.Context, url string) (int, []byte, error) { return 200, nil, nil },
				Now:       func() time.Time { return start },
				ReadFile: func(path string) ([]byte, error) {
					if path == "/var/lib/omahab/api.token" {
						return []byte(token), nil
					}
					if path == "/etc/omahab/backup.env" {
						return nil, errors.New("not found")
					}
					return nil, errors.New("not found")
				},
				WriteFile:  func(path string, data []byte, perm uint32) error { return nil },
				MkdirAll:   func(path string, perm uint32) error { return nil },
				LookupUser: func(name string) (int, int, string, error) { return 0, 0, "", errors.New("lookup failed") },
				Chown:      func(path string, uid, gid int) error { return nil },
				Chmod:      func(path string, perm uint32) error { return nil },
			},
		},
		{
			name: "mkdir fails",
			probe: Probes{
				Systemctl: func(ctx context.Context, args ...string) (string, error) { return "", nil },
				HTTPSGet:  func(ctx context.Context, url string) (int, []byte, error) { return 200, nil, nil },
				Now:       func() time.Time { return start },
				ReadFile: func(path string) ([]byte, error) {
					if path == "/var/lib/omahab/api.token" {
						return []byte(token), nil
					}
					if path == "/etc/omahab/backup.env" {
						return nil, errors.New("not found")
					}
					return nil, errors.New("not found")
				},
				WriteFile:  func(path string, data []byte, perm uint32) error { return nil },
				MkdirAll:   func(path string, perm uint32) error { return errors.New("mkdir failed") },
				LookupUser: func(name string) (int, int, string, error) { return 1000, 1000, home, nil },
				Chown:      func(path string, uid, gid int) error { return nil },
				Chmod:      func(path string, perm uint32) error { return nil },
			},
		},
		{
			name: "chown dir fails",
			probe: Probes{
				Systemctl: func(ctx context.Context, args ...string) (string, error) { return "", nil },
				HTTPSGet:  func(ctx context.Context, url string) (int, []byte, error) { return 200, nil, nil },
				Now:       func() time.Time { return start },
				ReadFile: func(path string) ([]byte, error) {
					if path == "/var/lib/omahab/api.token" {
						return []byte(token), nil
					}
					if path == "/etc/omahab/backup.env" {
						return nil, errors.New("not found")
					}
					return nil, errors.New("not found")
				},
				WriteFile:  func(path string, data []byte, perm uint32) error { return nil },
				MkdirAll:   func(path string, perm uint32) error { return nil },
				LookupUser: func(name string) (int, int, string, error) { return 1000, 1000, home, nil },
				Chown:      func(path string, uid, gid int) error { return errors.New("chown failed") },
				Chmod:      func(path string, perm uint32) error { return nil },
			},
		},
		{
			name: "chmod dir fails",
			probe: Probes{
				Systemctl: func(ctx context.Context, args ...string) (string, error) { return "", nil },
				HTTPSGet:  func(ctx context.Context, url string) (int, []byte, error) { return 200, nil, nil },
				Now:       func() time.Time { return start },
				ReadFile: func(path string) ([]byte, error) {
					if path == "/var/lib/omahab/api.token" {
						return []byte(token), nil
					}
					if path == "/etc/omahab/backup.env" {
						return nil, errors.New("not found")
					}
					return nil, errors.New("not found")
				},
				WriteFile:  func(path string, data []byte, perm uint32) error { return nil },
				MkdirAll:   func(path string, perm uint32) error { return nil },
				LookupUser: func(name string) (int, int, string, error) { return 1000, 1000, home, nil },
				Chown:      func(path string, uid, gid int) error { return nil },
				Chmod:      func(path string, perm uint32) error { return errors.New("chmod failed") },
			},
		},
		{
			name: "write token fails",
			probe: Probes{
				Systemctl: func(ctx context.Context, args ...string) (string, error) { return "", nil },
				HTTPSGet:  func(ctx context.Context, url string) (int, []byte, error) { return 200, nil, nil },
				Now:       func() time.Time { return start },
				ReadFile: func(path string) ([]byte, error) {
					if path == "/var/lib/omahab/api.token" {
						return []byte(token), nil
					}
					if path == "/etc/omahab/backup.env" {
						return nil, errors.New("not found")
					}
					return nil, errors.New("not found")
				},
				WriteFile: func(path string, data []byte, perm uint32) error {
					if path == home+"/.config/omahab/token" {
						return errors.New("write token failed")
					}
					return nil
				},
				MkdirAll:   func(path string, perm uint32) error { return nil },
				LookupUser: func(name string) (int, int, string, error) { return 1000, 1000, home, nil },
				Chown:      func(path string, uid, gid int) error { return nil },
				Chmod:      func(path string, perm uint32) error { return nil },
			},
		},
		{
			name: "chown token fails",
			probe: Probes{
				Systemctl: func(ctx context.Context, args ...string) (string, error) { return "", nil },
				HTTPSGet:  func(ctx context.Context, url string) (int, []byte, error) { return 200, nil, nil },
				Now:       func() time.Time { return start },
				ReadFile: func(path string) ([]byte, error) {
					if path == "/var/lib/omahab/api.token" {
						return []byte(token), nil
					}
					if path == "/etc/omahab/backup.env" {
						return nil, errors.New("not found")
					}
					return nil, errors.New("not found")
				},
				WriteFile:  func(path string, data []byte, perm uint32) error { return nil },
				MkdirAll:   func(path string, perm uint32) error { return nil },
				LookupUser: func(name string) (int, int, string, error) { return 1000, 1000, home, nil },
				Chown: func(path string, uid, gid int) error {
					if path == home+"/.config/omahab/token" {
						return errors.New("chown token failed")
					}
					return nil
				},
				Chmod: func(path string, perm uint32) error { return nil },
			},
		},
		{
			name: "empty home",
			probe: Probes{
				Systemctl: func(ctx context.Context, args ...string) (string, error) { return "", nil },
				HTTPSGet:  func(ctx context.Context, url string) (int, []byte, error) { return 200, nil, nil },
				Now:       func() time.Time { return start },
				ReadFile: func(path string) ([]byte, error) {
					if path == "/var/lib/omahab/api.token" {
						return []byte(token), nil
					}
					if path == "/etc/omahab/backup.env" {
						return nil, errors.New("not found")
					}
					return nil, errors.New("not found")
				},
				WriteFile:  func(path string, data []byte, perm uint32) error { return nil },
				MkdirAll:   func(path string, perm uint32) error { return nil },
				LookupUser: func(name string) (int, int, string, error) { return 1000, 1000, "", nil },
				Chown:      func(path string, uid, gid int) error { return nil },
				Chmod:      func(path string, perm uint32) error { return nil },
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := newDaemonService(t, tc.probe)
			res := svc.runDaemonStep(ctx, InstallOptions{TargetUser: "omahab"})
			if res.Status != JournalFailed {
				t.Fatalf("expected failed for %q got %q", tc.name, res.Status)
			}
			if strings.Contains(res.Error, token) {
				t.Fatalf("error leaked token for %q: %q", tc.name, res.Error)
			}
			if res.Error == "" {
				t.Fatalf("expected error for %q", tc.name)
			}
		})
	}
}

func TestDaemonProvisionsUserTokenRootFallback(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	token := "root-fallback-token-xyz"
	home := "/root"
	tokenPath := home + "/.config/omahab/token"
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var writes []string
	probes := Probes{
		Systemctl: func(ctx context.Context, args ...string) (string, error) { return "", nil },
		HTTPSGet:  func(ctx context.Context, url string) (int, []byte, error) { return 200, nil, nil },
		Now:       func() time.Time { return start },
		ReadFile: func(path string) ([]byte, error) {
			if path == "/var/lib/omahab/api.token" {
				return []byte(token), nil
			}
			if path == "/etc/omahab/backup.env" {
				return nil, errors.New("not found")
			}
			if path == tokenPath {
				return nil, errors.New("not found")
			}
			return nil, errors.New("unexpected " + path)
		},
		WriteFile: func(path string, data []byte, perm uint32) error {
			writes = append(writes, path)
			if path == tokenPath && perm != 0o600 {
				t.Fatalf("perm %o", perm)
			}
			if path == tokenPath && string(data) != token+"\n" {
				t.Fatalf("data %q", string(data))
			}
			return nil
		},
		MkdirAll: func(path string, perm uint32) error {
			if perm != 0o700 {
				t.Fatalf("mkdir perm %o", perm)
			}
			return nil
		},
		LookupUser: func(name string) (int, int, string, error) {
			if name == "root" {
				return 0, 0, "/root", nil
			}
			t.Fatalf("expected root lookup got %q", name)
			return 0, 0, "", errors.New("unexpected")
		},
		Chown: func(path string, uid, gid int) error {
			if uid != 0 || gid != 0 {
				t.Fatalf("root chown should be 0:0 got %d:%d", uid, gid)
			}
			return nil
		},
		Chmod: func(path string, perm uint32) error { return nil },
	}
	// empty TargetUser should resolve to root via resolveTargetUser (which fallback to root when no AuthorizedKeys probe)
	svc := newDaemonService(t, probes)
	res := svc.runDaemonStep(ctx, InstallOptions{})
	if res.Status != JournalCompleted {
		t.Fatalf("status %q err %q", res.Status, res.Error)
	}
	found := false
	for _, p := range writes {
		if p == tokenPath {
			found = true
		}
	}
	if !found {
		t.Fatalf("root token not written, writes %v", writes)
	}
	if strings.Contains(res.Error, token) {
		t.Fatalf("leaked token")
	}
}

func TestDaemonProvisionsUserTokenCorrectsOwnershipAndMode(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	token := "correct-ownership-token"
	home := "/home/omahab"
	tokenPath := home + "/.config/omahab/token"
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// existing file with same token but wrong perms/owner
	files := map[string][]byte{
		tokenPath: []byte(token + "\n"),
	}
	var writes []string
	probes := Probes{
		Systemctl: func(ctx context.Context, args ...string) (string, error) { return "", nil },
		HTTPSGet:  func(ctx context.Context, url string) (int, []byte, error) { return 200, nil, nil },
		Now:       func() time.Time { return start },
		ReadFile: func(path string) ([]byte, error) {
			if path == "/var/lib/omahab/api.token" {
				return []byte(token), nil
			}
			if path == "/etc/omahab/backup.env" {
				return nil, errors.New("not found")
			}
			if d, ok := files[path]; ok {
				return append([]byte(nil), d...), nil
			}
			return nil, errors.New("not found")
		},
		WriteFile: func(path string, data []byte, perm uint32) error {
			writes = append(writes, path)
			files[path] = append([]byte(nil), data...)
			return nil
		},
		MkdirAll: func(path string, perm uint32) error { return nil },
		StatFile: func(path string) (bool, uint32, error) {
			if path == tokenPath {
				return false, 0o644, nil // wrong perm
			}
			return false, 0, errors.New("not found")
		},
		FileOwner: func(path string) (int, int, error) {
			if path == tokenPath {
				return 0, 0, nil // wrong owner (root instead of 1000)
			}
			return 0, 0, errors.New("not found")
		},
		LookupUser: func(name string) (int, int, string, error) { return 1000, 1000, home, nil },
		Chown:      func(path string, uid, gid int) error { return nil },
		Chmod:      func(path string, perm uint32) error { return nil },
	}
	svc := newDaemonService(t, probes)
	res := svc.runDaemonStep(ctx, InstallOptions{TargetUser: "omahab"})
	if res.Status != JournalCompleted {
		t.Fatalf("status %q err %q", res.Status, res.Error)
	}
	// should have rewritten due to wrong perm/owner
	found := false
	for _, p := range writes {
		if p == tokenPath {
			found = true
		}
	}
	if !found {
		t.Fatalf("should rewrite token when perm/owner wrong, writes %v", writes)
	}
}
