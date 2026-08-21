package exposure

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/omahab/omahab/internal/store"
)

// probe reads the live state of every resource family this controller manages
// for svc. Missing clients are reported as unknown scopes rather than errors,
// so planning can proceed when the absent scopes would not be touched. Hard
// read failures are errors: reconciliation must never guess live state.
func (s *Service) probe(ctx context.Context, svc ServiceRecord) (*Observation, []string, error) {
	obs := &Observation{ServiceID: svc.ID, ObservedAt: nowUTC(), Drift: []string{}}
	var missing, errs []string
	if c := s.clients.DNS; c != nil {
		recs, err := c.ListRecords(ctx)
		if err != nil {
			errs = append(errs, "dns: "+err.Error())
		} else {
			obs.VanityDNS = findRecord(recs, svc.Hostname, "CNAME")
			obs.AnchorDNS = findRecord(recs, svc.HomeAnchor, "A")
		}
	} else {
		missing = append(missing, ScopeDNS)
	}
	if c := s.clients.Tunnel; c != nil {
		rules, err := c.ListIngress(ctx)
		if err != nil {
			errs = append(errs, "tunnel: "+err.Error())
		} else {
			obs.TunnelIngress = findRule(rules, svc.Hostname)
		}
	} else {
		missing = append(missing, ScopeTunnel)
	}
	if c := s.clients.Access; c != nil {
		app, err := c.GetApplication(ctx, svc.Hostname)
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				errs = append(errs, "access: "+err.Error())
			}
		} else {
			obs.AccessApp = app
		}
	} else {
		missing = append(missing, ScopeAccess)
	}
	if c := s.clients.Edge; c != nil {
		routes, err := c.ListRoutes(ctx)
		if err != nil {
			errs = append(errs, "edge: "+err.Error())
		} else {
			obs.EdgeRoute = findRoute(routes, svc.Hostname)
		}
	} else {
		missing = append(missing, ScopeEdge)
	}
	if len(errs) > 0 {
		return obs, missing, fmt.Errorf("exposure: reading live state failed: %s", strings.Join(errs, "; "))
	}
	return obs, missing, nil
}

// Apply executes a persisted plan. It re-validates the preconditions that
// make exposure changes safe: the plan must still match the service's current
// desired-state revision, the required acknowledgement must be current, and
// every client scope the steps touch must be configured. A failing step
// aborts execution and rolls back already-applied steps using their planning
// snapshots; nothing is ever half-applied silently.
func (s *Service) Apply(ctx context.Context, planID string) (Plan, error) {
	plan, err := s.getPlan(ctx, planID)
	if err != nil {
		return Plan{}, err
	}
	if plan.Status != PlanPending {
		return plan, store.Conflictf("plan %s is already %s", planID, plan.Status)
	}
	svc, err := s.getService(ctx, plan.ServiceID)
	if err != nil {
		return plan, err
	}
	if plan.ServiceRevision != svc.Revision {
		return plan, store.Conflictf("service desired state changed since the plan was created; plan again")
	}
	if plan.Kind == PlanKindExposure && plan.ToExposure != svc.Exposure {
		return plan, store.Conflictf("plan targets %q exposure but service desires %q", plan.ToExposure, svc.Exposure)
	}
	if plan.RequiresAck {
		ack, ok, err := s.getAck(ctx, svc.ID)
		if err != nil {
			return plan, err
		}
		if !ok || ack.Revision != svc.Revision {
			return plan, fmt.Errorf("%w: %s does not authenticate users itself; call AcknowledgePublic for revision %d first", ErrAcknowledgementRequired, svc.Hostname, svc.Revision)
		}
	}
	if err := s.requireClients(plan.Steps); err != nil {
		return plan, err
	}

	results := make([]StepResult, 0, len(plan.Steps))
	failIdx, failErr := -1, error(nil)
	for i, step := range plan.Steps {
		status, err := s.execStep(ctx, step)
		res := StepResult{Index: i, Kind: step.Kind, Description: step.Description, Status: status}
		if err != nil {
			res.Status = statusFailed
			res.Error = err.Error()
			results = append(results, res)
			failIdx, failErr = i, err
			break
		}
		results = append(results, res)
	}
	now := nowUTC()
	plan.AppliedAt = &now
	plan.Results = results
	if failIdx >= 0 {
		rbErr := s.rollbackApplied(ctx, plan.Steps, results)
		switch {
		case rbErr != nil:
			plan.Status = PlanFailed
			plan.Error = fmt.Sprintf("step %d (%s) failed: %v; rollback incomplete: %v", failIdx, plan.Steps[failIdx].Kind, failErr, rbErr)
		default:
			plan.Status = PlanRolledBack
			plan.Error = fmt.Sprintf("step %d (%s) failed: %v; earlier steps were rolled back", failIdx, plan.Steps[failIdx].Kind, failErr)
		}
		if err := s.updatePlan(ctx, &plan); err != nil {
			return plan, err
		}
		s.recordObservation(ctx, svc, &plan)
		return plan, fmt.Errorf("%w: %s", ErrApplyFailed, plan.Error)
	}

	plan.Status = PlanApplied
	if err := s.updatePlan(ctx, &plan); err != nil {
		return plan, err
	}
	if plan.Kind == PlanKindDelete {
		if err := s.deleteServiceRows(ctx, svc.ID); err != nil {
			return plan, err
		}
		return plan, nil
	}
	s.recordObservation(ctx, svc, &plan)
	return plan, nil
}

