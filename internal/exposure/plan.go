package exposure

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/store"
)

// PlanKind distinguishes exposure transitions from full teardown.
type PlanKind string

const (
	PlanKindExposure PlanKind = "exposure"
	PlanKindDelete   PlanKind = "delete"
)

// PlanStatus is the lifecycle of a persisted plan.
type PlanStatus string

const (
	PlanPending    PlanStatus = "pending"
	PlanApplied    PlanStatus = "applied"
	PlanFailed     PlanStatus = "failed"
	PlanRolledBack PlanStatus = "rolled_back"
)

// StepKind identifies the resource operation a step performs.
type StepKind string

const (
	StepDNSEnsure     StepKind = "dns.ensure"
	StepDNSRemove     StepKind = "dns.remove"
	StepAccessEnsure  StepKind = "access.ensure"
	StepAccessRemove  StepKind = "access.remove"
	StepIngressEnsure StepKind = "ingress.ensure"
	StepIngressRemove StepKind = "ingress.remove"
	StepEdgeEnsure    StepKind = "edge.ensure"
	StepEdgeRemove    StepKind = "edge.remove"
)

// Per-step execution statuses, persisted in step results.
const (
	statusApplied        = "applied"
	statusUnchanged      = "unchanged"
	statusFailed         = "failed"
	statusRollbackFailed = "rollback_failed"
)

// Step is one ordered, inspectable, reversible resource operation. The
// optional Previous fields snapshot live state at planning time; Apply uses
// them to undo the step if a later step fails.
type Step struct {
	Kind        StepKind     `json:"kind"`
	Hostname    string       `json:"hostname"`
	Description string       `json:"description"`
	Rollback    string       `json:"rollback"`
	Record      *Record      `json:"record,omitempty"`      // dns.ensure / dns.remove
	PrevRecord  *Record      `json:"prev_record,omitempty"` // state before dns.ensure
	App         *AccessApp   `json:"app,omitempty"`         // access.ensure / access.remove
	PrevApp     *AccessApp   `json:"prev_app,omitempty"`    // state before access.ensure
	Rule        *IngressRule `json:"rule,omitempty"`        // ingress.ensure / ingress.remove
	PrevRule    *IngressRule `json:"prev_rule,omitempty"`   // state before ingress.ensure
	Route       *Route       `json:"route,omitempty"`       // edge.ensure / edge.remove
	PrevRoute   *Route       `json:"prev_route,omitempty"`  // state before edge.ensure
}

// StepResult records what happened to one step during Apply.
type StepResult struct {
	Index          int      `json:"index"`
	Kind           StepKind `json:"kind"`
	Description    string   `json:"description"`
	Status         string   `json:"status"`
	Error          string   `json:"error,omitempty"`
	RollbackStatus string   `json:"rollback_status,omitempty"`
	RollbackError  string   `json:"rollback_error,omitempty"`
}

// Plan is a persisted, ordered reconciliation proposal and, after Apply, its
// execution record.
type Plan struct {
	ID              string          `json:"id"`
	ServiceID       string          `json:"service_id"`
	ServiceRevision int64           `json:"service_revision"`
	Kind            PlanKind        `json:"kind"`
	Hostname        string          `json:"hostname"`
	FromExposure    domain.Exposure `json:"from_exposure"` // inferred from live state, "" when unmanaged
	ToExposure      domain.Exposure `json:"to_exposure"`   // "" for delete plans
	Steps           []Step          `json:"steps"`
	Warnings        []string        `json:"warnings"`
	RequiresAck     bool            `json:"requires_ack"`
	Status          PlanStatus      `json:"status"`
	Results         []StepResult    `json:"results,omitempty"`
	Error           string          `json:"error,omitempty"`
	CreatedAt       time.Time       `json:"created_at"`
	AppliedAt       *time.Time      `json:"applied_at,omitempty"`
}

// desiredResources is the resource set that must exist for a desired
// exposure. rule and app are nil when the exposure does not use them.
type desiredResources struct {
	anchor Record
	vanity Record
	route  Route
	rule   *IngressRule
	app    *AccessApp
}

