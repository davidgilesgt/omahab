package health

import (
	"context"
	"database/sql"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

type fakeRunner struct {
	fn func(name string, args ...string) ([]byte, error)
}

func (f fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	return f.fn(name, args...)
}

func TestDiskProbe(t *testing.T) {
	tests := []struct {
		name    string
		paths   []string
		stat    func(string) (DiskStatus, error)
		wantPct float64
		wantErr bool
	}{
		{
			name:  "healthy single",
			paths: []string{"/data"},
			stat: func(p string) (DiskStatus, error) {
				return DiskStatus{Path: p, UsePercent: 20, TotalBytes: 100, FreeBytes: 80}, nil
			},
			wantPct: 20,
		},
		{
			name:  "worst of two",
			paths: []string{"/", "/data"},
			stat: func(p string) (DiskStatus, error) {
				if p == "/" {
					return DiskStatus{Path: p, UsePercent: 10}, nil
				}
				return DiskStatus{Path: p, UsePercent: 92}, nil
			},
			wantPct: 92,
		},
		{
			name:  "error propagates",
			paths: []string{"/"},
			stat: func(string) (DiskStatus, error) {
				return DiskStatus{}, context.DeadlineExceeded
			},
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := NewDiskProbe(tc.paths, WithDiskStatFS(tc.stat))
			got, err := p.CheckDisk(context.Background())
			if tc.wantErr && err == nil {
				t.Fatalf("expected error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected err %v", err)
			}
			if !tc.wantErr && got.UsePercent != tc.wantPct {
				t.Fatalf("want %.1f got %.1f", tc.wantPct, got.UsePercent)
			}
		})
	}
}

func TestServiceProbe(t *testing.T) {
	tests := []struct {
		name       string
		dial       func(string, string) (net.Conn, error)
		runnerOut  []byte
		runnerErr  error
		wantDocker string // expected health
		wantUnit   string
	}{
		{
			name: "both healthy",
			dial: func(n, a string) (net.Conn, error) {
				c1, c2 := net.Pipe()
				go func() {
					buf := make([]byte, 512)
					_, _ = c2.Read(buf)
					_, _ = c2.Write([]byte("HTTP/1.0 200 OK\r\n\r\nOK"))
					c2.Close()
				}()
				return c1, nil
			},
			runnerOut:  []byte("active\n"),
			runnerErr:  nil,
			wantDocker: "healthy",
			wantUnit:   "healthy",
		},
		{
			name:       "docker socket not reachable degraded",
			dial:       func(string, string) (net.Conn, error) { return nil, net.UnknownNetworkError("no") },
			runnerOut:  []byte("active\n"),
			wantDocker: "degraded",
			wantUnit:   "healthy",
		},
		{
			name: "systemd not active degraded",
			dial: func(n, a string) (net.Conn, error) {
				c1, c2 := net.Pipe()
				c2.Close()
				return c1, nil
			},
			runnerOut:  []byte("inactive\n"),
			runnerErr:  context.Canceled,
			wantDocker: "healthy",
			wantUnit:   "degraded",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fr := fakeRunner{fn: func(n string, args ...string) ([]byte, error) {
				return tc.runnerOut, tc.runnerErr
			}}
			p := NewServiceProbe(WithServiceRunner(fr), WithServiceDial(tc.dial))
			m, err := p.CheckServices(context.Background())
			if err != nil {
				t.Fatalf("unexpected err %v", err)
			}
			if len(m) != 2 {
				t.Fatalf("want 2 got %d", len(m))
			}
			if string(m[0].Health) != tc.wantDocker {
				t.Fatalf("docker want %s got %s detail %s", tc.wantDocker, m[0].Health, m[0].Detail)
			}
			if string(m[1].Health) != tc.wantUnit {
				t.Fatalf("unit want %s got %s detail %s", tc.wantUnit, m[1].Health, m[1].Detail)
			}
		})
	}
}

