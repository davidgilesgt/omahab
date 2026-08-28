package installer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func newFirewallService(t *testing.T, probes Probes) *Service {
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

// ---------------------------------------------------------------------------
// Template seam
// ---------------------------------------------------------------------------

func TestFirewallNftablesConfTemplate(t *testing.T) {
	t.Parallel()
	conf := NftablesConf()

	mustContain := []string{
		`#!/usr/sbin/nft -f`,
		`# Managed by Omahab installer. Default-deny inbound; outbound unaffected.`,
		`destroy table inet omahab`,
		`table inet omahab {`,
		`chain input {`,
		`type filter hook input priority 10; policy drop;`,
		`iifname "lo" accept`,
		`ct state invalid drop`,
		`ct state established,related accept`,
		`tcp dport 22 accept comment "ssh"`,
		`udp dport 41641 accept comment "tailscale direct"`,
		`iifname "tailscale0" tcp dport 8484 accept comment "omahab dashboard via tailscale"`,
		`iifname "tailscale0" tcp dport { 80, 443 } accept comment "caddy https via tailscale"`,
		`icmp type { destination-unreachable, time-exceeded, parameter-problem, echo-request } limit rate 10/second accept`,
		`ip6 nexthdr ipv6-icmp icmpv6 type { destination-unreachable, time-exceeded, parameter-problem, echo-request, nd-router-advert, nd-neighbor-solicit, nd-neighbor-advert } limit rate 20/second accept`,
	}
	for _, s := range mustContain {
		if !strings.Contains(conf, s) {
			t.Fatalf("NftablesConf missing required line %q\nfull conf:\n%s", s, conf)
		}
	}
	if strings.Contains(conf, "flush ruleset") {
		t.Fatalf("NftablesConf must not contain 'flush ruleset' (would destroy Docker rules)")
	}
	if strings.Contains(conf, "destroy ruleset") {
		t.Fatalf("NftablesConf must not contain 'destroy ruleset' (would destroy Docker rules)")
	}
	// only destroy our table (safe re-apply); never flush/destroy ruleset
	if !strings.Contains(conf, "destroy table inet omahab") {
		t.Fatalf("NftablesConf must destroy only inet omahab")
	}
	if strings.Contains(conf, "flush table inet omahab") {
		t.Fatalf("NftablesConf should use destroy, not flush, for initial absent-table tolerance")
	}
	// ensure exactly one destroy directive for our table and no flush directives
	countDestroy := strings.Count(conf, "destroy")
	if countDestroy != 1 {
		t.Fatalf("expected exactly one destroy directive, got %d", countDestroy)
	}
	if strings.Contains(conf, "flush") {
		t.Fatalf("NftablesConf must not contain any flush directive (use destroy)")
	}
	if strings.Contains(conf, "policy drop") == false {
		t.Fatalf("missing policy drop")
	}
	// Security invariant: TCP 8484 must be admitted only on tailscale0;
	// public interfaces must remain blocked. Loopback reachability is via
	// iifname "lo" accept, not a public 8484 rule.
	if !strings.Contains(conf, `iifname "tailscale0" tcp dport 8484`) {
		t.Fatalf("NftablesConf must gate 8484 on tailscale0, got:\n%s", conf)
	}
	if !strings.Contains(conf, `iifname "lo" accept`) {
		t.Fatalf("NftablesConf must allow loopback via iifname lo")
	}
	// No unrestricted 8484 accept. Every tcp dport 8484 line must be gated by tailscale0.
	total := strings.Count(conf, "tcp dport 8484")
	gated := strings.Count(conf, `iifname "tailscale0" tcp dport 8484`)
	if total != gated || total == 0 {
		t.Fatalf("NftablesConf must contain only tailscale0-gated 8484 rules: total %d gated %d\nconf:\n%s", total, gated, conf)
	}
	if !strings.Contains(conf, `iifname "tailscale0" tcp dport { 80, 443 } accept comment "caddy https via tailscale"`) {
		t.Fatalf("NftablesConf must contain tailscale0-gated 80/443 rule, got:\n%s", conf)
	}
	// Every tcp 80/443 occurrence must be gated by tailscale0 (no public accept).
	for _, line := range strings.Split(conf, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.Contains(line, "tcp dport") && (strings.Contains(line, " 80") || strings.Contains(line, " 443") || strings.Contains(line, "{ 80")) {
			if !strings.Contains(line, `iifname "tailscale0"`) {
				t.Fatalf("NftablesConf must contain only tailscale0-gated 80/443 rules: ungated line %q\nconf:\n%s", line, conf)
			}
		}
	}
	if strings.Contains(conf, "0.0.0.0:8484") {
		t.Fatalf("NftablesConf must not contain 0.0.0.0:8484 literal (listen address is systemd, not firewall)")
	}
	// Ensure no forward/output chain touched
	if strings.Contains(conf, "chain forward") || strings.Contains(conf, "chain output") {
		t.Fatalf("must not touch forward/output chains")
	}
	if !strings.HasSuffix(conf, "\n") {
		t.Fatalf("NftablesConf should end with newline")
	}
}

func TestPackagedOmahabdServiceListensOnIPv4Wildcard(t *testing.T) {
	t.Parallel()
	// Locate deploy/systemd/omahabd.service relative to repo root.
	// When running `go test ./...` the working directory is the package
	// directory, so we try a few relative candidates and also an absolute
	// fallback via git root.
	candidates := []string{
		"../../deploy/systemd/omahabd.service",
		"../../../deploy/systemd/omahabd.service",
		"deploy/systemd/omahabd.service",
	}
	var data []byte
	var lastErr error
	for _, p := range candidates {
		b, err := os.ReadFile(p)
		if err == nil {
			data = b
			lastErr = nil
			break
		}
		lastErr = err
	}
	if data == nil {
		// Last resort: try to find repo root via git.
		t.Fatalf("cannot locate omahabd.service (tried %v): %v", candidates, lastErr)
	}
	content := string(data)
	if !strings.Contains(content, "Environment=OMAHAB_LISTEN=0.0.0.0:8484") {
		t.Fatalf("packaged omahabd.service must set Environment=OMAHAB_LISTEN=0.0.0.0:8484; got:\n%s", content)
	}
	if strings.Contains(content, "Environment=OMAHAB_LISTEN=127.0.0.1") {
		t.Fatalf("packaged omahabd.service must not set loopback OMAHAB_LISTEN; got:\n%s", content)
	}
	if strings.Count(content, "OMAHAB_LISTEN") != 1 {
		t.Fatalf("packaged omahabd.service must contain exactly one OMAHAB_LISTEN, got %d", strings.Count(content, "OMAHAB_LISTEN"))
	}
	// The service must still contain the nftables boundary comment so the
	// security invariant is visible next to the bind address.
	if !strings.Contains(content, "tailscale0") {
		t.Fatalf("packaged omahabd.service should document tailscale0 admission boundary near OMAHAB_LISTEN; got:\n%s", content)
	}
	if !strings.Contains(content, `iifname "lo"`) && !strings.Contains(content, "lo may reach 8484") {
		// Accept either the nft comment phrasing or a direct iifname lo hint;
		// the firewall file is the source of truth, but the service should
		// at least mention that loopback remains usable.
		t.Fatalf("packaged omahabd.service should mention loopback remains usable; got:\n%s", content)
	}
	// Ensure no public 8484 accept is implied by binding; we intentionally
	// bind wildcard but gate via nftables — the service must not add an
	// unrestricted firewall rule itself.
	if strings.Contains(content, "0.0.0.0:8484") && !strings.Contains(content, "nftables") {
		t.Fatalf("packaged service binds 0.0.0.0:8484 but should reference nftables as admission boundary")
	}
}

// ---------------------------------------------------------------------------
// Helpers for fake filesystem probes
// ---------------------------------------------------------------------------

type fakeFS struct {
	files map[string][]byte
	order *[]string
}

func (f *fakeFS) exists(path string) bool {
	if f.order != nil {
		*f.order = append(*f.order, "exists:"+path)
	}
	_, ok := f.files[path]
	return ok
}
func (f *fakeFS) read(path string) ([]byte, error) {
	if f.order != nil {
		*f.order = append(*f.order, "read:"+path)
	}
	b, ok := f.files[path]
	if !ok {
		return nil, fmt.Errorf("file not found: %s", path)
	}
	cp := make([]byte, len(b))
	copy(cp, b)
	return cp, nil
}
func (f *fakeFS) write(path string, data []byte, perm uint32) error {
	if f.order != nil {
		*f.order = append(*f.order, fmt.Sprintf("write:%s:%d", path, perm))
	}
	if f.files == nil {
		f.files = make(map[string][]byte)
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	f.files[path] = cp
	return nil
}
func (f *fakeFS) remove(path string) error {
	if f.order != nil {
		*f.order = append(*f.order, "remove:"+path)
	}
	delete(f.files, path)
	return nil
}

func indexOf(order []string, substr string) int {
	for i, s := range order {
		if strings.Contains(s, substr) {
			return i
		}
	}
	return -1
}

// ---------------------------------------------------------------------------
// Happy path: backup -> write(temp) -> validate -> write(final) -> enable/start
// ---------------------------------------------------------------------------

func TestFirewallHappyPathOrdering(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var order []string
	fs := &fakeFS{
		files: map[string][]byte{
			nftConfPath: []byte("old existing conf"),
		},
		order: &order,
	}
	var syscalls [][]string
	var cmdCalls [][]string

	svc := newFirewallService(t, Probes{
		FileExists: func(path string) bool { return fs.exists(path) },
		ReadFile:   func(path string) ([]byte, error) { return fs.read(path) },
		WriteFile:  func(path string, data []byte, perm uint32) error { return fs.write(path, data, perm) },
		RemoveFile: func(path string) error { return fs.remove(path) },
		CommandOutput: func(_ context.Context, name string, args ...string) (string, error) {
			cp := append([]string{name}, args...)
			cmdCalls = append(cmdCalls, cp)
			order = append(order, fmt.Sprintf("cmd:%s:%s", name, strings.Join(args, " ")))
			return "", nil
		},
		Systemctl: func(_ context.Context, args ...string) (string, error) {
			cp := append([]string(nil), args...)
			syscalls = append(syscalls, cp)
			order = append(order, "systemctl:"+strings.Join(args, " "))
			return "", nil
		},
		ServiceActive: func(name string) (bool, error) { return false, nil },
	})

	res := svc.runFirewallStep(ctx, InstallOptions{})
	if res.Status != JournalCompleted {
		t.Fatalf("status %q want %q error %q order %v", res.Status, JournalCompleted, res.Error, order)
	}
	// backup must exist
	if _, ok := fs.files[nftBackupPath]; !ok {
		t.Fatalf("backup not created, order %v", order)
	}
	if string(fs.files[nftBackupPath]) != "old existing conf" {
		t.Fatalf("backup content = %q want %q", string(fs.files[nftBackupPath]), "old existing conf")
	}
	// final conf must be desired
	if string(fs.files[nftConfPath]) != NftablesConf() {
		t.Fatalf("final conf mismatch")
	}
	// ordering checks: backup write before temp write before validate before final write before enable/start
	backupIdx := indexOf(order, "write:"+nftBackupPath+":")
	tempIdx := indexOf(order, "write:"+nftTempPath+":")
	validateIdx := indexOf(order, "cmd:nft")
	// final write is exact write of nftConfPath (not backup/temp)
	finalIdx := indexOf(order, "write:"+nftConfPath+":")
	enableIdx := indexOf(order, "systemctl:enable")
	startIdx := indexOf(order, "systemctl:start")
	if startIdx == -1 {
		startIdx = indexOf(order, "systemctl:restart")
	}
	if backupIdx == -1 {
		t.Fatalf("backup write missing order %v", order)
	}
	if tempIdx == -1 {
		t.Fatalf("temp write missing order %v", order)
	}
	if validateIdx == -1 {
		t.Fatalf("validate missing order %v", order)
	}
	if finalIdx == -1 {
		t.Fatalf("final write missing order %v", order)
	}
	if enableIdx == -1 || startIdx == -1 {
		t.Fatalf("systemctl calls missing order %v syscalls %v", order, syscalls)
	}
	if !(backupIdx < tempIdx && tempIdx < validateIdx && validateIdx < finalIdx && finalIdx < enableIdx && enableIdx < startIdx) {
		t.Fatalf("ordering violated: backup %d temp %d validate %d final %d enable %d start %d order %v", backupIdx, tempIdx, validateIdx, finalIdx, enableIdx, startIdx, order)
	}
	// validate must be on temp path when content changed
	foundTempValidate := false
	for _, c := range cmdCalls {
		if len(c) >= 4 && c[0] == "nft" && c[1] == "-c" && c[2] == "-f" && c[3] == nftTempPath {
			foundTempValidate = true
		}
		if len(c) >= 4 && c[3] == nftConfPath && strings.Contains(strings.Join(c, " "), nftConfPath) {
			// should not validate final path directly when changed; only temp
			// but we allow final path not validated directly (temp only)
		}
	}
	if !foundTempValidate {
		t.Fatalf("expected validation of temp path %q, got calls %v", nftTempPath, cmdCalls)
	}
	// ensure enable/start args correct
	if len(syscalls) < 2 {
		t.Fatalf("expected at least 2 systemctl calls, got %v", syscalls)
	}
	if syscalls[0][0] != "enable" || syscalls[0][1] != "nftables" {
		t.Fatalf("first systemctl want enable nftables, got %v", syscalls[0])
	}
	if syscalls[1][0] != "start" && syscalls[1][0] != "restart" {
		t.Fatalf("second systemctl want start/restart nftables, got %v", syscalls[1])
	}
}

// Happy path with no existing file (no backup)

func TestFirewallHappyPathNoExistingFile(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var order []string
	fs := &fakeFS{files: map[string][]byte{}, order: &order}
	var cmdCalls [][]string
	var syscalls [][]string

	svc := newFirewallService(t, Probes{
		FileExists: func(path string) bool { return fs.exists(path) },
		ReadFile:   func(path string) ([]byte, error) { return fs.read(path) },
		WriteFile:  func(path string, data []byte, perm uint32) error { return fs.write(path, data, perm) },
		RemoveFile: func(path string) error { return fs.remove(path) },
		CommandOutput: func(_ context.Context, name string, args ...string) (string, error) {
			cp := append([]string{name}, args...)
			cmdCalls = append(cmdCalls, cp)
			order = append(order, "cmd:"+name)
			return "", nil
		},
		Systemctl: func(_ context.Context, args ...string) (string, error) {
			syscalls = append(syscalls, append([]string(nil), args...))
			order = append(order, "systemctl:"+strings.Join(args, " "))
			return "", nil
		},
	})

	res := svc.runFirewallStep(ctx, InstallOptions{})
	if res.Status != JournalCompleted {
		t.Fatalf("status %q error %q", res.Status, res.Error)
	}
	if _, ok := fs.files[nftBackupPath]; ok {
		t.Fatalf("backup should not be created when no existing file")
	}
	if string(fs.files[nftConfPath]) != NftablesConf() {
		t.Fatalf("final conf not written")
	}
	if len(cmdCalls) != 1 {
		t.Fatalf("expected 1 validate call, got %v", cmdCalls)
	}
	if cmdCalls[0][3] != nftTempPath {
		t.Fatalf("validate should be temp path when file missing, got %v", cmdCalls[0])
	}
	if len(syscalls) != 2 {
		t.Fatalf("expected enable+start, got %v", syscalls)
	}
}

// ---------------------------------------------------------------------------
// Existing identical conf skips write
// ---------------------------------------------------------------------------

func TestFirewallExistingIdenticalSkipsWrite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	desired := NftablesConf()
	var order []string
	fs := &fakeFS{
		files: map[string][]byte{nftConfPath: []byte(desired)},
		order: &order,
	}
	var writes []string
	var cmds [][]string
	var syscalls [][]string

	svc := newFirewallService(t, Probes{
		FileExists: func(path string) bool { return fs.exists(path) },
		ReadFile:   func(path string) ([]byte, error) { return fs.read(path) },
		WriteFile: func(path string, data []byte, perm uint32) error {
			writes = append(writes, path)
			return fs.write(path, data, perm)
		},
		CommandOutput: func(_ context.Context, name string, args ...string) (string, error) {
			cmds = append(cmds, append([]string{name}, args...))
			order = append(order, "cmd")
			return "", nil
		},
		Systemctl: func(_ context.Context, args ...string) (string, error) {
			syscalls = append(syscalls, append([]string(nil), args...))
			return "", nil
		},
		ServiceActive:  func(name string) (bool, error) { return true, nil },
		ServiceEnabled: func(name string) (bool, error) { return true, nil },
	})

	res := svc.runFirewallStep(ctx, InstallOptions{})
	if res.Status != JournalCompleted {
		t.Fatalf("status %q error %q", res.Status, res.Error)
	}
	if len(writes) != 0 {
		t.Fatalf("expected no writes when identical, got %v order %v", writes, order)
	}
	// still must validate existing file
	if len(cmds) != 1 {
		t.Fatalf("expected 1 validate for identical, got %v", cmds)
	}
	if cmds[0][3] != nftConfPath {
		t.Fatalf("identical should validate existing file %q, got %v", nftConfPath, cmds[0])
	}
	// skip re-enable when active+enabled
	if len(syscalls) != 0 {
		t.Fatalf("expected no systemctl when identical and active+enabled, got %v", syscalls)
	}
}

func TestFirewallExistingIdenticalValidatesAndEnablesWhenNotActive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	desired := NftablesConf()
	fs := &fakeFS{files: map[string][]byte{nftConfPath: []byte(desired)}}
	var syscalls [][]string
	svc := newFirewallService(t, Probes{
		FileExists: func(path string) bool { return fs.exists(path) },
		ReadFile:   func(path string) ([]byte, error) { return fs.read(path) },
		WriteFile:  func(path string, data []byte, perm uint32) error { return fs.write(path, data, perm) },
		CommandOutput: func(_ context.Context, name string, args ...string) (string, error) {
			return "", nil
		},
		Systemctl: func(_ context.Context, args ...string) (string, error) {
			syscalls = append(syscalls, append([]string(nil), args...))
			return "", nil
		},
		ServiceActive:  func(name string) (bool, error) { return false, nil },
		ServiceEnabled: func(name string) (bool, error) { return false, nil },
	})
	res := svc.runFirewallStep(ctx, InstallOptions{})
	if res.Status != JournalCompleted {
		t.Fatalf("status %q error %q", res.Status, res.Error)
	}
	if len(syscalls) != 2 {
		t.Fatalf("expected enable+start when not active, got %v", syscalls)
	}
}

