package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
)

// installEntry ties an asset-relative source to a destination path and permission.
type installEntry struct {
	src  string
	dst  string
	perm uint32
}

// binaryUnits is the six systemd units managed by the binaries step.
var binaryUnits = []string{
	"omahabd.service",
	"omahab-backup.service",
	"omahab-backup.timer",
	"omahab-verify.service",
	"omahab-verify.timer",
	"cloudflared.service",
}

// collectInstallEntries enumerates the ordered install entries from fsys.
// Order: bin/omahab, bin/omahabd, systemd units (in binaryUnits order),
// tmpfiles.d/omahab.conf, catalog/** (sorted), web/** (sorted, optional).
// Missing required assets are collected and reported together.
func collectInstallEntries(fsys fs.FS) ([]installEntry, error) {
	if fsys == nil {
		return nil, fmt.Errorf("no install assets configured (rebuild with scripts/build.sh or pass --asset-dir)")
	}
	var missing []string
	var entries []installEntry

	orderedFixed := []struct {
		src  string
		dst  string
		perm uint32
	}{
		{"bin/omahab", "/usr/bin/omahab", 0o755},
		{"bin/omahabd", "/usr/bin/omahabd", 0o755},
		{"systemd/omahabd.service", "/usr/lib/systemd/system/omahabd.service", 0o644},
		{"systemd/omahab-backup.service", "/usr/lib/systemd/system/omahab-backup.service", 0o644},
		{"systemd/omahab-backup.timer", "/usr/lib/systemd/system/omahab-backup.timer", 0o644},
		{"systemd/omahab-verify.service", "/usr/lib/systemd/system/omahab-verify.service", 0o644},
		{"systemd/omahab-verify.timer", "/usr/lib/systemd/system/omahab-verify.timer", 0o644},
		{"systemd/cloudflared.service", "/usr/lib/systemd/system/cloudflared.service", 0o644},
		{"tmpfiles.d/omahab.conf", "/usr/lib/tmpfiles.d/omahab.conf", 0o644},
	}
	for _, f := range orderedFixed {
		if _, err := fs.Stat(fsys, f.src); err != nil {
			missing = append(missing, f.src)
			continue
		}
		entries = append(entries, installEntry{src: f.src, dst: f.dst, perm: f.perm})
	}

	// catalog/** — required, at least one file
	var catalogFiles []installEntry
	catalogErr := fs.WalkDir(fsys, "catalog", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel := strings.TrimPrefix(p, "catalog/")
		if rel == p {
			rel = path.Base(p)
		}
		dst := path.Join("/usr/share/omahab/catalog", rel)
		catalogFiles = append(catalogFiles, installEntry{src: p, dst: dst, perm: 0o644})
		return nil
	})
	if catalogErr != nil {
		if isNotExist(catalogErr) {
			missing = append(missing, "catalog/")
		} else {
			return nil, fmt.Errorf("walk catalog: %w", catalogErr)
		}
	} else {
		if len(catalogFiles) == 0 {
			missing = append(missing, "catalog/")
		} else {
			sort.Slice(catalogFiles, func(i, j int) bool { return catalogFiles[i].src < catalogFiles[j].src })
			entries = append(entries, catalogFiles...)
		}
		// Ensure runtime catalog is present even if catalog.json exists.
		hasRuntimeCatalog := false
		for _, e := range catalogFiles {
			if e.src == "catalog/apps-catalog.json" {
				hasRuntimeCatalog = true
				break
			}
		}
		if !hasRuntimeCatalog {
			if _, err := fs.Stat(fsys, "catalog/apps-catalog.json"); err != nil {
				missing = append(missing, "catalog/apps-catalog.json")
			}
		}
	}
	if catalogErr != nil {
		if _, err := fs.Stat(fsys, "catalog/apps-catalog.json"); err != nil {
			found := false
			for _, m := range missing {
				if m == "catalog/apps-catalog.json" {
					found = true
					break
				}
			}
			if !found {
				missing = append(missing, "catalog/apps-catalog.json")
			}
		}
	}

	// web/** — optional subtree
	var webFiles []installEntry
	webErr := fs.WalkDir(fsys, "web", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel := strings.TrimPrefix(p, "web/")
		if rel == p {
			rel = path.Base(p)
		}
		dst := path.Join("/usr/share/omahab/web", rel)
		webFiles = append(webFiles, installEntry{src: p, dst: dst, perm: 0o644})
		return nil
	})
	if webErr != nil {
		if !isNotExist(webErr) {
			return nil, fmt.Errorf("walk web: %w", webErr)
		}
		// optional — missing web is not an error
	} else {
		sort.Slice(webFiles, func(i, j int) bool { return webFiles[i].src < webFiles[j].src })
		entries = append(entries, webFiles...)
	}

	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required asset(s): %s", strings.Join(missing, ", "))
	}
	return entries, nil
}

