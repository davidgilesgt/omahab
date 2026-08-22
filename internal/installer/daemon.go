package installer

import (
	"context"
	"fmt"
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
	return RunResult{Step: StepDaemon, Status: JournalCompleted}
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
