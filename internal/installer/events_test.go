package installer

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/omahab/omahab/internal/secrets"

	_ "modernc.org/sqlite"
)

func TestOrderedStepsRecoveryBetweenDaemonAndManifest(t *testing.T) {
	idxDaemon, idxRecovery, idxManifest := -1, -1, -1
	for i, s := range OrderedSteps {
		switch s {
		case StepDaemon:
			idxDaemon = i
		case StepRecovery:
			idxRecovery = i
		case StepManifest:
			idxManifest = i
		}
	}
	if idxDaemon == -1 || idxRecovery == -1 || idxManifest == -1 {
		t.Fatalf("steps missing: daemon=%d recovery=%d manifest=%d ordered=%v", idxDaemon, idxRecovery, idxManifest, OrderedSteps)
	}
	if !(idxDaemon < idxRecovery && idxRecovery < idxManifest) {
		t.Fatalf("recovery not between daemon and manifest: %v", OrderedSteps)
	}
}

func pad16(s string) string {
	if len(s) >= 16 {
		return s
	}
	return s + strings.Repeat(" ", 16-len(s))
}

func TestPlainEmitterGolden(t *testing.T) {
	var buf bytes.Buffer
	emit := NewPlainEmitter(&buf, false)
	checks := []CheckResult{
		{Name: "os", Level: LevelPass, Message: "Debian 13"},
		{Name: "arch", Level: LevelPass, Message: "amd64"},
	}
	for _, c := range checks {
		emit(PreflightCheck{Result: c})
	}
	results := []RunResult{
		{Step: StepPreflight, Status: JournalCompleted},
		{Step: StepSSHKeys, Status: JournalCompleted},
		{Step: StepSSHDHardening, Status: JournalCompleted},
		{Step: StepSystemPrepare, Status: JournalCompleted},
		{Step: StepPackages, Status: JournalCompleted},
		{Step: StepBinaries, Status: JournalCompleted},
		{Step: StepFirewall, Status: JournalCompleted},
		{Step: StepServices, Status: JournalCompleted},
		{Step: StepDaemon, Status: JournalCompleted},
		{Step: StepRecovery, Status: JournalCompleted},
		{Step: StepManifest, Status: JournalCompleted},
	}
	for _, r := range results {
		emit(StepStarted{Step: r.Step})
		emit(StepFinished{Result: r})
	}
	got := buf.String()
	var wantBuf bytes.Buffer
	for _, c := range checks {
		var status string
		switch c.Level {
		case LevelPass:
			status = "PASS"
		case LevelWarn:
			status = "WARN"
		case LevelFail:
			status = "FAIL"
		}
		extra := ""
		if c.Dirty {
			extra = " [dirty]"
		}
		wantBuf.WriteString("  " + status + " " + pad16(c.Name) + " " + c.Message + extra + "\n")
		if c.Remediation != "" && c.Level == LevelFail {
			wantBuf.WriteString("       -> " + c.Remediation + "\n")
		}
	}
	for _, r := range results {
		wantBuf.WriteString("  [ok] " + r.Step + "\n")
		if r.Step == StepPreflight {
			wantBuf.WriteString("  Recommendation: enable LUKS on bare metal and encrypted Proxmox storage for VMs (DESIGN.md §9) — offline disk access bypasses OS controls.\n")
		}
	}
	want := wantBuf.String()
	if got != want {
		t.Fatalf("plain emitter mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestJSONEmitterOneObjectPerEvent(t *testing.T) {
	var buf bytes.Buffer
	emit := NewJSONEmitter(&buf)
	events := []Event{
		PreflightCheck{Result: CheckResult{Name: "os", Level: LevelPass, Message: "Debian 13"}},
		StepStarted{Step: StepPreflight},
		StepLog{Step: StepPackages, Line: "running apt update"},
		StepFinished{Result: RunResult{Step: StepPreflight, Status: JournalCompleted}},
		PromptNeeded{Kind: PromptKindRecoveryKey},
	}
	for _, e := range events {
		emit(e)
	}
	raw := strings.TrimSpace(buf.String())
	lines := strings.Split(raw, "\n")
	if len(lines) != len(events) {
		t.Fatalf("expected %d JSON lines, got %d: %q", len(events), len(lines), raw)
	}
	for i, line := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("line %d invalid JSON: %v %q", i, err, line)
		}
		if _, ok := m["type"]; !ok {
			t.Fatalf("line %d missing type: %q", i, line)
		}
		if got := m["type"].(string); got != EventTypeFor(events[i]) {
			t.Fatalf("line %d type %q want %q", i, got, EventTypeFor(events[i]))
		}
	}
}

func TestStepLogStreaming(t *testing.T) {
	var buf bytes.Buffer
	emit := NewPlainEmitter(&buf, false)
	emit(StepStarted{Step: StepPackages})
	emit(StepLog{Step: StepPackages, Line: "downloading tailscale keyring"})
	emit(StepLog{Step: StepPackages, Line: "running apt update"})
	emit(StepFinished{Result: RunResult{Step: StepPackages, Status: JournalCompleted}})
	got := buf.String()
	for _, want := range []string{"downloading tailscale keyring", "running apt update"} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected log %q in %q", want, got)
		}
	}
}