func isNotExist(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, fs.ErrNotExist) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "file does not exist") || strings.Contains(msg, "no such file") || strings.Contains(msg, "not exist")
}

// InstallPaths returns the deterministic destination → permission mapping for
// the given asset filesystem. It walks catalog/** and web/** so callers can
// install or verify the full set without reimplementing the walk. The map key
// is the absolute destination path; the value is the expected file permission
// (0755 for binaries, 0644 for everything else). Required assets missing
// produce an error that names the missing asset paths.
func InstallPaths(fsys fs.FS) (map[string]uint32, error) {
	entries, err := collectInstallEntries(fsys)
	if err != nil {
		return nil, err
	}
	m := make(map[string]uint32, len(entries))
	for _, e := range entries {
		m[e.dst] = e.perm
	}
	return m, nil
}

// runBinariesStep installs binaries, units, tmpfiles config, catalog and web
// assets from Service.Assets() to their fixed destinations with exact perms.
// Parent directories are created via probes.MkdirAll 0755. Writes are
// idempotent (ReadFile compare). It then runs systemd-tmpfiles --create and
// records binary hashes in the journal state (best-effort).
func (s *Service) runBinariesStep(ctx context.Context, opts InstallOptions) RunResult {
	_ = opts
	fsys := s.Assets()
	if fsys == nil {
		return RunResult{Step: StepBinaries, Status: JournalFailed, Error: "no install assets configured (rebuild with scripts/build.sh or pass --asset-dir)"}
	}
	entries, err := collectInstallEntries(fsys)
	if err != nil {
		return RunResult{Step: StepBinaries, Status: JournalFailed, Error: err.Error()}
	}
	for _, e := range entries {
		data, err := fs.ReadFile(fsys, e.src)
		if err != nil {
			return RunResult{Step: StepBinaries, Status: JournalFailed, Error: fmt.Sprintf("read asset %s: %v", e.src, err)}
		}
		dir := path.Dir(e.dst)
		if s.probes.MkdirAll != nil {
			if err := s.probes.MkdirAll(dir, 0o755); err != nil {
				return RunResult{Step: StepBinaries, Status: JournalFailed, Error: fmt.Sprintf("mkdir %s: %v", dir, err)}
			}
		}
		needWrite := true
		if s.probes.ReadFile != nil {
			if existing, err := s.probes.ReadFile(e.dst); err == nil {
				if bytes.Equal(existing, data) {
					needWrite = false
				}
			}
		}
		if needWrite {
			if s.probes.WriteFile == nil {
				return RunResult{Step: StepBinaries, Status: JournalFailed, Error: fmt.Sprintf("write file probe not configured for %s", e.dst)}
			}
			if err := s.probes.WriteFile(e.dst, data, e.perm); err != nil {
				return RunResult{Step: StepBinaries, Status: JournalFailed, Error: fmt.Sprintf("write %s: %v", e.dst, err)}
			}
		}
	}
	if s.probes.CommandOutput != nil {
		if _, err := s.probes.CommandOutput(ctx, "systemd-tmpfiles", "--create"); err != nil {
			return RunResult{Step: StepBinaries, Status: JournalFailed, Error: fmt.Sprintf("systemd-tmpfiles --create: %v", err)}
		}
	}
	// Best-effort: install shell completions so `omahab` is immediately
	// discoverable in the user's shell (bash/zsh/fish). Failures here never
	// fail the step — the CLI is already in /usr/bin which is on PATH, but
	// completions make tab-complete work without manual `omahab completion`.
	installShellCompletions(ctx, s.probes)
	// Best-effort hash bookkeeping: hash the two binaries on the destination
	// filesystem and store dest→sha256 json in journal state. Do not fail the
	// step if hashing fails.
	if s.journal != nil && s.probes.SHA256File != nil {
		hashes := make(map[string]string)
		for _, p := range []string{"/usr/bin/omahab", "/usr/bin/omahabd"} {
			if h, err := s.probes.SHA256File(p); err == nil {
				hashes[p] = h
			}
		}
		if len(hashes) > 0 {
			if b, err := json.Marshal(hashes); err == nil {
				_ = s.journal.SetState(ctx, "binaries_sha256", string(b))
			}
		}
	}
	return RunResult{Step: StepBinaries, Status: JournalCompleted}
}

