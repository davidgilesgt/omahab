package client

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
)

// Launcher launches external apps. It is injectable for tests.
type Launcher interface {
	OpenURL(url string) error
	OpenTerminal(dir string) error
	OpenTerminalCommand(args []string) error
	LaunchHermes(url string) error
}

// ExecLauncher uses OS defaults (xdg-open/open).
type ExecLauncher struct {
	// Override for tests.
	OpenFunc             func(url string) error
	TerminalFunc         func(dir string) error
	TerminalCommandFunc  func(args []string) error
	HermesFunc           func(url string) error
}

func (e *ExecLauncher) OpenURL(url string) error {
	if e.OpenFunc != nil {
		return e.OpenFunc(url)
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("open url: %w", err)
	}
	return nil
}
func (e *ExecLauncher) OpenTerminal(dir string) error {
	if e.TerminalFunc != nil {
		return e.TerminalFunc(dir)
	}
	// Try common terminals; fallback to xdg-open on dir.
	terms := [][]string{
		{"xdg-terminal-exec", dir},
		{"alacritty", "--working-directory", dir},
		{"kitty", "--directory", dir},
		{"gnome-terminal", "--working-directory=" + dir},
	}
	for _, t := range terms {
		if _, err := exec.LookPath(t[0]); err == nil {
			cmd := exec.Command(t[0], t[1:]...)
			cmd.Dir = dir
			if err := cmd.Start(); err == nil {
				return nil
			}
		}
	}
	// Fallback: open directory
	return e.OpenURL("file://" + dir)
}

func (e *ExecLauncher) OpenTerminalCommand(args []string) error {
	if e.TerminalCommandFunc != nil {
		return e.TerminalCommandFunc(args)
	}
	if len(args) == 0 {
		return fmt.Errorf("no command provided")
	}
	// Try terminals with -e / -- support: alacritty -e, kitty, gnome-terminal --, xterm -e
	terms := [][]string{
		// alacritty -e <cmd...>
		append([]string{"alacritty", "-e"}, args...),
		// kitty <cmd...> (kitty runs command directly)
		append([]string{"kitty"}, args...),
		// kitty with -- (alternative)
		append([]string{"kitty", "--"}, args...),
		// gnome-terminal -- <cmd...>
		append([]string{"gnome-terminal", "--"}, args...),
		// xterm -e <cmd...>
		append([]string{"xterm", "-e"}, args...),
		// xdg-terminal-exec <cmd...>
		append([]string{"xdg-terminal-exec"}, args...),
	}
	for _, t := range terms {
		if _, err := exec.LookPath(t[0]); err == nil {
			cmd := exec.Command(t[0], t[1:]...)
			if err := cmd.Start(); err == nil {
				return nil
			}
		}
	}
	// Fallback: try running directly (no terminal) – useful for tests
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err == nil {
		return nil
	}
	return fmt.Errorf("no terminal found to run %v", args)
}

func (e *ExecLauncher) LaunchHermes(url string) error {
	if e.HermesFunc != nil {
		return e.HermesFunc(url)
	}
	// Prefer official Hermes Desktop if installed, else open URL.
	if _, err := exec.LookPath("hermes-desktop"); err == nil {
		cmd := exec.Command("hermes-desktop", "--url", url)
		if err := cmd.Start(); err == nil {
			return nil
		}
	}
	// Fallback to browser with URL ready to paste.
	return e.OpenURL(url)
}
// NopLauncher does nothing (for tests/headless).
type NopLauncher struct{}

func (n *NopLauncher) OpenURL(url string) error                    { return nil }
func (n *NopLauncher) OpenTerminal(dir string) error               { return nil }
func (n *NopLauncher) OpenTerminalCommand(args []string) error     { return nil }
func (n *NopLauncher) LaunchHermes(url string) error               { return nil }

var _ Launcher = (*ExecLauncher)(nil)
var _ Launcher = (*NopLauncher)(nil)
