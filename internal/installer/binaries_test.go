package installer

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	_ "modernc.org/sqlite"
)

func newBinariesService(t *testing.T, probes Probes) *Service {
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

func defaultBinariesAssets() fstest.MapFS {
	return fstest.MapFS{
		"bin/omahab":                     &fstest.MapFile{Data: []byte("omahab-bin-content")},
		"bin/omahabd":                    &fstest.MapFile{Data: []byte("omahabd-bin-content")},
		"systemd/omahabd.service":        &fstest.MapFile{Data: []byte("[Unit] omahabd")},
		"systemd/omahab-backup.service":  &fstest.MapFile{Data: []byte("[Unit] backup service")},
		"systemd/omahab-backup.timer":    &fstest.MapFile{Data: []byte("[Unit] backup timer")},
		"systemd/omahab-verify.service":  &fstest.MapFile{Data: []byte("[Unit] verify service")},
		"systemd/omahab-verify.timer":    &fstest.MapFile{Data: []byte("[Unit] verify timer")},
		"systemd/cloudflared.service":    &fstest.MapFile{Data: []byte("[Unit] cloudflared")},
		"tmpfiles.d/omahab.conf":         &fstest.MapFile{Data: []byte("tmpfiles omahab")},
		"catalog/apps-catalog.json":      &fstest.MapFile{Data: []byte(`{"apps":[]}`)},
		"catalog/compose/example.yml":    &fstest.MapFile{Data: []byte("compose-data")},
		"web/index.html":                 &fstest.MapFile{Data: []byte("<html>hello</html>")},
		"web/assets/app.js":              &fstest.MapFile{Data: []byte("console.log(1)")},
	}
}

// TestBinariesHappyPath verifies every destination is written with exact perms,
// that systemd-tmpfiles is invoked, and that journal state records hashes.
func TestBinariesHappyPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	assets := defaultBinariesAssets()

	destData := map[string][]byte{}
	destPerm := map[string]uint32{}
	dirPerm := map[string]uint32{}
	var tmpfilesName string
	var tmpfilesArgs []string
	tmpfilesCalls := 0
	completionCalls := 0
	shaCalls := map[string]int{}
	hashes := map[string]string{
		"/usr/bin/omahab":  "hash-omahab",
		"/usr/bin/omahabd": "hash-omahabd",
	}

	svc := newBinariesService(t, Probes{
		MkdirAll: func(p string, perm uint32) error {
			dirPerm[p] = perm
			return nil
		},
		ReadFile: func(p string) ([]byte, error) {
			if d, ok := destData[p]; ok {
				cp := make([]byte, len(d))
				copy(cp, d)
				return cp, nil
			}
			return nil, errors.New("no such file")
		},
		WriteFile: func(p string, data []byte, perm uint32) error {
			cp := make([]byte, len(data))
			copy(cp, data)
			destData[p] = cp
			destPerm[p] = perm
			return nil
		},
		CommandOutput: func(ctx context.Context, name string, args ...string) (string, error) {
			if name == "systemd-tmpfiles" {
				tmpfilesName = name
				tmpfilesArgs = append([]string(nil), args...)
				tmpfilesCalls++
				return "", nil
			}
			if name == "/usr/bin/omahab" && len(args) >= 1 && args[0] == "completion" {
				completionCalls++
				shell := ""
				if len(args) >= 2 {
					shell = args[1]
				}
				return "# completion for " + shell + "\n", nil
			}
			t.Fatalf("unexpected CommandOutput %q %v", name, args)
			return "", nil
		},
		SHA256File: func(p string) (string, error) {
			shaCalls[p]++
			if h, ok := hashes[p]; ok {
				return h, nil
			}
			return "", errors.New("unknown file")
		},
	})
	svc.SetAssets(assets)

	res := svc.runBinariesStep(ctx, InstallOptions{})
	if res.Step != StepBinaries {
		t.Fatalf("step = %q, want %q", res.Step, StepBinaries)
	}
	if res.Status != JournalCompleted {
		t.Fatalf("status = %q, want %q (error %q)", res.Status, JournalCompleted, res.Error)
	}
	if res.Error != "" {
		t.Fatalf("unexpected error %q", res.Error)
	}

	// Expected dest → perm mapping (including shell completions)
	wantPerm := map[string]uint32{
		"/usr/bin/omahab":                              0o755,
		"/usr/bin/omahabd":                             0o755,
		"/usr/lib/systemd/system/omahabd.service":      0o644,
		"/usr/lib/systemd/system/omahab-backup.service": 0o644,
		"/usr/lib/systemd/system/omahab-backup.timer": 0o644,
		"/usr/lib/systemd/system/omahab-verify.service": 0o644,
		"/usr/lib/systemd/system/omahab-verify.timer": 0o644,
		"/usr/lib/systemd/system/cloudflared.service": 0o644,
		"/usr/lib/tmpfiles.d/omahab.conf":              0o644,
		"/usr/share/omahab/catalog/apps-catalog.json":  0o644,
		"/usr/share/omahab/catalog/compose/example.yml": 0o644,
		"/usr/share/omahab/web/index.html":             0o644,
		"/usr/share/omahab/web/assets/app.js":          0o644,
		"/usr/share/bash-completion/completions/omahab": 0o644,
		"/usr/share/zsh/site-functions/_omahab":        0o644,
		"/usr/share/fish/vendor_completions.d/omahab.fish": 0o644,
	}
	if !reflect.DeepEqual(destPerm, wantPerm) {
		t.Fatalf("perms mismatch:\n got  %v\n want %v", destPerm, wantPerm)
	}
	// Verify content matches assets
	checks := []struct{ src, dst string }{
		{"bin/omahab", "/usr/bin/omahab"},
		{"bin/omahabd", "/usr/bin/omahabd"},
		{"systemd/omahabd.service", "/usr/lib/systemd/system/omahabd.service"},
		{"systemd/omahab-backup.service", "/usr/lib/systemd/system/omahab-backup.service"},
		{"systemd/omahab-backup.timer", "/usr/lib/systemd/system/omahab-backup.timer"},
		{"systemd/omahab-verify.service", "/usr/lib/systemd/system/omahab-verify.service"},
		{"systemd/omahab-verify.timer", "/usr/lib/systemd/system/omahab-verify.timer"},
		{"systemd/cloudflared.service", "/usr/lib/systemd/system/cloudflared.service"},
		{"tmpfiles.d/omahab.conf", "/usr/lib/tmpfiles.d/omahab.conf"},
		{"catalog/apps-catalog.json", "/usr/share/omahab/catalog/apps-catalog.json"},
		{"catalog/compose/example.yml", "/usr/share/omahab/catalog/compose/example.yml"},
		{"web/index.html", "/usr/share/omahab/web/index.html"},
		{"web/assets/app.js", "/usr/share/omahab/web/assets/app.js"},
	}
	for _, c := range checks {
		got, ok := destData[c.dst]
		if !ok {
			t.Fatalf("missing dest %q (src %q)", c.dst, c.src)
		}
		want, _ := assets[c.src]
		wantData := want.Data
		if string(got) != string(wantData) {
			t.Fatalf("content mismatch %s: got %q want %q", c.dst, string(got), string(wantData))
		}
		// parent dirs should have been created with 0755
		dir := filepath.Dir(c.dst)
		// path.Dir uses slash; filepath.Dir normalizes; ensure lookup via same logic as code (path.Dir)
		// code uses path.Dir which equals slash semantics, so check slash version
		slashDir := c.dst[:strings.LastIndex(c.dst, "/")]
		if slashDir == "" {
			slashDir = "/"
		}
		_ = dir
		if perm, ok := dirPerm[slashDir]; !ok {
			t.Fatalf("MkdirAll not called for dir %q (dest %q)", slashDir, c.dst)
		} else if perm != 0o755 {
			t.Fatalf("MkdirAll perm for %q = %o want 0755", slashDir, perm)
		}
	}

	if tmpfilesCalls != 1 {
		t.Fatalf("tmpfiles calls = %d, want 1", tmpfilesCalls)
	}
	if tmpfilesName != "systemd-tmpfiles" {
		t.Fatalf("tmpfiles name = %q, want %q", tmpfilesName, "systemd-tmpfiles")
	}
	if !reflect.DeepEqual(tmpfilesArgs, []string{"--create"}) {
		t.Fatalf("tmpfiles args = %v, want [--create]", tmpfilesArgs)
	}
	if completionCalls != 3 {
		t.Fatalf("completion calls = %d, want 3", completionCalls)
	}
	// Verify completion files were written with correct content and perms
	for _, shell := range []string{"bash", "zsh", "fish"} {
		var dst string
		switch shell {
		case "bash":
			dst = "/usr/share/bash-completion/completions/omahab"
		case "zsh":
			dst = "/usr/share/zsh/site-functions/_omahab"
		case "fish":
			dst = "/usr/share/fish/vendor_completions.d/omahab.fish"
		}
		got, ok := destData[dst]
		if !ok {
			t.Fatalf("missing completion dest %q for shell %q", dst, shell)
		}
		want := "# completion for " + shell + "\n"
		if string(got) != want {
			t.Fatalf("completion content for %q: got %q want %q", shell, string(got), want)
		}
		if destPerm[dst] != 0o644 {
			t.Fatalf("completion perm for %q = %o want 0644", dst, destPerm[dst])
		}
	}
	if len(shaCalls) != 2 {
		t.Fatalf("SHA256 calls = %v, want 2", shaCalls)
	}
	js := NewJournalStore(svc.DB())
	state, err := js.GetState(ctx, "binaries_sha256")
	if err != nil {
		t.Fatalf("GetState: %v", err)
	}
	if state == "" {
		t.Fatal("expected journal binaries_sha256 state to be set, got empty")
	}
	var gotHashes map[string]string
	if err := json.Unmarshal([]byte(state), &gotHashes); err != nil {
		t.Fatalf("state json unmarshal: %v (raw %q)", err, state)
	}
	if !reflect.DeepEqual(gotHashes, hashes) {
		t.Fatalf("hashes state = %v, want %v", gotHashes, hashes)
	}
}

