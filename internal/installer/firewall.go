package installer

import (
	"bytes"
	"context"
	"fmt"
)

const (
	nftConfPath   = "/etc/nftables.conf"
	nftBackupPath = "/etc/nftables.conf.pre-omahab"
	nftTempPath   = "/etc/nftables.conf.omahab-new"
)

// NftablesConf returns the managed nftables configuration.
// It is the template seam for tests and for the firewall step.
// Uses `destroy table inet omahab` (nft >= 1.0.8: delete-if-exists) so
// `nft -c -f` succeeds on first install when the table does not yet exist.
// This only destroys our own table (safe re-apply); never `destroy ruleset`
// or `flush ruleset` — that would wipe Docker's rules.
func NftablesConf() string {
	return `#!/usr/sbin/nft -f
# Managed by Omahab installer. Default-deny inbound; outbound unaffected.
destroy table inet omahab
table inet omahab {
  chain input {
    type filter hook input priority 10; policy drop;
    iifname "lo" accept
    ct state invalid drop
    ct state established,related accept
    tcp dport 22 accept comment "ssh"
    udp dport 41641 accept comment "tailscale direct"
    iifname "tailscale0" tcp dport 8484 accept comment "omahab dashboard via tailscale"
    iifname "br-*" ip saddr 172.30.0.2 tcp dport 8484 accept comment "caddy dashboard upstream"
    iifname "tailscale0" tcp dport { 80, 443 } accept comment "caddy https via tailscale"
    icmp type { destination-unreachable, time-exceeded, parameter-problem, echo-request } limit rate 10/second accept
    ip6 nexthdr ipv6-icmp icmpv6 type { destination-unreachable, time-exceeded, parameter-problem, echo-request, nd-router-advert, nd-neighbor-solicit, nd-neighbor-advert } limit rate 20/second accept
  }
}
`
}

// runFirewallStep writes, validates, and applies the default-deny inbound
// nftables ruleset.
//
// Order:
//  1. If /etc/nftables.conf exists and differs from NftablesConf(), copy it to
//     /etc/nftables.conf.pre-omahab via ReadFile+WriteFile (0644) only if the
//     backup does not already exist (never overwrite a backup).
//  2. Write /etc/nftables.conf (0644) — skip when identical (idempotent).
//  3. Validate via nft -c -f before installing. When content changed we write
//     to a temp path (/etc/nftables.conf.omahab-new), validate that temp path,
//     then write the final path (rename semantics via WriteFile). When content
//     is identical we still validate the existing file and skip re-enable if
//     nftables is already active and enabled.
//  4. Systemctl enable nftables, then start (or restart when conf changed and
//     service already active). The restart choice is documented: we use restart
//     when the conf changed and the service is active, otherwise start.
func (s *Service) runFirewallStep(ctx context.Context, opts InstallOptions) RunResult {
	_ = opts
	failed := func(err error) RunResult {
		return RunResult{Step: StepFirewall, Status: JournalFailed, Error: err.Error()}
	}
	p := s.probes
	desired := NftablesConf()
	desiredBytes := []byte(desired)

	// helper: check file existence nil-safe
	fileExists := func(path string) bool {
		if p.FileExists != nil {
			return p.FileExists(path)
		}
		// Fallback: try ReadFile if available
		if p.ReadFile != nil {
			_, err := p.ReadFile(path)
			return err == nil
		}
		return false
	}

	existingExists := fileExists(nftConfPath)
	var existing []byte
	if existingExists {
		if p.ReadFile != nil {
			data, err := p.ReadFile(nftConfPath)
			if err != nil {
				return failed(fmt.Errorf("read %s: %w", nftConfPath, err))
			}
			existing = data
		} else {
			// Without ReadFile we cannot determine content; treat as non-identical
			// so we will overwrite — safest to assume difference.
			existing = nil
		}
	}

	identical := existingExists && p.ReadFile != nil && bytes.Equal(existing, desiredBytes)

	// Step 1: backup if needed (exists && differs && backup not exists)
	if existingExists && !identical {
		backupExists := fileExists(nftBackupPath)
		if !backupExists {
			if p.ReadFile == nil {
				return failed(fmt.Errorf("ReadFile probe not configured for backup"))
			}
			if p.WriteFile == nil {
				return failed(fmt.Errorf("WriteFile probe not configured for backup"))
			}
			// existing already read; if nil (probe missing earlier) re-read
			data := existing
			if data == nil {
				d, err := p.ReadFile(nftConfPath)
				if err != nil {
					return failed(fmt.Errorf("read %s for backup: %w", nftConfPath, err))
				}
				data = d
			}
			if err := p.WriteFile(nftBackupPath, data, 0o644); err != nil {
				return failed(fmt.Errorf("backup %s: %w", nftBackupPath, err))
			}
		}
	}

	// Steps 2 & 3: validation and write
	if identical {
		// Validate existing file (cheap)
		if p.CommandOutput != nil {
			out, err := p.CommandOutput(ctx, "nft", "-c", "-f", nftConfPath)
			if err != nil {
				msg := fmt.Sprintf("nft validation failed: %v", err)
				if out != "" {
					msg = fmt.Sprintf("nft validation failed: %v: %s", err, out)
				}
				return failed(fmt.Errorf("%s", msg))
			}
		}
		// Skip re-enable if service already active+enabled
		var active, enabled bool
		var activeErr, enabledErr error
		if p.ServiceActive != nil {
			active, activeErr = p.ServiceActive("nftables")
		}
		if p.ServiceEnabled != nil {
			enabled, enabledErr = p.ServiceEnabled("nftables")
		}
		if activeErr == nil && enabledErr == nil && active && enabled {
			return RunResult{Step: StepFirewall, Status: JournalCompleted}
		}
		// otherwise fall through to enable/start
	} else {
		// Content differs or file missing: temp-path validation before final write
		if p.WriteFile == nil {
			return failed(fmt.Errorf("WriteFile probe not configured"))
		}
		if p.CommandOutput == nil {
			return failed(fmt.Errorf("CommandOutput probe not configured"))
		}
		// Write temp file
		if err := p.WriteFile(nftTempPath, desiredBytes, 0o644); err != nil {
			return failed(fmt.Errorf("write %s: %w", nftTempPath, err))
		}
		// Validate temp file
		out, err := p.CommandOutput(ctx, "nft", "-c", "-f", nftTempPath)
		if err != nil {
			// Clean up temp best-effort, leave original conf untouched
			if p.RemoveFile != nil {
				_ = p.RemoveFile(nftTempPath)
			}
			msg := fmt.Sprintf("nft validation failed: %v", err)
			if out != "" {
				msg = fmt.Sprintf("nft validation failed: %v: %s", err, out)
			}
			return failed(fmt.Errorf("%s", msg))
		}
		// Validation succeeded: write final file
		if err := p.WriteFile(nftConfPath, desiredBytes, 0o644); err != nil {
			if p.RemoveFile != nil {
				_ = p.RemoveFile(nftTempPath)
			}
			return failed(fmt.Errorf("write %s: %w", nftConfPath, err))
		}
		// Clean temp file best-effort
		if p.RemoveFile != nil {
			_ = p.RemoveFile(nftTempPath)
		}
	}

	// Step 4: systemctl enable + start/restart
	if p.Systemctl == nil {
		return failed(fmt.Errorf("Systemctl probe not configured"))
	}
	if _, err := p.Systemctl(ctx, "enable", "nftables"); err != nil {
		return failed(fmt.Errorf("systemctl enable nftables: %w", err))
	}
	// Choose restart when conf changed and service already active, otherwise start.
	cmd := "start"
	if !identical {
		if p.ServiceActive != nil {
			if active, err := p.ServiceActive("nftables"); err == nil && active {
				cmd = "restart"
			}
		}
	}
	if _, err := p.Systemctl(ctx, cmd, "nftables"); err != nil {
		return failed(fmt.Errorf("systemctl %s nftables: %w", cmd, err))
	}

	return RunResult{Step: StepFirewall, Status: JournalCompleted}
}