func TestRecoveryStepRefusesWithoutKey(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	if err := EnsureMigrations(ctx, db); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	var kinds []PromptKind
	emit := func(e Event) {
		if p, ok := e.(PromptNeeded); ok {
			kinds = append(kinds, p.Kind)
		}
	}
	svc := NewService(db, Probes{
		ReadFile: func(path string) ([]byte, error) {
			return bytes.Repeat([]byte("a"), 32), nil
		},
		WriteFile: func(path string, data []byte, perm uint32) error { return nil },
	})
	opts := InstallOptions{RecoveryKey: "", StateDir: t.TempDir(), Emit: emit}
	res := svc.runRecoveryStep(ctx, opts)
	if res.Status != JournalFailed {
		t.Fatalf("expected fail, got %+v", res)
	}
	if !strings.Contains(res.Error, "recovery key required") {
		t.Fatalf("unexpected error %q", res.Error)
	}
	if len(kinds) != 1 || kinds[0] != PromptKindRecoveryKey {
		t.Fatalf("expected PromptNeeded recovery_key, got %v", kinds)
	}
}

func TestRecoveryStepHappyPath(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	if err := EnsureMigrations(ctx, db); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	pub, _, err := secrets.GenerateAgeKeyPair()
	if err != nil {
		t.Fatalf("generate age: %v", err)
	}
	var writtenPath string
	var writtenData []byte
	svc := NewService(db, Probes{
		ReadFile: func(path string) ([]byte, error) {
			return bytes.Repeat([]byte("x"), 32), nil
		},
		WriteFile: func(path string, data []byte, perm uint32) error {
			writtenPath = path
			writtenData = append([]byte(nil), data...)
			if perm != 0o600 {
				t.Fatalf("perm %o", perm)
			}
			return nil
		},
	})
	stateDir := t.TempDir()
	opts := InstallOptions{RecoveryKey: pub, RecoveryPath: filepath.Join(stateDir, "recovery.age"), StateDir: stateDir}
	res := svc.runRecoveryStep(ctx, opts)
	if res.Status != JournalCompleted {
		t.Fatalf("expected success, got %+v", res)
	}
	if writtenPath != filepath.Join(stateDir, "recovery.age") {
		t.Fatalf("path %q", writtenPath)
	}
	if !bytes.Contains(writtenData, []byte("BEGIN AGE ENCRYPTED FILE")) {
		t.Fatalf("not armored: %q", string(writtenData[:100]))
	}
}

func TestPreflightLUKSRecommendation(t *testing.T) {
	var buf bytes.Buffer
	emit := NewPlainEmitter(&buf, false)
	emit(StepFinished{Result: RunResult{Step: StepPreflight, Status: JournalCompleted}})
	got := buf.String()
	if !strings.Contains(got, "LUKS") {
		t.Fatalf("missing LUKS in %q", got)
	}
	if !strings.Contains(got, "encrypted") {
		t.Fatalf("missing encrypted in %q", got)
	}
}