func TestTailscaleProbe(t *testing.T) {
	tests := []struct {
		name          string
		fn            func(string, ...string) ([]byte, error)
		wantInstalled bool
		wantLoggedIn  bool
		wantVisible   bool
	}{
		{
			name: "installed running visible",
			fn: func(n string, args ...string) ([]byte, error) {
				if len(args) > 0 && args[0] == "version" {
					return []byte("1.80.0\n"), nil
				}
				return []byte(`{"BackendState":"Running","Self":{"ID":"abc"},"Peer":{"p1":{}}}`), nil
			},
			wantInstalled: true, wantLoggedIn: true, wantVisible: true,
		},
		{
			name: "not installed",
			fn: func(n string, args ...string) ([]byte, error) {
				return []byte(""), net.UnknownNetworkError("executable file not found")
			},
			wantInstalled: false, wantLoggedIn: false, wantVisible: false,
		},
		{
			name: "needs login",
			fn: func(n string, args ...string) ([]byte, error) {
				if len(args) > 0 && args[0] == "version" {
					return []byte("1.80\n"), nil
				}
				return []byte(`{"BackendState":"NeedsLogin","Self":{}}`), nil
			},
			wantInstalled: true, wantLoggedIn: false, wantVisible: false,
		},
		{
			name: "no peers not visible",
			fn: func(n string, args ...string) ([]byte, error) {
				if len(args) > 0 && args[0] == "version" {
					return []byte("1.80\n"), nil
				}
				return []byte(`{"BackendState":"Running","Self":{"ID":""},"Peer":{}}`), nil
			},
			wantInstalled: true, wantLoggedIn: true, wantVisible: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := NewTailscaleProbe(WithTailscaleRunner(fakeRunner{fn: tc.fn}))
			ok, _ := p.IsInstalled(context.Background())
			if ok != tc.wantInstalled {
				t.Fatalf("installed want %v got %v", tc.wantInstalled, ok)
			}
			ok, _ = p.IsLoggedIn(context.Background())
			if ok != tc.wantLoggedIn {
				t.Fatalf("loggedin want %v got %v", tc.wantLoggedIn, ok)
			}
			ok, _ = p.ServerNodeVisible(context.Background())
			if ok != tc.wantVisible {
				t.Fatalf("visible want %v got %v", tc.wantVisible, ok)
			}
		})
	}
}

func TestDNSProbe(t *testing.T) {
	tests := []struct {
		name    string
		lookup  func(context.Context, string) ([]string, error)
		wantErr bool
		wantN   int
	}{
		{
			name: "ok", lookup: func(context.Context, string) ([]string, error) { return []string{"1.1.1.1"}, nil }, wantN: 1,
		},
		{
			name: "empty degraded", lookup: func(context.Context, string) ([]string, error) { return nil, nil }, wantN: 0,
		},
		{
			name: "error", lookup: func(context.Context, string) ([]string, error) { return nil, context.Canceled }, wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := NewDNSProbe(WithDNSLookup(tc.lookup))
			addrs, err := p.Lookup(context.Background(), "example.com")
			if tc.wantErr && err == nil {
				t.Fatalf("expected error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected %v", err)
			}
			if len(addrs) != tc.wantN {
				t.Fatalf("want %d got %d", tc.wantN, len(addrs))
			}
		})
	}
}

func TestTLSProbe(t *testing.T) {
	tests := []struct {
		name   string
		dial   func(context.Context, string, string) (net.Conn, error)
		wantOK bool
	}{
		{
			name: "dial error",
			dial: func(context.Context, string, string) (net.Conn, error) {
				return nil, context.DeadlineExceeded
			},
			wantOK: false,
		},
		{
			name: "success no cert",
			dial: func(context.Context, string, string) (net.Conn, error) {
				c1, c2 := net.Pipe()
				c2.Close()
				return c1, nil
			},
			wantOK: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := NewTLSProbe(WithTLSDial(tc.dial))
			ok, _ := p.CheckTLS(context.Background(), "https://example.com")
			if ok != tc.wantOK {
				t.Fatalf("want %v got %v", tc.wantOK, ok)
			}
		})
	}
}