// TestBinariesIdempotentSecondRun verifies that a second run with identical
// content does not issue WriteFile calls.
func TestBinariesIdempotentSecondRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	assets := defaultBinariesAssets()

	destData := map[string][]byte{}
	writeCalls := 0
	tmpfilesCalls := 0

	probes := Probes{
		MkdirAll: func(p string, perm uint32) error { return nil },
		ReadFile: func(p string) ([]byte, error) {
			if d, ok := destData[p]; ok {
				cp := make([]byte, len(d))
				copy(cp, d)
				return cp, nil
			}
			return nil, errors.New("no such file")
		},
		WriteFile: func(p string, data []byte, perm uint32) error {
			cp := make([]byte, len(data))
			copy(cp, data)
			destData[p] = cp
			writeCalls++
			return nil
		},
		CommandOutput: func(ctx context.Context, name string, args ...string) (string, error) {
			if name == "systemd-tmpfiles" {
				tmpfilesCalls++
				return "", nil
			}
			if name == "/usr/bin/omahab" {
				// Completion probe: return empty to keep idempotent test focused on
				// main assets; completions are best-effort and empty means skipped.
				return "", nil
			}
			t.Fatalf("unexpected CommandOutput %q %v", name, args)
			return "", nil
		},
		SHA256File: func(p string) (string, error) { return "hash", nil },
	}

	svc := newBinariesService(t, probes)
	svc.SetAssets(assets)

	res1 := svc.runBinariesStep(ctx, InstallOptions{})
	if res1.Status != JournalCompleted {
		t.Fatalf("first run status %q, want %q (error %q)", res1.Status, JournalCompleted, res1.Error)
	}
	firstWrites := writeCalls
	if firstWrites == 0 {
		t.Fatal("first run should have performed writes")
	}
	if tmpfilesCalls != 1 {
		t.Fatalf("first run tmpfiles calls = %d, want 1", tmpfilesCalls)
	}
	// second run — same svc, same destData, should be idempotent
	writeCalls = 0
	tmpfilesCalls = 0
	res2 := svc.runBinariesStep(ctx, InstallOptions{})
	if res2.Status != JournalCompleted {
		t.Fatalf("second run status %q, want %q (error %q)", res2.Status, JournalCompleted, res2.Error)
	}
	if writeCalls != 0 {
		t.Fatalf("second run writes = %d, want 0 (idempotent)", writeCalls)
	}
	if tmpfilesCalls != 1 {
		t.Fatalf("second run tmpfiles should still be invoked once, got %d", tmpfilesCalls)
	}
	// third run with modified content should trigger writes for changed files
	// Mutate one catalog entry
	assets["catalog/apps-catalog.json"] = &fstest.MapFile{Data: []byte(`{"apps":["new"]}`)}
	writeCalls = 0
	tmpfilesCalls = 0
	res3 := svc.runBinariesStep(ctx, InstallOptions{})
	if res3.Status != JournalCompleted {
		t.Fatalf("third run status %q, want %q", res3.Status, JournalCompleted)
	}
	if writeCalls == 0 {
		t.Fatal("third run with changed catalog should have writes")
	}
	if writeCalls != 1 {
		t.Fatalf("third run writes = %d, want 1 (only changed file)", writeCalls)
	}
}

