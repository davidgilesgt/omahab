package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// BackupDriveStatus is the last snapshot info for a machine backup.
type BackupDriveStatus struct {
	LastSnapshotTime *time.Time `json:"last_snapshot_time,omitempty"`
	SnapshotID       string     `json:"snapshot_id,omitempty"`
	Error            string     `json:"error,omitempty"`
}

// BackupPathsFile returns the config file path for backup-drive paths.
// Default: ~/.config/omahab/backup-paths (XDG_CONFIG_HOME or $HOME/.config)
func BackupPathsFile() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "omahab", "backup-paths")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".config", "omahab", "backup-paths")
	}
	return filepath.Join(os.TempDir(), "omahab-backup-paths")
}

// SystemdUserDir returns ~/.config/systemd/user
func SystemdUserDir() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "systemd", "user")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".config", "systemd", "user")
	}
	return filepath.Join(os.TempDir(), "systemd-user")
}

// DefaultBackupPaths returns the default backup paths (HOME) and exclude patterns.
func DefaultBackupPaths() []string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.Getenv("HOME")
	}
	if home == "" {
		return []string{"."}
	}
	return []string{home}
}

// ExcludePatterns returns the default exclude patterns for machine backups.
func ExcludePatterns() []string {
	return []string{
		".cache",
		".local/share/Trash",
		"**/node_modules",
		"**/.git",
		"**/.cache",
		"Trash",
	}
}

// EnableBackupDrive writes the backup-paths file and installs the systemd user timer.
// paths is a slice of absolute paths; empty means DefaultBackupPaths().
func EnableBackupDrive(paths []string) error {
	if len(paths) == 0 {
		paths = DefaultBackupPaths()
	}
	// Expand ~ and clean.
	var cleaned []string
	for _, p := range paths {
		trim := strings.TrimSpace(p)
		if trim == "" {
			continue
		}
		if strings.HasPrefix(trim, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				trim = filepath.Join(home, trim[2:])
			}
		} else if trim == "~" {
			if home, err := os.UserHomeDir(); err == nil {
				trim = home
			}
		}
		trim = filepath.Clean(trim)
		if trim != "" {
			cleaned = append(cleaned, trim)
		}
	}
	if len(cleaned) == 0 {
		cleaned = DefaultBackupPaths()
	}
	// Write backup-paths file.
	cfgPath := BackupPathsFile()
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(cfgPath), err)
	}
	content := strings.Join(cleaned, "\n") + "\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0600); err != nil {
		return fmt.Errorf("write %s: %w", cfgPath, err)
	}
	// Install systemd user units.
	dir := SystemdUserDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("mkdir systemd user dir: %w", err)
	}
	// Discover omahab binary for ExecStart.
	bin := "omahab"
	if exe, err := os.Executable(); err == nil && strings.Contains(exe, "omahab") {
		bin = exe
	} else if p, err := exec.LookPath("omahab"); err == nil {
		bin = p
	}
	serviceContent := fmt.Sprintf(`[Unit]
Description=Omahab machine backup (restic)
Wants=network-online.target
After=network-online.target

[Service]
Type=oneshot
ExecStart=%s backup-drive run
`, bin)
	timerContent := `[Unit]
Description=Omahab machine backup timer (daily)

[Timer]
OnCalendar=daily
Persistent=true

[Install]
WantedBy=timers.target
`
	svcPath := filepath.Join(dir, "omahab-machine-backup.service")
	tmrPath := filepath.Join(dir, "omahab-machine-backup.timer")
	if err := os.WriteFile(svcPath, []byte(serviceContent), 0644); err != nil {
		return fmt.Errorf("write service: %w", err)
	}
	if err := os.WriteFile(tmrPath, []byte(timerContent), 0644); err != nil {
		return fmt.Errorf("write timer: %w", err)
	}
	// Enable timer via systemctl --user (best-effort).
	_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	_ = exec.Command("systemctl", "--user", "enable", "--now", "omahab-machine-backup.timer").Run()
	return nil
}

// LoadBackupPaths reads the backup-paths file, falling back to default.
func LoadBackupPaths() ([]string, error) {
	path := BackupPathsFile()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return DefaultBackupPaths(), nil
		}
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		trim := strings.TrimSpace(line)
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		out = append(out, trim)
	}
	if len(out) == 0 {
		return DefaultBackupPaths(), nil
	}
	return out, nil
}

// backupEnv builds the restic environment from the keyring.
func backupEnv(creds CredentialStore) ([]string, error) {
	if creds == nil {
		creds = NewMemoryCredentialStore()
	}
	repo, err := creds.Get(CredentialService, CredentialBackupRepo)
	if err != nil {
		return nil, fmt.Errorf("backup repo not configured: run 'omahab backup-drive enable' after enrollment: %w", err)
	}
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return nil, fmt.Errorf("backup repo empty")
	}
	pass, err := creds.Get(CredentialService, CredentialBackupPassword)
	if err != nil {
		return nil, fmt.Errorf("backup password not configured: %w", err)
	}
	restUser, _ := creds.Get(CredentialService, CredentialBackupRestUser)
	restPass, _ := creds.Get(CredentialService, CredentialBackupRestPassword)

	// Fallback: if rest user/pass missing, try to derive from repo? But require them.
	env := os.Environ()
	// Filter out existing RESTIC_* to avoid leakage? Keep.
	env = append(env, "RESTIC_REPOSITORY="+repo)
	env = append(env, "RESTIC_PASSWORD="+strings.TrimSpace(pass))
	if strings.TrimSpace(restUser) != "" {
		env = append(env, "RESTIC_REST_USERNAME="+strings.TrimSpace(restUser))
	}
	if strings.TrimSpace(restPass) != "" {
		env = append(env, "RESTIC_REST_PASSWORD="+strings.TrimSpace(restPass))
	}
	return env, nil
}