// RollbackFirewall disables nftables and restores the pre-omahab config.
// It is best-effort for systemctl disable+stop. If a backup exists it is
// restored over /etc/nftables.conf and re-applied via nft -f. Otherwise the
// current conf is removed only when its content still equals NftablesConf()
// (never delete an admin-authored conf). All probes are nil-checked.
func RollbackFirewall(ctx context.Context, p Probes) error {
	// Best-effort disable+stop
	if p.Systemctl != nil {
		_, _ = p.Systemctl(ctx, "disable", "nftables")
		_, _ = p.Systemctl(ctx, "stop", "nftables")
	}

	fileExists := func(path string) bool {
		if p.FileExists != nil {
			return p.FileExists(path)
		}
		if p.ReadFile != nil {
			_, err := p.ReadFile(path)
			return err == nil
		}
		return false
	}

	backupExists := fileExists(nftBackupPath)
	if backupExists {
		// Restore backup over conf
		if p.ReadFile != nil && p.WriteFile != nil {
			data, err := p.ReadFile(nftBackupPath)
			if err != nil {
				return fmt.Errorf("read backup %s: %w", nftBackupPath, err)
			}
			if err := p.WriteFile(nftConfPath, data, 0o644); err != nil {
				return fmt.Errorf("restore %s: %w", nftConfPath, err)
			}
			// Re-apply restored config via nft -f (apply, not check)
			if p.CommandOutput != nil {
				_, _ = p.CommandOutput(ctx, "nft", "-f", nftConfPath)
			}
		}
		return nil
	}

	// No backup: remove only if omahab-authored
	if fileExists(nftConfPath) {
		if p.ReadFile != nil {
			data, err := p.ReadFile(nftConfPath)
			if err == nil {
				if string(data) == NftablesConf() {
					if p.RemoveFile != nil {
						_ = p.RemoveFile(nftConfPath)
					}
				}
			}
		}
		// If ReadFile nil we cannot determine content: preserve file (do nothing)
	}
	return nil
}
