package platform

// EnvApplier applies environment variables to the user session.
// Linux: writes environment.d/90-omahab-agent-tools.conf and D-Bus SetEnvironment.
// Darwin: writes ~/.config/omahab/agent-tools.env, launchctl setenv per var,
// and ensures shell rc sources the env file.
type EnvApplier interface {
	SetEnvironment(assignments []string) error
	UnsetEnvironment(names []string) error
	FilePath() string
	EnsureShellInclude() error
}

// Scheduler manages the periodic backup job.
// Linux: systemd user timer at ~/.config/systemd/user/omahab-machine-backup.*
// Darwin: launchd plist at ~/Library/LaunchAgents/com.omahab.backup.plist
type Scheduler interface {
	Install(paths []string) error
	Uninstall() error
	IsInstalled() bool
}

// Terminal launches URLs and terminals.
// Linux: xdg-terminal-exec, alacritty, kitty, ghostty, gnome-terminal.
// Darwin: open -a Ghostty / open -a Terminal.
type Terminal interface {
	OpenURL(url string) error
	OpenTerminal(dir string) error
	OpenTerminalCommand(args []string) error
}

// Notifier posts desktop notifications.
// Linux: notify-send (mako). Darwin: terminal-notifier or osascript.
type Notifier interface {
	Notify(title, body string) error
}

// SecretStore persists device credentials.
// Both platforms use github.com/zalando/go-keyring; error copy differs per OS.
type SecretStore interface {
	Get(service, account string) (string, error)
	Set(service, account, value string) error
	Delete(service, account string) error
}

// DefaultEnvFile is the fallback env file name for darwin when XDG not set.
// Exported for tests.
const DefaultEnvFile = "90-omahab-agent-tools.conf"