// ---------------------------------------------------------------------------
// Validation failure leaves conf untouched (temp-path validation)
// ---------------------------------------------------------------------------

func TestFirewallValidationFailureLeavesConfUntouched(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fs := &fakeFS{files: map[string][]byte{nftConfPath: []byte("old conf")}}
	var writes []string
	svc := newFirewallService(t, Probes{
		FileExists: func(path string) bool { return fs.exists(path) },
		ReadFile:   func(path string) ([]byte, error) { return fs.read(path) },
		WriteFile: func(path string, data []byte, perm uint32) error {
			writes = append(writes, path)
			return fs.write(path, data, perm)
		},
		RemoveFile: func(path string) error { return fs.remove(path) },
		CommandOutput: func(_ context.Context, name string, args ...string) (string, error) {
			// fail validation on temp path
			if len(args) >= 3 && args[2] == nftTempPath {
				return "nft: syntax error", errors.New("exit 1")
			}
			return "", nil
		},
		Systemctl: func(_ context.Context, args ...string) (string, error) {
			t.Fatalf("systemctl should not be called on validation failure")
			return "", nil
		},
	})

	res := svc.runFirewallStep(ctx, InstallOptions{})
	if res.Status != JournalFailed {
		t.Fatalf("expected failed, got %q", res.Status)
	}
	if !strings.Contains(res.Error, "nft: syntax error") {
		t.Fatalf("error should contain validator output, got %q", res.Error)
	}
	if !strings.Contains(res.Error, "nft validation failed") {
		t.Fatalf("error should contain 'nft validation failed', got %q", res.Error)
	}
	// original conf must be untouched
	if string(fs.files[nftConfPath]) != "old conf" {
		t.Fatalf("original conf was overwritten on validation failure, got %q", string(fs.files[nftConfPath]))
	}
	// final write should not have happened
	for _, w := range writes {
		if w == nftConfPath {
			t.Fatalf("final conf should not be written on validation failure, writes %v", writes)
		}
	}
	// temp should have been written then cleaned (or at least written)
	foundTemp := false
	for _, w := range writes {
		if w == nftTempPath {
			foundTemp = true
		}
	}
	if !foundTemp {
		t.Fatalf("temp file should have been written for validation, writes %v", writes)
	}
	// systemctl should not be called already asserted
}

