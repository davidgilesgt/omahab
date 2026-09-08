package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// Paths for upgrade. Vars so tests can override to temp dirs.
var (
	canonicalFlakeDir    = "/etc/omahab/flake"
	installedHardwareRel = "nix/installed-hardware.nix"
	installLocalRel      = "nix/install-local.nix"
)

var (
	releaseFile           = "/etc/omahab-release"
	defaultReleaseURL     = "https://github.com/davidgilesgt/omahab/releases/latest/download/manifest.json"
	updateAvailablePath   = "/var/lib/omahab/update-available"
	updateCandidatePath   = "/var/lib/omahab/update-candidate.json"
)

// Upgrade result envelope.
type upgradeResult struct {
	Ref        string `json:"ref"`
	Result     string `json:"result"`
	RolledBack bool   `json:"rolled_back"`
	Error      string `json:"error,omitempty"`
}

type releaseManifest struct {
	Version     string `json:"version"`
	FlakeRef    string `json:"flake_ref"`
	ISO         string `json:"iso"`
	ISOSHA256   string `json:"iso_sha256"`
	PublishedAt string `json:"published_at"`
}

type checkUpdateResult struct {
	Current         string `json:"current"`
	Latest          string `json:"latest"`
	UpdateAvailable bool   `json:"update_available"`
	FlakeRef        string `json:"flake_ref"`
}

type updateCandidate struct {
	Version  string `json:"version"`
	FlakeRef string `json:"flake_ref"`
}

// Injectable operations for tests.

var (
	fsStat      = os.Stat
	fsReadFile  = os.ReadFile
	fsWriteFile = os.WriteFile
	fsMkdirAll  = os.MkdirAll
	fsRemove    = os.Remove
	fsRemoveAll = os.RemoveAll
	fsRename    = os.Rename
	fsMkdirTemp = os.MkdirTemp
	osChownFunc = os.Chown

	copyDirFunc                  = defaultCopyDir
	runNixMetadataFunc           = defaultRunNixMetadata
	runNixosRebuildSwitchFunc    = defaultRunNixosRebuildSwitch
	runNixosRebuildRollbackFunc  = defaultRunNixosRebuildRollback
	probeUpFunc                  = probeUp
	httpGetFunc                  = defaultHttpGet
	exchangeDirsFunc             = exchangeDirs
	healthTimeout                = 120 * time.Second
	healthInterval               = 5 * time.Second
	sleepFunc                    = time.Sleep
	nowFunc                      = time.Now
)

func defaultHttpGet(url string) (*http.Response, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	return client.Get(url)
}

func defaultCopyDir(src, dst string) error {
	info, err := fsStat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		data, err := fsReadFile(src)
		if err != nil {
			return err
		}
		if err := fsMkdirAll(filepath.Dir(dst), 0755); err != nil {
			return err
		}
		return fsWriteFile(dst, data, info.Mode())
	}
	if err := fsMkdirAll(dst, 0755); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		s := filepath.Join(src, e.Name())
		d := filepath.Join(dst, e.Name())
		if e.IsDir() {
			if err := defaultCopyDir(s, d); err != nil {
				return err
			}
		} else {
			// Handle symlinks: copy as file for now; if symlink, read target
			fi, err := e.Info()
			if err != nil {
				return err
			}
			mode := fi.Mode()
			if mode&os.ModeSymlink != 0 {
				// Resolve symlink target and copy file content if it's a file, or dir
				target, err := os.Readlink(s)
				if err != nil {
					return err
				}
				// If target is absolute, use it; else relative to src dir
				if !filepath.IsAbs(target) {
					target = filepath.Join(filepath.Dir(s), target)
				}
				// Recursively copy target to dst
				tInfo, err := fsStat(target)
				if err != nil {
					return err
				}
				if tInfo.IsDir() {
					if err := defaultCopyDir(target, d); err != nil {
						return err
					}
				} else {
					data, err := fsReadFile(target)
					if err != nil {
						return err
					}
					if err := fsWriteFile(d, data, tInfo.Mode()); err != nil {
						return err
					}
				}
				continue
			}
			data, err := fsReadFile(s)
			if err != nil {
				return err
			}
			if err := fsWriteFile(d, data, mode); err != nil {
				return err
			}
		}
	}
	return nil
}

