package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/omahab/omahab/internal/edge"
	"github.com/omahab/omahab/internal/exposure"
	"github.com/omahab/omahab/internal/store"
)

// ErrCaddyUnavailable is returned when the Caddy admin API is not reachable.
var ErrCaddyUnavailable = errors.New("caddy admin unavailable")

type edgeClient struct {
	baseURL    string
	httpClient *http.Client
	domain     string
	dnsToken   string
	configPath string
}

func newEdgeClient(baseURL string, httpClient *http.Client, domain, dnsToken, configPath string) *edgeClient {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if configPath == "" {
		configPath = "/var/lib/omahab/caddy/caddy.json"
	}
	return &edgeClient{
		baseURL:    baseURL,
		httpClient: httpClient,
		domain:     strings.TrimSpace(domain),
		dnsToken:   strings.TrimSpace(dnsToken),
		configPath: configPath,
	}
}

func (c *edgeClient) do(ctx context.Context, method, path string, body any, out any) error {
	url := c.baseURL + path
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reqBody = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Detect connection refused / unavailable
		if strings.Contains(err.Error(), "connect: connection refused") || strings.Contains(err.Error(), "no such host") || strings.Contains(err.Error(), "connection refused") {
			return fmt.Errorf("%w: %v", ErrCaddyUnavailable, err)
		}
		return fmt.Errorf("%w: %v", ErrCaddyUnavailable, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return mapHTTPStatus(resp.StatusCode, string(b))
	}
	if out != nil && len(b) > 0 {
		if err := json.Unmarshal(b, out); err != nil {
			return fmt.Errorf("edge: decode response: %w", err)
		}
	}
	return nil
}

func (c *edgeClient) ListRoutes(ctx context.Context) ([]exposure.Route, error) {
	var raw map[string]any
	err := c.do(ctx, http.MethodGet, "/config/", nil, &raw)
	if err != nil {
		if errors.Is(err, ErrCaddyUnavailable) {
			// Caddy admin down: the persisted caddy.json on disk is the
			// last state we applied.
			return c.routesFromFile()
		}
		if errors.Is(err, store.ErrNotFound) || strings.Contains(err.Error(), "404") {
			return []exposure.Route{}, nil
		}
		return nil, err
	}
	// Parse raw config to extract routes with @id prefix "omahab-"
	routes := []exposure.Route{}
	// Navigate raw["apps"]["http"]["servers"]["main"]["routes"]
	if apps, ok := raw["apps"].(map[string]any); ok {
		if httpCfg, ok := apps["http"].(map[string]any); ok {
			if servers, ok := httpCfg["servers"].(map[string]any); ok {
				if mainSrv, ok := servers["main"].(map[string]any); ok {
					if rlist, ok := mainSrv["routes"].([]any); ok {
						for _, r := range rlist {
							rm, ok := r.(map[string]any)
							if !ok {
								continue
							}
							id, _ := rm["@id"].(string)
							if !strings.HasPrefix(id, "omahab-") {
								continue
							}
							hostname := strings.TrimPrefix(id, "omahab-")
							// Also try to get host from match.host[0] if id not reliable
							if hostFromMatch := extractHost(rm); hostFromMatch != "" {
								hostname = hostFromMatch
							}
							upstream := extractUpstream(rm)
							if hostname == "" || upstream == "" {
								continue
							}
							// Upstream dial is like "host:port"; reconstruct upstream URL
							if !strings.HasPrefix(upstream, "http://") && !strings.HasPrefix(upstream, "https://") {
								upstream = "http://" + upstream
							}
							routes = append(routes, exposure.Route{Hostname: hostname, Upstream: upstream})
						}
					}
				}
			}
		}
	}
	if routes == nil {
		routes = []exposure.Route{}
	}
	return routes, nil
}

func extractHost(m map[string]any) string {
	if match, ok := m["match"].([]any); ok && len(match) > 0 {
		if first, ok := match[0].(map[string]any); ok {
			if hosts, ok := first["host"].([]any); ok && len(hosts) > 0 {
				if h, ok := hosts[0].(string); ok {
					return h
				}
			}
			// sometimes host is string slice parsed as []string
			if hosts, ok := first["host"].([]string); ok && len(hosts) > 0 {
				return hosts[0]
			}
		}
	}
	return ""
}

func extractUpstream(m map[string]any) string {
	if handles, ok := m["handle"].([]any); ok && len(handles) > 0 {
		if h, ok := handles[0].(map[string]any); ok {
			if ups, ok := h["upstreams"].([]any); ok && len(ups) > 0 {
				if up, ok := ups[0].(map[string]any); ok {
					if dial, ok := up["dial"].(string); ok {
						return dial
					}
				}
			}
		}
	}
	return ""
}

