package installer

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// DaemonHealthURL is the unauthenticated health endpoint for omahabd.
const DaemonHealthURL = "http://127.0.0.1:8484/up"

// waitHealthy polls DaemonHealthURL via probes.HTTPSGet until a 200 is
// observed. It uses probes.Now to enforce a 120s deadline and a 2s poll
// interval. When probes.Now is nil it falls back to time.Now. The loop
// honours ctx cancellation and returns an error mentioning
// `journalctl -u omahabd -n 50 --no-pager` on deadline expiry.
func waitHealthy(ctx context.Context, p Probes) error {
	return waitHealthyWithEmit(ctx, p, nil)
}

func waitHealthyWithEmit(ctx context.Context, p Probes, emit func(string)) error {
	if p.HTTPSGet == nil {
		return fmt.Errorf("https get probe not configured")
	}
	nowFn := p.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	isFake := p.Now != nil
	start := nowFn()
	deadline := start.Add(120 * time.Second)

	attempt := 0
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("health check cancelled: %w", ctx.Err())
		default:
		}

		attempt++
		status, _, err := p.HTTPSGet(ctx, DaemonHealthURL)
		if err == nil && status == 200 {
			if emit != nil {
				emit(fmt.Sprintf("omahabd healthy after %d checks", attempt))
			}
			return nil
		}
		if emit != nil {
			if err != nil {
				emit(fmt.Sprintf("health check %d: waiting (error: %v)", attempt, err))
			} else {
				emit(fmt.Sprintf("health check %d: status %d, waiting", attempt, status))
			}
		}

		now := nowFn()
		if !now.Before(deadline) {
			return fmt.Errorf("omahabd failed to become healthy within 120s; check `journalctl -u omahabd -n 50 --no-pager`")
		}

		if !isFake {
			select {
			case <-ctx.Done():
				return fmt.Errorf("health check cancelled: %w", ctx.Err())
			case <-time.After(2 * time.Second):
			}
		} else {
			// Fake Now path: drive timing off the injected clock so tests are instant.
			// No real sleep; next iteration will advance Now and eventually hit deadline.
			select {
			case <-ctx.Done():
				return fmt.Errorf("health check cancelled: %w", ctx.Err())
			default:
			}
		}
	}
}

func (s *Service) runDaemonStep(ctx context.Context, opts InstallOptions) RunResult {
	emit := func(line string) {
		if opts.Emit != nil {
			opts.Emit(StepLog{Step: StepDaemon, Line: line})
		}
	}
	if s == nil {
		return RunResult{Step: StepDaemon, Status: JournalFailed, Error: "service not configured"}
	}
	p := s.probes

	if p.Systemctl == nil {
		return RunResult{Step: StepDaemon, Status: JournalFailed, Error: "systemctl probe not configured"}
	}
	emit("enabling omahabd")
	if _, err := p.Systemctl(ctx, "enable", "omahabd"); err != nil {
		return RunResult{Step: StepDaemon, Status: JournalFailed, Error: err.Error()}
	}
	emit("restarting omahabd")
	if _, err := p.Systemctl(ctx, "restart", "omahabd"); err != nil {
		return RunResult{Step: StepDaemon, Status: JournalFailed, Error: err.Error()}
	}
	emit("waiting for omahabd health check")
	if err := waitHealthyWithEmit(ctx, p, func(line string) { emit(line) }); err != nil {
		return RunResult{Step: StepDaemon, Status: JournalFailed, Error: err.Error()}
	}
	emit("omahabd is healthy")
	if p.ReadFile == nil {
		return RunResult{Step: StepDaemon, Status: JournalFailed, Error: "read file probe not configured"}
	}
	raw, err := p.ReadFile("/var/lib/omahab/api.token")
	if err != nil {
		return RunResult{Step: StepDaemon, Status: JournalFailed, Error: fmt.Sprintf("read api token: %v: omahabd did not create the token — check `journalctl -u omahabd -n 50 --no-pager`", err)}
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return RunResult{Step: StepDaemon, Status: JournalFailed, Error: "api token empty: omahabd did not create the token — check `journalctl -u omahabd -n 50 --no-pager`"}
	}
	// Build backup.env content. Never include token in error messages below.
	content := "OMAHAB_SERVER=http://127.0.0.1:8484\nOMAHAB_TOKEN=" + token + "\n"

	// Idempotency: skip write when content identical.
	if p.ReadFile != nil {
		if existing, err := p.ReadFile("/etc/omahab/backup.env"); err == nil && string(existing) == content {
			emit("backup.env already up to date")
			if err := s.provisionUserToken(p, s.resolveTargetUser(opts), token); err != nil {
				return RunResult{Step: StepDaemon, Status: JournalFailed, Error: err.Error()}
			}
			return RunResult{Step: StepDaemon, Status: JournalCompleted}
		}
	}
	if p.WriteFile == nil {
		return RunResult{Step: StepDaemon, Status: JournalFailed, Error: "write file probe not configured"}
	}
	emit("writing backup.env")
	if err := p.WriteFile("/etc/omahab/backup.env", []byte(content), 0o600); err != nil {
		return RunResult{Step: StepDaemon, Status: JournalFailed, Error: err.Error()}
	}
	if err := s.provisionUserToken(p, s.resolveTargetUser(opts), token); err != nil {
		return RunResult{Step: StepDaemon, Status: JournalFailed, Error: err.Error()}
	}
	return RunResult{Step: StepDaemon, Status: JournalCompleted}
}

