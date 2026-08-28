package cloudflare

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	cloudflare "github.com/cloudflare/cloudflare-go/v2"
	"github.com/cloudflare/cloudflare-go/v2/option"
	"github.com/omahab/omahab/internal/exposure"
	"github.com/omahab/omahab/internal/store"
)

type tunnelClient struct {
	accountID string
	tunnelID  string
	client    *cloudflare.Client
}

func newTunnelClient(c *cloudflare.Client, accountID, tunnelID string) *tunnelClient {
	return &tunnelClient{accountID: accountID, tunnelID: tunnelID, client: c}
}

type tunnelConfigEnvelope struct {
	Result struct {
		Config struct {
			Ingress []struct {
				Hostname string `json:"hostname"`
				Service  string `json:"service"`
				Path     string `json:"path,omitempty"`
			} `json:"ingress"`
		} `json:"config"`
	} `json:"result"`
	Success bool `json:"success"`
}

func (c *tunnelClient) ListIngress(ctx context.Context) ([]exposure.IngressRule, error) {
	var env tunnelConfigEnvelope
	path := fmt.Sprintf("accounts/%s/cfd_tunnel/%s/configurations", c.accountID, c.tunnelID)
	if err := c.client.Execute(ctx, http.MethodGet, path, nil, &env); err != nil {
		return nil, mapAPIError(err)
	}
	out := make([]exposure.IngressRule, 0, len(env.Result.Config.Ingress))
	for _, ing := range env.Result.Config.Ingress {
		if ing.Hostname == "" {
			continue
		}
		if ing.Service == "http_status:404" {
			continue
		}
		out = append(out, exposure.IngressRule{
			Hostname: ing.Hostname,
			Origin:   ing.Service,
		})
	}
	return out, nil
}

func (c *tunnelClient) SetIngress(ctx context.Context, rules []exposure.IngressRule) error {
	ingress := make([]map[string]string, 0, len(rules)+1)
	for _, r := range rules {
		if r.Hostname == "" || r.Origin == "" {
			continue
		}
		ingress = append(ingress, map[string]string{
			"hostname": r.Hostname,
			"service":  r.Origin,
		})
	}
	ingress = append(ingress, map[string]string{"service": "http_status:404"})
	body := map[string]any{
		"config": map[string]any{
			"ingress": ingress,
		},
	}
	var env struct {
		Success bool `json:"success"`
	}
	path := fmt.Sprintf("accounts/%s/cfd_tunnel/%s/configurations", c.accountID, c.tunnelID)
	if err := c.client.Execute(ctx, http.MethodPut, path, body, &env); err != nil {
		return mapAPIError(err)
	}
	return nil
}

// CreateTunnel creates a new Cloudflare tunnel named name in the account
// configured on the client. It POSTs to /accounts/{account_id}/cfd_tunnel
// with {name:"omahab", config_src:"cloudflare"} and returns the tunnel id
// and token. It is intentionally on the concrete tunnelClient only, not on
// the narrow exposure.TunnelClient interface, so callers that already hold
// the account-scoped token can provision the tunnel before a TunnelID exists.
func (c *tunnelClient) CreateTunnel(ctx context.Context, name string) (id string, token string, err error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", "", fmt.Errorf("%w: tunnel name is required", store.ErrValidation)
	}
	if strings.TrimSpace(c.accountID) == "" {
		return "", "", fmt.Errorf("%w: accountID is required to create tunnel", store.ErrValidation)
	}
	if c.client == nil {
		return "", "", fmt.Errorf("cloudflare: tunnel client not configured")
	}
	body := map[string]any{
		"name":       name,
		"config_src": "cloudflare",
	}
	// Cloudflare envelope is flexible across API versions: id may be "id" or
	// nested, token may be "token" / "tunnel_token" / credentials file.
	var env struct {
		Success bool `json:"success"`
		Result  struct {
			ID          string `json:"id"`
			Token       string `json:"token"`
			TunnelToken string `json:"tunnel_token"`
			// Some API variants return credentials in a file-style object.
			Credentials struct {
				AccountTag string `json:"AccountTag"`
				TunnelID   string `json:"TunnelID"`
				TunnelName string `json:"TunnelName"`
				TunnelID2  string `json:"tunnel_id"`
				Token      string `json:"tunnel_token"`
			} `json:"credentials_file"`
		} `json:"result"`
		Errors   []struct{ Code int; Message string `json:"message"` } `json:"errors"`
		Messages []struct{ Code int; Message string `json:"message"` } `json:"messages"`
	}
	path := fmt.Sprintf("accounts/%s/cfd_tunnel", c.accountID)
	if err := c.client.Execute(ctx, http.MethodPost, path, body, &env); err != nil {
		return "", "", mapAPIError(err)
	}
	id = strings.TrimSpace(env.Result.ID)
	if id == "" {
		id = strings.TrimSpace(env.Result.Credentials.TunnelID)
	}
	if id == "" {
		id = strings.TrimSpace(env.Result.Credentials.TunnelID2)
	}
	token = strings.TrimSpace(env.Result.Token)
	if token == "" {
		token = strings.TrimSpace(env.Result.TunnelToken)
	}
	if token == "" {
		token = strings.TrimSpace(env.Result.Credentials.Token)
	}
	if id == "" {
		return "", "", fmt.Errorf("cloudflare: create tunnel returned empty id (result=%+v)", env.Result)
	}
	// Token may be empty for some account modes (remote config); return id anyway.
	return id, token, nil
}

// NewTunnelCreator returns a tunnelClient bound to accountID and token for
// the purpose of creating a tunnel before a tunnelID exists. The returned
// client is suitable for calling CreateTunnel only; ListIngress/SetIngress
// require a real tunnelID.
func NewTunnelCreator(accountID, token string, httpClient *http.Client, baseURL string) *tunnelClient {
	c := newCFClient(token, httpClient, baseURL)
	return newTunnelClient(c, accountID, "")
}

// Ensure tunnelCreator uses option import to avoid unused.
var _ = option.WithAPIToken
