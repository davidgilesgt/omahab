//go:build darwin

package platform

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/zalando/go-keyring"
)

// --- EnvApplier darwin ---

type DarwinEnvApplier struct{}

func NewEnvApplier() EnvApplier { return &DarwinEnvApplier{} }

func (e *DarwinEnvApplier) FilePath() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); strings.TrimSpace(dir) != "" {
		return filepath.Join(strings.TrimSpace(dir), "omahab", "agent-tools.env")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".config", "omahab", "agent-tools.env")
	}
	return filepath.Join(os.TempDir(), "omahab-agent-tools.env")
}

func (e *DarwinEnvApplier) SetEnvironment(assignments []string) error {
	for _, a := range assignments {
		idx := strings.Index(a, "=")
		if idx <= 0 {
			continue
		}
		key := a[:idx]
		val := a[idx+1:]
		if len(val) >= 2 && val[0] == '\'' && val[len(val)-1] == '\'' {
			inner := val[1 : len(val)-1]
			val = ""
			for i := 0; i < len(inner); i++ {
				if inner[i] == '\'' && i+1 < len(inner) && inner[i+1] == '\'' {
					val += "'"
					i++
				} else {
					val += string(inner[i])
				}
			}
		}
		cmd := exec.Command("launchctl", "setenv", key, val)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("launchctl setenv %s: %w (%s)", key, err, string(out))
		}
	}
	// Also write the file atomically for shell sourcing (rendered by caller; we ensure include below)
	return nil
}

func (e *DarwinEnvApplier) UnsetEnvironment(names []string) error {
	for _, n := range names {
		cmd := exec.Command("launchctl", "unsetenv", n)
		if out, err := cmd.CombinedOutput(); err != nil {
			_ = out
			continue
		}
	}
	return nil
}

func (e *DarwinEnvApplier) EnsureShellInclude() error {
	envFile := e.FilePath()
	// One-line include to source the env file. Use POSIX test + set -a for auto-export.
	includeLine := fmt.Sprintf(`[ -f "$HOME/.config/omahab/agent-tools.env" ] && set -a && . "$HOME/.config/omahab/agent-tools.env" && set +a # omahab-agent-tools`)
	// Also support XDG variant path
	if envFile != filepath.Join(os.Getenv("HOME"), ".config", "omahab", "agent-tools.env") {
		includeLine = fmt.Sprintf(`[ -f "%s" ] && set -a && . "%s" && set +a # omahab-agent-tools`, envFile, envFile)
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.Getenv("HOME")
		if home == "" {
			return nil
		}
	}
	candidates := []string{
		filepath.Join(home, ".zshrc"),
		filepath.Join(home, ".bashrc"),
		filepath.Join(home, ".bash_profile"),
		filepath.Join(home, ".config", "fish", "config.fish"),
	}
	for _, rc := range candidates {
		if _, err := os.Stat(rc); err != nil {
			if os.IsNotExist(err) {
				// Create fish config dir if needed, but don't create empty zshrc/bashrc automatically? We will create if parent exists.
				if strings.Contains(rc, "fish") {
					_ = os.MkdirAll(filepath.Dir(rc), 0755)
				} else {
					continue
				}
			} else {
				continue
			}
		}
		data, err := os.ReadFile(rc)
		if err != nil {
			continue
		}
		content := string(data)
		if strings.Contains(content, "omahab-agent-tools") {
			continue
		}
		// Append include line
		f, err := os.OpenFile(rc, os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			continue
		}
		if !strings.HasSuffix(content, "\n") && len(content) > 0 {
			_, _ = f.WriteString("\n")
		}
		if strings.Contains(rc, "fish") {
			fishLine := fmt.Sprintf("test -f %s; and set -a; and source %s; and set +a # omahab-agent-tools\n", envFile, envFile)
			if strings.Contains(content, "omahab-agent-tools") {
				_ = f.Close()
				continue
			}
			_, _ = f.WriteString(fishLine)
		} else {
			_, _ = f.WriteString(includeLine + "\n")
		}
		_ = f.Close()
	}
	return nil
}