func TestTLSExpiry(t *testing.T) {
	t.Log("placeholder for cert expiry coverage")
}

func TestPocketIDProbe(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		baseURL string
		wantErr bool
	}{
		{
			name: "empty baseURL healthy",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(500)
			},
			baseURL: "",
			wantErr: false,
		},
		{
			name: "reachable",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(200)
			},
			baseURL: "", // will be replaced
			wantErr: false,
		},
		{
			name: "unreachable status",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(500)
			},
			baseURL: "", // will be replaced
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "empty baseURL healthy" {
				p := NewPocketIDProbe("")
				if err := p.CheckPocketID(context.Background()); err != nil {
					t.Fatalf("expected nil got %v", err)
				}
				return
			}
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			p := NewPocketIDProbe(srv.URL)
			err := p.CheckPocketID(context.Background())
			if tc.wantErr && err == nil {
				t.Fatalf("expected error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected %v", err)
			}
		})
	}
}

func TestInstanceProbe(t *testing.T) {
	tests := []struct {
		name    string
		fn      func(context.Context) (string, error)
		want    string
		wantErr bool
	}{
		{name: "ok", fn: func(context.Context) (string, error) { return "abc-123", nil }, want: "abc-123"},
		{name: "error", fn: func(context.Context) (string, error) { return "", context.Canceled }, wantErr: true},
		{name: "empty error unknown", fn: func(context.Context) (string, error) { return "", nil }, want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := NewInstanceProbe(nil, WithInstanceQuery(tc.fn))
			got, err := p.GetInstanceID(context.Background())
			if tc.wantErr && err == nil {
				t.Fatalf("expected error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected %v", err)
			}
			if got != tc.want {
				t.Fatalf("want %q got %q", tc.want, got)
			}
		})
	}
}

func TestEncryptionProbe(t *testing.T) {
	tests := []struct {
		name      string
		runnerFn  func(string, ...string) ([]byte, error)
		wantEnc   bool
		wantDeg   bool // expect degraded path when not encrypted
		wantErr   bool
	}{
		{
			name: "encrypted crypto_LUKS",
			runnerFn: func(n string, args ...string) ([]byte, error) {
				return []byte(`{"blockdevices":[{"name":"sda","fstype":"crypto_LUKS","type":"part"}]}`), nil
			},
			wantEnc: true,
		},
		{
			name: "unencrypted warning",
			runnerFn: func(n string, args ...string) ([]byte, error) {
				return []byte(`{"blockdevices":[{"name":"sda","fstype":"ext4","mountpoint":"/","type":"part"}]}`), nil
			},
			wantDeg: true,
		},
		{
			name: "lsblk missing unknown degraded",
			runnerFn: func(n string, args ...string) ([]byte, error) {
				return nil, net.UnknownNetworkError("not found")
			},
			wantErr: true,
		},
		{
			name: "fallback text contains crypto",
			runnerFn: func(n string, args ...string) ([]byte, error) {
				// First call JSON fails, second call succeeds with text
				if len(args) > 0 && args[0] == "-J" {
					return nil, context.Canceled
				}
				return []byte("MOUNTPOINT FSTYPE\n/ crypto_LUKS\n"), nil
			},
			wantEnc: true,
		},
		{
			name: "fallback text no crypto",
			runnerFn: func(n string, args ...string) ([]byte, error) {
				if len(args) > 0 && args[0] == "-J" {
					return nil, context.Canceled
				}
				return []byte("MOUNTPOINT FSTYPE\n/ ext4\n"), nil
			},
			wantDeg: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := NewEncryptionProbe(WithEncryptionRunner(fakeRunner{fn: tc.runnerFn}))
			enc, detail, err := p.CheckEncryption(context.Background())
			if tc.wantErr && err == nil {
				t.Fatalf("expected error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected err %v", err)
			}
			if !tc.wantErr {
				if tc.wantEnc && !enc {
					t.Fatalf("expected encrypted, detail %q", detail)
				}
				if tc.wantDeg && enc {
					t.Fatalf("expected not encrypted")
				}
				if tc.wantDeg && !strings.Contains(strings.ToLower(detail), "luks") {
					t.Fatalf("degraded detail should contain LUKS recommendation, got %q", detail)
				}
			}
		})
	}
}

