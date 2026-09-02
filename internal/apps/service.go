// Package apps implements the curated platform application lifecycle
// (DESIGN.md §6.1): a validated bundle catalog pinned by digest, desired and
// observed state persisted in SQLite, Docker Compose deployment through an
// explicit Runner interface, current/previous release retention for
// rollback, health observation, and normalized event emission.
//
// State machine (desired -> observed):
//
//	install:   absent        -> provisioning -> running | failed
//	start:     stopped|failed-> running
//	stop:      running|failed-> stopped
//	update:    any installed -> provisioning -> running|stopped (release swapped) | failed (release unchanged)
//	rollback:  any installed -> provisioning -> running|stopped (release swapped) | failed (release unchanged)
//	uninstall: any installed -> absent (rows removed; volumes kept)
//	health:    running       -> health in {healthy, unhealthy, unknown}; observed unchanged
//
// Every transition writes its final state in one transaction after the
// runner succeeds, so the persisted record never claims success the runner
// did not report. Runner diagnostics are redacted against the secret
// environment projection before they are persisted or emitted.
package apps

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/omahab/omahab/internal/domain"
)

// Desired and observed state values persisted per application.
const (
	DesiredRunning = "running"
	DesiredStopped = "stopped"

	ObservedAbsent       = "absent"
	ObservedProvisioning = "provisioning"
	ObservedRunning      = "running"
	ObservedStopped      = "stopped"
	ObservedFailed       = "failed"
)

// EnvSource projects secret environment variables for an application at
// deploy time. It is invoked for every runner operation so updates and
// rollbacks pick up current secret values; values never persist.
type EnvSource func(ctx context.Context, app domain.Application) ([]string, error)

// Options configures a Service. Catalog and Runner are required; the rest
// are optional. Now exists so transitions are deterministically testable.
type Options struct {
	Catalog *Catalog
	Runner  Runner
	Events  EventSink
	Env     EnvSource
	Now     func() time.Time
}

// Service is the platform application lifecycle controller.
type Service struct {
	db      *sql.DB
	catalog *Catalog
	catMu   sync.RWMutex
	runner  Runner
	events  EventSink
	env     EnvSource
	now     func() time.Time
	locks   keyedLocks

	updateMu      sync.Mutex
	updateEmitted map[string]string
}

// NewService validates options and returns the service.
func NewService(db *sql.DB, opt Options) (*Service, error) {
	if db == nil {
		return nil, invalid("db is required")
	}
	if opt.Catalog == nil {
		return nil, invalid("catalog is required")
	}
	if opt.Runner == nil {
		return nil, invalid("runner is required")
	}
	events := opt.Events
	if events == nil {
		events = LogEventSink{}
	}
	now := opt.Now
	if now == nil {
		now = time.Now
	}
	return &Service{
		db:            db,
		catalog:       opt.Catalog,
		runner:        opt.Runner,
		events:        events,
		env:           opt.Env,
		now:           now,
		updateEmitted: make(map[string]string),
	}, nil
}

// Status is the JSON view of one application. It embeds the shared domain
// view and adds controller-owned fields. It deliberately carries no secret
// material: environment projections exist only in memory between the env
// source and the runner.
type Status struct {
	domain.Application
	BundleID          string `json:"bundle_id"`
	PreviousDigest    string `json:"previous_digest,omitempty"`
	RollbackAvailable bool   `json:"rollback_available"`
	Error             string `json:"error,omitempty"`
}

// InstallRequest installs a curated bundle. Name defaults to the bundle ID;
// exposure defaults to the bundle default (private when unset) and may not
// exceed the bundle's capability; non-private exposure requires a hostname.
type InstallRequest struct {
	BundleID string          `json:"bundle_id"`
	Name     string          `json:"name,omitempty"`
	Hostname string          `json:"hostname,omitempty"`
	Exposure domain.Exposure `json:"exposure,omitempty"`
}