// desiredFor computes the resources that must exist for svc's desired
// exposure.
func (s *Service) desiredFor(svc ServiceRecord) desiredResources {
	anchor := Record{Name: svc.HomeAnchor, Type: "A", Content: s.cfg.TailscaleIP, TTL: dnsTTL, Proxied: false}
	var vanity Record
	if svc.Exposure == domain.ExposurePrivate {
		// DNS-only: the address resolves publicly but is routable only on
		// the tailnet (DESIGN.md 7.2).
		vanity = Record{Name: svc.Hostname, Type: "CNAME", Content: svc.HomeAnchor, TTL: dnsTTL, Proxied: false}
	} else {
		// Tunnel CNAMEs must be proxied for Cloudflare to route them.
		vanity = Record{Name: svc.Hostname, Type: "CNAME", Content: s.cfg.TunnelDNS, TTL: dnsTTL, Proxied: true}
	}
	d := desiredResources{
		anchor: anchor,
		vanity: vanity,
		route:  Route{Hostname: svc.Hostname, Upstream: svc.Upstream},
	}
	if svc.Exposure != domain.ExposurePrivate {
		rule := IngressRule{Hostname: svc.Hostname, Origin: svc.TunnelOrigin}
		d.rule = &rule
	}
	if svc.Exposure == domain.ExposureShared {
		app := s.accessAppFor(svc)
		d.app = &app
	}
	return d
}

// accessAppFor returns the identity gate for a shared service.
func (s *Service) accessAppFor(svc ServiceRecord) AccessApp {
	return AccessApp{
		Name:     svc.Hostname,
		Hostname: svc.Hostname,
		Policies: []AccessPolicy{{Name: "omahab-shared", Include: s.cfg.sharedGroups()}},
	}
}

// inferExposure classifies the live DNS/Access state into an exposure level.
// The empty string means "not managed yet" or drifted to an unknown target.
func (s *Service) inferExposure(svc ServiceRecord, obs *Observation) domain.Exposure {
	v := obs.VanityDNS
	if v == nil || v.Content == "" {
		return ""
	}
	switch v.Content {
	case svc.HomeAnchor:
		return domain.ExposurePrivate
	case s.cfg.TunnelDNS:
		if obs.AccessApp != nil {
			return domain.ExposureShared
		}
		return domain.ExposurePublic
	}
	return ""
}

// buildPlan observes live state and persists a plan for svc.
func (s *Service) buildPlan(ctx context.Context, svc ServiceRecord, kind PlanKind) (Plan, error) {
	obs, missing, err := s.probe(ctx, svc)
	if err != nil {
		return Plan{}, err
	}
	obs.Drift = s.computeDrift(svc, obs, missing)
	obs.Reconciled = len(obs.Drift) == 0
	des := s.desiredFor(svc)
	plan := Plan{
		ID:              store.NewID(),
		ServiceID:       svc.ID,
		ServiceRevision: svc.Revision,
		Kind:            kind,
		Hostname:        svc.Hostname,
		FromExposure:    s.inferExposure(svc, obs),
		ToExposure:      svc.Exposure,
		Steps:           []Step{},
		Warnings:        []string{},
		Status:          PlanPending,
		CreatedAt:       nowUTC(),
	}
	switch kind {
	case PlanKindExposure:
		plan.Steps = s.exposureSteps(svc, obs, des)
		plan.Warnings, plan.RequiresAck = s.planWarnings(svc)
		if err := s.checkOrdering(svc, plan.FromExposure, plan.ToExposure, obs, plan.Steps); err != nil {
			return Plan{}, err
		}
	case PlanKindDelete:
		plan.Steps = s.deleteSteps(svc, obs, des)
		plan.ToExposure = ""
		plan.Warnings = []string{fmt.Sprintf("applying this plan deletes service %s and its DNS records", svc.Hostname)}
		if plan.FromExposure == domain.ExposureShared || plan.FromExposure == domain.ExposurePublic {
			plan.Warnings = append(plan.Warnings, "the service is currently exposed through the tunnel; its vanity record is removed first")
		}
	}
	if err := s.requireClients(plan.Steps); err != nil {
		return Plan{}, err
	}
	if err := s.insertPlan(ctx, &plan); err != nil {
		return Plan{}, err
	}
	if err := s.putObservation(ctx, obs, plan.ID); err != nil {
		return Plan{}, err
	}
	return plan, nil
}