func TestServiceRunEmitsEvents(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	if err := EnsureMigrations(ctx, db); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	pub, _, err := secrets.GenerateAgeKeyPair()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	stateDir := t.TempDir()
	var events []Event
	emit := func(e Event) { events = append(events, e) }
	probes := Probes{
		OSRelease: func() (OSInfo, error) { return OSInfo{ID: "debian", VersionID: "13", Codename: "trixie"}, nil },
		Arch: func() (string, error) { return "amd64", nil },
		DirExists: func(p string) bool {
			if p == "/run/systemd/system" {
				return true
			}
			return false
		},
		DirNotEmpty: func(p string) (bool, error) { return false, nil },
		CommandExists: func(name string) bool {
			if name == "apt-get" {
				return true
			}
			return false
		},
		CommandOutput: func(ctx context.Context, name string, args ...string) (string, error) { return "", nil },
		RunningPids: func() ([]int, error) { return nil, nil },
		ProcessCmdline: func(pid int) (string, error) { return "", nil },
		ListeningPorts: func() ([]int, error) { return nil, nil },
		ServiceActive: func(name string) (bool, error) { return false, nil },
		APTSources: func() ([]AptSource, error) {
			return []AptSource{{File: "/etc/apt/sources.list", Line: "deb https://deb.debian.org/debian trixie main"}}, nil
		},
		MemInfo: func() (MemInfo, error) { return MemInfo{Total: 4 * 1024 * 1024 * 1024}, nil },
		DiskInfo: func(p string) (DiskInfo, error) {
			return DiskInfo{Total: 30 * 1024 * 1024 * 1024, Free: 10 * 1024 * 1024 * 1024}, nil
		},
		FileOwner: func(p string) (int, int, error) { return 0, 0, nil },
		Now: func() time.Time { return time.Now() },
		DNSLookup: func(ctx context.Context, host string) ([]string, error) { return []string{"1.2.3.4"}, nil },
		HTTPSGet: func(ctx context.Context, url string) (int, []byte, error) { return 200, []byte("ok"), nil },
		AuthorizedKeys: func(user string) (string, []string, error) {
			return "/root/.ssh/authorized_keys", []string{"ssh-ed25519 AAAAC3test root@host"}, nil
		},
		ActiveSSHSession: func() (bool, string, error) { return true, "1.2.3.4", nil },
		FileExists: func(path string) bool { return false },
		DownloadFile: func(ctx context.Context, url, destPath string) error { return nil },
		LookupUser: func(name string) (int, int, string, error) { return 1001, 1001, "/var/lib/omahab-builder", nil },
		Chown: func(path string, uid, gid int) error { return nil },
		ReadFile: func(path string) ([]byte, error) {
			if strings.Contains(path, "subuid") || strings.Contains(path, "subgid") {
				return nil, fmt.Errorf("no such file")
			}
			if strings.Contains(path, "master.key") {
				return bytes.Repeat([]byte("k"), 32), nil
			}
			if strings.Contains(path, "backup.env") {
				return nil, sql.ErrNoRows
			}
			if strings.Contains(path, "api.token") {
				return []byte("test-token-1234567890abcdef1234567890"), nil
			}
			return nil, sql.ErrNoRows
		},
		WriteFile: func(path string, data []byte, perm uint32) error { return nil },
		RemoveFile: func(path string) error { return nil },
		MkdirAll: func(path string, perm uint32) error { return nil },
		StatFile: func(path string) (bool, uint32, error) { return false, 0, nil },
		Systemctl: func(ctx context.Context, args ...string) (string, error) { return "", nil },
		APTRefresh: func(ctx context.Context) error { return nil },
		APTInstall: func(ctx context.Context, packages ...string) error { return nil },
		SSHDConfigTest: func(ctx context.Context) error { return nil },
		SSHDReload: func(ctx context.Context) error { return nil },
		WriteAuthorizedKeys: func(user, path string, keys []string) error { return nil },
		SecondSessionProbe: func(ctx context.Context) (bool, error) { return true, nil },
		ScheduleRollback: func(ctx context.Context, after time.Duration, restorePath string) error { return nil },
		CancelRollback: func(ctx context.Context) error { return nil },
		RollbackActive: func(ctx context.Context) (bool, error) { return false, nil },
		FetchGitHubKeys: func(ctx context.Context, username string) ([]string, error) {
			return []string{"ssh-ed25519 AAAAC3test"}, nil
		},
	}
	svc := NewService(db, probes)
	svc.SetAssets(fstest.MapFS{
		"bin/omahab":                            {Data: []byte("dummy")},
		"bin/omahabd":                           {Data: []byte("dummy")},
		"systemd/omahabd.service":               {Data: []byte("[Unit]\nDescription=test")},
		"systemd/omahab-builder.socket":         {Data: []byte("[Unit]\nDescription=test")},
		"systemd/omahab-builder.service":        {Data: []byte("[Unit]\nDescription=test")},
		"systemd/omahab-builder-prune.service": {Data: []byte("[Unit]\nDescription=test")},
		"systemd/omahab-builder-prune.timer":   {Data: []byte("[Unit]\nDescription=test")},
		"systemd/omahab-backup.service":         {Data: []byte("[Unit]\nDescription=test")},
		"systemd/omahab-backup.timer":           {Data: []byte("[Unit]\nDescription=test")},
		"systemd/omahab-verify.service":         {Data: []byte("[Unit]\nDescription=test")},
		"systemd/omahab-verify.timer":           {Data: []byte("[Unit]\nDescription=test")},
		"systemd/cloudflared.service":           {Data: []byte("[Unit]\nDescription=test")},
		"tmpfiles.d/omahab.conf":                {Data: []byte("test")},
		"catalog/catalog.json":                  {Data: []byte(`{"apps":[]}`)},
		"catalog/apps-catalog.json":             {Data: []byte(`{"bundles":[]}`)},
	})
	opts := InstallOptions{Version: "0.0.0-test", UntilStep: StepManifest, StateDir: stateDir, RecoveryKey: pub, Emit: emit}
	if _, err := svc.Run(ctx, opts); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var sawLog, sawStarted bool
	var sawFinished, sawPreflightCheck int
	for _, e := range events {
		switch e.(type) {
		case StepLog:
			sawLog = true
		case StepStarted:
			sawStarted = true
		case StepFinished:
			sawFinished++
		case PreflightCheck:
			sawPreflightCheck++
		}
	}
	if !sawStarted {
		t.Fatalf("no StepStarted")
	}
	if !sawLog {
		t.Fatalf("no StepLog, streaming not active")
	}
	if sawFinished == 0 {
		t.Fatalf("no StepFinished")
	}
	if sawPreflightCheck == 0 {
		t.Fatalf("no PreflightCheck")
	}
}