// rollbackApplied undoes the applied steps in reverse order. results is
// mutated in place (the slice shares its backing array with the caller).
func (s *Service) rollbackApplied(ctx context.Context, steps []Step, results []StepResult) error {
	var firstErr error
	for i := len(results) - 1; i >= 0; i-- {
		if results[i].Status != statusApplied {
			continue
		}
		status, err := s.undoStep(ctx, steps[i])
		if err != nil {
			results[i].RollbackStatus = statusRollbackFailed
			results[i].RollbackError = err.Error()
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		results[i].RollbackStatus = status
	}
	return firstErr
}

// execStep performs one step against the external clients and reports whether
// anything changed.
func (s *Service) execStep(ctx context.Context, step Step) (string, error) {
	switch step.Kind {
	case StepDNSEnsure:
		return s.ensureRecord(ctx, step.Record)
	case StepDNSRemove:
		return s.removeRecord(ctx, step.Record.Name, step.Record.Type)
	case StepAccessEnsure:
		return s.ensureAccess(ctx, step.App)
	case StepAccessRemove:
		return s.removeAccess(ctx, step.App.Hostname)
	case StepIngressEnsure:
		return s.ensureIngress(ctx, step.Rule)
	case StepIngressRemove:
		return s.removeIngress(ctx, step.Rule.Hostname)
	case StepEdgeEnsure:
		return s.ensureRoute(ctx, step.Route)
	case StepEdgeRemove:
		return s.removeRoute(ctx, step.Route.Hostname)
	default:
		return "", fmt.Errorf("unknown step kind %q", step.Kind)
	}
}

// undoStep inverts one applied step using the previous-state snapshot
// captured at planning time.
func (s *Service) undoStep(ctx context.Context, step Step) (string, error) {
	switch step.Kind {
	case StepDNSEnsure:
		if step.PrevRecord != nil {
			return s.ensureRecord(ctx, step.PrevRecord)
		}
		return s.removeRecord(ctx, step.Record.Name, step.Record.Type)
	case StepDNSRemove:
		return s.ensureRecord(ctx, step.Record)
	case StepAccessEnsure:
		if step.PrevApp != nil {
			return s.ensureAccess(ctx, step.PrevApp)
		}
		return s.removeAccess(ctx, step.App.Hostname)
	case StepAccessRemove:
		return s.ensureAccess(ctx, step.App)
	case StepIngressEnsure:
		if step.PrevRule != nil {
			return s.ensureIngress(ctx, step.PrevRule)
		}
		return s.removeIngress(ctx, step.Rule.Hostname)
	case StepIngressRemove:
		return s.ensureIngress(ctx, step.Rule)
	case StepEdgeEnsure:
		if step.PrevRoute != nil {
			return s.ensureRoute(ctx, step.PrevRoute)
		}
		return s.removeRoute(ctx, step.Route.Hostname)
	case StepEdgeRemove:
		return s.ensureRoute(ctx, step.Route)
	default:
		return "", fmt.Errorf("unknown step kind %q", step.Kind)
	}
}

func (s *Service) ensureRecord(ctx context.Context, want *Record) (string, error) {
	recs, err := s.clients.DNS.ListRecords(ctx)
	if err != nil {
		return "", err
	}
	if cur := findRecord(recs, want.Name, want.Type); cur != nil {
		if recordMatches(cur, want) {
			return statusUnchanged, nil
		}
		if err := s.clients.DNS.ReplaceRecord(ctx, cur.ID, *want); err != nil {
			return "", err
		}
		return statusApplied, nil
	}
	id, err := s.clients.DNS.CreateRecord(ctx, *want)
	if err != nil {
		return "", err
	}
	want.ID = id
	return statusApplied, nil
}

func (s *Service) removeRecord(ctx context.Context, name, typ string) (string, error) {
	recs, err := s.clients.DNS.ListRecords(ctx)
	if err != nil {
		return "", err
	}
	cur := findRecord(recs, name, typ)
	if cur == nil {
		return statusUnchanged, nil
	}
	if err := s.clients.DNS.DeleteRecord(ctx, cur.ID); err != nil {
		return "", err
	}
	return statusApplied, nil
}

func (s *Service) ensureAccess(ctx context.Context, want *AccessApp) (string, error) {
	cur, err := s.clients.Access.GetApplication(ctx, want.Hostname)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	if cur != nil && accessAppsEqual(cur, want) {
		return statusUnchanged, nil
	}
	next := *want
	if cur != nil {
		next.ID = cur.ID
	}
	if _, err := s.clients.Access.PutApplication(ctx, next); err != nil {
		return "", err
	}
	return statusApplied, nil
}

func (s *Service) removeAccess(ctx context.Context, hostname string) (string, error) {
	cur, err := s.clients.Access.GetApplication(ctx, hostname)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return statusUnchanged, nil
		}
		return "", err
	}
	if cur == nil {
		return statusUnchanged, nil
	}
	if err := s.clients.Access.DeleteApplication(ctx, cur.ID); err != nil {
		return "", err
	}
	return statusApplied, nil
}