func (c *edgeClient) routesFromFile() ([]exposure.Route, error) {
	data, err := os.ReadFile(c.configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return []exposure.Route{}, nil
		}
		return nil, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return []exposure.Route{}, nil
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	routes := []exposure.Route{}
	if apps, ok := raw["apps"].(map[string]any); ok {
		if httpCfg, ok := apps["http"].(map[string]any); ok {
			if servers, ok := httpCfg["servers"].(map[string]any); ok {
				if mainSrv, ok := servers["main"].(map[string]any); ok {
					if rlist, ok := mainSrv["routes"].([]any); ok {
						for _, r := range rlist {
							rm, ok := r.(map[string]any)
							if !ok {
								continue
							}
							id, _ := rm["@id"].(string)
							if !strings.HasPrefix(id, "omahab-") {
								continue
							}
							hostname := strings.TrimPrefix(id, "omahab-")
							if hostFromMatch := extractHost(rm); hostFromMatch != "" {
								hostname = hostFromMatch
							}
							upstream := extractUpstream(rm)
							if hostname == "" || upstream == "" {
								continue
							}
							if !strings.HasPrefix(upstream, "http://") && !strings.HasPrefix(upstream, "https://") {
								upstream = "http://" + upstream
							}
							routes = append(routes, exposure.Route{Hostname: hostname, Upstream: upstream})
						}
					}
				}
			}
		}
	}
	if routes == nil {
		routes = []exposure.Route{}
	}
	return routes, nil
}

func (c *edgeClient) PutRoute(ctx context.Context, route exposure.Route) error {
	if strings.TrimSpace(route.Hostname) == "" || strings.TrimSpace(route.Upstream) == "" {
		return fmt.Errorf("%w: hostname and upstream are required", store.ErrValidation)
	}
	routes, err := c.ListRoutes(ctx)
	if err != nil {
		if errors.Is(err, ErrCaddyUnavailable) {
			return err
		}
		// Treat not found / empty as no existing routes
		routes = []exposure.Route{}
	}
	return c.putRouteViaConfigWithRoutes(ctx, routes, route)
}

func (c *edgeClient) putRouteViaConfigWithRoutes(ctx context.Context, existing []exposure.Route, route exposure.Route) error {
	// Upsert
	found := false
	for i, r := range existing {
		if strings.EqualFold(r.Hostname, route.Hostname) {
			existing[i] = route
			found = true
			break
		}
	}
	if !found {
		existing = append(existing, route)
	}
	return c.applyRoutes(ctx, existing)
}

func (c *edgeClient) DeleteRoute(ctx context.Context, hostname string) error {
	if strings.TrimSpace(hostname) == "" {
		return fmt.Errorf("%w: hostname is required", store.ErrValidation)
	}
	routes, err := c.ListRoutes(ctx)
	if err != nil {
		if errors.Is(err, ErrCaddyUnavailable) {
			return err
		}
		if strings.Contains(err.Error(), "404") {
			return nil
		}
		return err
	}
	return c.deleteRouteViaConfigWithRoutes(ctx, routes, hostname)
}

func (c *edgeClient) deleteRouteViaConfigWithRoutes(ctx context.Context, existing []exposure.Route, hostname string) error {
	filtered := make([]exposure.Route, 0, len(existing))
	for _, r := range existing {
		if !strings.EqualFold(r.Hostname, hostname) {
			filtered = append(filtered, r)
		}
	}
	// Idempotent: if nothing removed, still apply? No need, but ensure file reflects current
	if len(filtered) == len(existing) {
		// Check if hostname was present; if not, idempotent success
		found := false
		for _, r := range existing {
			if strings.EqualFold(r.Hostname, hostname) {
				found = true
				break
			}
		}
		if !found {
			return nil
		}
	}
	return c.applyRoutes(ctx, filtered)
}

func (c *edgeClient) applyRoutes(ctx context.Context, routes []exposure.Route) error {
	// Render Caddy JSON (the DNS-01 token lives in caddy's environment, not
	// in the config; see edge.RenderConfig).
	cfgBytes, err := edge.RenderConfig(c.domain, routes)
	if err != nil {
		return err
	}
	// Write to configPath with 0600
	dir := filepath.Dir(c.configPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("edge: mkdir %s: %w", dir, err)
	}
	if err := os.WriteFile(c.configPath, cfgBytes, 0o600); err != nil {
		return fmt.Errorf("edge: write %s: %w", c.configPath, err)
	}
	// POST /load with same JSON
	url := c.baseURL + "/load"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(cfgBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		if errors.Is(err, ErrCaddyUnavailable) || strings.Contains(err.Error(), "connection refused") || strings.Contains(err.Error(), "no such host") || strings.Contains(err.Error(), "connect: connection refused") {
			return nil
		}
		return nil
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return mapHTTPStatus(resp.StatusCode, string(b))
	}
	return nil
}