// CatalogBundles lists the curated bundles available for install, in catalog
// order. It returns the validated, immutable entries only.
func (s *Service) CatalogBundles() []Bundle {
	s.catMu.RLock()
	defer s.catMu.RUnlock()
	return s.catalog.Bundles()
}

// Install validates the request, claims the app row with observed state
// provisioning, deploys through the runner, and finalizes to running.
func (s *Service) Install(ctx context.Context, req InstallRequest) (Status, error) {
	s.catMu.RLock()
	bundle, ok := s.catalog.Get(req.BundleID)
	s.catMu.RUnlock()
	if !ok {
		return Status{}, invalid("unknown bundle %q", req.BundleID)
	}
	if !supportedArchitectures[runtime.GOARCH] || !containsString(bundle.Architectures, runtime.GOARCH) {
		return Status{}, fmt.Errorf("%w: bundle %q needs one of %s, host is %s",
			ErrUnsupportedArch, bundle.ID, strings.Join(bundle.Architectures, ", "), runtime.GOARCH)
	}
	name := req.Name
	if name == "" {
		name = bundle.ID
	}
	if !validSlug(name) {
		return Status{}, invalid("name %q must be 1-63 chars of [a-z0-9-], starting with a letter", name)
	}
	exposure := req.Exposure
	if exposure == "" {
		exposure = bundle.DefaultExposure
	}
	if exposureRank(exposure) < 0 {
		return Status{}, invalid("exposure %q is not private, shared, or public", exposure)
	}
	if exposureRank(exposure) > exposureRank(bundle.MaxExposure) {
		return Status{}, invalid("bundle %q supports at most %q exposure", bundle.ID, bundle.MaxExposure)
	}
	hostname := req.Hostname
	if hostname == "" && exposureRank(exposure) > 0 {
		return Status{}, invalid("hostname is required for %q exposure", exposure)
	}
	if hostname != "" && !hostnameRe.MatchString(hostname) {
		return Status{}, invalid("hostname %q is not a valid lowercase hostname", hostname)
	}
	compose, err := renderCompose(bundle, bundle.Digest)
	if err != nil {
		return Status{}, err
	}

	unlock := s.locks.acquire("name:" + name)
	defer unlock()
	if _, err := getAppByName(ctx, s.db, name); err == nil {
		return Status{}, fmt.Errorf("%w: app %q", ErrAlreadyExists, name)
	} else if !errors.Is(err, ErrNotFound) {
		return Status{}, err
	}

	ts := s.now().UTC()
	rec := appRecord{
		ID:               domain.ID(newID("app")),
		Name:             name,
		BundleID:         bundle.ID,
		Image:            bundle.Image,
		Digest:           bundle.Digest,
		Hostname:         hostname,
		Exposure:         exposure,
		Health:           domain.HealthUnknown,
		DesiredState:     DesiredRunning,
		ObservedState:    ObservedProvisioning,
		CurrentReleaseID: domain.ID(newID("rel")),
		InstalledAt:      &ts,
		UpdatedAt:        ts,
	}
	rel := releaseRecord{ID: rec.CurrentReleaseID, AppID: rec.ID, Digest: bundle.Digest, Compose: compose, CreatedAt: ts}
	if err := createAppWithRelease(ctx, s.db, rec, rel); err != nil {
		return Status{}, err
	}

	spec := DeploySpec{Compose: compose, Digest: bundle.Digest, Health: bundle.HealthCheck}
	env, envErr := s.envFor(ctx, rec)
	spec.Env = env
	deployErr := envErr
	if deployErr == nil {
		deployErr = s.runner.Deploy(ctx, rec.application(), spec)
	}
	if deployErr != nil {
		msg := redact(deployErr.Error(), env)
		s.bestEffortRemove(ctx, rec.application(), spec)
		if err := setObserved(ctx, s.db, rec.ID, ObservedFailed, domain.HealthUnknown, msg, s.now()); err != nil {
			return Status{}, err
		}
		s.emit(ctx, EventInstallFailed, SeverityError, rec.ID, "install of "+name+" failed",
			map[string]any{"bundle_id": bundle.ID, "digest": bundle.Digest, "error": msg})
		return Status{}, fmt.Errorf("%w: %s", ErrRunner, msg)
	}

	health := s.probe(ctx, rec, spec)
	if err := setObserved(ctx, s.db, rec.ID, ObservedRunning, health, "", s.now()); err != nil {
		return Status{}, err
	}
	s.emit(ctx, EventInstalled, SeverityInfo, rec.ID, name+" installed", map[string]any{
		"bundle_id": bundle.ID, "digest": bundle.Digest, "exposure": string(exposure),
	})
	return s.Status(ctx, rec.ID)
}