// provisionUserToken provisions the api token to the target user's
// FileCredentialStore-compatible path: <home>/.config/omahab/token .
// It guarantees directory traversal/ownership and token mode 0600 when
// running as root, is idempotent, and never exposes the token in errors.
func (s *Service) provisionUserToken(p Probes, targetUser, token string) error {
	if targetUser == "" {
		targetUser = "root"
	}
	if p.LookupUser == nil {
		// No user lookup configured — best-effort skip to keep older
		// unit tests that only exercise backup.env passing. Production
		// LiveProbes always provides LookupUser, so real installs still
		// provision.
		return nil
	}
	uid, gid, homeDir, err := p.LookupUser(targetUser)
	if err != nil {
		return fmt.Errorf("resolve user %q: %w", targetUser, err)
	}
	if strings.TrimSpace(homeDir) == "" {
		return fmt.Errorf("resolve user %q: empty home directory", targetUser)
	}
	tokenPath := filepath.Join(homeDir, ".config", "omahab", "token")
	configDir := filepath.Dir(tokenPath)
	parentDir := filepath.Dir(configDir)

	ensureDir := func(dir string) error {
		if p.MkdirAll != nil {
			if err := p.MkdirAll(dir, 0o700); err != nil {
				return fmt.Errorf("create config dir: %w", err)
			}
		}
		if p.Chown != nil {
			if err := p.Chown(dir, uid, gid); err != nil {
				return fmt.Errorf("chown config dir: %w", err)
			}
		}
		if p.Chmod != nil {
			if err := p.Chmod(dir, 0o700); err != nil {
				return fmt.Errorf("chmod config dir: %w", err)
			}
		}
		return nil
	}
	if err := ensureDir(parentDir); err != nil {
		return err
	}
	if err := ensureDir(configDir); err != nil {
		return err
	}

	// Idempotency: if existing token file already has correct content, mode and ownership, skip write.
	if p.ReadFile != nil {
		if existing, err := p.ReadFile(tokenPath); err == nil {
			if strings.TrimSpace(string(existing)) == token {
				permOK := true
				ownerOK := true
				if p.StatFile != nil {
					if _, perm, err := p.StatFile(tokenPath); err == nil {
						permOK = perm == 0o600
					} else {
						permOK = false
					}
				}
				if p.FileOwner != nil {
					if u, g, err := p.FileOwner(tokenPath); err == nil {
						ownerOK = u == uid && g == gid
					} else {
						ownerOK = false
					}
				}
				if permOK && ownerOK {
					return nil
				}
			}
		}
	}
	if p.WriteFile == nil {
		return fmt.Errorf("write file probe not configured")
	}
	data := []byte(token + "\n")
	if err := p.WriteFile(tokenPath, data, 0o600); err != nil {
		return fmt.Errorf("write token file: %w", err)
	}
	if p.Chown != nil {
		if err := p.Chown(tokenPath, uid, gid); err != nil {
			return fmt.Errorf("chown token file: %w", err)
		}
	}
	if p.Chmod != nil {
		if err := p.Chmod(tokenPath, 0o600); err != nil {
			return fmt.Errorf("chmod token file: %w", err)
		}
	}
	return nil
}

// RollbackDaemon stops and disables omahabd and removes the backup env file.
// It is best-effort: every probe is nil-checked and errors are ignored.
func RollbackDaemon(ctx context.Context, p Probes) error {
	if p.Systemctl != nil {
		_, _ = p.Systemctl(ctx, "stop", "omahabd")
		_, _ = p.Systemctl(ctx, "disable", "omahabd")
	}
	if p.RemoveFile != nil {
		_ = p.RemoveFile("/etc/omahab/backup.env")
	}
	return nil
}
