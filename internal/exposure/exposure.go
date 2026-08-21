// Package exposure implements private/shared/public desired-state
// reconciliation for Cloudflare DNS, Cloudflare Tunnel, Cloudflare Access,
// and the local Caddy edge (DESIGN.md section 7).
//
// Model:
//
//   - Every service has exactly one desired exposure: private, shared, or
//     public. New services start private (DESIGN.md principle 3).
//   - Private: the vanity hostname is a DNS-only CNAME to a stable
//     "<host>.home.<domain>" anchor, which is an A record pointing at the
//     server's Tailscale IP. The anchor never changes with exposure, so
//     flipping back and forth touches only the vanity record.
//   - Shared: the vanity CNAME points at the Cloudflare tunnel (proxied) and
//     an Access application gates the hostname behind identity.
//   - Public: the vanity CNAME points at the tunnel with no identity gate.
//     Making an unauthenticated service public requires an explicit
//     acknowledgement bound to the current desired-state revision; there is
//     no silent private-to-public fallback.
//
// Reconciliation is plan-based and persisted:
//
//   - Plan observes live state first, then computes an ordered step list.
//     Each changing step carries a previous-state snapshot so Apply can roll
//     it back, and a human-readable description plus rollback description so
//     the plan is inspectable before anything runs.
//   - Apply executes steps in order, records per-step results, and rolls back
//     completed steps (using the snapshots) when a step fails.
//   - Ordering invariants are enforced before a plan is persisted: the
//     identity gate and tunnel path are created before DNS points at the
//     tunnel, and DNS is repointed away from the tunnel before public paths
//     are removed.
//
// The package owns its SQLite tables (see Migrations) and depends only on the
// narrow client interfaces in clients.go, never on SDK types.
package exposure

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/store"
)

const (
	// defaultTunnelOrigin forwards tunnel traffic to the local Caddy edge,
	// which routes by Host header to the service upstream.
	defaultTunnelOrigin = "http://127.0.0.1:80"
	// dnsTTL is the TTL used for DNS-only records. Proxied records ignore
	// their TTL upstream.
	dnsTTL = 300
)

// Config carries the instance-wide values reconciliation targets.
type Config struct {
	// Domain is the apex domain, for example "example.com".
	Domain string
	// TailscaleIP is the server's stable tailnet address, for example
	// "100.64.0.5". Private anchors point here.
	TailscaleIP string
	// TunnelDNS is the tunnel target for shared/public vanity records, for
	// example "<tunnel-id>.cfargotunnel.com". Such records must be proxied
	// for Cloudflare to route them through the tunnel.
	TunnelDNS string
	// SharedAccessGroups holds the identity selectors allowed through the
	// shared identity gate. Defaults to ["group:members"].
	SharedAccessGroups []string
}

func (c Config) validate() error {
	if err := validateHostname(c.Domain); err != nil {
		return store.Validationf("domain: %v", err)
	}
	if c.TailscaleIP == "" {
		return store.Validationf("tailscale IP is required")
	}
	if net.ParseIP(c.TailscaleIP) == nil {
		return store.Validationf("tailscale IP %q is not an IP address", c.TailscaleIP)
	}
	if err := validateHostname(c.TunnelDNS); err != nil {
		return store.Validationf("tunnel DNS target: %v", err)
	}
	return nil
}

// sharedGroups returns the identity selectors for the shared gate.
func (c Config) sharedGroups() []string {
	if len(c.SharedAccessGroups) == 0 {
		return []string{"group:members"}
	}
	return c.SharedAccessGroups
}

// AuthMode describes whether a service authenticates users itself.
type AuthMode string

const (
	// AuthNone: the application performs no authentication of its own.
	AuthNone AuthMode = "none"
	// AuthNative: the application authenticates users itself (for example
	// through native OIDC), so public exposure does not remove the only
	// gate.
	AuthNative AuthMode = "native"
)

func (a AuthMode) Valid() bool { return a == AuthNone || a == AuthNative }

// ServiceRecord is the persisted desired state of one exposed service.
type ServiceRecord struct {
	ID           string          `json:"id"`
	Hostname     string          `json:"hostname"`      // vanity, e.g. ai.example.com
	HomeAnchor   string          `json:"home_anchor"`   // stable private anchor, e.g. ai.home.example.com
	Upstream     string          `json:"upstream"`      // local service address for the edge route
	TunnelOrigin string          `json:"tunnel_origin"` // where cloudflared forwards to
	Exposure     domain.Exposure `json:"exposure"`      // desired exposure
	AppAuth      AuthMode        `json:"app_auth"`      // application-level authentication
	Revision     int64           `json:"revision"`      // desired-state revision; invalidates plans and acks
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
}

// Observation is the persisted observed state of one service: what actually
// exists in DNS, the tunnel, Access, and the local edge right now.
type Observation struct {
	ServiceID     string       `json:"service_id"`
	ObservedAt    time.Time    `json:"observed_at"`
	VanityDNS     *Record      `json:"vanity_dns,omitempty"`
	AnchorDNS     *Record      `json:"anchor_dns,omitempty"`
	TunnelIngress *IngressRule `json:"tunnel_ingress,omitempty"`
	AccessApp     *AccessApp   `json:"access_app,omitempty"`
	EdgeRoute     *Route       `json:"edge_route,omitempty"`
	Reconciled    bool         `json:"reconciled"`
	Drift         []string     `json:"drift"`
	Error         string       `json:"error,omitempty"`
	PlanID        string       `json:"plan_id,omitempty"`
}

// Service is the exposure controller.
type Service struct {
	db      *store.Store
	cfg     Config
	clients Clients
}

// New creates the exposure controller. clients may be partially configured;
// operations that would need an absent scope fail with ErrMissingClient.
func New(db *store.Store, cfg Config, clients Clients) (*Service, error) {
	if db == nil {
		return nil, store.Validationf("store is required")
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &Service{db: db, cfg: cfg, clients: clients}, nil
}

// validateHostname accepts lowercase DNS hostnames with valid labels.
func validateHostname(name string) error {
	if name == "" {
		return errors.New("hostname is empty")
	}
	if len(name) > 253 {
		return fmt.Errorf("hostname %q is longer than 253 characters", name)
	}
	if strings.ToLower(name) != name {
		return fmt.Errorf("hostname %q must be lowercase", name)
	}
	labels := strings.Split(name, ".")
	if len(labels) < 2 {
		return fmt.Errorf("hostname %q needs at least two labels", name)
	}
	for _, label := range labels {
		if label == "" {
			return fmt.Errorf("hostname %q has an empty label", name)
		}
		if len(label) > 63 {
			return fmt.Errorf("hostname label %q is longer than 63 characters", label)
		}
		for i := range len(label) {
			c := label[i]
			switch {
			case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			case c == '-' && i > 0 && i < len(label)-1:
			default:
				return fmt.Errorf("hostname label %q contains invalid character %q", label, string(rune(c)))
			}
		}
	}
	return nil
}

// homeAnchor derives the stable private anchor for a vanity hostname by
// inserting the "home" label after the host label (DESIGN.md 7.2):
// ai.example.com -> ai.home.example.com.
func homeAnchor(hostname, domain string) (string, error) {
	suffix := "." + domain
	if !strings.HasSuffix(hostname, suffix) {
		return "", fmt.Errorf("hostname %q is not under domain %q", hostname, domain)
	}
	prefix := strings.TrimSuffix(hostname, suffix)
	if prefix == "" {
		return "", fmt.Errorf("hostname must be a subdomain of %q, not the apex", domain)
	}
	return prefix + ".home." + domain, nil
}