// exposureSteps produces the ordered step list for an exposure transition.
// The phase order is the safety property (DESIGN.md 7.3): foundations first,
// then the identity gate, then the tunnel path, then the single DNS step that
// switches reachability, then cleanup of now-unreachable resources.
func (s *Service) exposureSteps(svc ServiceRecord, obs *Observation, des desiredResources) []Step {
	steps := []Step{}

	// Phase 1 — foundations that do not change reachability: the stable
	// private anchor and the local edge route.
	if !recordMatches(obs.AnchorDNS, &des.anchor) {
		steps = append(steps, Step{
			Kind:        StepDNSEnsure,
			Hostname:    svc.Hostname,
			Record:      cloneRecord(&des.anchor),
			PrevRecord:  cloneRecord(obs.AnchorDNS),
			Description: fmt.Sprintf("point private anchor %s at %s (DNS-only)", des.anchor.Name, des.anchor.Content),
			Rollback:    dnsRollbackText(obs.AnchorDNS),
		})
	}
	if obs.EdgeRoute == nil || obs.EdgeRoute.Upstream != des.route.Upstream {
		steps = append(steps, Step{
			Kind:        StepEdgeEnsure,
			Hostname:    svc.Hostname,
			Route:       cloneRoute(&des.route),
			PrevRoute:   cloneRoute(obs.EdgeRoute),
			Description: fmt.Sprintf("route %s to %s on the local edge", des.route.Hostname, des.route.Upstream),
			Rollback:    routeRollbackText(obs.EdgeRoute),
		})
	}

	// Phase 2 — the shared identity gate, created before any public path.
	if des.app != nil && !accessAppsEqual(obs.AccessApp, des.app) {
		steps = append(steps, Step{
			Kind:        StepAccessEnsure,
			Hostname:    svc.Hostname,
			App:         cloneApp(des.app),
			PrevApp:     cloneApp(obs.AccessApp),
			Description: fmt.Sprintf("ensure the Access identity gate on %s for %s", svc.Hostname, strings.Join(s.cfg.sharedGroups(), ", ")),
			Rollback:    accessRollbackText(obs.AccessApp, svc.Hostname),
		})
	}

	// Phase 3 — the tunnel path, published while DNS still points elsewhere.
	if des.rule != nil && (obs.TunnelIngress == nil || obs.TunnelIngress.Origin != des.rule.Origin) {
		steps = append(steps, Step{
			Kind:        StepIngressEnsure,
			Hostname:    svc.Hostname,
			Rule:        cloneRule(des.rule),
			PrevRule:    cloneRule(obs.TunnelIngress),
			Description: fmt.Sprintf("publish %s through the tunnel to %s", des.rule.Hostname, des.rule.Origin),
			Rollback:    ruleRollbackText(obs.TunnelIngress, svc.Hostname),
		})
	}

	// Phase 4 — the reachability switch: the only step that changes who can
	// reach the service.
	if !recordMatches(obs.VanityDNS, &des.vanity) {
		steps = append(steps, Step{
			Kind:        StepDNSEnsure,
			Hostname:    svc.Hostname,
			Record:      cloneRecord(&des.vanity),
			PrevRecord:  cloneRecord(obs.VanityDNS),
			Description: fmt.Sprintf("point %s at %s (proxied=%t)", des.vanity.Name, des.vanity.Content, des.vanity.Proxied),
			Rollback:    dnsRollbackText(obs.VanityDNS),
		})
	}

	// Phase 5 — cleanup of resources that are no longer reachable.
	if des.rule == nil && obs.TunnelIngress != nil {
		steps = append(steps, Step{
			Kind:        StepIngressRemove,
			Hostname:    svc.Hostname,
			Rule:        cloneRule(obs.TunnelIngress),
			Description: fmt.Sprintf("remove the tunnel ingress for %s", svc.Hostname),
			Rollback:    fmt.Sprintf("republish %s through the tunnel to %s", svc.Hostname, obs.TunnelIngress.Origin),
		})
	}
	if des.app == nil && obs.AccessApp != nil {
		steps = append(steps, Step{
			Kind:        StepAccessRemove,
			Hostname:    svc.Hostname,
			App:         cloneApp(obs.AccessApp),
			Description: fmt.Sprintf("remove the Access identity gate from %s", svc.Hostname),
			Rollback:    fmt.Sprintf("recreate the Access gate on %s", svc.Hostname),
		})
	}
	return steps
}