func TestFirewallValidationFailureLeavesMissingConfAbsent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fs := &fakeFS{files: map[string][]byte{}}
	svc := newFirewallService(t, Probes{
		FileExists: func(path string) bool { return fs.exists(path) },
		ReadFile:   func(path string) ([]byte, error) { return fs.read(path) },
		WriteFile:  func(path string, data []byte, perm uint32) error { return fs.write(path, data, perm) },
		RemoveFile: func(path string) error { return fs.remove(path) },
		CommandOutput: func(_ context.Context, name string, args ...string) (string, error) {
			return "bad", errors.New("fail")
		},
		Systemctl: func(_ context.Context, args ...string) (string, error) {
			t.Fatalf("should not reach systemctl")
			return "", nil
		},
	})
	res := svc.runFirewallStep(ctx, InstallOptions{})
	if res.Status != JournalFailed {
		t.Fatalf("want failed")
	}
	if _, ok := fs.files[nftConfPath]; ok {
		t.Fatalf("conf should not be created on validation failure")
	}
}

// ---------------------------------------------------------------------------
// Pre-existing backup never overwritten
// ---------------------------------------------------------------------------

func TestFirewallPreExistingBackupNeverOverwritten(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backupContent := "original backup"
	fs := &fakeFS{files: map[string][]byte{
		nftConfPath:   []byte("old conf to be replaced"),
		nftBackupPath: []byte(backupContent),
	}}
	var backupWrites int
	svc := newFirewallService(t, Probes{
		FileExists: func(path string) bool { return fs.exists(path) },
		ReadFile:   func(path string) ([]byte, error) { return fs.read(path) },
		WriteFile: func(path string, data []byte, perm uint32) error {
			if path == nftBackupPath {
				backupWrites++
			}
			return fs.write(path, data, perm)
		},
		RemoveFile:    func(path string) error { return fs.remove(path) },
		CommandOutput: func(_ context.Context, name string, args ...string) (string, error) { return "", nil },
		Systemctl:     func(_ context.Context, args ...string) (string, error) { return "", nil },
	})

	res := svc.runFirewallStep(ctx, InstallOptions{})
	if res.Status != JournalCompleted {
		t.Fatalf("status %q error %q", res.Status, res.Error)
	}
	if backupWrites != 0 {
		t.Fatalf("backup should not be overwritten, writes %d", backupWrites)
	}
	if string(fs.files[nftBackupPath]) != backupContent {
		t.Fatalf("backup content changed to %q", string(fs.files[nftBackupPath]))
	}
	// final conf should still be updated
	if string(fs.files[nftConfPath]) != NftablesConf() {
		t.Fatalf("final conf not updated")
	}
}

