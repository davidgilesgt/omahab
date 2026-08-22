package cloudflare

import (
	"fmt"
	"net/http"

	cloudflare "github.com/cloudflare/cloudflare-go/v2"
	"github.com/cloudflare/cloudflare-go/v2/option"
	"github.com/omahab/omahab/internal/exposure"
)

// Options configures Cloudflare clients. One token per scope; never a global
// API key. Empty token → nil client field, not an error (the scoped boundary
// stays visible at plan time via exposure.ErrMissingClient).
type Options struct {
	APITokenDNS    string
	APITokenTunnel string
	APITokenAccess string

	// ZoneID is the Cloudflare zone identifier for the apex domain (required
	// when APITokenDNS or email routing is used).
	ZoneID string

	// AccountID is the Cloudflare account identifier (required when
	// APITokenTunnel or APITokenAccess is used).
	AccountID string

	// TunnelID is the Cloudflare tunnel identifier (required when
	// APITokenTunnel is used).
	TunnelID string

	// HTTPClient is an injectable HTTP client for tests. If nil,
	// http.DefaultClient is used.
	HTTPClient *http.Client

	// BaseURL overrides the Cloudflare API base URL (e.g. httptest server).
	// Empty means https://api.cloudflare.com/client/v4/.
	BaseURL string

	// CaddyAddr is the Caddy admin endpoint for the local edge, e.g.
	// http://127.0.0.1:2019. Empty → Edge client is nil.
	CaddyAddr string
}

func newCFClient(token string, httpClient *http.Client, baseURL string) *cloudflare.Client {
	opts := []option.RequestOption{
		option.WithAPIToken(token),
		option.WithMaxRetries(0),
	}
	if httpClient != nil {
		opts = append(opts, option.WithHTTPClient(httpClient))
	}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	return cloudflare.NewClient(opts...)
}

// NewClients builds exposure.Clients with one scoped client per token.
// Empty token → nil field, not an error. Missing Zone/Account/Tunnel IDs when
// the corresponding token is set is an error (the client would be unusable).
func NewClients(o Options) (exposure.Clients, error) {
	var c exposure.Clients

	if o.APITokenDNS != "" {
		if o.ZoneID == "" {
			return exposure.Clients{}, fmt.Errorf("cloudflare: ZoneID required when APITokenDNS is set")
		}
		cf := newCFClient(o.APITokenDNS, o.HTTPClient, o.BaseURL)
		c.DNS = newDNSClient(cf, o.ZoneID)
	}

	if o.APITokenTunnel != "" {
		if o.AccountID == "" || o.TunnelID == "" {
			return exposure.Clients{}, fmt.Errorf("cloudflare: AccountID and TunnelID required when APITokenTunnel is set")
		}
		cf := newCFClient(o.APITokenTunnel, o.HTTPClient, o.BaseURL)
		c.Tunnel = newTunnelClient(cf, o.AccountID, o.TunnelID)
	}

	if o.APITokenAccess != "" {
		if o.AccountID == "" {
			return exposure.Clients{}, fmt.Errorf("cloudflare: AccountID required when APITokenAccess is set")
		}
		cf := newCFClient(o.APITokenAccess, o.HTTPClient, o.BaseURL)
		c.Access = newAccessClient(cf, o.AccountID)
	}

	if o.CaddyAddr != "" {
		c.Edge = newEdgeClient(o.CaddyAddr, o.HTTPClient)
	}

	return c, nil
}

// Ensure exposure interfaces are satisfied.
var (
	_ exposure.DNSClient    = (*dnsClient)(nil)
	_ exposure.TunnelClient = (*tunnelClient)(nil)
	_ exposure.AccessClient = (*accessClient)(nil)
	_ exposure.EdgeClient   = (*edgeClient)(nil)
)