func defaultRunNixMetadata(ref string) (string, error) {
	out, err := exec.Command("nix", "flake", "metadata", "--json", ref).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("nix flake metadata: %w: %s", err, string(ee.Stderr))
		}
		return "", fmt.Errorf("nix flake metadata: %w: %s", err, string(out))
	}
	return parseMetadataPath(out)
}

func parseMetadataPath(data []byte) (string, error) {
	// Try top-level "path"
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return "", fmt.Errorf("parse metadata: %w", err)
	}
	if v, ok := raw["path"]; ok {
		var p string
		if err := json.Unmarshal(v, &p); err == nil && strings.TrimSpace(p) != "" {
			return strings.TrimSpace(p), nil
		}
	}
	// Fallback: locked.path or resolved path structures
	var m struct {
		Path   string `json:"path"`
		Locked struct {
			Path string `json:"path"`
		} `json:"locked"`
		Resolved struct {
			Path string `json:"path"`
		} `json:"resolved"`
	}
	if err := json.Unmarshal(data, &m); err == nil {
		if strings.TrimSpace(m.Path) != "" {
			return strings.TrimSpace(m.Path), nil
		}
		if strings.TrimSpace(m.Locked.Path) != "" {
			return strings.TrimSpace(m.Locked.Path), nil
		}
		if strings.TrimSpace(m.Resolved.Path) != "" {
			return strings.TrimSpace(m.Resolved.Path), nil
		}
	}
	return "", fmt.Errorf("metadata missing path: %s", string(data))
}

func defaultRunNixosRebuildSwitch(flakeArg string) ([]byte, error) {
	return exec.Command("nixos-rebuild", "switch", "--flake", flakeArg).CombinedOutput()
}

func defaultRunNixosRebuildRollback() ([]byte, error) {
	return exec.Command("nixos-rebuild", "switch", "--rollback").CombinedOutput()
}

func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := fsMkdirAll(dir, 0755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// Ensure cleanup on failure if not renamed
	cleanup := true
	defer func() {
		tmp.Close()
		if cleanup {
			_ = fsRemove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		return err
	}
	// Best-effort chown to root
	_ = osChownFunc(tmpName, 0, 0)
	if err := tmp.Close(); err != nil {
		return err
	}
	// On darwin, need to ensure file is synced? Not required.
	if err := fsRename(tmpName, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}

// newSystemCmd builds `omahab system`
func newSystemCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "system",
		Short: "Appliance system operations",
	}
	cmd.AddCommand(newSystemUpgradeCmd())
	cmd.AddCommand(newSystemCheckUpdateCmd())
	return cmd
}

func newSystemUpgradeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "upgrade",
		Short: "Switch to the pinned system generation (with rollback on failure)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSystemUpgrade()
		},
	}
}

