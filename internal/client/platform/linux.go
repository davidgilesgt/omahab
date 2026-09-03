//go:build linux

package platform

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/godbus/dbus/v5"
	"github.com/zalando/go-keyring"
)

// --- EnvApplier linux ---

type LinuxEnvApplier struct{}

func NewEnvApplier() EnvApplier { return &LinuxEnvApplier{} }

func (e *LinuxEnvApplier) FilePath() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); strings.TrimSpace(dir) != "" {
		return filepath.Join(strings.TrimSpace(dir), "environment.d", DefaultEnvFile)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".config", "environment.d", DefaultEnvFile)
	}
	return filepath.Join(os.TempDir(), DefaultEnvFile)
}

func (e *LinuxEnvApplier) SetEnvironment(assignments []string) error {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return fmt.Errorf("D-Bus session bus not available: %w", err)
	}
	defer conn.Close()
	obj := conn.Object("org.freedesktop.systemd1", dbus.ObjectPath("/org/freedesktop/systemd1"))
	call := obj.Call("org.freedesktop.systemd1.Manager.SetEnvironment", 0, assignments)
	if call.Err != nil {
		return fmt.Errorf("SetEnvironment: %w", call.Err)
	}
	return nil
}

func (e *LinuxEnvApplier) UnsetEnvironment(names []string) error {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return fmt.Errorf("D-Bus session bus not available: %w", err)
	}
	defer conn.Close()
	obj := conn.Object("org.freedesktop.systemd1", dbus.ObjectPath("/org/freedesktop/systemd1"))
	call := obj.Call("org.freedesktop.systemd1.Manager.UnsetEnvironment", 0, names)
	if call.Err != nil {
		return fmt.Errorf("UnsetEnvironment: %w", call.Err)
	}
	return nil
}

func (e *LinuxEnvApplier) EnsureShellInclude() error {
	// No-op on Linux: environment.d is sourced by systemd user manager automatically.
	return nil
}

// --- Scheduler linux ---

type LinuxScheduler struct{}

func NewScheduler() Scheduler { return &LinuxScheduler{} }

func (s *LinuxScheduler) Install(paths []string) error {
	if len(paths) == 0 {
		return fmt.Errorf("paths required")
	}
	// Clean paths (same logic as backup_drive.EnableBackupDrive)
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
		return fmt.Errorf("no valid paths")
	}
	cfgPath := backupPathsFile()
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(cfgPath), err)
	}
	content := strings.Join(cleaned, "\n") + "\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0600); err != nil {
		return fmt.Errorf("write %s: %w", cfgPath, err)
	}
	dir := systemdUserDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("mkdir systemd user dir: %w", err)
	}
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
	_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	_ = exec.Command("systemctl", "--user", "enable", "--now", "omahab-machine-backup.timer").Run()
	return nil
}

func (s *LinuxScheduler) Uninstall() error {
	dir := systemdUserDir()
	_ = exec.Command("systemctl", "--user", "disable", "--now", "omahab-machine-backup.timer").Run()
	_ = os.Remove(filepath.Join(dir, "omahab-machine-backup.service"))
	_ = os.Remove(filepath.Join(dir, "omahab-machine-backup.timer"))
	_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	return nil
}

func (s *LinuxScheduler) IsInstalled() bool {
	tmrPath := filepath.Join(systemdUserDir(), "omahab-machine-backup.timer")
	_, err := os.Stat(tmrPath)
	return err == nil
}

func backupPathsFile() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); strings.TrimSpace(dir) != "" {
		return filepath.Join(strings.TrimSpace(dir), "omahab", "backup-paths")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".config", "omahab", "backup-paths")
	}
	return filepath.Join(os.TempDir(), "omahab-backup-paths")
}

func systemdUserDir() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); strings.TrimSpace(dir) != "" {
		return filepath.Join(strings.TrimSpace(dir), "systemd", "user")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".config", "systemd", "user")
	}
	return filepath.Join(os.TempDir(), "systemd-user")
}

// --- Terminal linux ---

type LinuxTerminal struct{}

func NewTerminal() Terminal { return &LinuxTerminal{} }