// deleteSteps produces the teardown order: cut DNS reachability first, then
// remove the public path, the gate, the local route, and finally the anchor.
func (s *Service) deleteSteps(svc ServiceRecord, obs *Observation, des desiredResources) []Step {
	steps := make([]Step, 0, 5)

	vanity := cloneRecord(obs.VanityDNS)
	if vanity == nil {
		vanity = cloneRecord(&des.vanity)
	}
	steps = append(steps, Step{
		Kind:        StepDNSRemove,
		Hostname:    svc.Hostname,
		Record:      vanity,
		Description: fmt.Sprintf("remove vanity record %s, cutting public and private DNS reachability first", svc.Hostname),
		Rollback:    fmt.Sprintf("recreate CNAME %s -> %s", vanity.Name, vanity.Content),
	})

	rule := cloneRule(obs.TunnelIngress)
	if rule == nil && des.rule != nil {
		rule = cloneRule(des.rule)
	}
	if rule != nil {
		steps = append(steps, Step{
			Kind:        StepIngressRemove,
			Hostname:    svc.Hostname,
			Rule:        rule,
			Description: fmt.Sprintf("remove the tunnel ingress for %s", svc.Hostname),
			Rollback:    fmt.Sprintf("republish %s through the tunnel to %s", rule.Hostname, rule.Origin),
		})
	}

	app := cloneApp(obs.AccessApp)
	if app == nil && des.app != nil {
		app = cloneApp(des.app)
	}
	if app != nil {
		steps = append(steps, Step{
			Kind:        StepAccessRemove,
			Hostname:    svc.Hostname,
			App:         app,
			Description: fmt.Sprintf("remove the Access identity gate from %s", svc.Hostname),
			Rollback:    fmt.Sprintf("recreate the Access gate on %s", svc.Hostname),
		})
	}

	route := cloneRoute(obs.EdgeRoute)
	if route == nil {
		route = cloneRoute(&des.route)
	}
	steps = append(steps, Step{
		Kind:        StepEdgeRemove,
		Hostname:    svc.Hostname,
		Route:       route,
		Description: fmt.Sprintf("remove the local edge route for %s", svc.Hostname),
		Rollback:    fmt.Sprintf("restore edge route %s -> %s", route.Hostname, route.Upstream),
	})

	anchor := cloneRecord(obs.AnchorDNS)
	if anchor == nil {
		anchor = cloneRecord(&des.anchor)
	}
	steps = append(steps, Step{
		Kind:        StepDNSRemove,
		Hostname:    svc.Hostname,
		Record:      anchor,
		Description: fmt.Sprintf("remove the private anchor %s", svc.HomeAnchor),
		Rollback:    fmt.Sprintf("recreate A %s -> %s", anchor.Name, anchor.Content),
	})
	return steps
}

// planWarnings returns operator-facing warnings for the desired exposure and
// whether an explicit acknowledgement is required before Apply.
func (s *Service) planWarnings(svc ServiceRecord) ([]string, bool) {
	var warnings []string
	switch svc.Exposure {
	case domain.ExposurePublic:
		warnings = append(warnings, fmt.Sprintf("%s will resolve to the Cloudflare tunnel and be reachable from the internet without an identity gate", svc.Hostname))
		if svc.AppAuth == AuthNative {
			warnings = append(warnings, "the service relies on its own application-level authentication; verify it before applying")
		} else {
			warnings = append(warnings, "the service does not authenticate users itself; an explicit acknowledgement (AcknowledgePublic) is required before Apply")
			return warnings, true
		}
	case domain.ExposureShared:
		warnings = append(warnings, fmt.Sprintf("%s will be published through the Cloudflare tunnel behind an Access identity gate (%s)", svc.Hostname, strings.Join(s.cfg.sharedGroups(), ", ")))
	case domain.ExposurePrivate:
		// Private is the default posture; no warning.
	}
	return warnings, false
}

