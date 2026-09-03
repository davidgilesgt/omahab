//go:build darwin

package client

import (
	"fmt"
	"os/exec"
)

// realDBus on macOS uses launchctl to set/unset environment for the user session.
// It satisfies the same SystemdDBus interface so the rest of EnvironmentManager is portable.
// Variables are also written to ~/.config/omahab/agent-tools.env and sourced via shell rc,
// but launchctl is the live-apply path.
type realDBus struct{}

func (r *realDBus) SetEnvironment(assignments []string) error {
	for _, a := range assignments {
		// assignments are "KEY=VALUE" already escaped via renderEnvironmentFile.
		// launchctl wants `setenv KEY VALUE` (value without extra quoting — we pass as single arg).
		// Split on first '=' to get key/value.
		kv := a
		idx := -1
		for i, c := range kv {
			if c == '=' {
				idx = i
				break
			}
		}
		if idx <= 0 {
			continue
		}
		key := kv[:idx]
		val := kv[idx+1:]
		// Unquote value if it was quoted for systemd (handle single-quoted values).
		if len(val) >= 2 && val[0] == '\'' && val[len(val)-1] == '\'' {
			// Simple single-quote unescape: '' -> '
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
	return nil
}

func (r *realDBus) UnsetEnvironment(names []string) error {
	for _, n := range names {
		cmd := exec.Command("launchctl", "unsetenv", n)
		if out, err := cmd.CombinedOutput(); err != nil {
			// unset of missing var is not fatal — log and continue.
			_ = out
			continue
		}
	}
	return nil
}