// ---------------------------------------------------------------------------
// Rollback tests
// ---------------------------------------------------------------------------

func TestFirewallRollbackRestoresBackup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backupData := []byte("backup rules")
	fs := &fakeFS{files: map[string][]byte{
		nftBackupPath: []byte("backup rules"),
		nftConfPath:   []byte(NftablesConf()),
	}}
	var syscalls [][]string
	var cmds [][]string
	probes := Probes{
		FileExists: func(path string) bool { return fs.exists(path) },
		ReadFile:   func(path string) ([]byte, error) { return fs.read(path) },
		WriteFile:  func(path string, data []byte, perm uint32) error { return fs.write(path, data, perm) },
		CommandOutput: func(_ context.Context, name string, args ...string) (string, error) {
			cmds = append(cmds, append([]string{name}, args...))
			return "", nil
		},
		Systemctl: func(_ context.Context, args ...string) (string, error) {
			syscalls = append(syscalls, append([]string(nil), args...))
			return "", nil
		},
	}
	if err := RollbackFirewall(ctx, probes); err != nil {
		t.Fatalf("rollback error %v", err)
	}
	if string(fs.files[nftConfPath]) != string(backupData) {
		t.Fatalf("conf after rollback = %q want %q", string(fs.files[nftConfPath]), string(backupData))
	}
	// systemctl disable+stop best-effort
	foundDisable, foundStop := false, false
	for _, c := range syscalls {
		if len(c) == 2 && c[0] == "disable" && c[1] == "nftables" {
			foundDisable = true
		}
		if len(c) == 2 && c[0] == "stop" && c[1] == "nftables" {
			foundStop = true
		}
	}
	if !foundDisable || !foundStop {
		t.Fatalf("expected disable+stop, got %v", syscalls)
	}
	// should have re-run nft -f
	foundNft := false
	for _, c := range cmds {
		if len(c) >= 2 && c[0] == "nft" && c[1] == "-f" && len(c) >= 3 && c[2] == nftConfPath {
			foundNft = true
		}
		// also accept -c -f? but we use -f
	}
	if !foundNft {
		t.Fatalf("expected nft -f %s call, got %v", nftConfPath, cmds)
	}
}