// checkOrdering enforces the safety invariants that make accidental public
// exposure structurally impossible in a generated plan:
//
//   - DNS may point at the tunnel only after the tunnel path exists and, for
//     shared, after the identity gate exists.
//   - Returning to private must repoint DNS before the public path or the
//     gate is removed.
//   - The gate must not be removed before a DNS transition in the same plan.
//
// It runs on every generated exposure plan before the plan is persisted; a
// violation is a planner bug, not a user error.
func (s *Service) checkOrdering(svc ServiceRecord, from, to domain.Exposure, obs *Observation, steps []Step) error {
	vanity := svc.Hostname
	gateEnsure, vanityTunnel, vanityAnchor := -1, -1, -1
	ingressEnsure, ingressRemove, gateRemove := -1, -1, -1
	for i, st := range steps {
		switch st.Kind {
		case StepDNSEnsure:
			if st.Record != nil && strings.EqualFold(st.Record.Name, vanity) {
				if st.Record.Content == s.cfg.TunnelDNS && vanityTunnel < 0 {
					vanityTunnel = i
				}
				if st.Record.Content == svc.HomeAnchor && vanityAnchor < 0 {
					vanityAnchor = i
				}
			}
		case StepAccessEnsure:
			if st.App != nil && strings.EqualFold(st.App.Hostname, vanity) && gateEnsure < 0 {
				gateEnsure = i
			}
		case StepAccessRemove:
			if st.App != nil && strings.EqualFold(st.App.Hostname, vanity) && gateRemove < 0 {
				gateRemove = i
			}
		case StepIngressEnsure:
			if st.Rule != nil && strings.EqualFold(st.Rule.Hostname, vanity) && ingressEnsure < 0 {
				ingressEnsure = i
			}
		case StepIngressRemove:
			if st.Rule != nil && strings.EqualFold(st.Rule.Hostname, vanity) && ingressRemove < 0 {
				ingressRemove = i
			}
		}
	}
	if vanityTunnel >= 0 {
		if obs.TunnelIngress == nil && (ingressEnsure < 0 || ingressEnsure > vanityTunnel) {
			return fmt.Errorf("exposure: unsafe plan order: tunnel ingress must be created before %s points at the tunnel", vanity)
		}
		if to == domain.ExposureShared && obs.AccessApp == nil && (gateEnsure < 0 || gateEnsure > vanityTunnel) {
			return fmt.Errorf("exposure: unsafe plan order: the identity gate must be created before %s points at the tunnel", vanity)
		}
	}
	if to == domain.ExposurePrivate && from != "" && from != domain.ExposurePrivate {
		if vanityAnchor < 0 {
			return fmt.Errorf("exposure: unsafe plan order: returning %s to private must repoint the vanity record at %s first", vanity, svc.HomeAnchor)
		}
		if ingressRemove >= 0 && ingressRemove < vanityAnchor {
			return fmt.Errorf("exposure: unsafe plan order: tunnel ingress removal must follow the vanity repoint")
		}
		if gateRemove >= 0 && gateRemove < vanityAnchor {
			return fmt.Errorf("exposure: unsafe plan order: identity gate removal must follow the vanity repoint")
		}
	}
	if from == domain.ExposureShared && to == domain.ExposurePublic && vanityTunnel >= 0 && gateRemove >= 0 && gateRemove < vanityTunnel {
		return fmt.Errorf("exposure: unsafe plan order: DNS must settle before the identity gate is removed")
	}
	return nil
}

// requireClients verifies that every scope the steps depend on is configured,
// keeping the scoped token boundary visible at operation time.
func (s *Service) requireClients(steps []Step) error {
	seen := map[string]bool{}
	for _, st := range steps {
		if sc := stepScope(st.Kind); sc != "" {
			seen[sc] = true
		}
	}
	scopes := make([]string, 0, len(seen))
	for sc := range seen {
		scopes = append(scopes, sc)
	}
	missing := s.clients.missingScopes(scopes...)
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("%w: %s", ErrMissingClient, strings.Join(missing, ", "))
	}
	return nil
}

// computeDrift lists differences between desired and observed state. Families
// whose client scope is not configured are reported as unknown, never as
// "absent".
func (s *Service) computeDrift(svc ServiceRecord, obs *Observation, missing []string) []string {
	unknown := make(map[string]bool, len(missing))
	drift := []string{}
	for _, sc := range missing {
		unknown[sc] = true
		drift = append(drift, fmt.Sprintf("live state unknown: %s client not configured", sc))
	}
	des := s.desiredFor(svc)
	if !unknown[ScopeDNS] {
		if obs.VanityDNS == nil {
			drift = append(drift, fmt.Sprintf("vanity CNAME %s is missing", svc.Hostname))
		} else if !recordMatches(obs.VanityDNS, &des.vanity) {
			drift = append(drift, fmt.Sprintf("vanity CNAME %s points at %s (proxied=%t), want %s", svc.Hostname, obs.VanityDNS.Content, obs.VanityDNS.Proxied, des.vanity.Content))
		}
		if obs.AnchorDNS == nil {
			drift = append(drift, fmt.Sprintf("anchor A %s is missing", svc.HomeAnchor))
		} else if !recordMatches(obs.AnchorDNS, &des.anchor) {
			drift = append(drift, fmt.Sprintf("anchor A %s points at %s, want %s", svc.HomeAnchor, obs.AnchorDNS.Content, des.anchor.Content))
		}
	}
	if !unknown[ScopeTunnel] {
		switch {
		case des.rule == nil && obs.TunnelIngress != nil:
			drift = append(drift, fmt.Sprintf("tunnel ingress for %s exists but desired exposure is %s", svc.Hostname, svc.Exposure))
		case des.rule != nil && obs.TunnelIngress == nil:
			drift = append(drift, fmt.Sprintf("tunnel ingress for %s is missing", svc.Hostname))
		case des.rule != nil && obs.TunnelIngress != nil && obs.TunnelIngress.Origin != des.rule.Origin:
			drift = append(drift, fmt.Sprintf("tunnel ingress for %s targets %s, want %s", svc.Hostname, obs.TunnelIngress.Origin, des.rule.Origin))
		}
	}
	if !unknown[ScopeAccess] {
		switch {
		case des.app == nil && obs.AccessApp != nil:
			drift = append(drift, fmt.Sprintf("Access gate on %s exists but desired exposure is %s", svc.Hostname, svc.Exposure))
		case des.app != nil && obs.AccessApp == nil:
			drift = append(drift, fmt.Sprintf("Access gate on %s is missing", svc.Hostname))
		case des.app != nil && obs.AccessApp != nil && !accessAppsEqual(obs.AccessApp, des.app):
			drift = append(drift, fmt.Sprintf("Access gate on %s has different policies", svc.Hostname))
		}
	}
	if !unknown[ScopeEdge] {
		if obs.EdgeRoute == nil {
			drift = append(drift, fmt.Sprintf("edge route for %s is missing", svc.Hostname))
		} else if obs.EdgeRoute.Upstream != des.route.Upstream {
			drift = append(drift, fmt.Sprintf("edge route for %s targets %s, want %s", svc.Hostname, obs.EdgeRoute.Upstream, des.route.Upstream))
		}
	}
	return drift
}