// TestBinariesMissingAssetFS verifies the exact error when no asset FS configured.
func TestBinariesMissingAssetFS(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := newBinariesService(t, Probes{
		MkdirAll:      func(p string, perm uint32) error { return nil },
		ReadFile:      func(p string) ([]byte, error) { return nil, errors.New("no such file") },
		WriteFile:     func(p string, data []byte, perm uint32) error { return nil },
		CommandOutput: func(ctx context.Context, name string, args ...string) (string, error) { return "", nil },
	})
	// Do not call SetAssets — leave nil
	res := svc.runBinariesStep(ctx, InstallOptions{})
	if res.Step != StepBinaries {
		t.Fatalf("step = %q, want %q", res.Step, StepBinaries)
	}
	if res.Status != JournalFailed {
		t.Fatalf("status = %q, want %q", res.Status, JournalFailed)
	}
	want := "no install assets configured (rebuild with scripts/build.sh or pass --asset-dir)"
	if res.Error != want {
		t.Fatalf("error = %q, want %q", res.Error, want)
	}
	// Also via explicit nil MapFS (typed nil)
	svc2 := newBinariesService(t, Probes{})
	var nilFS fstest.MapFS
	_ = nilFS
	svc2.SetAssets(nil)
	res2 := svc2.runBinariesStep(ctx, InstallOptions{})
	if res2.Status != JournalFailed {
		t.Fatalf("nil FS status = %q, want %q", res2.Status, JournalFailed)
	}
	if res2.Error != want {
		t.Fatalf("nil FS error = %q, want %q", res2.Error, want)
	}
	// InstallPaths(nil) should also fail with same message
	if _, err := InstallPaths(nil); err == nil {
		t.Fatal("expected InstallPaths(nil) error")
	} else if err.Error() != want {
		t.Fatalf("InstallPaths(nil) error = %q, want %q", err.Error(), want)
	}
}

