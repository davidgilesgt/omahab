package edge

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/omahab/omahab/internal/exposure"
)

// RenderPrivateConfig produces Caddy JSON for private-only (Cloudflare-optional) mode.
// It uses Tailscale MagicDNS + tailscale cert (Caddy tls get_certificate tailscale)
// and exposes the dashboard + apps on stable ports on a single hostname
// omahab.<tailnet>.ts.net. When tailnetHost is empty, it falls back to "omahab.ts.net".
// Routes are the same as exposure routes but hostname is replaced with tailnetHost
// and each app is bound to a stable port (dashboard 443, photos 8443, docs 8444, save 8445, etc.).
// For now we generate a single server on :443 for the dashboard and additional
// servers per app port so Caddy can terminate TLS via Tailscale.
func RenderPrivateConfig(tailnetHost string, routes []exposure.Route) ([]byte, error) {
	tailnetHost = strings.TrimSpace(tailnetHost)
	if tailnetHost == "" {
		tailnetHost = "omahab.ts.net"
	}
	// Normalize: ensure no scheme, no trailing dot
	tailnetHost = strings.TrimPrefix(tailnetHost, "https://")
	tailnetHost = strings.TrimPrefix(tailnetHost, "http://")
	tailnetHost = strings.TrimSuffix(tailnetHost, "/")

	// Stable port map for known apps (from plan B4: photos 8443, docs 8444, etc.)
	portMap := map[string]string{
		"photos":   ":8443",
		"paperless": ":8444", // docs
		"docs":     ":8444",
		"karakeep": ":8445", // save
		"save":     ":8445",
		"immich":   ":8443",
		"forgejo":  ":8446", // git
		"git":      ":8446",
		"woodpecker": ":8447", // ci
		"ci":       ":8447",
		"hermes":   ":8448", // ai
		"ai":       ":8448",
	}

	// Build servers: one per port + main for dashboard
	servers := map[string]any{}

	// Main server for dashboard/home on :443
	var mainRoutes []any
	for _, r := range routes {
		hostname := strings.TrimSpace(r.Hostname)
		if hostname == "" {
			continue
		}
		// In private mode, replace domain hostname with tailnetHost for dashboard only?
		// For apps, we keep port-based routing, so we don't need host match per app.
		// Simplified: main server handles tailnetHost -> dashboard upstream
		if strings.HasPrefix(hostname, "omahab.") || hostname == tailnetHost {
			upstream := strings.TrimSpace(r.Upstream)
			dial := upstream
			if strings.HasPrefix(dial, "http://") {
				dial = strings.TrimPrefix(dial, "http://")
			} else if strings.HasPrefix(dial, "https://") {
				dial = strings.TrimPrefix(dial, "https://")
			}
			dial = strings.TrimSuffix(dial, "/")
			mainRoutes = append(mainRoutes, map[string]any{
				"@id":  "omahab-" + hostname,
				"match": []any{map[string]any{"host": []string{tailnetHost}}},
				"handle": []any{map[string]any{
					"handler": "reverse_proxy",
					"upstreams": []any{map[string]any{"dial": dial}},
				}},
			})
		}
	}
	if len(mainRoutes) == 0 {
		// At minimum, expose dashboard on tailnetHost -> 127.0.0.1:3000 placeholder
		mainRoutes = []any{
			map[string]any{
				"match": []any{map[string]any{"host": []string{tailnetHost}}},
				"handle": []any{map[string]any{
					"handler": "reverse_proxy",
					"upstreams": []any{map[string]any{"dial": "127.0.0.1:3000"}},
				}},
			},
		}
	}
	servers["main"] = map[string]any{
		"listen": []string{":443", ":80"},
		"routes": mainRoutes,
		"tls_connection_policies": []any{
			map[string]any{"match": map[string]any{"sni": []string{tailnetHost}}},
		},
	}

	// Per-app port servers
	for app, port := range portMap {
		// Find upstream for this app from routes, if any
		var dial string
		for _, r := range routes {
			if strings.Contains(strings.ToLower(r.Hostname), app) {
				dial = r.Upstream
				break
			}
		}
		if dial == "" {
			continue
		}
		if strings.HasPrefix(dial, "http://") {
			dial = strings.TrimPrefix(dial, "http://")
		} else if strings.HasPrefix(dial, "https://") {
			dial = strings.TrimPrefix(dial, "https://")
		}
		dial = strings.TrimSuffix(dial, "/")
		srvName := "app-" + app
		servers[srvName] = map[string]any{
			"listen": []string{port},
			"routes": []any{
				map[string]any{
					"handle": []any{map[string]any{
						"handler": "reverse_proxy",
						"upstreams": []any{map[string]any{"dial": dial}},
					}},
				},
			},
		}
	}

	cfg := map[string]any{
		"apps": map[string]any{
			"http": map[string]any{
				"servers": servers,
			},
			"tls": map[string]any{
				"automation": map[string]any{
					"policies": []any{
						map[string]any{
							"subjects": []string{tailnetHost},
							"issuers": []any{
								map[string]any{"module": "tailscale"},
							},
						},
					},
				},
				// Caddy's "get_certificate tailscale" is represented as automation with tailscale issuer
				// and tls connection policy. The JSON above is sufficient for Caddy with caddy-tailscale plugin.
				// For vanilla Caddy, the same is achieved via "tls": {"get_certificate": "tailscale"} at server level.
			},
		},
	}
	// Also add tls.get_certificate hint for Caddy versions that expect it
	if httpApp, ok := cfg["apps"].(map[string]any)["http"].(map[string]any); ok {
		if serversMap, ok := httpApp["servers"].(map[string]any); ok {
			for _, srv := range serversMap {
				if m, ok := srv.(map[string]any); ok {
					// Add tls field to each server for get_certificate
					m["tls_connection_policies"] = []any{
						map[string]any{
							"match": map[string]any{"sni": []string{tailnetHost}},
						},
					}
				}
			}
		}
	}

	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("edge private marshal: %w", err)
	}
	return out, nil
}