func runSystemUpgrade() error {
	// 1. Require canonical flake + machine modules before any switching.
	if _, err := fsStat(canonicalFlakeDir); err != nil {
		return fmt.Errorf("canonical flake %s missing: %w", canonicalFlakeDir, err)
	}
	if _, err := fsStat(filepath.Join(canonicalFlakeDir, "flake.nix")); err != nil {
		return fmt.Errorf("canonical flake %s/flake.nix missing: %w", canonicalFlakeDir, err)
	}
	for _, rel := range []string{installedHardwareRel, installLocalRel} {
		p := filepath.Join(canonicalFlakeDir, rel)
		if _, err := fsStat(p); err != nil {
			return fmt.Errorf("required %s missing: %w", p, err)
		}
	}

	// 2. Load candidate.
	candBytes, err := fsReadFile(updateCandidatePath)
	if err != nil {
		if os.IsNotExist(err) {
			msg := "no pending update (run omahab system check-update first)"
			if flagJSON {
				_ = printJSON(upgradeResult{Result: "no_pending", Error: msg})
			} else {
				fmt.Println(msg)
			}
			return nil
		}
		return fmt.Errorf("read candidate %s: %w", updateCandidatePath, err)
	}
	var cand updateCandidate
	if err := json.Unmarshal(candBytes, &cand); err != nil {
		return fmt.Errorf("invalid candidate %s: %w", updateCandidatePath, err)
	}
	cand.Version = strings.TrimSpace(cand.Version)
	cand.FlakeRef = strings.TrimSpace(cand.FlakeRef)
	if cand.Version == "" || cand.FlakeRef == "" {
		return fmt.Errorf("candidate %s missing version or flake_ref", updateCandidatePath)
	}

	// 3. Resolve flake ref to immutable store path.
	storePath, err := runNixMetadataFunc(cand.FlakeRef)
	if err != nil {
		return fmt.Errorf("resolve %q: %w", cand.FlakeRef, err)
	}
	storePath = strings.TrimSpace(storePath)
	if storePath == "" {
		return fmt.Errorf("resolved store path empty for %q", cand.FlakeRef)
	}
	if _, err := fsStat(storePath); err != nil {
		return fmt.Errorf("resolved store path %s missing: %w", storePath, err)
	}

	// 4. Stage: copy store path to temp sibling.
	parentDir := filepath.Dir(canonicalFlakeDir)
	stagingDir, err := fsMkdirTemp(parentDir, ".flake-staging-*")
	if err != nil {
		return fmt.Errorf("create staging: %w", err)
	}
	// Ensure staging is removed on any failure; on success after exchange it holds old tree which we still want to remove.
	defer func() {
		_ = fsRemoveAll(stagingDir)
	}()

	if err := copyDirFunc(storePath, stagingDir); err != nil {
		return fmt.Errorf("copy store %s to staging: %w", storePath, err)
	}

	// 5. Overlay exactly the two machine modules from canonical to staging.
	for _, rel := range []string{installedHardwareRel, installLocalRel} {
		src := filepath.Join(canonicalFlakeDir, rel)
		dst := filepath.Join(stagingDir, rel)
		data, err := fsReadFile(src)
		if err != nil {
			return fmt.Errorf("read %s: %w", src, err)
		}
		if err := fsMkdirAll(filepath.Dir(dst), 0755); err != nil {
			return fmt.Errorf("mkdir %s: %w", filepath.Dir(dst), err)
		}
		if err := fsWriteFile(dst, data, 0644); err != nil {
			return fmt.Errorf("write %s: %w", dst, err)
		}
	}

	// 6. Rebuild.
	flakeArg := "path:" + stagingDir + "#omahab-installed"
	if !flagJSON {
		fmt.Printf("Switching to %s (resolved %s) …\n", cand.FlakeRef, storePath)
	}
	out, err := runNixosRebuildSwitchFunc(flakeArg)
	if err != nil {
		return fmt.Errorf("nixos-rebuild switch: %w: %s", err, lastLines(string(out), 5))
	}

	// 7. Health gate.
	deadline := nowFunc().Add(healthTimeout)
	healthOK := false
	for nowFunc().Before(deadline) {
		if err := probeUpFunc(); err == nil {
			healthOK = true
			break
		}
		sleepFunc(healthInterval)
	}
	if !healthOK {
		if !flagJSON {
			fmt.Println("Health check failed — rolling back.")
		}
		out, err := runNixosRebuildRollbackFunc()
		if err != nil {
			if flagJSON {
				_ = printJSON(upgradeResult{Ref: cand.FlakeRef, Result: "rolled_back", RolledBack: true, Error: "health check failed"})
				return handleFailure(fmt.Errorf("rollback failed: %w: %s", err, lastLines(string(out), 5)))
			}
			return fmt.Errorf("rollback failed: %w: %s", err, lastLines(string(out), 5))
		}
		if flagJSON {
			_ = printJSON(upgradeResult{Ref: cand.FlakeRef, Result: "rolled_back", RolledBack: true, Error: "health check failed"})
			return handleFailure(fmt.Errorf("upgrade failed health check; rolled back"))
		}
		fmt.Println("Rolled back to the previous generation.")
		return fmt.Errorf("upgrade failed health check; rolled back")
	}

	// 8. Promotion: atomically exchange staging and canonical.
	if err := exchangeDirsFunc(stagingDir, canonicalFlakeDir); err != nil {
		if !flagJSON {
			fmt.Printf("Promotion failed (%v) — rolling back.\n", err)
		}
		out, rbErr := runNixosRebuildRollbackFunc()
		if rbErr != nil {
			if flagJSON {
				_ = printJSON(upgradeResult{Ref: cand.FlakeRef, Result: "rolled_back", RolledBack: true, Error: fmt.Sprintf("promotion failed: %v", err)})
				return handleFailure(fmt.Errorf("rollback failed after promotion failure: %w: %s", rbErr, lastLines(string(out), 5)))
			}
			return fmt.Errorf("promotion failed: %v; rollback failed: %w: %s", err, rbErr, lastLines(string(out), 5))
		}
		if flagJSON {
			_ = printJSON(upgradeResult{Ref: cand.FlakeRef, Result: "rolled_back", RolledBack: true, Error: fmt.Sprintf("promotion failed: %v", err)})
			return handleFailure(fmt.Errorf("promotion failed: %v; rolled back", err))
		}
		return fmt.Errorf("promotion failed: %v; rolled back", err)
	}
	// Success: clear pending markers. stagingDir now holds old tree; defer will remove it.
	_ = fsRemove(updateCandidatePath)
	_ = fsRemove(updateAvailablePath)

	if flagJSON {
		return printJSON(upgradeResult{Ref: cand.FlakeRef, Result: "healthy"})
	}
	fmt.Println("Upgrade healthy.")
	return nil
}

func newSystemCheckUpdateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "check-update",
		Short: "Check for a newer release",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSystemCheckUpdate()
		},
	}
}

func runSystemCheckUpdate() error {
	url := strings.TrimSpace(os.Getenv("OMAHAB_RELEASE_URL"))
	if url == "" {
		url = defaultReleaseURL
	}
	resp, err := httpGetFunc(url)
	if err != nil {
		return handleFailure(fmt.Errorf("check %s: %w", url, err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return handleFailure(fmt.Errorf("check %s: unexpected status %d", url, resp.StatusCode))
	}
	var m releaseManifest
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return handleFailure(fmt.Errorf("manifest %s: %w", url, err))
	}
	m.Version = strings.TrimSpace(m.Version)
	m.FlakeRef = strings.TrimSpace(m.FlakeRef)
	if m.Version == "" || m.FlakeRef == "" {
		return handleFailure(fmt.Errorf("manifest %s: missing version or flake_ref", url))
	}
	cur := strings.TrimPrefix(strings.TrimSpace(version), "v")
	latestRaw := m.Version
	latest := strings.TrimPrefix(latestRaw, "v")
	updateAvailable := latest != "" && latest != cur
	res := checkUpdateResult{
		Current:         cur,
		Latest:          latest,
		UpdateAvailable: updateAvailable,
		FlakeRef:        m.FlakeRef,
	}
	if updateAvailable {
		// Validate flake ref resolves before persisting.
		if _, err := runNixMetadataFunc(m.FlakeRef); err != nil {
			return handleFailure(fmt.Errorf("resolve flake %q: %w", m.FlakeRef, err))
		}
		cand := updateCandidate{Version: latestRaw, FlakeRef: m.FlakeRef}
		data, err := json.Marshal(cand)
		if err != nil {
			return handleFailure(fmt.Errorf("marshal candidate: %w", err))
		}
		if err := atomicWriteFile(updateCandidatePath, data, 0600); err != nil {
			return handleFailure(fmt.Errorf("write candidate: %w", err))
		}
		if err := atomicWriteFile(updateAvailablePath, []byte(latest), 0644); err != nil {
			return handleFailure(fmt.Errorf("write update-available: %w", err))
		}
	} else {
		// No update: clear both markers.
		_ = fsRemove(updateAvailablePath)
		_ = fsRemove(updateCandidatePath)
	}
	if flagJSON {
		return printJSON(res)
	}
	if updateAvailable {
		fmt.Printf("Update available: v%s → v%s\n  sudo omahab system upgrade\n", cur, latest)
	} else {
		fmt.Printf("Up to date (v%s)\n", cur)
	}
	return nil
}

func probeUp() error {
	out, err := exec.Command("curl", "-sf", "-m", "5", "http://127.0.0.1:8484/up").Output()
	if err != nil {
		return err
	}
	if !strings.Contains(string(out), "up") {
		return fmt.Errorf("unexpected /up body: %s", out)
	}
	return nil
}

func readFileOr(path, fallback string) string {
	data, err := fsReadFile(path)
	if err != nil {
		return fallback
	}
	return string(data)
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}