// completionTarget ties a cobra completion shell to its system location.
type completionTarget struct {
	shell string
	dst   string
}

var completionTargets = []completionTarget{
	{"bash", "/usr/share/bash-completion/completions/omahab"},
	{"zsh", "/usr/share/zsh/site-functions/_omahab"},
	{"fish", "/usr/share/fish/vendor_completions.d/omahab.fish"},
}

// installShellCompletions attempts to generate and write shell completions for
// each target. It shells out to the freshly installed /usr/bin/omahab via
// probes.CommandOutput so the completion output always matches the installed
// binary. Any failure is silent — completions are a convenience, not a
// correctness requirement.
func installShellCompletions(ctx context.Context, p Probes) {
	if p.CommandOutput == nil || p.WriteFile == nil {
		return
	}
	for _, t := range completionTargets {
		out, err := p.CommandOutput(ctx, "/usr/bin/omahab", "completion", t.shell)
		if err != nil || strings.TrimSpace(out) == "" {
			continue
		}
		dir := path.Dir(t.dst)
		if p.MkdirAll != nil {
			_ = p.MkdirAll(dir, 0o755)
		}
		// Idempotent write: skip if existing content identical.
		if p.ReadFile != nil {
			if existing, err := p.ReadFile(t.dst); err == nil && string(existing) == out {
				continue
			}
		}
		// Best-effort write; ignore errors (e.g. read-only /usr/share in tests).
		_ = p.WriteFile(t.dst, []byte(out), 0o644)
	}
}

// RollbackBinaries removes the exact files the binaries step installed.
//
// It removes /usr/bin/omahab, /usr/bin/omahabd, the six systemd units,
// /usr/lib/tmpfiles.d/omahab.conf, and shell completions. It does NOT remove
// /usr/share/omahab (catalog/web) — those trees may contain data later steps
// referenced and leaving them is safer on rollback than deleting shared
// application assets.
func RollbackBinaries(ctx context.Context, p Probes) error {
	paths := []string{
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
	for _, dest := range paths {
		if p.RemoveFile != nil {
			if err := p.RemoveFile(dest); err != nil {
				if isNotExist(err) {
					continue
				}
				// Be tolerant of fake errors that are not wrapped fs.ErrNotExist
				// but still represent a missing file.
				low := strings.ToLower(err.Error())
				if strings.Contains(low, "no such file") || strings.Contains(low, "not exist") || strings.Contains(low, "file does not exist") {
					continue
				}
				return fmt.Errorf("remove %s: %w", dest, err)
			}
		}
	}
	if p.Systemctl != nil {
		_, _ = p.Systemctl(ctx, "daemon-reload")
	}
	return nil
}