// Start reconciles desired state to running.
func (s *Service) Start(ctx context.Context, id domain.ID) (Status, error) {
	rec, unlock, err := s.lockedApp(ctx, id)
	if err != nil {
		return Status{}, err
	}
	defer unlock()
	if rec.ObservedState == ObservedProvisioning {
		return Status{}, fmt.Errorf("%w: app %s is provisioning", ErrConflict, id)
	}
	if rec.DesiredState == DesiredRunning && rec.ObservedState == ObservedRunning {
		return s.Status(ctx, id)
	}
	if err := setDesired(ctx, s.db, id, DesiredRunning, s.now()); err != nil {
		return Status{}, err
	}
	spec, err := s.specFor(ctx, rec)
	if err != nil {
		return Status{}, err
	}
	if err := s.runner.Start(ctx, rec.application(), spec); err != nil {
		msg := redact(err.Error(), spec.Env)
		if setErr := setObserved(ctx, s.db, id, rec.ObservedState, rec.Health, msg, s.now()); setErr != nil {
			return Status{}, setErr
		}
		s.emit(ctx, EventStartFailed, SeverityError, id, rec.Name+" failed to start",
			map[string]any{"digest": rec.Digest, "error": msg})
		return Status{}, fmt.Errorf("%w: %s", ErrRunner, msg)
	}
	if err := setObserved(ctx, s.db, id, ObservedRunning, domain.HealthUnknown, "", s.now()); err != nil {
		return Status{}, err
	}
	s.emit(ctx, EventStarted, SeverityInfo, id, rec.Name+" started", map[string]any{"digest": rec.Digest})
	return s.Status(ctx, id)
}

// Stop reconciles desired state to stopped. Persistent data is untouched.
func (s *Service) Stop(ctx context.Context, id domain.ID) (Status, error) {
	rec, unlock, err := s.lockedApp(ctx, id)
	if err != nil {
		return Status{}, err
	}
	defer unlock()
	switch rec.ObservedState {
	case ObservedProvisioning:
		return Status{}, fmt.Errorf("%w: app %s is provisioning", ErrConflict, id)
	case ObservedStopped:
		// Already stopped. If a start was pending (desired running), Stop
		// cancels it by reconciling desired state; either way no runner
		// action is needed.
		if rec.DesiredState != DesiredStopped {
			if err := setDesired(ctx, s.db, id, DesiredStopped, s.now()); err != nil {
				return Status{}, err
			}
		}
		return s.Status(ctx, id)
	case ObservedAbsent:
		return Status{}, fmt.Errorf("%w: app %s is %s", ErrConflict, id, rec.ObservedState)
	}
	if err := setDesired(ctx, s.db, id, DesiredStopped, s.now()); err != nil {
		return Status{}, err
	}
	spec, err := s.specFor(ctx, rec)
	if err != nil {
		return Status{}, err
	}
	if err := s.runner.Stop(ctx, rec.application(), spec); err != nil {
		msg := redact(err.Error(), spec.Env)
		if setErr := setObserved(ctx, s.db, id, rec.ObservedState, rec.Health, msg, s.now()); setErr != nil {
			return Status{}, setErr
		}
		s.emit(ctx, EventStopFailed, SeverityError, id, rec.Name+" failed to stop",
			map[string]any{"digest": rec.Digest, "error": msg})
		return Status{}, fmt.Errorf("%w: %s", ErrRunner, msg)
	}
	if err := setObserved(ctx, s.db, id, ObservedStopped, domain.HealthUnknown, "", s.now()); err != nil {
		return Status{}, err
	}
	s.emit(ctx, EventStopped, SeverityInfo, id, rec.Name+" stopped", map[string]any{"digest": rec.Digest})
	return s.Status(ctx, id)
}