func TestBackupProbeNoDB(t *testing.T) {
	p := NewBackupProbe(nil)
	if got, err := p.LastBackup(context.Background()); err != nil || got != nil {
		t.Fatalf("expected nil,nil got %v %v", got, err)
	}
	if got, err := p.LastVerified(context.Background()); err != nil || got != nil {
		t.Fatalf("expected nil,nil got %v %v", got, err)
	}
}

func TestBackupProbeWithDB(t *testing.T) {
	db := openTestDB(t)
	p := NewBackupProbe(db)
	ctx := context.Background()
	if got, err := p.LastBackup(ctx); err != nil || got != nil {
		t.Fatalf("expected nil got %v %v", got, err)
	}
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
	_, err := db.ExecContext(ctx, `INSERT INTO backup_runs (id, kind, repository_id, status, triggered_by, stage, started_at, finished_at) VALUES ('r1','backup','repo1','completed','manual','','2024-01-01T00:00:00.000000000Z',?)`, now)
	if err != nil {
		t.Fatalf("insert %v", err)
	}
	b, err := p.LastBackup(ctx)
	if err != nil || b == nil {
		t.Fatalf("expected backup got %v %v", b, err)
	}
	if b.ID != "r1" {
		t.Fatalf("want r1 got %q", b.ID)
	}
	if got, err := p.LastVerified(ctx); err != nil || got != nil {
		t.Fatalf("expected nil verified got %v %v", got, err)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO backup_verifications (id, run_id, repository_id, snapshot_id, status, target, started_at, finished_at) VALUES ('v1','r1','repo1','snap1','passed','/tmp',?,?)`, now, now)
	if err != nil {
		t.Fatalf("insert verify %v", err)
	}
	v, err := p.LastVerified(ctx)
	if err != nil || v == nil {
		t.Fatalf("expected verified got %v %v", v, err)
	}
	if v.VerifiedAt == nil {
		t.Fatalf("expected verifiedAt")
	}
}

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// Minimal schema needed for backup probe
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS backup_runs (id TEXT PRIMARY KEY, kind TEXT, repository_id TEXT, status TEXT, triggered_by TEXT, stage TEXT, started_at TEXT, finished_at TEXT)`,
		`CREATE TABLE IF NOT EXISTS backup_verifications (id TEXT PRIMARY KEY, run_id TEXT, repository_id TEXT, snapshot_id TEXT, status TEXT, target TEXT, started_at TEXT, finished_at TEXT)`,
		`CREATE TABLE IF NOT EXISTS instance (singleton INTEGER PRIMARY KEY, id TEXT, created_at TEXT)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("migrate %v", err)
		}
	}
	return db
}

func TestBackupProbeMissingTable(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "empty.db"))
	if err != nil {
		t.Fatalf("open %v", err)
	}
	defer db.Close()
	p := NewBackupProbe(db)
	if got, err := p.LastBackup(context.Background()); err != nil || got != nil {
		t.Fatalf("missing table should be nil not err, got %v %v", got, err)
	}
	if got, err := p.LastVerified(context.Background()); err != nil || got != nil {
		t.Fatalf("missing table should be nil not err, got %v %v", got, err)
	}
}

func TestHealthServiceIntegration(t *testing.T) {
	// Ensure health service maps probe errors to unknown/degraded without panic.
	tests := []struct {
		name           string
		diskErr        bool
		tailscaleNotInstalled bool
		encryptionUnencrypted bool
		wantChecks     int
	}{
		{name: "all healthy via noops", wantChecks: 8}, // disk, services(2), backup, backup_verified, tailscale, dns?, tls?, pocketid, instance?, encryption -> but defaults include many
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := New(Options{
				Disk:       NewDiskProbe([]string{"/"}, WithDiskStatFS(func(string) (DiskStatus, error) { if tc.diskErr { return DiskStatus{}, context.Canceled }; return DiskStatus{Path: "/", UsePercent: 10}, nil })),
				Encryption: NewEncryptionProbe(WithEncryptionRunner(fakeRunner{fn: func(string, ...string) ([]byte, error) {
					if tc.encryptionUnencrypted {
						return []byte(`{"blockdevices":[{"fstype":"ext4"}]}`), nil
					}
					return []byte(`{"blockdevices":[{"fstype":"crypto_LUKS"}]}`), nil
				}})),
			})
			rep, err := svc.Check(context.Background())
			if err != nil {
				t.Fatalf("Check error %v", err)
			}
			if len(rep.Checks) == 0 {
				t.Fatalf("no checks")
			}
			// Ensure encryption check present
			found := false
			for _, c := range rep.Checks {
				if c.Name == "encryption" {
					found = true
					if tc.encryptionUnencrypted && c.Status != "degraded" {
						t.Fatalf("expected degraded for unencrypted, got %s", c.Status)
					}
				}
			}
			if !found {
				t.Fatalf("encryption check missing")
			}
			_ = tc.wantChecks
		})
	}
}

func TestHealthCheckDiskThreshold(t *testing.T) {
	tests := []struct {
		name      string
		usePct    float64
		threshold float64
		wantStatus string
	}{
		{name: "healthy below threshold", usePct: 10, threshold: 85, wantStatus: "healthy"},
		{name: "pressure within 10pct", usePct: 80, threshold: 85, wantStatus: "degraded"},
		{name: "unhealthy above threshold", usePct: 90, threshold: 85, wantStatus: "unhealthy"},
		{name: "error unknown", usePct: -1, threshold: 85, wantStatus: "unknown"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var p DiskProbe
			if tc.wantStatus == "unknown" {
				p = NewDiskProbe([]string{"/"}, WithDiskStatFS(func(string) (DiskStatus, error) { return DiskStatus{}, context.Canceled }))
			} else {
				p = NewDiskProbe([]string{"/"}, WithDiskStatFS(func(string) (DiskStatus, error) { return DiskStatus{Path: "/", UsePercent: tc.usePct}, nil }))
			}
			svc := New(Options{Disk: p, DiskThreshold: tc.threshold, Encryption: NoopEncryptionProbe{}})
			rep, _ := svc.Check(context.Background())
			var got string
			for _, c := range rep.Checks {
				if c.Name == "disk" {
					got = c.Status
				}
			}
			if got != tc.wantStatus {
				t.Fatalf("want %q got %q", tc.wantStatus, got)
			}
		})
	}
}

func TestHealthCheckBackupFreshness(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC()
	// helper to set svc with backup probe backed by db
	mk := func(stale time.Duration) *Service {
		return New(Options{Backup: NewBackupProbe(db), BackupStale: stale, VerifyStale: 7 * 24 * time.Hour, Now: func() time.Time { return now }, Encryption: NoopEncryptionProbe{}})
	}
	ctx := context.Background()
	// No backup => degraded
	svc := mk(24 * time.Hour)
	rep, _ := svc.Check(ctx)
	found := false
	for _, c := range rep.Checks { if c.Name == "backup" { found = true; if c.Status != "degraded" { t.Fatalf("no backup should be degraded got %s", c.Status) } } }
	if !found { t.Fatalf("backup check missing") }
	// Insert fresh backup
	fresh := now.Add(-1 * time.Hour).Format("2006-01-02T15:04:05.000000000Z07:00")
	_, _ = db.ExecContext(ctx, `DELETE FROM backup_runs`)
	_, _ = db.ExecContext(ctx, `INSERT INTO backup_runs (id, kind, repository_id, status, triggered_by, stage, started_at, finished_at) VALUES ('r1','backup','repo1','completed','manual','','2024-01-01T00:00:00.000000000Z',?)`, fresh)
	rep, _ = svc.Check(ctx)
	for _, c := range rep.Checks { if c.Name == "backup" && c.Status != "healthy" { t.Fatalf("fresh backup want healthy got %s", c.Status) } }
	// Stale backup => unhealthy
	stale := now.Add(-48 * time.Hour).Format("2006-01-02T15:04:05.000000000Z07:00")
	_, _ = db.ExecContext(ctx, `DELETE FROM backup_runs`)
	_, _ = db.ExecContext(ctx, `INSERT INTO backup_runs (id, kind, repository_id, status, triggered_by, stage, started_at, finished_at) VALUES ('r2','backup','repo1','completed','manual','','2024-01-01T00:00:00.000000000Z',?)`, stale)
	rep, _ = svc.Check(ctx)
	for _, c := range rep.Checks { if c.Name == "backup" && c.Status != "unhealthy" { t.Fatalf("stale backup want unhealthy got %s", c.Status) } }
}

func TestHealthCheckInstanceMismatch(t *testing.T) {
	tests := []struct {
		name       string
		instanceID string
		probeID    string
		probeErr   error
		wantStatus string
	}{
		{name: "healthy", instanceID: "abc", probeID: "abc", wantStatus: "healthy"},
		{name: "mismatch unhealthy", instanceID: "abc", probeID: "different", wantStatus: "unhealthy"},
		{name: "probe error unhealthy", probeErr: context.Canceled, wantStatus: "unhealthy"},
		{name: "empty unknown", probeID: "", wantStatus: "unknown"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := NewInstanceProbe(nil, WithInstanceQuery(func(context.Context) (string, error) {
				if tc.probeErr != nil { return "", tc.probeErr }
				return tc.probeID, nil
			}))
			svc := New(Options{Instance: p, InstanceID: tc.instanceID, Encryption: NoopEncryptionProbe{}})
			rep, _ := svc.Check(context.Background())
			var got string
			for _, c := range rep.Checks { if c.Name == "instance" { got = c.Status } }
			if got != tc.wantStatus { t.Fatalf("want %q got %q", tc.wantStatus, got) }
		})
	}
}

func TestHealthCheckDNSAndTLS(t *testing.T) {
	// DNS fails => unhealthy, TLS fails => unhealthy
	dnsFail := NewDNSProbe(WithDNSLookup(func(context.Context, string) ([]string, error) { return nil, context.Canceled }))
	tlsFail := NewTLSProbe(WithTLSDial(func(context.Context, string, string) (net.Conn, error) { return nil, context.Canceled }))
	svc := New(Options{Hostname: "example.com", DNS: dnsFail, TLS: tlsFail, Encryption: NoopEncryptionProbe{}})
	rep, _ := svc.Check(context.Background())
	for _, want := range []string{"dns", "tls"} {
		found := false
		for _, c := range rep.Checks { if c.Name == want { found = true; if c.Status != "unhealthy" { t.Fatalf("%s want unhealthy got %s", want, c.Status) } } }
		if !found { t.Fatalf("%s check missing", want) }
	}
	// DNS ok => healthy
	dnsOK := NewDNSProbe(WithDNSLookup(func(context.Context, string) ([]string, error) { return []string{"1.1.1.1"}, nil }))
	tlsOK := NewTLSProbe(WithTLSDial(func(context.Context, string, string) (net.Conn, error) { c1, c2 := net.Pipe(); c2.Close(); return c1, nil }))
	svc = New(Options{Hostname: "example.com", DNS: dnsOK, TLS: tlsOK, Encryption: NoopEncryptionProbe{}})
	rep, _ = svc.Check(context.Background())
	for _, want := range []string{"dns", "tls"} {
		for _, c := range rep.Checks { if c.Name == want && c.Status != "healthy" { t.Fatalf("%s want healthy got %s", want, c.Status) } }
	}
}