// --- Scheduler darwin ---

type DarwinScheduler struct{}

func NewScheduler() Scheduler { return &DarwinScheduler{} }

func (s *DarwinScheduler) Install(paths []string) error {
	if len(paths) == 0 {
		return fmt.Errorf("paths required")
	}
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
	cfgPath := backupPathsFileDarwin()
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(cfgPath), err)
	}
	content := strings.Join(cleaned, "\n") + "\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0600); err != nil {
		return fmt.Errorf("write %s: %w", cfgPath, err)
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.Getenv("HOME")
		if home == "" {
			return fmt.Errorf("no home dir")
		}
	}
	dir := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	bin := "omahab"
	if exe, err := os.Executable(); err == nil && strings.Contains(exe, "omahab") {
		bin = exe
	} else if p, err := exec.LookPath("omahab"); err == nil {
		bin = p
	}
	plistPath := filepath.Join(dir, "com.omahab.backup.plist")
	plistContent := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>com.omahab.backup</string>
  <key>ProgramArguments</key><array><string>%s</string><string>backup-drive</string><string>run</string></array>
  <key>StartCalendarInterval</key><dict><key>Hour</key><integer>2</integer><key>Minute</key><integer>0</integer></dict>
  <key>StandardOutPath</key><string>%s/Library/Logs/omahab-backup.log</string>
  <key>StandardErrorPath</key><string>%s/Library/Logs/omahab-backup.log</string>
</dict>
</plist>
`, bin, home, home)
	if err := os.WriteFile(plistPath, []byte(plistContent), 0644); err != nil {
		return fmt.Errorf("write plist: %w", err)
	}
	_ = exec.Command("launchctl", "load", "-w", plistPath).Run()
	_ = exec.Command("launchctl", "bootstrap", "gui/"+fmt.Sprint(os.Getuid()), plistPath).Run()
	return nil
}

func (s *DarwinScheduler) Uninstall() error {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.Getenv("HOME")
		if home == "" {
			return nil
		}
	}
	plistPath := filepath.Join(home, "Library", "LaunchAgents", "com.omahab.backup.plist")
	_ = exec.Command("launchctl", "unload", "-w", plistPath).Run()
	_ = exec.Command("launchctl", "bootout", "gui/"+fmt.Sprint(os.Getuid()), plistPath).Run()
	_ = os.Remove(plistPath)
	return nil
}

func (s *DarwinScheduler) IsInstalled() bool {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.Getenv("HOME")
		if home == "" {
			return false
		}
	}
	plistPath := filepath.Join(home, "Library", "LaunchAgents", "com.omahab.backup.plist")
	_, err = os.Stat(plistPath)
	return err == nil
}

func backupPathsFileDarwin() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); strings.TrimSpace(dir) != "" {
		return filepath.Join(strings.TrimSpace(dir), "omahab", "backup-paths")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".config", "omahab", "backup-paths")
	}
	return filepath.Join(os.TempDir(), "omahab-backup-paths")
}

// --- Terminal darwin ---

type DarwinTerminal struct{}

func NewTerminal() Terminal { return &DarwinTerminal{} }

func (t *DarwinTerminal) OpenURL(url string) error {
	cmd := exec.Command("open", url)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("open url: %w", err)
	}
	return nil
}

func (t *DarwinTerminal) OpenTerminal(dir string) error {
	// Try Ghostty first, then Terminal.app
	if _, err := exec.LookPath("open"); err == nil {
		for _, app := range []string{"Ghostty", "Terminal"} {
			cmd := exec.Command("open", "-a", app, dir)
			if err := cmd.Start(); err == nil {
				return nil
			}
		}
		cmd := exec.Command("open", dir)
		if err := cmd.Start(); err == nil {
			return nil
		}
	}
	return t.OpenURL("file://" + dir)
}

func (t *DarwinTerminal) OpenTerminalCommand(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("no command provided")
	}
	// Try Ghostty with --args, then Terminal
	if _, err := exec.LookPath("open"); err == nil {
		// Ghostty supports opening with command via --args -e ?
		for _, app := range []string{"Ghostty", "Terminal"} {
			oa := []string{"-a", app}
			// For Terminal.app, use --args to pass command? Terminal doesn't support direct command; fallback to AppleScript.
			// Try open -a Ghostty --args <cmd>
			cmdArgs := append(oa, "--args")
			cmdArgs = append(cmdArgs, args...)
			cmd := exec.Command("open", cmdArgs...)
			if err := cmd.Start(); err == nil {
				return nil
			}
		}
		// Fallback: use osascript to tell Terminal to do script
		script := fmt.Sprintf(`tell application "Terminal" to do script "%s"`, escapeAppleScript(strings.Join(args, " ")))
		cmd := exec.Command("osascript", "-e", script)
		if err := cmd.Start(); err == nil {
			return nil
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

func escapeAppleScript(s string) string {
	out := ""
	for _, r := range s {
		if r == '"' {
			out += `\"`
		} else if r == '\\' {
			out += `\\`
		} else {
			out += string(r)
		}
	}
	return out
}