// Update deploys a new pinned digest and/or catalog compose for the app's
// bundle. Mutable tags are rejected. Same digest with a changed compose is a
// valid new release. The current release pointer only moves after a successful
// deploy; on failure the previous version is restored and the app records
// the failure without changing its release history. When digest and rendered
// compose both match the active release, Update returns the current status.
func (s *Service) Update(ctx context.Context, id domain.ID, digest string) (Status, error) {
	rec, unlock, err := s.lockedApp(ctx, id)
	if err != nil {
		return Status{}, err
	}
	defer unlock()
	if rec.ObservedState == ObservedProvisioning {
		return Status{}, fmt.Errorf("%w: app %s is provisioning", ErrConflict, id)
	}
	if !ValidDigest(digest) {
		return Status{}, invalid("digest %q must be a pinned sha256 digest; mutable tags are not accepted", digest)
	}
	s.catMu.RLock()
	bundle, ok := s.catalog.Get(rec.BundleID)
	s.catMu.RUnlock()
	if !ok {
		return Status{}, fmt.Errorf("%w: bundle %q missing from catalog", ErrConflict, rec.BundleID)
	}
	if bundle.Runtime == RuntimeSystemd {
		return Status{}, invalid("bundle %q is a native system service managed by the system image; updates come from the nixpkgs pin", rec.BundleID)
	}
	compose, err := renderCompose(bundle, digest)
	if err != nil {
		return Status{}, err
	}
	if rec.CurrentReleaseID != "" {
		current, err := getRelease(ctx, s.db, rec.CurrentReleaseID)
		if err != nil {
			return Status{}, err
		}
		if digest == rec.Digest && compose == current.Compose {
			return s.Status(ctx, id)
		}
	}

	rel := releaseRecord{ID: domain.ID(newID("rel")), AppID: rec.ID, Digest: digest, Compose: compose, CreatedAt: s.now().UTC()}
	if err := insertRelease(ctx, s.db, rel); err != nil {
		return Status{}, err
	}
	if err := setObserved(ctx, s.db, rec.ID, ObservedProvisioning, domain.HealthUnknown, "", s.now()); err != nil {
		return Status{}, err
	}

	spec := DeploySpec{Compose: compose, Digest: digest, Health: bundle.HealthCheck}
	env, envErr := s.envFor(ctx, rec)
	spec.Env = env
	deployErr := envErr
	if deployErr == nil {
		deployErr = s.runner.Deploy(ctx, rec.application(), spec)
	}
	if deployErr != nil {
		msg := redact(deployErr.Error(), env)
		s.dropRelease(ctx, rel.ID)
		data := map[string]any{"digest": digest, "current_digest": rec.Digest, "error": msg}
		if restoreErr := s.redeployCurrent(ctx, rec); restoreErr != nil {
			combined := msg + "; restoring current release also failed: " + redact(restoreErr.Error(), env)
			if err := setObserved(ctx, s.db, rec.ID, ObservedFailed, domain.HealthUnknown, combined, s.now()); err != nil {
				return Status{}, err
			}
			data["error"] = combined
			data["restored"] = false
			s.emit(ctx, EventUpdateFailed, SeverityError, id, "update of "+rec.Name+" failed and rollback to current failed", data)
			return Status{}, fmt.Errorf("%w: %s", ErrRunner, combined)
		}
		observed := ObservedRunning
		if rec.DesiredState == DesiredStopped {
			observed = ObservedStopped
		}
		if err := setObserved(ctx, s.db, rec.ID, observed, domain.HealthUnknown, msg, s.now()); err != nil {
			return Status{}, err
		}
		data["restored"] = true
		s.emit(ctx, EventUpdateFailed, SeverityError, id, "update of "+rec.Name+" failed; current release restored", data)
		return Status{}, fmt.Errorf("%w: %s", ErrRunner, msg)
	}

	observed, err := s.reconcileStoppedDesired(ctx, rec, spec)
	if err != nil {
		return Status{}, err
	}
	if err := activateRelease(ctx, s.db, rec.ID, rel.ID, domain.ID(rec.CurrentReleaseID), digest, observed, domain.HealthUnknown, "", s.now()); err != nil {
		return Status{}, err
	}
	s.emit(ctx, EventUpdated, SeverityInfo, id, rec.Name+" updated", map[string]any{
		"digest": digest, "previous_digest": rec.Digest,
	})
	return s.Status(ctx, id)
}

