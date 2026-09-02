package installer

import "context"

// EnabledUnits returns the systemd units managed by the services step in
// deterministic enable order. It enables the Podman builder socket and its
// weekly prune timer before the control plane, plus tailscaled.
// Deliberately NOT enabled (design; next-steps output explains):
//   - cloudflared (needs /etc/omahab/cloudflared/config.yml from tunnel enrollment)
//   - omahab-backup.timer and omahab-verify.timer (need a configured backup repository)
//   - omahab-clientd (companion-only, binary not installed on server)
func EnabledUnits() []string {
	return []string{"tailscaled", "omahab-builder.socket", "omahab-builder-prune.timer", "omahabd"}
}

func (s *Service) runServicesStep(ctx context.Context, opts InstallOptions) RunResult {
	_ = opts

	if s.probes.Systemctl == nil {
		return RunResult{Step: StepServices, Status: JournalFailed, Error: "systemctl probe not configured"}
	}

	if _, err := s.probes.Systemctl(ctx, "daemon-reload"); err != nil {
		return RunResult{Step: StepServices, Status: JournalFailed, Error: err.Error()}
	}

	for _, unit := range EnabledUnits() {
		if _, err := s.probes.Systemctl(ctx, "enable", unit); err != nil {
			return RunResult{Step: StepServices, Status: JournalFailed, Error: err.Error()}
		}
	}

	return RunResult{Step: StepServices, Status: JournalCompleted}
}
// RollbackServices disables the units enabled by the services step.
// It is best-effort: errors from systemctl are ignored. It deliberately
// does NOT disable tailscaled — it may predate the install and is the
// recovery path — and always attempts a daemon-reload afterwards.
// It disables the builder socket/timer and omahabd but preserves
// /var/lib/omahab-builder (CI cache/state) on rollback.
func RollbackServices(ctx context.Context, p Probes) error {
	if p.Systemctl == nil {
		return nil
	}
	_, _ = p.Systemctl(ctx, "disable", "omahab-builder-prune.timer")
	_, _ = p.Systemctl(ctx, "disable", "omahab-builder.socket")
	_, _ = p.Systemctl(ctx, "disable", "omahabd")
	_, _ = p.Systemctl(ctx, "daemon-reload")
	return nil
}