func TestFirewallRollbackWithoutBackupRemovesOmahabAuthored(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fs := &fakeFS{files: map[string][]byte{nftConfPath: []byte(NftablesConf())}}
	var removed []string
	probes := Probes{
		FileExists: func(path string) bool { return fs.exists(path) },
		ReadFile:   func(path string) ([]byte, error) { return fs.read(path) },
		WriteFile:  func(path string, data []byte, perm uint32) error { return fs.write(path, data, perm) },
		RemoveFile: func(path string) error {
			removed = append(removed, path)
			return fs.remove(path)
		},
		Systemctl: func(_ context.Context, args ...string) (string, error) { return "", nil },
	}
	if err := RollbackFirewall(ctx, probes); err != nil {
		t.Fatalf("rollback %v", err)
	}
	if len(removed) != 1 || removed[0] != nftConfPath {
		t.Fatalf("expected remove %q, got %v", nftConfPath, removed)
	}
	if _, ok := fs.files[nftConfPath]; ok {
		t.Fatalf("file should be removed")
	}
}

func TestFirewallRollbackPreservesForeignConf(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	foreign := "# custom admin conf\nflush ruleset\n"
	fs := &fakeFS{files: map[string][]byte{nftConfPath: []byte(foreign)}}
	var removed []string
	probes := Probes{
		FileExists: func(path string) bool { return fs.exists(path) },
		ReadFile:   func(path string) ([]byte, error) { return fs.read(path) },
		RemoveFile: func(path string) error {
			removed = append(removed, path)
			return fs.remove(path)
		},
		Systemctl: func(_ context.Context, args ...string) (string, error) { return "", nil },
	}
	if err := RollbackFirewall(ctx, probes); err != nil {
		t.Fatalf("rollback %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("should not remove foreign conf, removed %v", removed)
	}
	if string(fs.files[nftConfPath]) != foreign {
		t.Fatalf("foreign conf altered")
	}
}

func TestFirewallRollbackWithoutBackupNoFile(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fs := &fakeFS{files: map[string][]byte{}}
	probes := Probes{
		FileExists: func(path string) bool { return fs.exists(path) },
		ReadFile:   func(path string) ([]byte, error) { return fs.read(path) },
		RemoveFile: func(path string) error { return fs.remove(path) },
		Systemctl:  func(_ context.Context, args ...string) (string, error) { return "", nil },
	}
	if err := RollbackFirewall(ctx, probes); err != nil {
		t.Fatalf("rollback %v", err)
	}
	// no panic, no file
}

func TestFirewallRollbackBestEffortSystemctlFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fs := &fakeFS{files: map[string][]byte{nftBackupPath: []byte("bak")}}
	probes := Probes{
		FileExists: func(path string) bool { return fs.exists(path) },
		ReadFile:   func(path string) ([]byte, error) { return fs.read(path) },
		WriteFile:  func(path string, data []byte, perm uint32) error { return fs.write(path, data, perm) },
		CommandOutput: func(_ context.Context, name string, args ...string) (string, error) {
			return "", nil
		},
		Systemctl: func(_ context.Context, args ...string) (string, error) {
			return "", errors.New("systemctl fail")
		},
	}
	if err := RollbackFirewall(ctx, probes); err != nil {
		t.Fatalf("rollback should be best-effort despite systemctl failure, got %v", err)
	}
	if string(fs.files[nftConfPath]) != "bak" {
		t.Fatalf("backup not restored despite systemctl failure")
	}
}