func (t *LinuxTerminal) OpenURL(url string) error {
	cmd := exec.Command("xdg-open", url)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("open url: %w", err)
	}
	return nil
}

func (t *LinuxTerminal) OpenTerminal(dir string) error {
	terms := [][]string{
		{"xdg-terminal-exec", dir},
		{"alacritty", "--working-directory", dir},
		{"kitty", "--directory", dir},
		{"ghostty", "--working-directory=" + dir},
		{"gnome-terminal", "--working-directory=" + dir},
	}
	for _, tt := range terms {
		if _, err := exec.LookPath(tt[0]); err == nil {
			cmd := exec.Command(tt[0], tt[1:]...)
			cmd.Dir = dir
			if err := cmd.Start(); err == nil {
				return nil
			}
		}
	}
	return t.OpenURL("file://" + dir)
}

func (t *LinuxTerminal) OpenTerminalCommand(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("no command provided")
	}
	terms := [][]string{
		append([]string{"alacritty", "-e"}, args...),
		append([]string{"kitty"}, args...),
		append([]string{"ghostty", "-e"}, args...),
		append([]string{"kitty", "--"}, args...),
		append([]string{"gnome-terminal", "--"}, args...),
		append([]string{"xterm", "-e"}, args...),
		append([]string{"xdg-terminal-exec"}, args...),
	}
	for _, tt := range terms {
		if _, err := exec.LookPath(tt[0]); err == nil {
			cmd := exec.Command(tt[0], tt[1:]...)
			if err := cmd.Start(); err == nil {
				return nil
			}
		}
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err == nil {
		return nil
	}
	return fmt.Errorf("no terminal found to run %v", args)
}

// --- Notifier linux ---

type LinuxNotifier struct{}

func NewNotifier() Notifier { return &LinuxNotifier{} }

func (n *LinuxNotifier) Notify(title, body string) error {
	cmd := exec.Command("notify-send", "--app-name=Omahab", "--urgency=normal", title, body)
	if err := cmd.Run(); err != nil {
		cmd2 := exec.Command("notify-send", title, body)
		return cmd2.Run()
	}
	return nil
}

// --- SecretStore linux ---

type LinuxSecretStore struct{}

func NewSecretStore() SecretStore { return &LinuxSecretStore{} }

func (s *LinuxSecretStore) Get(service, account string) (string, error) {
	v, err := keyring.Get(service, account)
	if err != nil {
		if isNotFound(err) {
			return "", fmt.Errorf("credential not found")
		}
		if isSecretServiceUnavailable(err) {
			return "", fmt.Errorf("keyring unavailable (Secret Service not found: %v) — is org.freedesktop.secrets running? Ensure a keyring daemon (gnome-keyring, kwallet) is active in this desktop session", err)
		}
		return "", fmt.Errorf("keyring get %s/%s: %w", service, account, err)
	}
	if strings.TrimSpace(v) == "" {
		return "", fmt.Errorf("credential not found")
	}
	return v, nil
}

func (s *LinuxSecretStore) Set(service, account, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("keyring set %s/%s: value is required", service, account)
	}
	err := keyring.Set(service, account, value)
	if err != nil {
		if isSecretServiceUnavailable(err) {
			return fmt.Errorf("keyring unavailable (Secret Service not found: %v) — is org.freedesktop.secrets running? Ensure a keyring daemon (gnome-keyring, kwallet) is active in this desktop session", err)
		}
		return fmt.Errorf("keyring set %s/%s: %w", service, account, err)
	}
	return nil
}

func (s *LinuxSecretStore) Delete(service, account string) error {
	err := keyring.Delete(service, account)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		if isSecretServiceUnavailable(err) {
			return fmt.Errorf("keyring unavailable (Secret Service not found: %v) — is org.freedesktop.secrets running? Ensure a keyring daemon (gnome-keyring, kwallet) is active in this desktop session", err)
		}
		return fmt.Errorf("keyring delete %s/%s: %w", service, account, err)
	}
	return nil
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") || strings.Contains(msg, "no such") || err == keyring.ErrNotFound
}

func isSecretServiceUnavailable(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "secret") || strings.Contains(msg, "dbus") || strings.Contains(msg, "keyring") && strings.Contains(msg, "not available")
}
