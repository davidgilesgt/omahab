package exposure

import "context"

// Client scopes. Each scope maps to exactly one scoped credential held by one
// client implementation; Omahab never requests a global API key or an
// account-wide super-token (DESIGN.md 7.4). The official Cloudflare Go SDK is
// used only behind these boundaries; generated SDK types must not leak into
// the domain model (DESIGN.md 7.3).
const (
	// ScopeDNS identifies the zone-scoped DNS client (Zone / DNS / Edit).
	ScopeDNS = "dns"
	// ScopeTunnel identifies the tunnel-scoped client (Account / Cloudflare
	// Tunnel / Edit), bound to exactly one tunnel at construction time.
	ScopeTunnel = "tunnel"
	// ScopeAccess identifies the Access client (Account / Access: Apps and
	// Policies / Edit).
	ScopeAccess = "access"
	// ScopeEdge identifies the local Caddy admin client. It carries no
	// Cloudflare credential at all.
	ScopeEdge = "edge"
)

// Record is a DNS record expressed in Omahab terms, independent of any SDK.
type Record struct {
	ID      string `json:"id,omitempty"`
	Name    string `json:"name"`
	Type    string `json:"type"` // "A" or "CNAME"
	Content string `json:"content"`
	TTL     int    `json:"ttl"`
	Proxied bool   `json:"proxied"`
}

// DNSClient is the narrow DNS boundary for one Cloudflare zone. The record
// set is small (one vanity and one anchor record per service), so listing the
// zone is the read path.
type DNSClient interface {
	ListRecords(ctx context.Context) ([]Record, error)
	CreateRecord(ctx context.Context, rec Record) (id string, err error)
	ReplaceRecord(ctx context.Context, id string, rec Record) error
	DeleteRecord(ctx context.Context, id string) error
}

// IngressRule publishes one hostname through the tunnel to a local origin.
type IngressRule struct {
	Hostname string `json:"hostname"`
	Origin   string `json:"origin"`
}

// TunnelClient controls the ingress rules of exactly one Cloudflare tunnel.
// Implementations are constructed with the tunnel-scoped token and the tunnel
// ID, and must preserve ingress rules for hostnames this controller does not
// manage.
type TunnelClient interface {
	ListIngress(ctx context.Context) ([]IngressRule, error)
	SetIngress(ctx context.Context, rules []IngressRule) error
}

// AccessPolicy grants access to the identities matched by Include. Selectors
// are interpreted by the Access adapter, for example Pocket ID groups such
// as "group:members".
type AccessPolicy struct {
	Name    string   `json:"name"`
	Include []string `json:"include"`
}

// AccessApp is a Cloudflare Access application: the identity gate placed in
// front of a shared service.
type AccessApp struct {
	ID       string         `json:"id,omitempty"`
	Name     string         `json:"name"`
	Hostname string         `json:"hostname"`
	Policies []AccessPolicy `json:"policies"`
}

// AccessClient manages Access applications for one Cloudflare account.
// GetApplication returns store.ErrNotFound when no application covers hostname.
type AccessClient interface {
	GetApplication(ctx context.Context, hostname string) (*AccessApp, error)
	PutApplication(ctx context.Context, app AccessApp) (id string, err error)
	DeleteApplication(ctx context.Context, id string) error
}

// Route is a local Caddy route mapping a hostname to its upstream service.
type Route struct {
	Hostname string `json:"hostname"`
	Upstream string `json:"upstream"`
}

// EdgeClient manages local Caddy routes through Caddy's admin endpoint.
type EdgeClient interface {
	ListRoutes(ctx context.Context) ([]Route, error)
	PutRoute(ctx context.Context, route Route) error
	DeleteRoute(ctx context.Context, hostname string) error
}

// Clients bundles the external control boundaries used for reconciliation.
// Nil fields are allowed: plans that would touch an absent scope are rejected
// with ErrMissingClient instead of silently skipping steps, so the scoped
// token boundary stays visible per operation.
type Clients struct {
	DNS    DNSClient    // ScopeDNS
	Tunnel TunnelClient // ScopeTunnel
	Access AccessClient // ScopeAccess
	Edge   EdgeClient   // ScopeEdge
}

func (c Clients) has(scope string) bool {
	switch scope {
	case ScopeDNS:
		return c.DNS != nil
	case ScopeTunnel:
		return c.Tunnel != nil
	case ScopeAccess:
		return c.Access != nil
	case ScopeEdge:
		return c.Edge != nil
	}
	return false
}

// missingScopes lists which of the given scopes are not configured.
func (c Clients) missingScopes(scopes ...string) []string {
	var missing []string
	for _, sc := range scopes {
		if sc != "" && !c.has(sc) {
			missing = append(missing, sc)
		}
	}
	return missing
}

// stepScope maps a step kind to the client scope it depends on.
func stepScope(kind StepKind) string {
	switch kind {
	case StepDNSEnsure, StepDNSRemove:
		return ScopeDNS
	case StepAccessEnsure, StepAccessRemove:
		return ScopeAccess
	case StepIngressEnsure, StepIngressRemove:
		return ScopeTunnel
	case StepEdgeEnsure, StepEdgeRemove:
		return ScopeEdge
	}
	return ""
}
