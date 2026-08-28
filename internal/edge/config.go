package edge

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/omahab/omahab/internal/exposure"
)

// RenderConfig produces the full Caddy JSON configuration for the given domain,
// DNS token, and routes. The token is embedded directly (0600 on host, ro in
// container) as required by the catalog note — never via Docker labels.
// It generates apps.http.servers.main listening on :443 (+ :80) with one route
// per entry (@id "omahab-"+hostname, reverse_proxy to upstream) and a single
// TLS automation policy using Cloudflare DNS-01.
func RenderConfig(domain string, dnsToken string, routes []exposure.Route) ([]byte, error) {
	_ = domain // reserved for future default hostname; routes already carry FQDNs
	if strings.TrimSpace(dnsToken) == "" {
		return nil, fmt.Errorf("edge: dns token is required for Caddy TLS automation")
	}
	// Build Caddy routes
	caddyRoutes := make([]any, 0, len(routes))
	for _, r := range routes {
		hostname := strings.TrimSpace(r.Hostname)
		upstream := strings.TrimSpace(r.Upstream)
		if hostname == "" || upstream == "" {
			continue
		}
		// Normalize upstream: strip scheme if present for dial (Caddy expects host:port)
		// Keep as provided if it already looks like host:port; if scheme present, extract host.
		dial := upstream
		if strings.HasPrefix(dial, "http://") {
			dial = strings.TrimPrefix(dial, "http://")
		} else if strings.HasPrefix(dial, "https://") {
			dial = strings.TrimPrefix(dial, "https://")
		}
		// Remove trailing slash
		dial = strings.TrimSuffix(dial, "/")
		entry := map[string]any{
			"@id":  "omahab-" + hostname,
			"match": []any{map[string]any{"host": []string{hostname}}},
			"handle": []any{
				map[string]any{
					"handler": "reverse_proxy",
					"upstreams": []any{
						map[string]any{"dial": dial},
					},
				},
			},
		}
		caddyRoutes = append(caddyRoutes, entry)
	}

	// If no routes, still produce valid config with empty routes slice
	if caddyRoutes == nil {
		caddyRoutes = []any{}
	}

	cfg := map[string]any{
		"apps": map[string]any{
			"http": map[string]any{
				"servers": map[string]any{
					"main": map[string]any{
						"listen": []string{":443", ":80"},
						"routes": caddyRoutes,
					},
				},
			},
			"tls": map[string]any{
				"automation": map[string]any{
					"policies": []any{
						map[string]any{
							"issuers": []any{
								map[string]any{
									"module": "acme",
									"challenges": map[string]any{
										"dns": map[string]any{
											"provider": map[string]any{
												"name":      "cloudflare",
												"api_token": dnsToken,
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("edge: marshal caddy config: %w", err)
	}
	return out, nil
}
