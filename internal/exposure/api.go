package exposure

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/store"
)

// UpsertInput creates or updates a service's desired state. Empty optional
// fields keep their current value on update and take defaults on create.
type UpsertInput struct {
	// Hostname is the vanity hostname under the instance domain, for
	// example "ai.example.com". It identifies the service and cannot be
	// changed after creation.
	Hostname string `json:"hostname"`
	// Upstream is the local service address the edge routes to, for
	// example "immich:8080" or "127.0.0.1:8096".
	Upstream string `json:"upstream"`
	// TunnelOrigin is where cloudflared forwards tunnel traffic. Defaults
	// to the local edge, which routes by Host header.
	TunnelOrigin string `json:"tunnel_origin,omitempty"`
	// Exposure is the desired exposure. Defaults to private on create.
	Exposure domain.Exposure `json:"exposure,omitempty"`
	// AppAuth declares whether the application authenticates users itself.
	// Defaults to none on create.
	AppAuth AuthMode `json:"app_auth,omitempty"`
}

// UpsertService creates a service or updates its desired state. New services
// default to private exposure with no application-level authentication.
// Material changes bump the desired-state revision, which invalidates
// pending plans and public acknowledgements.
func (s *Service) UpsertService(ctx context.Context, in UpsertInput) (ServiceRecord, error) {
	hostname := strings.ToLower(strings.TrimSpace(in.Hostname))
	if err := validateHostname(hostname); err != nil {
		return ServiceRecord{}, store.Validationf("hostname: %v", err)
	}
	anchor, err := homeAnchor(hostname, s.cfg.Domain)
	if err != nil {
		return ServiceRecord{}, store.Validationf("anchor: %v", err)
	}
	upstream := strings.TrimSpace(in.Upstream)
	if upstream == "" || strings.ContainsAny(upstream, " \t\r\n") {
		return ServiceRecord{}, store.Validationf("upstream must be a host[:port] or URL without whitespace")
	}
	origin := strings.TrimSpace(in.TunnelOrigin)
	if origin == "" {
		origin = defaultTunnelOrigin
	}
	if strings.ContainsAny(origin, " \t\r\n") {
		return ServiceRecord{}, store.Validationf("tunnel origin must not contain whitespace")
	}

	existing, err := s.getServiceByHostname(ctx, hostname)
	if errors.Is(err, store.ErrNotFound) {
		exposure := in.Exposure
		if exposure == "" {
			exposure = domain.ExposurePrivate
		}
		if !exposure.Valid() {
			return ServiceRecord{}, store.Validationf("unknown exposure %q", exposure)
		}
		auth := in.AppAuth
		if auth == "" {
			auth = AuthNone
		}
		if !auth.Valid() {
			return ServiceRecord{}, store.Validationf("unknown app auth %q", auth)
		}
		now := nowUTC()
		rec := ServiceRecord{
			ID:           store.NewID(),
			Hostname:     hostname,
			HomeAnchor:   anchor,
			Upstream:     upstream,
			TunnelOrigin: origin,
			Exposure:     exposure,
			AppAuth:      auth,
			Revision:     1,
			CreatedAt:    now,
			UpdatedAt:    now,
		}
		if err := s.insertService(ctx, &rec); err != nil {
			return ServiceRecord{}, err
		}
		return rec, nil
	}
	if err != nil {
		return ServiceRecord{}, err
	}

	changed := false
	if upstream != existing.Upstream {
		existing.Upstream = upstream
		changed = true
	}
	if origin != existing.TunnelOrigin {
		existing.TunnelOrigin = origin
		changed = true
	}
	if in.Exposure != "" {
		if !in.Exposure.Valid() {
			return ServiceRecord{}, store.Validationf("unknown exposure %q", in.Exposure)
		}
		if in.Exposure != existing.Exposure {
			existing.Exposure = in.Exposure
			changed = true
		}
	}
	if in.AppAuth != "" {
		if !in.AppAuth.Valid() {
			return ServiceRecord{}, store.Validationf("unknown app auth %q", in.AppAuth)
		}
		if in.AppAuth != existing.AppAuth {
			existing.AppAuth = in.AppAuth
			changed = true
		}
	}
	if changed {
		existing.Revision++
		existing.UpdatedAt = nowUTC()
		if err := s.updateService(ctx, &existing); err != nil {
			return ServiceRecord{}, err
		}
	}
	return existing, nil
}

// GetService returns one service by ID.
func (s *Service) GetService(ctx context.Context, id string) (ServiceRecord, error) {
	return s.getService(ctx, id)
}

// GetServiceByHostname returns one service by vanity hostname.
func (s *Service) GetServiceByHostname(ctx context.Context, hostname string) (ServiceRecord, error) {
	return s.getServiceByHostname(ctx, strings.ToLower(strings.TrimSpace(hostname)))
}

// ListServices returns all services ordered by hostname.
func (s *Service) ListServices(ctx context.Context) ([]ServiceRecord, error) {
	return s.listServices(ctx)
}

