package cloudflare

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/omahab/omahab/internal/edge"
	"github.com/omahab/omahab/internal/exposure"
)

func TestEdgeListRoutes_FileFallback(t *testing.T) {
	// Prepare a temp Caddy JSON with one omahab route
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "caddy.json")
	token := "test-token"
	domain := "example.test"
	initialRoute := exposure.Route{Hostname: "id.example.test", Upstream: "http://pocket-id:8080"}
	cfgBytes, err := edge.RenderConfig(domain, token, []exposure.Route{initialRoute})
	if err != nil {
		t.Fatalf("RenderConfig: %v", err)
	}
	if err := os.WriteFile(cfgPath, cfgBytes, 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Server that will be unavailable (closed immediately)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srvURL := srv.URL
	srv.Close() // now any request to srvURL will fail with connection refused
	clients, err := NewClients(Options{
		CaddyAddr:       srvURL,
		Domain:          domain,
		DNSToken:        token,
		CaddyConfigPath: cfgPath,
	})
	if err != nil {
		t.Fatalf("NewClients: %v", err)
	}
	routes, err := clients.Edge.ListRoutes(context.Background())
	if err != nil {
		t.Fatalf("ListRoutes fallback failed: %v", err)
	}
	if len(routes) != 1 || routes[0].Hostname != initialRoute.Hostname {
		t.Fatalf("fallback routes mismatch: got %+v want %+v", routes, initialRoute)
	}
}

func TestEdgeListRoutes_FileMissingReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "nonexistent", "caddy.json") // file missing
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srvURL := srv.URL
	srv.Close()
	clients, err := NewClients(Options{
		CaddyAddr:       srvURL,
		Domain:          "example.test",
		DNSToken:        "test-token",
		CaddyConfigPath: cfgPath,
	})
	if err != nil {
		t.Fatalf("NewClients: %v", err)
	}
	routes, err := clients.Edge.ListRoutes(context.Background())
	if err != nil {
		t.Fatalf("ListRoutes missing file should not error: %v", err)
	}
	if len(routes) != 0 {
		t.Fatalf("expected empty routes for missing file, got %+v", routes)
	}
}

func TestEdgeApplyRoutes_SucceedsWhenLoadUnavailable(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "caddy.json")
	// Create a server that will be closed to simulate Caddy admin unavailable
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// This handler should not be hit because we close server before PutRoute
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
	srvURL := srv.URL
	srv.Close()
	clients, err := NewClients(Options{
		CaddyAddr:       srvURL,
		Domain:          "example.test",
		DNSToken:        "test-token",
		CaddyConfigPath: cfgPath,
	})
	if err != nil {
		t.Fatalf("NewClients: %v", err)
	}
	route := exposure.Route{Hostname: "id.example.test", Upstream: "http://pocket-id:8080"}
	if err := clients.Edge.PutRoute(context.Background(), route); err != nil {
		t.Fatalf("PutRoute should succeed despite /load unavailable (file is truth): %v", err)
	}
	// Verify file was written and contains the route
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read caddy.json: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Check that file contains expected listen and route
	if _, ok := raw["apps"]; !ok {
		t.Fatalf("file missing apps: %s", string(data))
	}
	// Also verify ListRoutes file fallback can read it back (re-open with unavailable server)
	routes, err := clients.Edge.ListRoutes(context.Background())
	if err != nil {
		t.Fatalf("ListRoutes after apply: %v", err)
	}
	found := false
	for _, r := range routes {
		if r.Hostname == route.Hostname {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("applied route not found via file fallback: %v", routes)
	}
}
