package cloudflare

import (
	"context"
	"fmt"
	"net/http"

	cloudflare "github.com/cloudflare/cloudflare-go/v2"
	"github.com/omahab/omahab/internal/exposure"
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