// RunBackupDrive performs restic backup + forget (no prune) for the configured paths.
// It uses the keyring for credentials at runtime and never writes them to files.
func RunBackupDrive(ctx context.Context, creds CredentialStore) error {
	paths, err := LoadBackupPaths()
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		return fmt.Errorf("no backup paths configured")
	}
	env, err := backupEnv(creds)
	if err != nil {
		return err
	}
	// Generate exclude file.
	excludes := ExcludePatterns()
	tmpExclude, err := os.CreateTemp("", "omahab-exclude-*")
	if err != nil {
		return fmt.Errorf("create exclude file: %w", err)
	}
	defer os.Remove(tmpExclude.Name())
	for _, pat := range excludes {
		fmt.Fprintln(tmpExclude, pat)
	}
	tmpExclude.Close()

	// Ensure repo is initialized (best-effort). Try init if snapshots fails? We'll try backup and if it reports not initialized, run init.
	// Build backup args.
	backupArgs := []string{"backup", "--exclude-file", tmpExclude.Name()}
	backupArgs = append(backupArgs, paths...)

	// Run restic backup.
	if err := runRestic(ctx, env, backupArgs...); err != nil {
		// If error indicates repo not initialized, try init and retry once.
		if strings.Contains(strings.ToLower(err.Error()), "not initialized") || strings.Contains(err.Error(), "is not a valid repository") {
			if initErr := runRestic(ctx, env, "init"); initErr == nil {
				// Retry backup.
				if err2 := runRestic(ctx, env, backupArgs...); err2 == nil {
					err = nil
				} else {
					return fmt.Errorf("restic backup after init: %w", err2)
				}
			}
		}
		if err != nil {
			return fmt.Errorf("restic backup: %w", err)
		}
	}
	// Run forget (no prune, append-only server).
	forgetArgs := []string{"forget", "--keep-daily", "14", "--keep-weekly", "8", "--keep-monthly", "12"}
	if err := runRestic(ctx, env, forgetArgs...); err != nil {
		// Forget is best-effort; on append-only, prune not allowed but forget should succeed.
		// If it fails due to no snapshots, ignore.
		if !strings.Contains(err.Error(), "no snapshots") {
			return fmt.Errorf("restic forget: %w", err)
		}
	}
	return nil
}

// StatusBackupDrive returns the last snapshot time from `restic snapshots --latest 1 --json`.
func StatusBackupDrive(ctx context.Context, creds CredentialStore) (*BackupDriveStatus, error) {
	env, err := backupEnv(creds)
	if err != nil {
		return &BackupDriveStatus{Error: err.Error()}, err
	}
	args := []string{"snapshots", "--latest", "1", "--json"}
	out, err := runResticOutput(ctx, env, args...)
	if err != nil {
		// If repo not yet initialized or no snapshots, return no snapshot.
		if strings.Contains(err.Error(), "not initialized") || strings.Contains(strings.ToLower(err.Error()), "no snapshot") {
			return &BackupDriveStatus{}, nil
		}
		return &BackupDriveStatus{Error: err.Error()}, err
	}
	// Parse output: restic snapshots --json returns JSON array of snapshots.
	var snaps []struct {
		ID   string `json:"id"`
		Time string `json:"time"`
	}
	if err := json.Unmarshal(out, &snaps); err != nil {
		// Try single object.
		var single struct {
			ID   string `json:"id"`
			Time string `json:"time"`
		}
		if err2 := json.Unmarshal(out, &single); err2 == nil && single.ID != "" {
			snaps = []struct {
				ID   string `json:"id"`
				Time string `json:"time"`
			}{{ID: single.ID, Time: single.Time}}
		} else {
			return &BackupDriveStatus{Error: fmt.Sprintf("parse snapshots: %v", err)}, err
		}
	}
	if len(snaps) == 0 {
		return &BackupDriveStatus{}, nil
	}
	// Find latest by time (already sorted latest 1 should be latest).
	snap := snaps[0]
	t, err := time.Parse(time.RFC3339, snap.Time)
	if err != nil {
		// Try RFC3339Nano.
		t, _ = time.Parse(time.RFC3339Nano, snap.Time)
	}
	return &BackupDriveStatus{LastSnapshotTime: &t, SnapshotID: snap.ID}, nil
}

func runRestic(ctx context.Context, env []string, args ...string) error {
	_, err := runResticOutput(ctx, env, args...)
	return err
}

func runResticOutput(ctx context.Context, env []string, args ...string) ([]byte, error) {
	bin := "restic"
	if p, err := exec.LookPath("restic"); err == nil {
		bin = p
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("%s", msg)
	}
	return stdout.Bytes(), nil
}