// TestBinariesMissingAssetEntry verifies missing required entries are listed.
func TestBinariesMissingAssetEntry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	base := defaultBinariesAssets()

	cases := []struct {
		name    string
		missing string
	}{
		{"missing bin/omahabd", "bin/omahabd"},
		{"missing bin/omahab", "bin/omahab"},
		{"missing systemd unit", "systemd/cloudflared.service"},
		{"missing tmpfiles", "tmpfiles.d/omahab.conf"},
		{"missing catalog", "catalog/apps-catalog.json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assets := fstest.MapFS{}
			for k, v := range base {
				assets[k] = v
			}
			// Special handling for catalog missing: remove all catalog entries
			if strings.HasPrefix(tc.missing, "catalog") {
				for k := range assets {
					if strings.HasPrefix(k, "catalog/") {
						delete(assets, k)
					}
				}
			} else {
				delete(assets, tc.missing)
			}
			svc := newBinariesService(t, Probes{
				MkdirAll:      func(p string, perm uint32) error { return nil },
				ReadFile:      func(p string) ([]byte, error) { return nil, errors.New("no such file") },
				WriteFile:     func(p string, data []byte, perm uint32) error { return nil },
				CommandOutput: func(ctx context.Context, name string, args ...string) (string, error) { return "", nil },
			})
			svc.SetAssets(assets)
			res := svc.runBinariesStep(ctx, InstallOptions{})
			if res.Status != JournalFailed {
				t.Fatalf("status = %q, want %q", res.Status, JournalFailed)
			}
			if res.Error == "" {
				t.Fatal("expected non-empty error for missing asset")
			}
			// Error must list the missing entry
			if !strings.Contains(res.Error, tc.missing) {
				// for catalog case, error mentions catalog/
				if tc.missing == "catalog/apps-catalog.json" && !strings.Contains(res.Error, "catalog") {
					t.Fatalf("error %q should mention %q", res.Error, tc.missing)
				} else if tc.missing != "catalog/apps-catalog.json" {
					t.Fatalf("error %q should mention %q", res.Error, tc.missing)
				}
			}
			// InstallPaths should also fail and mention it
			if _, err := InstallPaths(assets); err == nil {
				t.Fatal("expected InstallPaths error for missing asset")
			} else if !strings.Contains(err.Error(), strings.TrimSuffix(tc.missing, "/")) && !strings.Contains(err.Error(), "catalog") {
				t.Fatalf("InstallPaths error %q should mention %q", err.Error(), tc.missing)
			}
		})
	}
}