func TestFirewallRollbackNilProbesNoPanic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	if err := RollbackFirewall(ctx, Probes{}); err != nil {
		t.Fatalf("nil probes rollback should not error, got %v", err)
	}
}

func TestFirewallNilProbesNoPanic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := newFirewallService(t, Probes{})
	res := svc.runFirewallStep(ctx, InstallOptions{})
	if res.Step != StepFirewall {
		t.Fatalf("step %q", res.Step)
	}
	// should not panic, should be failed (since required probes missing)
	if res.Status != JournalFailed {
		t.Fatalf("expected failed with nil probes, got %q", res.Status)
	}
}

func TestFirewallSystemctlEnableFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fs := &fakeFS{files: map[string][]byte{}}
	svc := newFirewallService(t, Probes{
		FileExists: func(path string) bool { return fs.exists(path) },
		ReadFile:   func(path string) ([]byte, error) { return fs.read(path) },
		WriteFile:  func(path string, data []byte, perm uint32) error { return fs.write(path, data, perm) },
		RemoveFile: func(path string) error { return fs.remove(path) },
		CommandOutput: func(_ context.Context, name string, args ...string) (string, error) {
			return "", nil
		},
		Systemctl: func(_ context.Context, args ...string) (string, error) {
			if args[0] == "enable" {
				return "", errors.New("enable failed")
			}
			return "", nil
		},
	})
	res := svc.runFirewallStep(ctx, InstallOptions{})
	if res.Status != JournalFailed {
		t.Fatalf("want failed on enable")
	}
	if !strings.Contains(res.Error, "enable") {
		t.Fatalf("error should mention enable, got %q", res.Error)
	}
}

func TestFirewallSystemctlStartFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fs := &fakeFS{files: map[string][]byte{}}
	svc := newFirewallService(t, Probes{
		FileExists: func(path string) bool { return fs.exists(path) },
		ReadFile:   func(path string) ([]byte, error) { return fs.read(path) },
		WriteFile:  func(path string, data []byte, perm uint32) error { return fs.write(path, data, perm) },
		RemoveFile: func(path string) error { return fs.remove(path) },
		CommandOutput: func(_ context.Context, name string, args ...string) (string, error) {
			return "", nil
		},
		Systemctl: func(_ context.Context, args ...string) (string, error) {
			if args[0] == "start" || args[0] == "restart" {
				return "", errors.New("start failed")
			}
			return "", nil
		},
	})
	res := svc.runFirewallStep(ctx, InstallOptions{})
	if res.Status != JournalFailed {
		t.Fatalf("want failed on start")
	}
}

func TestFirewallWritePerms0644(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fs := &fakeFS{files: map[string][]byte{}}
	var perms []uint32
	svc := newFirewallService(t, Probes{
		FileExists: func(path string) bool { return fs.exists(path) },
		ReadFile:   func(path string) ([]byte, error) { return fs.read(path) },
		WriteFile: func(path string, data []byte, perm uint32) error {
			perms = append(perms, perm)
			return fs.write(path, data, perm)
		},
		RemoveFile:    func(path string) error { return fs.remove(path) },
		CommandOutput: func(_ context.Context, name string, args ...string) (string, error) { return "", nil },
		Systemctl:     func(_ context.Context, args ...string) (string, error) { return "", nil },
	})
	res := svc.runFirewallStep(ctx, InstallOptions{})
	if res.Status != JournalCompleted {
		t.Fatalf("status %q error %q", res.Status, res.Error)
	}
	for _, p := range perms {
		if p != 0o644 {
			t.Fatalf("write perm = %o want 0644", p)
		}
	}
}

func TestFirewallIdempotentSecondRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fs := &fakeFS{files: map[string][]byte{}}
	probes := Probes{
		FileExists: func(path string) bool { return fs.exists(path) },
		ReadFile:   func(path string) ([]byte, error) { return fs.read(path) },
		WriteFile:  func(path string, data []byte, perm uint32) error { return fs.write(path, data, perm) },
		RemoveFile: func(path string) error { return fs.remove(path) },
		CommandOutput: func(_ context.Context, name string, args ...string) (string, error) {
			return "", nil
		},
		Systemctl:      func(_ context.Context, args ...string) (string, error) { return "", nil },
		ServiceActive:  func(name string) (bool, error) { return true, nil },
		ServiceEnabled: func(name string) (bool, error) { return true, nil },
	}
	svc := newFirewallService(t, probes)

	res1 := svc.runFirewallStep(ctx, InstallOptions{})
	if res1.Status != JournalCompleted {
		t.Fatalf("first run %q %q", res1.Status, res1.Error)
	}
	// second run should be idempotent: identical conf, active+enabled -> no writes, no systemctl
	var writes2 int
	probes2 := Probes{
		FileExists: func(path string) bool { return fs.exists(path) },
		ReadFile:   func(path string) ([]byte, error) { return fs.read(path) },
		WriteFile: func(path string, data []byte, perm uint32) error {
			writes2++
			return fs.write(path, data, perm)
		},
		CommandOutput: func(_ context.Context, name string, args ...string) (string, error) {
			return "", nil
		},
		Systemctl: func(_ context.Context, args ...string) (string, error) {
			t.Fatalf("systemctl should be skipped on second identical run")
			return "", nil
		},
		ServiceActive:  func(name string) (bool, error) { return true, nil },
		ServiceEnabled: func(name string) (bool, error) { return true, nil },
	}
	svc2 := newFirewallService(t, probes2)
	// share fs via probes2 closure captures same fs
	// need to re-assign svc2 probes? but we used newFirewallService which creates new DB; share same fs variable
	// second run uses same fs map, so identical check should be true
	res2 := svc2.runFirewallStep(ctx, InstallOptions{})
	if res2.Status != JournalCompleted {
		t.Fatalf("second run %q %q", res2.Status, res2.Error)
	}
	if writes2 != 0 {
		t.Fatalf("second run should skip writes, got %d", writes2)
	}
}
