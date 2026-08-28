package cloudflare

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
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

type tunnelListItem struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	ConfigSrc string  `json:"config_src"`
	DeletedAt *string `json:"deleted_at"`
	IsDeleted *bool   `json:"is_deleted"`
}

// EnsureTunnel ensures a Cloudflare tunnel named name exists in the account
// configured on the client. It is idempotent: it first lists existing tunnels
// via GET accounts/{accountID}/cfd_tunnel?name=<escaped>&is_deleted=false&per_page=100
// and filters exact active-name matches. One remotely-managed match is adopted
// via the token endpoint; no POST is issued. If no match exists it creates the
// tunnel with POST {name, config_src:"cloudflare"} and fetches the connector
// token if the create response omits it. A POST 409 race re-lists once and
// adopts the exact remotely-managed match. Multiple matches, empty IDs/tokens,
// or a conflicting non-remotely-managed tunnel return a redacted
// store.ErrConflict error without deleting or rotating any tunnel.
func (c *tunnelClient) EnsureTunnel(ctx context.Context, name string) (string, string, error) {
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

	matches, err := c.listTunnels(ctx, name)
	if err != nil {
		return "", "", err
	}

	switch len(matches) {
	case 0:
		// proceed to create
	case 1:
		m := matches[0]
		if m.ConfigSrc != "cloudflare" {
			return "", "", fmt.Errorf("%w: conflicting tunnel %q is local-managed; will not adopt or delete", store.ErrConflict, name)
		}
		if strings.TrimSpace(m.ID) == "" {
			return "", "", fmt.Errorf("%w: tunnel %q returned empty id", store.ErrConflict, name)
		}
		tok, err := c.fetchTunnelToken(ctx, m.ID)
		if err != nil {
			return "", "", err
		}
		if strings.TrimSpace(tok) == "" {
			return "", "", fmt.Errorf("%w: tunnel %q token is empty", store.ErrConflict, name)
		}
		return strings.TrimSpace(m.ID), strings.TrimSpace(tok), nil
	default:
		return "", "", fmt.Errorf("%w: multiple tunnels named %q exist; will not adopt or delete", store.ErrConflict, name)
	}

	// No match: create
	id, tok, err := c.createTunnel(ctx, name)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			// 409 race: re-list once and adopt exact remotely-managed match
			matches2, lerr := c.listTunnels(ctx, name)
			if lerr != nil {
				return "", "", lerr
			}
			if len(matches2) == 1 && matches2[0].ConfigSrc == "cloudflare" {
				if strings.TrimSpace(matches2[0].ID) == "" {
					return "", "", fmt.Errorf("%w: tunnel %q returned empty id after conflict", store.ErrConflict, name)
				}
				tok2, ferr := c.fetchTunnelToken(ctx, matches2[0].ID)
				if ferr != nil {
					return "", "", ferr
				}
				if strings.TrimSpace(tok2) == "" {
					return "", "", fmt.Errorf("%w: tunnel %q token is empty after conflict", store.ErrConflict, name)
				}
				return strings.TrimSpace(matches2[0].ID), strings.TrimSpace(tok2), nil
			}
			if len(matches2) == 0 {
				return "", "", fmt.Errorf("%w: tunnel %q conflict but no matching tunnel found on re-list", store.ErrConflict, name)
			}
			if len(matches2) > 1 {
				return "", "", fmt.Errorf("%w: multiple tunnels named %q exist after conflict", store.ErrConflict, name)
			}
			// single but local-managed
			return "", "", fmt.Errorf("%w: conflicting tunnel %q is local-managed after conflict", store.ErrConflict, name)
		}
		return "", "", err
	}
	if strings.TrimSpace(id) == "" {
		return "", "", fmt.Errorf("%w: tunnel %q returned empty id", store.ErrConflict, name)
	}
	// If create response omitted token, fetch it
	if strings.TrimSpace(tok) == "" {
		fetched, ferr := c.fetchTunnelToken(ctx, id)
		if ferr != nil {
			return "", "", ferr
		}
		if strings.TrimSpace(fetched) == "" {
			return "", "", fmt.Errorf("%w: tunnel %q token is empty", store.ErrConflict, name)
		}
		tok = fetched
	}
	if strings.TrimSpace(tok) == "" {
		return "", "", fmt.Errorf("%w: tunnel %q token is empty", store.ErrConflict, name)
	}
	return strings.TrimSpace(id), strings.TrimSpace(tok), nil
}

func (c *tunnelClient) listTunnels(ctx context.Context, name string) ([]tunnelListItem, error) {
	path := fmt.Sprintf("accounts/%s/cfd_tunnel?name=%s&is_deleted=false&per_page=100", c.accountID, url.QueryEscape(name))
	var env struct {
		Success bool            `json:"success"`
		Result  json.RawMessage `json:"result"`
	}
	if err := c.client.Execute(ctx, http.MethodGet, path, nil, &env); err != nil {
		return nil, mapAPIError(err)
	}
	if len(env.Result) == 0 || strings.TrimSpace(string(env.Result)) == "null" {
		return nil, nil
	}
	// Try array form (typical list)
	var arr []tunnelListItem
	if err := json.Unmarshal(env.Result, &arr); err == nil {
		filtered := make([]tunnelListItem, 0, len(arr))
		for _, it := range arr {
			if it.IsDeleted != nil && *it.IsDeleted {
				continue
			}
			if it.DeletedAt != nil {
				da := strings.TrimSpace(*it.DeletedAt)
				if da != "" && da != "null" {
					continue
				}
			}
			if strings.TrimSpace(it.Name) != name {
				continue
			}
			filtered = append(filtered, it)
		}
		return filtered, nil
	}
	// Try single object form
	var single tunnelListItem
	if err := json.Unmarshal(env.Result, &single); err == nil {
		if strings.TrimSpace(single.ID) == "" && strings.TrimSpace(single.Name) == "" {
			return nil, nil
		}
		if single.IsDeleted != nil && *single.IsDeleted {
			return nil, nil
		}
		if single.DeletedAt != nil {
			da := strings.TrimSpace(*single.DeletedAt)
			if da != "" && da != "null" {
				return nil, nil
			}
		}
		if strings.TrimSpace(single.Name) != name {
			return nil, nil
		}
		return []tunnelListItem{single}, nil
	}
	return nil, fmt.Errorf("cloudflare: list tunnel returned unexpected result")
}

