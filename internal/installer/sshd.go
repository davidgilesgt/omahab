package installer

import (
	"context"
	"fmt"
	"time"
)

const (
	sshdDropInPath = "/etc/ssh/sshd_config.d/10-omahab.conf"
	sshdBackupPath = "/var/lib/omahab/sshd-10-omahab.conf.bak"
	rollbackAfter  = 10 * time.Minute
)

// SSHDHardeningConfig is the desired final sshd drop-in content.
type SSHDHardeningConfig struct {
	PubkeyAuth      bool
	PasswordAuth    bool
	KbdInteractive  bool
	PermitRootLogin string // "no" or "prohibit-password"
}

// DefaultHardenedConfig is the final policy per DESIGN.md 5.5.
var DefaultHardenedConfig = SSHDHardeningConfig{
	PubkeyAuth:      true,
	PasswordAuth:    false,
	KbdInteractive:  false,
	PermitRootLogin: "no",
}

func (c SSHDHardeningConfig) Render() string {
	lines := "# Managed by omahab-install — do not edit manually\n"
	if c.PubkeyAuth {
		lines += "PubkeyAuthentication yes\n"
	} else {
		lines += "PubkeyAuthentication no\n"
	}
	if c.PasswordAuth {
		lines += "PasswordAuthentication yes\n"
	} else {
		lines += "PasswordAuthentication no\n"
	}
	if c.KbdInteractive {
		lines += "KbdInteractiveAuthentication yes\n"
	} else {
		lines += "KbdInteractiveAuthentication no\n"
	}
	lines += fmt.Sprintf("PermitRootLogin %s\n", c.PermitRootLogin)
	return lines
}

// PrepareSSHDHardening writes the drop-in, validates, schedules rollback, and reloads.
// It does NOT yet disable password auth — that happens after second-session confirmation.
func PrepareSSHDHardening(ctx context.Context, probes Probes, cfg SSHDHardeningConfig) error {
	if probes.ActiveSSHSession != nil {
		ok, _, err := probes.ActiveSSHSession()
		if err != nil {
			return fmt.Errorf("check active session: %w", err)
		}
		if !ok {
			return fmt.Errorf("%w: no active SSH session — refusing to harden SSH without a live connection", ErrSSHLockout)
		}
	}
	// Backup existing drop-in if present.
	var backupExists bool
	if probes.FileExists != nil {
		backupExists = probes.FileExists(sshdDropInPath)
		_ = backupExists
	}
	if probes.ReadFile != nil && probes.FileExists != nil && probes.FileExists(sshdDropInPath) {
		data, err := probes.ReadFile(sshdDropInPath)
		if err == nil {
			if probes.WriteFile != nil {
				_ = probes.WriteFile(sshdBackupPath, data, 0o600)
			}
		}
	} else if probes.FileExists != nil && !probes.FileExists(sshdDropInPath) {
		// No existing file — create empty backup marker so rollback knows to remove.
		if probes.WriteFile != nil {
			_ = probes.WriteFile(sshdBackupPath+".empty", []byte("empty"), 0o600)
		}
	}

	content := cfg.Render()
	if probes.WriteFile == nil {
		return fmt.Errorf("write file probe not configured")
	}
	if err := probes.WriteFile(sshdDropInPath, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write sshd drop-in: %w", err)
	}
	if probes.SSHDConfigTest != nil {
		if err := probes.SSHDConfigTest(ctx); err != nil {
			// Rollback immediately on validation failure
			_ = rollbackSSHDFile(probes)
			return fmt.Errorf("sshd config validation failed: %w", err)
		}
	}
	if probes.ScheduleRollback != nil {
		if err := probes.ScheduleRollback(ctx, rollbackAfter, sshdBackupPath); err != nil {
			_ = rollbackSSHDFile(probes)
			return fmt.Errorf("schedule rollback timer: %w", err)
		}
	}
	if probes.SSHDReload != nil {
		if err := probes.SSHDReload(ctx); err != nil {
			if probes.CancelRollback != nil {
				_ = probes.CancelRollback(ctx)
			}
			_ = rollbackSSHDFile(probes)
			return fmt.Errorf("sshd reload failed: %w", err)
		}
	}
	return nil
}

// ConfirmSecondSession checks for a second SSH session and, if present, cancels the rollback timer.
func ConfirmSecondSession(ctx context.Context, probes Probes) error {
	if probes.SecondSessionProbe == nil {
		return fmt.Errorf("second session probe not configured")
	}
	ok, err := probes.SecondSessionProbe(ctx)
	if err != nil {
		return fmt.Errorf("check second session: %w", err)
	}
	if !ok {
		return ErrNotConfirmed
	}
	if probes.CancelRollback != nil {
		if err := probes.CancelRollback(ctx); err != nil {
			return fmt.Errorf("cancel rollback timer: %w", err)
		}
	}
	// Remove backup marker
	if probes.RemoveFile != nil {
		_ = probes.RemoveFile(sshdBackupPath)
		_ = probes.RemoveFile(sshdBackupPath + ".empty")
	}
	return nil
}

// RollbackSSHD restores the previous sshd drop-in and reloads.
func RollbackSSHD(ctx context.Context, probes Probes) error {
	if probes.CancelRollback != nil {
		_ = probes.CancelRollback(ctx)
	}
	if err := rollbackSSHDFile(probes); err != nil {
		return err
	}
	if probes.SSHDConfigTest != nil {
		if err := probes.SSHDConfigTest(ctx); err != nil {
			return fmt.Errorf("rollback validation failed: %w", err)
		}
	}
	if probes.SSHDReload != nil {
		if err := probes.SSHDReload(ctx); err != nil {
			return fmt.Errorf("rollback reload failed: %w", err)
		}
	}
	return nil
}

func rollbackSSHDFile(probes Probes) error {
	if probes.ReadFile != nil && probes.WriteFile != nil {
		// If backup exists, restore it.
		if probes.FileExists != nil && probes.FileExists(sshdBackupPath) {
			data, err := probes.ReadFile(sshdBackupPath)
			if err != nil {
				return fmt.Errorf("read backup: %w", err)
			}
			if err := probes.WriteFile(sshdDropInPath, data, 0o644); err != nil {
				return fmt.Errorf("restore sshd drop-in: %w", err)
			}
			return nil
		}
		// Empty marker means remove the drop-in.
		if probes.FileExists != nil && probes.FileExists(sshdBackupPath+".empty") {
			if probes.RemoveFile != nil {
				_ = probes.RemoveFile(sshdDropInPath)
			}
			return nil
		}
	}
	// Best-effort remove if no backup logic.
	if probes.RemoveFile != nil {
		_ = probes.RemoveFile(sshdDropInPath)
	}
	return nil
}

// IsRollbackActive reports whether the rollback timer is still armed.
func IsRollbackActive(ctx context.Context, probes Probes) (bool, error) {
	if probes.RollbackActive == nil {
		return false, nil
	}
	return probes.RollbackActive(ctx)
}
