package backups

import (
	"context"
	"fmt"
	"time"

	"github.com/omahab/omahab/internal/domain"
)

// Status computes the current backup health report without persisting it.
//
// Health rules, in order:
//
//  1. No completed backup, or the last completed backup is older than the
//     recovery point objective (24h by default): unhealthy.
//  2. No demonstrated verified restore, or the last one is older than
//     VerifyInterval: degraded. A backup is never healthy without a
//     successful verified restore.
//  3. Otherwise: healthy.
func (s *Service) Status(ctx context.Context) (StatusReport, error) {
	return s.evaluateHealth(ctx, false)
}

// EvaluateHealth computes the report, persists it as the current health
// state, and emits a normalized backup.health_changed event on
// transitions. The daemon's health loop calls this at least daily so the
// recovery point objective is evaluated continuously.
func (s *Service) EvaluateHealth(ctx context.Context) (StatusReport, error) {
	return s.evaluateHealth(ctx, true)
}

func (s *Service) evaluateHealth(ctx context.Context, persist bool) (StatusReport, error) {
	now := s.nowUTC()
	rep := StatusReport{
		Health:         domain.HealthUnhealthy,
		RPOLimit:       s.cfg.RPO,
		RTOLimit:       s.cfg.RTO,
		VerifyInterval: s.cfg.VerifyInterval,
		Repositories:   []Repository{},
	}

	repos, err := s.listRepositories(ctx)
	if err != nil {
		return rep, err
	}
	rep.Repositories = repos

	if rep.ActiveRun, err = s.activeRun(ctx); err != nil {
		return rep, err
	}
	if rep.LastBackupAt, err = s.lastCompletedBackupAt(ctx); err != nil {
		return rep, err
	}
	if rep.LastVerifiedAt, err = s.lastVerifiedRestoreAt(ctx); err != nil {
		return rep, err
	}

	switch {
	case rep.LastBackupAt == nil:
		rep.RPOExceeded = true
		rep.Reason = "no completed backup"
	case now.Sub(*rep.LastBackupAt) > s.cfg.RPO:
		rep.RPOExceeded = true
		rep.Reason = fmt.Sprintf("last backup is older than the %s recovery point objective", s.cfg.RPO)
	default:
		switch {
		case rep.LastVerifiedAt == nil:
			rep.Reason = "no successful verified restore"
		case now.Sub(*rep.LastVerifiedAt) > s.cfg.VerifyInterval:
			rep.VerificationOverdue = true
			rep.Reason = fmt.Sprintf("last verified restore is older than the %s verification interval", s.cfg.VerifyInterval)
		default:
			rep.Health = domain.HealthHealthy
			rep.Reason = "recent backup with a verified restore"
		}
		if rep.Health != domain.HealthHealthy {
			rep.Health = domain.HealthDegraded
		}
	}

	if !persist {
		return rep, nil
	}

	previous, existed, err := s.getHealthState(ctx)
	if err != nil {
		return rep, err
	}
	if err := s.saveHealthState(ctx, rep.Health, rep); err != nil {
		return rep, err
	}
	changed := !existed || previous != rep.Health
	// Never announce the initial transition to healthy; it is the expected
	// steady state, not an incident.
	if changed && !(previous == domain.HealthUnknown && rep.Health == domain.HealthHealthy) {
		s.emit(ctx, EventBackupHealthChanged, severityForHealth(rep.Health), "",
			fmt.Sprintf("backup health changed from %s to %s: %s", previous, rep.Health, rep.Reason),
			map[string]any{
				"from":                 string(previous),
				"to":                   string(rep.Health),
				"reason":               rep.Reason,
				"last_backup_at":       formatOptionalTime(rep.LastBackupAt),
				"last_verified_at":     formatOptionalTime(rep.LastVerifiedAt),
				"rpo_exceeded":         rep.RPOExceeded,
				"verification_overdue": rep.VerificationOverdue,
			})
	}
	return rep, nil
}

func formatOptionalTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return fmtTime(*t)
}