func (c *tunnelClient) fetchTunnelToken(ctx context.Context, tunnelID string) (string, error) {
	tunnelID = strings.TrimSpace(tunnelID)
	if tunnelID == "" {
		return "", fmt.Errorf("%w: tunnel id is required for token fetch", store.ErrValidation)
	}
	path := fmt.Sprintf("accounts/%s/cfd_tunnel/%s/token", c.accountID, tunnelID)
	var env struct {
		Success bool            `json:"success"`
		Result  json.RawMessage `json:"result"`
	}
	if err := c.client.Execute(ctx, http.MethodGet, path, nil, &env); err != nil {
		return "", mapAPIError(err)
	}
	if len(env.Result) == 0 || strings.TrimSpace(string(env.Result)) == "null" {
		return "", fmt.Errorf("%w: tunnel token is empty", store.ErrConflict)
	}
	rawStr := strings.TrimSpace(string(env.Result))
	// Result may be a JSON string directly, e.g. "eyJ..."
	if len(rawStr) >= 2 && rawStr[0] == '"' && rawStr[len(rawStr)-1] == '"' {
		var s string
		if err := json.Unmarshal(env.Result, &s); err == nil {
			s = strings.TrimSpace(s)
			if s != "" {
				return s, nil
			}
			return "", fmt.Errorf("%w: tunnel token is empty", store.ErrConflict)
		}
	}
	// Result may be an object with token fields
	var obj struct {
		Token       string `json:"token"`
		TunnelToken string `json:"tunnel_token"`
	}
	if err := json.Unmarshal(env.Result, &obj); err == nil {
		tok := strings.TrimSpace(obj.Token)
		if tok == "" {
			tok = strings.TrimSpace(obj.TunnelToken)
		}
		if tok != "" {
			return tok, nil
		}
		// Try generic map for other casings
		var m map[string]json.RawMessage
		if err2 := json.Unmarshal(env.Result, &m); err2 == nil {
			for _, key := range []string{"token", "tunnel_token", "tunnelToken"} {
				if raw, ok := m[key]; ok {
					var s string
					if err3 := json.Unmarshal(raw, &s); err3 == nil {
						s = strings.TrimSpace(s)
						if s != "" {
							return s, nil
						}
					}
				}
			}
		}
	}
	// If object parsing failed and it wasn't a string, it's empty/malformed
	return "", fmt.Errorf("%w: tunnel token is empty", store.ErrConflict)
}

func (c *tunnelClient) createTunnel(ctx context.Context, name string) (string, string, error) {
	body := map[string]any{
		"name":       name,
		"config_src": "cloudflare",
	}
	var env struct {
		Success bool `json:"success"`
		Result  struct {
			ID          string `json:"id"`
			Token       string `json:"token"`
			TunnelToken string `json:"tunnel_token"`
			Credentials struct {
				AccountTag string `json:"AccountTag"`
				TunnelID   string `json:"TunnelID"`
				TunnelName string `json:"TunnelName"`
				TunnelID2  string `json:"tunnel_id"`
				Token      string `json:"tunnel_token"`
			} `json:"credentials_file"`
		} `json:"result"`
	}
	path := fmt.Sprintf("accounts/%s/cfd_tunnel", c.accountID)
	if err := c.client.Execute(ctx, http.MethodPost, path, body, &env); err != nil {
		return "", "", mapAPIError(err)
	}
	id := strings.TrimSpace(env.Result.ID)
	if id == "" {
		id = strings.TrimSpace(env.Result.Credentials.TunnelID)
	}
	if id == "" {
		id = strings.TrimSpace(env.Result.Credentials.TunnelID2)
	}
	token := strings.TrimSpace(env.Result.Token)
	if token == "" {
		token = strings.TrimSpace(env.Result.TunnelToken)
	}
	if token == "" {
		token = strings.TrimSpace(env.Result.Credentials.Token)
	}
	// Empty ID is returned without error so caller can handle without treating it as a 409 race.
	// Empty token is allowed here; caller will fetch via token endpoint.
	return id, token, nil
}

// NewTunnelCreator returns a tunnelClient bound to accountID and token for
// the purpose of provisioning a tunnel before a tunnelID exists. The returned
// client is suitable for calling EnsureTunnel only; ListIngress/SetIngress
// require a real tunnelID.
func NewTunnelCreator(accountID, token string, httpClient *http.Client, baseURL string) *tunnelClient {
	c := newCFClient(token, httpClient, baseURL)
	return newTunnelClient(c, accountID, "")
}

// Ensure tunnelCreator uses option import to avoid unused.
var _ = option.WithAPIToken