// ensureIngress adds or updates one hostname rule, preserving every other
// rule in the tunnel configuration.
func (s *Service) ensureIngress(ctx context.Context, want *IngressRule) (string, error) {
	rules, err := s.clients.Tunnel.ListIngress(ctx)
	if err != nil {
		return "", err
	}
	for i, r := range rules {
		if strings.EqualFold(r.Hostname, want.Hostname) {
			if r.Origin == want.Origin {
				return statusUnchanged, nil
			}
			rules[i] = *want
			if err := s.clients.Tunnel.SetIngress(ctx, rules); err != nil {
				return "", err
			}
			return statusApplied, nil
		}
	}
	if err := s.clients.Tunnel.SetIngress(ctx, append(rules, *want)); err != nil {
		return "", err
	}
	return statusApplied, nil
}

func (s *Service) removeIngress(ctx context.Context, hostname string) (string, error) {
	rules, err := s.clients.Tunnel.ListIngress(ctx)
	if err != nil {
		return "", err
	}
	var kept []IngressRule
	found := false
	for _, r := range rules {
		if strings.EqualFold(r.Hostname, hostname) {
			found = true
			continue
		}
		kept = append(kept, r)
	}
	if !found {
		return statusUnchanged, nil
	}
	if err := s.clients.Tunnel.SetIngress(ctx, kept); err != nil {
		return "", err
	}
	return statusApplied, nil
}

func (s *Service) ensureRoute(ctx context.Context, want *Route) (string, error) {
	routes, err := s.clients.Edge.ListRoutes(ctx)
	if err != nil {
		return "", err
	}
	for _, r := range routes {
		if strings.EqualFold(r.Hostname, want.Hostname) && r.Upstream == want.Upstream {
			return statusUnchanged, nil
		}
	}
	if err := s.clients.Edge.PutRoute(ctx, *want); err != nil {
		return "", err
	}
	return statusApplied, nil
}

func (s *Service) removeRoute(ctx context.Context, hostname string) (string, error) {
	routes, err := s.clients.Edge.ListRoutes(ctx)
	if err != nil {
		return "", err
	}
	for _, r := range routes {
		if strings.EqualFold(r.Hostname, hostname) {
			if err := s.clients.Edge.DeleteRoute(ctx, hostname); err != nil {
				return "", err
			}
			return statusApplied, nil
		}
	}
	return statusUnchanged, nil
}

// recordObservation refreshes and persists observed state after an apply.
// Observation failures are captured into the observation rather than
// returned: the operator must see what the controller sees.
func (s *Service) recordObservation(ctx context.Context, svc ServiceRecord, plan *Plan) {
	obs, missing, err := s.probe(ctx, svc)
	if err != nil {
		obs = &Observation{
			ServiceID:  svc.ID,
			ObservedAt: nowUTC(),
			Drift:      []string{"observation failed: " + err.Error()},
			Error:      err.Error(),
		}
	} else {
		obs.Drift = s.computeDrift(svc, obs, missing)
		obs.Reconciled = len(obs.Drift) == 0
		if plan.Status != PlanApplied {
			obs.Error = plan.Error
		}
	}
	_ = s.putObservation(ctx, obs, plan.ID)
}

func findRecord(recs []Record, name, typ string) *Record {
	for i := range recs {
		if strings.EqualFold(recs[i].Name, name) && recs[i].Type == typ {
			r := recs[i]
			return &r
		}
	}
	return nil
}

func findRule(rules []IngressRule, hostname string) *IngressRule {
	for i := range rules {
		if strings.EqualFold(rules[i].Hostname, hostname) {
			r := rules[i]
			return &r
		}
	}
	return nil
}

func findRoute(routes []Route, hostname string) *Route {
	for i := range routes {
		if strings.EqualFold(routes[i].Hostname, hostname) {
			r := routes[i]
			return &r
		}
	}
	return nil
}