// SetExposure changes the desired exposure and bumps the revision, which
// invalidates pending plans and any public acknowledgement. The change takes
// effect only when a new plan is applied.
func (s *Service) SetExposure(ctx context.Context, serviceID string, exposure domain.Exposure) (ServiceRecord, error) {
	if !exposure.Valid() {
		return ServiceRecord{}, store.Validationf("unknown exposure %q", exposure)
	}
	svc, err := s.getService(ctx, serviceID)
	if err != nil {
		return ServiceRecord{}, err
	}
	if svc.Exposure == exposure {
		return svc, nil
	}
	svc.Exposure = exposure
	svc.Revision++
	svc.UpdatedAt = nowUTC()
	if err := s.updateService(ctx, &svc); err != nil {
		return ServiceRecord{}, err
	}
	return svc, nil
}

// AcknowledgePublic records an explicit, revision-bound acknowledgement that
// an unauthenticated service may be made public. The acknowledgement goes
// stale as soon as the desired state changes (revision bump), and Apply will
// reject the plan again until it is renewed. This is the guard against silent
// private-to-public transitions.
func (s *Service) AcknowledgePublic(ctx context.Context, serviceID string) (string, error) {
	svc, err := s.getService(ctx, serviceID)
	if err != nil {
		return "", err
	}
	if svc.Exposure != domain.ExposurePublic {
		return "", store.Validationf("%s desires %q exposure; acknowledgement applies to public", svc.Hostname, svc.Exposure)
	}
	if svc.AppAuth != AuthNone {
		return "", store.Validationf("%s authenticates users itself; no acknowledgement is required", svc.Hostname)
	}
	ack := publicAck{ServiceID: svc.ID, AckID: store.NewID(), Revision: svc.Revision, CreatedAt: nowUTC()}
	if err := s.putAck(ctx, ack); err != nil {
		return "", err
	}
	return ack.AckID, nil
}

// Plan computes and persists a reconciliation plan for the service's current
// desired exposure. Planning observes live state first, and every changing
// step captures the previous state so Apply can roll back. The returned plan
// is inspectable (ordered steps, descriptions, warnings) and must be applied
// explicitly.
func (s *Service) Plan(ctx context.Context, serviceID string) (Plan, error) {
	svc, err := s.getService(ctx, serviceID)
	if err != nil {
		return Plan{}, err
	}
	return s.buildPlan(ctx, svc, PlanKindExposure)
}

// Delete plans the full teardown of a service: the vanity record is removed
// first (cutting any public reachability), followed by the tunnel path, the
// identity gate, the local edge route, and the anchor. Service rows are
// removed when the plan is applied; the plan itself is kept as an audit
// trail.
func (s *Service) Delete(ctx context.Context, serviceID string) (Plan, error) {
	svc, err := s.getService(ctx, serviceID)
	if err != nil {
		return Plan{}, err
	}
	return s.buildPlan(ctx, svc, PlanKindDelete)
}

// GetPlan returns one persisted plan by ID.
func (s *Service) GetPlan(ctx context.Context, planID string) (Plan, error) {
	return s.getPlan(ctx, planID)
}

// ListPlans returns recent plans for a service, newest first, as the audit
// trail of what changed and why.
func (s *Service) ListPlans(ctx context.Context, serviceID string, limit int) ([]Plan, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	return s.listPlans(ctx, serviceID, limit)
}

// Observe reads live state, computes drift against the desired state, and
// persists the observation. All client scopes must be configured for a full
// observation.
func (s *Service) Observe(ctx context.Context, serviceID string) (Observation, error) {
	svc, err := s.getService(ctx, serviceID)
	if err != nil {
		return Observation{}, err
	}
	obs, missing, err := s.probe(ctx, svc)
	if err != nil {
		return *obs, err
	}
	if len(missing) > 0 {
		return *obs, fmt.Errorf("%w: %s", ErrMissingClient, strings.Join(missing, ", "))
	}
	obs.Drift = s.computeDrift(svc, obs, missing)
	obs.Reconciled = len(obs.Drift) == 0
	if err := s.putObservation(ctx, obs, ""); err != nil {
		return *obs, err
	}
	return *obs, nil
}

// Report is the inspectable summary of one service: desired state, persisted
// observation, and recent plans.
type Report struct {
	Service     ServiceRecord `json:"service"`
	Observation *Observation  `json:"observation,omitempty"`
	PendingPlan *Plan         `json:"pending_plan,omitempty"`
	LastPlan    *Plan         `json:"last_plan,omitempty"`
}

// Status returns the full inspectable state of one service without touching
// any external client.
func (s *Service) Status(ctx context.Context, serviceID string) (Report, error) {
	svc, err := s.getService(ctx, serviceID)
	if err != nil {
		return Report{}, err
	}
	rep := Report{Service: svc}
	if obs, ok, err := s.getObservation(ctx, serviceID); err != nil {
		return Report{}, err
	} else if ok {
		rep.Observation = &obs
	}
	plans, err := s.listPlans(ctx, serviceID, 10)
	if err != nil {
		return Report{}, err
	}
	for i := range plans {
		if plans[i].Status == PlanPending && rep.PendingPlan == nil {
			p := plans[i]
			rep.PendingPlan = &p
		}
	}
	if len(plans) > 0 {
		p := plans[0]
		rep.LastPlan = &p
	}
	return rep, nil
}