// TestBinariesTmpfilesFailure verifies propagation of systemd-tmpfiles errors.
func TestBinariesTmpfilesFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	assets := defaultBinariesAssets()

	svc := newBinariesService(t, Probes{
		MkdirAll: func(p string, perm uint32) error { return nil },
		ReadFile: func(p string) ([]byte, error) { return nil, errors.New("no such file") },
		WriteFile: func(p string, data []byte, perm uint32) error { return nil },
		CommandOutput: func(ctx context.Context, name string, args ...string) (string, error) {
			return "", errors.New("tmpfiles failed: exit 1")
		},
		SHA256File: func(p string) (string, error) { return "hash", nil },
	})
	svc.SetAssets(assets)
	res := svc.runBinariesStep(ctx, InstallOptions{})
	if res.Step != StepBinaries {
		t.Fatalf("step = %q, want %q", res.Step, StepBinaries)
	}
	if res.Status != JournalFailed {
		t.Fatalf("status = %q, want %q", res.Status, JournalFailed)
	}
	if !strings.Contains(strings.ToLower(res.Error), "systemd-tmpfiles") {
		t.Fatalf("error %q should mention systemd-tmpfiles", res.Error)
	}
	// Nil CommandOutput should NOT fail — step should succeed (nil-check)
	svc2 := newBinariesService(t, Probes{
		MkdirAll:  func(p string, perm uint32) error { return nil },
		ReadFile:  func(p string) ([]byte, error) { return nil, errors.New("no such file") },
		WriteFile: func(p string, data []byte, perm uint32) error { return nil },
		// CommandOutput nil
		SHA256File: func(p string) (string, error) { return "hash", nil },
	})
	svc2.SetAssets(assets)
	res2 := svc2.runBinariesStep(ctx, InstallOptions{})
	if res2.Status != JournalCompleted {
		t.Fatalf("nil CommandOutput status = %q, want %q (error %q)", res2.Status, JournalCompleted, res2.Error)
	}
}

// TestBinariesRollback tests RollbackBinaries removes the fixed set and tolerates missing files.
func TestBinariesRollback(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	removed := []string{}
	var daemonReloadCalls int
	probes := Probes{
		RemoveFile: func(p string) error {
			removed = append(removed, p)
			return nil
		},
		Systemctl: func(ctx context.Context, args ...string) (string, error) {
			if len(args) == 1 && args[0] == "daemon-reload" {
				daemonReloadCalls++
			}
			return "", nil
		},
	}
	if err := RollbackBinaries(ctx, probes); err != nil {
		t.Fatalf("RollbackBinaries: %v", err)
	}
	want := []string{
		"/usr/bin/omahab",
		"/usr/bin/omahabd",
		"/usr/lib/systemd/system/omahabd.service",
		"/usr/lib/systemd/system/omahab-backup.service",
		"/usr/lib/systemd/system/omahab-backup.timer",
		"/usr/lib/systemd/system/omahab-verify.service",
		"/usr/lib/systemd/system/omahab-verify.timer",
		"/usr/lib/systemd/system/cloudflared.service",
		"/usr/lib/tmpfiles.d/omahab.conf",
		"/usr/share/bash-completion/completions/omahab",
		"/usr/share/zsh/site-functions/_omahab",
		"/usr/share/fish/vendor_completions.d/omahab.fish",
	}
	sort.Strings(removed)
	sort.Strings(want)
	if !reflect.DeepEqual(removed, want) {
		t.Fatalf("removed = %v, want %v", removed, want)
	}
	// Must NOT remove /usr/share/omahab (but completions in /usr/share are ok)
	for _, p := range removed {
		if strings.HasPrefix(p, "/usr/share/omahab/") {
			t.Fatalf("rollback should not remove %q", p)
		}
	}
	if daemonReloadCalls != 1 {
		t.Fatalf("daemon-reload calls = %d, want 1", daemonReloadCalls)
	}
}

func TestBinariesRollbackToleratesMissing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	removed := []string{}
	probes := Probes{
		RemoveFile: func(p string) error {
			removed = append(removed, p)
			// simulate missing for half the paths
			if strings.Contains(p, "cloudflared") || p == "/usr/bin/omahab" {
				return errors.New("no such file or directory")
			}
			return nil
		},
		Systemctl: func(ctx context.Context, args ...string) (string, error) { return "", nil },
	}
	if err := RollbackBinaries(ctx, probes); err != nil {
		t.Fatalf("RollbackBinaries with missing files should not error, got %v", err)
	}
	if len(removed) != 12 {
		t.Fatalf("removed count = %d, want 12 (tolerate missing but still attempt all) %v", len(removed), removed)
	}
	// Also tolerates nil probes
	if err := RollbackBinaries(ctx, Probes{}); err != nil {
		t.Fatalf("RollbackBinaries with nil probes should not error, got %v", err)
	}
	// RemoveFile returns wrapped ErrNotExist should also be ignored
	probes2 := Probes{
		RemoveFile: func(p string) error {
			return errors.Join(errors.New("wrap"), errors.New("file does not exist"))
		},
	}
	// Need to use fs.ErrNotExist wrapping — simulate via fmt with %w
	probes3 := Probes{
		RemoveFile: func(p string) error {
			return errors.Join(errors.New("wrapped"), &testNotExistError{msg: "file does not exist"})
		},
		Systemctl: func(ctx context.Context, args ...string) (string, error) { return "", nil },
	}
	_ = probes2
	if err := RollbackBinaries(ctx, probes3); err != nil {
		t.Fatalf("RollbackBinaries with fs.ErrNotExist-like should not error, got %v", err)
	}
	_ = probes3
}

