//go:build !linux && !darwin

package platform

import (
	"fmt"
	"log/slog"
)

// --- EnvApplier other ---

type OtherEnvApplier struct{}

func NewEnvApplier() EnvApplier { return &OtherEnvApplier{} }
func (e *OtherEnvApplier) FilePath() string                      { return "/tmp/omahab-agent-tools.env" }
func (e *OtherEnvApplier) SetEnvironment([]string) error         { return fmt.Errorf("SetEnvironment not supported on this platform") }
func (e *OtherEnvApplier) UnsetEnvironment([]string) error       { return fmt.Errorf("UnsetEnvironment not supported on this platform") }
func (e *OtherEnvApplier) EnsureShellInclude() error             { return nil }

// --- Scheduler other ---

type OtherScheduler struct{}

func NewScheduler() Scheduler { return &OtherScheduler{} }
func (s *OtherScheduler) Install([]string) error { return fmt.Errorf("scheduler not supported on this platform") }
func (s *OtherScheduler) Uninstall() error       { return nil }
func (s *OtherScheduler) IsInstalled() bool      { return false }

// --- Terminal other ---

type OtherTerminal struct{}

func NewTerminal() Terminal { return &OtherTerminal{} }
func (t *OtherTerminal) OpenURL(url string) error {
	slog.Default().Info("open url stub (other platform)", "url", url)
	return nil
}
func (t *OtherTerminal) OpenTerminal(dir string) error {
	slog.Default().Info("open terminal stub (other platform)", "dir", dir)
	return nil
}
func (t *OtherTerminal) OpenTerminalCommand(args []string) error {
	slog.Default().Info("open terminal command stub (other platform)", "args", args)
	return nil
}

// --- Notifier other ---

type OtherNotifier struct{}

func NewNotifier() Notifier { return &OtherNotifier{} }
func (n *OtherNotifier) Notify(title, body string) error {
	slog.Default().Info("notify stub (other platform)", "title", title, "body", body)
	return nil
}

// --- SecretStore other ---

type OtherSecretStore struct{}

func NewSecretStore() SecretStore { return &OtherSecretStore{} }
func (s *OtherSecretStore) Get(_, _ string) (string, error) { return "", fmt.Errorf("credential not found") }
func (s *OtherSecretStore) Set(_, _, _ string) error        { return fmt.Errorf("keyring not supported on this platform") }
func (s *OtherSecretStore) Delete(_, _ string) error        { return nil }