// Rollback redeploys the retained previous release and swaps the pointers,
// keeping the replaced release as the new previous. Exactly two releases are
// retained after any successful transition.
func (s *Service) Rollback(ctx context.Context, id domain.ID) (Status, error) {
	rec, unlock, err := s.lockedApp(ctx, id)
	if err != nil {
		return Status{}, err
	}
	defer unlock()
	if rec.ObservedState == ObservedProvisioning {
		return Status{}, fmt.Errorf("%w: app %s is provisioning", ErrConflict, id)
	}
	if rec.PreviousReleaseID == "" {
		return Status{}, fmt.Errorf("%w: app %s has no previous release to roll back to", ErrConflict, id)
	}
	prev, err := getRelease(ctx, s.db, domain.ID(rec.PreviousReleaseID))
	if err != nil {
		return Status{}, err
	}
	spec := DeploySpec{Compose: prev.Compose, Digest: prev.Digest, Health: s.healthCheckFor(rec.BundleID)}
	if err := setObserved(ctx, s.db, rec.ID, ObservedProvisioning, domain.HealthUnknown, "", s.now()); err != nil {
		return Status{}, err
	}
	env, envErr := s.envFor(ctx, rec)
	spec.Env = env
	deployErr := envErr
	if deployErr == nil {
		deployErr = s.runner.Deploy(ctx, rec.application(), spec)
	}
	if deployErr != nil {
		msg := redact(deployErr.Error(), env)
		if restoreErr := s.redeployCurrent(ctx, rec); restoreErr != nil {
			combined := msg + "; restoring current release also failed: " + redact(restoreErr.Error(), env)
			if err := setObserved(ctx, s.db, rec.ID, ObservedFailed, domain.HealthUnknown, combined, s.now()); err != nil {
				return Status{}, err
			}
			s.emit(ctx, EventRollbackFailed, SeverityError, id, "rollback of "+rec.Name+" failed and restore failed",
				map[string]any{"digest": prev.Digest, "error": combined})
			return Status{}, fmt.Errorf("%w: %s", ErrRunner, combined)
		}
		observed := ObservedRunning
		if rec.DesiredState == DesiredStopped {
			observed = ObservedStopped
		}
		if err := setObserved(ctx, s.db, rec.ID, observed, domain.HealthUnknown, msg, s.now()); err != nil {
			return Status{}, err
		}
		s.emit(ctx, EventRollbackFailed, SeverityError, id, "rollback of "+rec.Name+" failed; current release restored",
			map[string]any{"digest": prev.Digest, "error": msg})
		return Status{}, fmt.Errorf("%w: %s", ErrRunner, msg)
	}

	observed, err := s.reconcileStoppedDesired(ctx, rec, spec)
	if err != nil {
		return Status{}, err
	}
	if err := activateRelease(ctx, s.db, rec.ID, prev.ID, domain.ID(rec.CurrentReleaseID), prev.Digest, observed, domain.HealthUnknown, "", s.now()); err != nil {
		return Status{}, err
	}
	s.emit(ctx, EventRolledBack, SeverityInfo, id, rec.Name+" rolled back", map[string]any{
		"digest": prev.Digest, "from_digest": rec.Digest,
	})
	return s.Status(ctx, id)
}