// testNotExistError mimics fs.ErrNotExist via Is
type testNotExistError struct{ msg string }

func (e *testNotExistError) Error() string { return e.msg }
func (e *testNotExistError) Is(target error) bool { return true }

// TestBinariesInstallPaths verifies the seam returns dest→perm with catalog/web expansion.
func TestBinariesInstallPaths(t *testing.T) {
	t.Parallel()

	assets := fstest.MapFS{
		"bin/omahab":                    &fstest.MapFile{Data: []byte("a")},
		"bin/omahabd":                   &fstest.MapFile{Data: []byte("b")},
		"systemd/omahabd.service":       &fstest.MapFile{Data: []byte("1")},
		"systemd/omahab-backup.service": &fstest.MapFile{Data: []byte("2")},
		"systemd/omahab-backup.timer":   &fstest.MapFile{Data: []byte("3")},
		"systemd/omahab-verify.service": &fstest.MapFile{Data: []byte("4")},
		"systemd/omahab-verify.timer":   &fstest.MapFile{Data: []byte("5")},
		"systemd/cloudflared.service":   &fstest.MapFile{Data: []byte("6")},
		"tmpfiles.d/omahab.conf":        &fstest.MapFile{Data: []byte("7")},
		"catalog/a.json":                  &fstest.MapFile{Data: []byte("{}")},
		"catalog/b.json":                  &fstest.MapFile{Data: []byte("{}")},
		"catalog/apps-catalog.json":       &fstest.MapFile{Data: []byte("{}")},
		"catalog/nested/c.yml":            &fstest.MapFile{Data: []byte("c")},
		"web/app/index.html":              &fstest.MapFile{Data: []byte("html")},
	}
	m, err := InstallPaths(assets)
	if err != nil {
		t.Fatalf("InstallPaths: %v", err)
	}
	want := map[string]uint32{
		"/usr/bin/omahab":                              0o755,
		"/usr/bin/omahabd":                             0o755,
		"/usr/lib/systemd/system/omahabd.service":      0o644,
		"/usr/lib/systemd/system/omahab-backup.service": 0o644,
		"/usr/lib/systemd/system/omahab-backup.timer": 0o644,
		"/usr/lib/systemd/system/omahab-verify.service": 0o644,
		"/usr/lib/systemd/system/omahab-verify.timer": 0o644,
		"/usr/lib/systemd/system/cloudflared.service": 0o644,
		"/usr/lib/tmpfiles.d/omahab.conf":              0o644,
		"/usr/share/omahab/catalog/a.json":             0o644,
		"/usr/share/omahab/catalog/b.json":             0o644,
		"/usr/share/omahab/catalog/apps-catalog.json":  0o644,
		"/usr/share/omahab/catalog/nested/c.yml":       0o644,
		"/usr/share/omahab/web/app/index.html":         0o644,
	}
	if !reflect.DeepEqual(m, want) {
		t.Fatalf("InstallPaths map mismatch:\n got  %v\n want %v", m, want)
	}
	// Optional web: absent should still succeed
	assets2 := fstest.MapFS{
		"bin/omahab":                    &fstest.MapFile{Data: []byte("a")},
		"bin/omahabd":                   &fstest.MapFile{Data: []byte("b")},
		"systemd/omahabd.service":       &fstest.MapFile{Data: []byte("1")},
		"systemd/omahab-backup.service": &fstest.MapFile{Data: []byte("2")},
		"systemd/omahab-backup.timer":   &fstest.MapFile{Data: []byte("3")},
		"systemd/omahab-verify.service": &fstest.MapFile{Data: []byte("4")},
		"systemd/omahab-verify.timer":   &fstest.MapFile{Data: []byte("5")},
		"systemd/cloudflared.service":   &fstest.MapFile{Data: []byte("6")},
		"tmpfiles.d/omahab.conf":        &fstest.MapFile{Data: []byte("7")},
		"catalog/x.json":                &fstest.MapFile{Data: []byte("{}")},
		"catalog/apps-catalog.json":     &fstest.MapFile{Data: []byte("{}")},
	}
	m2, err := InstallPaths(assets2)
	if err != nil {
		t.Fatalf("InstallPaths without web: %v", err)
	}
	if _, ok := m2["/usr/share/omahab/web/app/index.html"]; ok {
		t.Fatal("unexpected web entry when web absent")
	}
	if len(m2) != 11 { // 2 bins + 6 units + 1 tmpfiles + 2 catalog
		t.Fatalf("expected 11 entries without web, got %d: %v", len(m2), m2)
	}
	// Missing required should error and mention missing
	bad := fstest.MapFS{
		"bin/omahab": &fstest.MapFile{Data: []byte("a")},
		// bin/omahabd missing
		"systemd/omahabd.service":       &fstest.MapFile{Data: []byte("1")},
		"systemd/omahab-backup.service": &fstest.MapFile{Data: []byte("2")},
		"systemd/omahab-backup.timer":   &fstest.MapFile{Data: []byte("3")},
		"systemd/omahab-verify.service": &fstest.MapFile{Data: []byte("4")},
		"systemd/omahab-verify.timer":   &fstest.MapFile{Data: []byte("5")},
		"systemd/cloudflared.service":   &fstest.MapFile{Data: []byte("6")},
		"tmpfiles.d/omahab.conf":        &fstest.MapFile{Data: []byte("7")},
		"catalog/x.json":                &fstest.MapFile{Data: []byte("{}")},
	}
	if _, err := InstallPaths(bad); err == nil {
		t.Fatal("expected error for missing bin/omahabd")
	} else if !strings.Contains(err.Error(), "bin/omahabd") {
		t.Fatalf("error %q should mention bin/omahabd", err.Error())
	}
}