// recordMatches compares two records. Proxied records always report the
// "auto" TTL upstream, so TTL is only compared for DNS-only records.
func recordMatches(a, b *Record) bool {
	if a == nil || b == nil {
		return false
	}
	if a.Name != b.Name || a.Type != b.Type || a.Content != b.Content || a.Proxied != b.Proxied {
		return false
	}
	return b.Proxied || a.TTL == b.TTL
}

// accessAppsEqual compares gates by name, hostname, and policies.
func accessAppsEqual(a, b *AccessApp) bool {
	if a == nil || b == nil {
		return false
	}
	if a.Name != b.Name || a.Hostname != b.Hostname || len(a.Policies) != len(b.Policies) {
		return false
	}
	for i := range a.Policies {
		if a.Policies[i].Name != b.Policies[i].Name {
			return false
		}
		if !sameStrings(a.Policies[i].Include, b.Policies[i].Include) {
			return false
		}
	}
	return true
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	sa := append([]string(nil), a...)
	sb := append([]string(nil), b...)
	sort.Strings(sa)
	sort.Strings(sb)
	for i := range sa {
		if sa[i] != sb[i] {
			return false
		}
	}
	return true
}

func cloneRecord(r *Record) *Record {
	if r == nil {
		return nil
	}
	c := *r
	return &c
}

func cloneRule(r *IngressRule) *IngressRule {
	if r == nil {
		return nil
	}
	c := *r
	return &c
}

func cloneRoute(r *Route) *Route {
	if r == nil {
		return nil
	}
	c := *r
	return &c
}

func cloneApp(a *AccessApp) *AccessApp {
	if a == nil {
		return nil
	}
	c := *a
	c.Policies = append([]AccessPolicy(nil), a.Policies...)
	for i := range c.Policies {
		c.Policies[i].Include = append([]string(nil), a.Policies[i].Include...)
	}
	return &c
}

func dnsRollbackText(prev *Record) string {
	if prev == nil {
		return "remove the record again"
	}
	return fmt.Sprintf("restore %s %s -> %s (proxied=%t)", prev.Name, prev.Type, prev.Content, prev.Proxied)
}

func routeRollbackText(prev *Route) string {
	if prev == nil {
		return "remove the edge route again"
	}
	return fmt.Sprintf("restore edge route %s -> %s", prev.Hostname, prev.Upstream)
}

func accessRollbackText(prev *AccessApp, hostname string) string {
	if prev == nil {
		return fmt.Sprintf("remove the Access gate on %s again", hostname)
	}
	return fmt.Sprintf("restore the previous Access gate on %s", hostname)
}

func ruleRollbackText(prev *IngressRule, hostname string) string {
	if prev == nil {
		return fmt.Sprintf("remove the tunnel ingress for %s again", hostname)
	}
	return fmt.Sprintf("restore tunnel ingress %s -> %s", prev.Hostname, prev.Origin)
}