// Uninstall tears the stack down and removes its rows. Persistent volumes
// survive; deleting application data is a separate operation.
func (s *Service) Uninstall(ctx context.Context, id domain.ID) error {
	rec, unlock, err := s.lockedApp(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	s.catMu.RLock()
	bundle, ok := s.catalog.Get(rec.BundleID)
	s.catMu.RUnlock()
	if ok && bundle.Runtime == RuntimeSystemd {
		return invalid("bundle %q is a native system service defined by the system closure; it cannot be uninstalled", rec.BundleID)
	}
	spec, err := s.specFor(ctx, rec)
	if err != nil {
		return err
	}
	if err := s.runner.Remove(ctx, rec.application(), spec); err != nil {
		msg := redact(err.Error(), spec.Env)
		if setErr := setObserved(ctx, s.db, id, ObservedFailed, rec.Health, msg, s.now()); setErr != nil {
			return setErr
		}
		s.emit(ctx, EventUninstallFailed, SeverityError, id, "uninstall of "+rec.Name+" failed",
			map[string]any{"digest": rec.Digest, "error": msg})
		return fmt.Errorf("%w: %s", ErrRunner, msg)
	}
	if err := deleteApp(ctx, s.db, id); err != nil {
		return err
	}
	s.emit(ctx, EventUninstalled, SeverityInfo, id, rec.Name+" uninstalled", map[string]any{"digest": rec.Digest})
	return nil
}

// CheckHealth observes health through the runner and records transitions.
// service.unhealthy / service.healthy events follow the normalized design
// vocabulary.
func (s *Service) CheckHealth(ctx context.Context, id domain.ID) (Status, error) {
	rec, unlock, err := s.lockedApp(ctx, id)
	if err != nil {
		return Status{}, err
	}
	defer unlock()
	if rec.ObservedState != ObservedRunning {
		return Status{}, fmt.Errorf("%w: health check requires a running app, %s is %q", ErrConflict, id, rec.ObservedState)
	}
	spec, err := s.specFor(ctx, rec)
	if err != nil {
		return Status{}, err
	}
	health, err := s.runner.Check(ctx, rec.application(), spec)
	if err != nil {
		return Status{}, fmt.Errorf("%w: %v", ErrRunner, err)
	}
	if health != rec.Health {
		if err := setHealth(ctx, s.db, id, health, s.now()); err != nil {
			return Status{}, err
		}
		switch health {
		case domain.HealthUnhealthy:
			s.emit(ctx, EventUnhealthy, SeverityWarning, id, rec.Name+" is unhealthy",
				map[string]any{"digest": rec.Digest})
		case domain.HealthHealthy:
			if rec.Health == domain.HealthUnhealthy {
				s.emit(ctx, EventHealthy, SeverityInfo, id, rec.Name+" recovered",
					map[string]any{"digest": rec.Digest})
			}
		}
	}
	return s.Status(ctx, id)
}

// Status returns the current view of one application.
func (s *Service) Status(ctx context.Context, id domain.ID) (Status, error) {
	rec, err := getApp(ctx, s.db, id)
	if err != nil {
		return Status{}, err
	}
	return s.view(ctx, rec)
}

// List returns every application ordered by name.
func (s *Service) List(ctx context.Context) ([]Status, error) {
	recs, err := listApps(ctx, s.db)
	if err != nil {
		return nil, err
	}
	out := make([]Status, 0, len(recs))
	for _, rec := range recs {
		v, err := s.view(ctx, rec)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func (s *Service) view(ctx context.Context, rec appRecord) (Status, error) {
	st := Status{Application: rec.application(), BundleID: rec.BundleID, Error: rec.LastError}
	if rec.PreviousReleaseID != "" {
		prev, err := getRelease(ctx, s.db, domain.ID(rec.PreviousReleaseID))
		if err != nil {
			if !errors.Is(err, ErrNotFound) {
				return Status{}, err
			}
		} else {
			st.PreviousDigest = prev.Digest
			st.RollbackAvailable = true
		}
	}
	return st, nil
}

// specFor rebuilds the deploy spec for the current release, including a
// fresh secret projection and the bundle's health check.
func (s *Service) specFor(ctx context.Context, rec appRecord) (DeploySpec, error) {
	rel, err := getRelease(ctx, s.db, domain.ID(rec.CurrentReleaseID))
	if err != nil {
		return DeploySpec{}, err
	}
	spec := DeploySpec{Compose: rel.Compose, Digest: rel.Digest, Health: s.healthCheckFor(rec.BundleID)}
	env, err := s.envFor(ctx, rec)
	if err != nil {
		return DeploySpec{}, err
	}
	spec.Env = env
	return spec, nil
}

func (s *Service) healthCheckFor(bundleID string) HealthCheck {
	s.catMu.RLock()
	b, ok := s.catalog.Get(bundleID)
	s.catMu.RUnlock()
	if ok {
		return b.HealthCheck
	}
	return HealthCheck{Kind: CheckNone}
}

func (s *Service) envFor(ctx context.Context, rec appRecord) ([]string, error) {
	if s.env == nil {
		return nil, nil
	}
	env, err := s.env(ctx, rec.application())
	if err != nil {
		return nil, fmt.Errorf("env source: %w", err)
	}
	for _, kv := range env {
		if !strings.Contains(kv, "=") {
			return nil, invalid("env entry %q must be NAME=VALUE", kv)
		}
	}
	return env, nil
}

// reconcileStoppedDesired stops a freshly deployed stack whose desired state
// is stopped, returning the resulting observed state.
func (s *Service) reconcileStoppedDesired(ctx context.Context, rec appRecord, spec DeploySpec) (string, error) {
	if rec.DesiredState != DesiredStopped {
		return ObservedRunning, nil
	}
	if err := s.runner.Stop(ctx, rec.application(), spec); err != nil {
		if setErr := setObserved(ctx, s.db, rec.ID, ObservedRunning, domain.HealthUnknown, redact(err.Error(), spec.Env), s.now()); setErr != nil {
			return "", setErr
		}
		return "", fmt.Errorf("%w: deployed but stop failed: %s", ErrRunner, redact(err.Error(), spec.Env))
	}
	return ObservedStopped, nil
}

// redeployCurrent restores the current release after a failed deploy.
func (s *Service) redeployCurrent(ctx context.Context, rec appRecord) error {
	spec, err := s.specFor(ctx, rec)
	if err != nil {
		return err
	}
	if err := s.runner.Deploy(ctx, rec.application(), spec); err != nil {
		return err
	}
	if rec.DesiredState == DesiredStopped {
		return s.runner.Stop(ctx, rec.application(), spec)
	}
	return nil
}

func (s *Service) probe(ctx context.Context, rec appRecord, spec DeploySpec) domain.Health {
	health, err := s.runner.Check(ctx, rec.application(), spec)
	if err != nil {
		return domain.HealthUnknown
	}
	return health
}

func (s *Service) bestEffortRemove(ctx context.Context, app domain.Application, spec DeploySpec) {
	if err := s.runner.Remove(ctx, app, spec); err != nil {
		slog.Warn("apps: cleanup after failed deploy failed", "app", string(app.ID), "err", redact(err.Error(), spec.Env))
	}
}

func (s *Service) dropRelease(ctx context.Context, id domain.ID) {
	if err := deleteRelease(ctx, s.db, id); err != nil {
		slog.Warn("apps: dropping failed release failed", "release", string(id), "err", err)
	}
}

func (s *Service) emit(ctx context.Context, typ, severity string, id domain.ID, msg string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	ev := domain.Event{
		ID:         domain.ID(newID("evt")),
		Type:       typ,
		Severity:   severity,
		ResourceID: id,
		Message:    msg,
		Data:       data,
		CreatedAt:  s.now().UTC(),
	}
	if err := s.events.Emit(ctx, ev); err != nil {
		slog.Warn("apps: event sink rejected event", "type", typ, "err", err)
	}
}

// SetCatalog replaces the curated catalog atomically. It is used when a new
// signed catalog is applied and allows CheckForUpdates to detect new digests
// without restarting the service.
func (s *Service) SetCatalog(c *Catalog) error {
	if c == nil {
		return invalid("catalog is required")
	}
	s.catMu.Lock()
	s.catalog = c
	s.catMu.Unlock()
	return nil
}

// Catalog returns the current catalog snapshot.
func (s *Service) CatalogSnapshot() *Catalog {
	s.catMu.RLock()
	defer s.catMu.RUnlock()
	return s.catalog
}

// CheckForUpdates compares each installed app's digest with its bundle's
// catalog digest and emits service.update_available on transitions to an
// update-available state. It emits exactly once per distinct new digest and
// is idempotent on re-observation of the same digest.
func (s *Service) CheckForUpdates(ctx context.Context) ([]Status, error) {
	recs, err := listApps(ctx, s.db)
	if err != nil {
		return nil, err
	}
	var withUpdates []Status
	s.catMu.RLock()
	cat := s.catalog
	s.catMu.RUnlock()
	if cat == nil {
		return nil, nil
	}
	for _, rec := range recs {
		bundle, ok := cat.Get(rec.BundleID)
		if !ok {
			continue
		}
		if bundle.Runtime == RuntimeSystemd {
			continue // versions track the nixpkgs pin, not the catalog
		}
		if bundle.Digest == "" || rec.Digest == bundle.Digest {
			continue
		}
		st, err := s.view(ctx, rec)
		if err != nil {
			continue
		}
		withUpdates = append(withUpdates, st)
		key := string(rec.ID)
		s.updateMu.Lock()
		last := s.updateEmitted[key]
		if last == bundle.Digest {
			s.updateMu.Unlock()
			continue
		}
		s.updateEmitted[key] = bundle.Digest
		s.updateMu.Unlock()
		s.emit(ctx, EventUpdateAvailable, SeverityInfo, rec.ID,
			rec.Name+" update available",
			map[string]any{
				"bundle_id":  rec.BundleID,
				"old_digest": rec.Digest,
				"new_digest": bundle.Digest,
			})
	}
	return withUpdates, nil
}

// ResetUpdateAvailableDedup clears the in-memory dedup state for
// service.update_available. It is exposed for tests and for catalog
// rollback scenarios.
func (s *Service) ResetUpdateAvailableDedup() {
	s.updateMu.Lock()
	s.updateEmitted = make(map[string]string)
	s.updateMu.Unlock()
}

func (s *Service) lockedApp(ctx context.Context, id domain.ID) (appRecord, func(), error) {
	unlock := s.locks.acquire(string(id))
	rec, err := getApp(ctx, s.db, id)
	if err != nil {
		unlock()
		return appRecord{}, nil, err
	}
	return rec, unlock, nil
}

func containsString(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

// redact replaces secret environment values in msg so runner diagnostics can
// be persisted and emitted without leaking secrets. Longer values are
// replaced first so overlapping secrets cannot survive partial masking.
func redact(msg string, env []string) string {
	values := make([]string, 0, len(env))
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i > 0 && i+1 < len(kv) {
			values = append(values, kv)
		}
	}
	sort.Slice(values, func(i, j int) bool {
		return len(values[i][strings.IndexByte(values[i], '=')+1:]) > len(values[j][strings.IndexByte(values[j], '=')+1:])
	})
	for _, kv := range values {
		i := strings.IndexByte(kv, '=')
		msg = strings.ReplaceAll(msg, kv[i+1:], "$"+kv[:i])
	}
	return msg
}

// keyedLocks serializes mutating operations per application (and per name
// during install) so lifecycle transitions never interleave.
type keyedLocks struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func (l *keyedLocks) acquire(key string) func() {
	l.mu.Lock()
	if l.locks == nil {
		l.locks = make(map[string]*sync.Mutex)
	}
	m := l.locks[key]
	if m == nil {
		m = &sync.Mutex{}
		l.locks[key] = m
	}
	l.mu.Unlock()
	m.Lock()
	return m.Unlock
}
