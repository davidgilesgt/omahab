package logging

import (
	"log/slog"
	"os"
	"path/filepath"

	"github.com/basecamp/once/internal/fsutil"
)

// ToLogFile switches logging to a file for the duration of fn, then restores
// stderr logging when fn returns.
func ToLogFile(fn func() error) error {
	dir, err := stateDir()
	if err != nil {
		slog.Warn("Could not determine log directory", "error", err)
		return fn()
	}

	path := filepath.Join(dir, "once.log")
	file, err := fsutil.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		slog.Warn("Could not open log file", "error", err)
		return fn()
	}

	defer func() {
		SetupStderr()
		file.Close()
	}()

	slog.SetDefault(slog.New(slog.NewTextHandler(file, nil)))

	return fn()
}

func SetupStderr() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
}

// Helpers

func stateDir() (string, error) {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "once"), nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	return filepath.Join(home, ".local", "state", "once"), nil
}