// TestBinariesSHA256BestEffort ensures hash errors do not fail the step.
func TestBinariesSHA256BestEffort(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	assets := fstest.MapFS{
		"bin/omahab":                    &fstest.MapFile{Data: []byte("a")},
		"bin/omahabd":                   &fstest.MapFile{Data: []byte("b")},
		"systemd/omahabd.service":       &fstest.MapFile{Data: []byte("1")},
		"systemd/omahab-backup.service": &fstest.MapFile{Data: []byte("2")},
		"systemd/omahab-backup.timer":   &fstest.MapFile{Data: []byte("3")},
		"systemd/omahab-verify.service": &fstest.MapFile{Data: []byte("4")},
		"systemd/omahab-verify.timer":   &fstest.MapFile{Data: []byte("5")},
		"systemd/cloudflared.service":   &fstest.MapFile{Data: []byte("6")},
		"tmpfiles.d/omahab.conf":        &fstest.MapFile{Data: []byte("7")},
		"catalog/c.json":                &fstest.MapFile{Data: []byte("{}")},
		"catalog/apps-catalog.json":     &fstest.MapFile{Data: []byte("{}")},
	}

	// SHA256File fails for one binary — step should still complete
	svc := newBinariesService(t, Probes{
		MkdirAll:  func(p string, perm uint32) error { return nil },
		ReadFile:  func(p string) ([]byte, error) { return nil, errors.New("no such file") },
		WriteFile: func(p string, data []byte, perm uint32) error { return nil },
		CommandOutput: func(ctx context.Context, name string, args ...string) (string, error) {
			return "", nil
		},
		SHA256File: func(p string) (string, error) {
			if p == "/usr/bin/omahab" {
				return "", errors.New("hash error")
			}
			return "hash-omahabd", nil
		},
	})
	svc.SetAssets(assets)
	res := svc.runBinariesStep(ctx, InstallOptions{})
	if res.Status != JournalCompleted {
		t.Fatalf("status = %q, want %q (error %q)", res.Status, JournalCompleted, res.Error)
	}
	js := NewJournalStore(svc.DB())
	state, _ := js.GetState(ctx, "binaries_sha256")
	if state == "" {
		t.Fatal("expected state to be set even with partial hash")
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(state), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := m["/usr/bin/omahab"]; ok {
		t.Fatal("should not have hash for failed omahab")
	}
	if m["/usr/bin/omahabd"] != "hash-omahabd" {
		t.Fatalf("wrong hash for omahabd: %v", m)
	}

	// SHA256File nil — also best-effort, no panic, success
	svc2 := newBinariesService(t, Probes{
		MkdirAll:      func(p string, perm uint32) error { return nil },
		ReadFile:      func(p string) ([]byte, error) { return nil, errors.New("no such file") },
		WriteFile:     func(p string, data []byte, perm uint32) error { return nil },
		CommandOutput: func(ctx context.Context, name string, args ...string) (string, error) { return "", nil },
		// SHA256File nil
	})
	svc2.SetAssets(assets)
	res2 := svc2.runBinariesStep(ctx, InstallOptions{})
	if res2.Status != JournalCompleted {
		t.Fatalf("nil SHA256 status = %q, want completed", res2.Status)
	}

	// SHA256File all fail — journal state may be empty or absent, but step still completes
	svc3 := newBinariesService(t, Probes{
		MkdirAll:      func(p string, perm uint32) error { return nil },
		ReadFile:      func(p string) ([]byte, error) { return nil, errors.New("no such file") },
		WriteFile:     func(p string, data []byte, perm uint32) error { return nil },
		CommandOutput: func(ctx context.Context, name string, args ...string) (string, error) { return "", nil },
		SHA256File:    func(p string) (string, error) { return "", errors.New("hash fail") },
	})
	svc3.SetAssets(assets)
	res3 := svc3.runBinariesStep(ctx, InstallOptions{})
	if res3.Status != JournalCompleted {
		t.Fatalf("all hash fail status = %q, want completed", res3.Status)
	}
}

// TestBinariesWriteFailurePropagation checks probe errors fail the step.
func TestBinariesWriteFailurePropagation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	assets := fstest.MapFS{
		"bin/omahab":                    &fstest.MapFile{Data: []byte("a")},
		"bin/omahabd":                   &fstest.MapFile{Data: []byte("b")},
		"systemd/omahabd.service":       &fstest.MapFile{Data: []byte("1")},
		"systemd/omahab-backup.service": &fstest.MapFile{Data: []byte("2")},
		"systemd/omahab-backup.timer":   &fstest.MapFile{Data: []byte("3")},
		"systemd/omahab-verify.service": &fstest.MapFile{Data: []byte("4")},
		"systemd/omahab-verify.timer":   &fstest.MapFile{Data: []byte("5")},
		"systemd/cloudflared.service":   &fstest.MapFile{Data: []byte("6")},
		"tmpfiles.d/omahab.conf":        &fstest.MapFile{Data: []byte("7")},
		"catalog/c.json":                &fstest.MapFile{Data: []byte("{}")},
		"catalog/apps-catalog.json":     &fstest.MapFile{Data: []byte("{}")},
	}
	svc := newBinariesService(t, Probes{
		MkdirAll: func(p string, perm uint32) error { return errors.New("mkdir failed") },
		ReadFile: func(p string) ([]byte, error) { return nil, errors.New("no such file") },
		WriteFile: func(p string, data []byte, perm uint32) error { return nil },
		CommandOutput: func(ctx context.Context, name string, args ...string) (string, error) { return "", nil },
	})
	svc.SetAssets(assets)
	res := svc.runBinariesStep(ctx, InstallOptions{})
	if res.Status != JournalFailed {
		t.Fatalf("status = %q, want failed", res.Status)
	}
	if !strings.Contains(strings.ToLower(res.Error), "mkdir") {
		t.Fatalf("error %q should mention mkdir", res.Error)
	}

	// WriteFile failure
	svc2 := newBinariesService(t, Probes{
		MkdirAll:  func(p string, perm uint32) error { return nil },
		ReadFile:  func(p string) ([]byte, error) { return nil, errors.New("no such file") },
		WriteFile: func(p string, data []byte, perm uint32) error { return errors.New("write failed") },
		CommandOutput: func(ctx context.Context, name string, args ...string) (string, error) { return "", nil },
	})
	svc2.SetAssets(assets)
	res2 := svc2.runBinariesStep(ctx, InstallOptions{})
	if res2.Status != JournalFailed {
		t.Fatalf("write failure status = %q, want failed", res2.Status)
	}
	if !strings.Contains(strings.ToLower(res2.Error), "write") {
		t.Fatalf("error %q should mention write", res2.Error)
	}

	// Nil WriteFile when write needed should fail, not panic
	svc3 := newBinariesService(t, Probes{
		MkdirAll: func(p string, perm uint32) error { return nil },
		ReadFile: func(p string) ([]byte, error) { return nil, errors.New("no such file") },
		// WriteFile nil
		CommandOutput: func(ctx context.Context, name string, args ...string) (string, error) { return "", nil },
	})
	svc3.SetAssets(assets)
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic with nil WriteFile: %v", r)
		}
	}()
	res3 := svc3.runBinariesStep(ctx, InstallOptions{})
	if res3.Status != JournalFailed {
		t.Fatalf("nil WriteFile status = %q, want failed", res3.Status)
	}
}

// TestBinariesNilProbes ensures no panic with entirely nil probes (except assets).
func TestBinariesNilProbes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	assets := defaultBinariesAssets()
	svc := newBinariesService(t, Probes{})
	svc.SetAssets(assets)
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic with nil probes: %v", r)
		}
	}()
	res := svc.runBinariesStep(ctx, InstallOptions{})
	// With nil WriteFile, it must fail (needs write) but not panic
	if res.Status != JournalFailed {
		t.Fatalf("status = %q, want failed with nil probes", res.Status)
	}
	if res.Error == "" {
		t.Fatal("expected error with nil probes")
	}
}