// --- Notifier darwin ---

type DarwinNotifier struct{}

func NewNotifier() Notifier { return &DarwinNotifier{} }

func (n *DarwinNotifier) Notify(title, body string) error {
	if _, err := exec.LookPath("terminal-notifier"); err == nil {
		cmd := exec.Command("terminal-notifier", "-title", title, "-message", body, "-group", "omahab")
		if err := cmd.Run(); err == nil {
			return nil
		}
	}
	script := `display notification "` + escapeAppleScript(body) + `" with title "` + escapeAppleScript(title) + `"`
	cmd := exec.Command("osascript", "-e", script)
	if err := cmd.Run(); err != nil {
		return nil
	}
	return nil
}

// --- SecretStore darwin ---

type DarwinSecretStore struct{}

func NewSecretStore() SecretStore { return &DarwinSecretStore{} }

func (s *DarwinSecretStore) Get(service, account string) (string, error) {
	v, err := keyring.Get(service, account)
	if err != nil {
		if isNotFoundDarwin(err) {
			return "", fmt.Errorf("credential not found")
		}
		if isKeychainUnavailable(err) {
			return "", fmt.Errorf("keyring unavailable (Keychain not found: %v) — is Keychain Access available? Ensure the login keychain is unlocked in this macOS session", err)
		}
		return "", fmt.Errorf("keyring get %s/%s: %w", service, account, err)
	}
	if strings.TrimSpace(v) == "" {
		return "", fmt.Errorf("credential not found")
	}
	return v, nil
}

func (s *DarwinSecretStore) Set(service, account, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("keyring set %s/%s: value is required", service, account)
	}
	err := keyring.Set(service, account, value)
	if err != nil {
		if isKeychainUnavailable(err) {
			return fmt.Errorf("keyring unavailable (Keychain not found: %v) — is Keychain Access available? Ensure the login keychain is unlocked in this macOS session", err)
		}
		return fmt.Errorf("keyring set %s/%s: %w", service, account, err)
	}
	return nil
}

func (s *DarwinSecretStore) Delete(service, account string) error {
	err := keyring.Delete(service, account)
	if err != nil {
		if isNotFoundDarwin(err) {
			return nil
		}
		if isKeychainUnavailable(err) {
			return fmt.Errorf("keyring unavailable (Keychain not found: %v) — is Keychain Access available? Ensure the login keychain is unlocked in this macOS session", err)
		}
		return fmt.Errorf("keyring delete %s/%s: %w", service, account, err)
	}
	return nil
}

func isNotFoundDarwin(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") || strings.Contains(msg, "no such") || err == keyring.ErrNotFound
}

func isKeychainUnavailable(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "keychain") || strings.Contains(msg, "secitem") || strings.Contains(msg, "keyring") && strings.Contains(msg, "not available")
}
