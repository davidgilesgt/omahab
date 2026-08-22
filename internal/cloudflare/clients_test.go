package cloudflare

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/omahab/omahab/internal/exposure"
	"github.com/omahab/omahab/internal/store"
)

func TestNewClients_EmptyTokens(t *testing.T) {
	clients, err := NewClients(Options{})
	if err != nil {
		t.Fatalf("NewClients empty: %v", err)
	}
	if clients.DNS != nil || clients.Tunnel != nil || clients.Access != nil || clients.Edge != nil {
		t.Fatalf("expected all nil clients for empty tokens, got %+v", clients)
	}
}

func TestNewClients_ScopedTokens(t *testing.T) {
	if _, err := NewClients(Options{APITokenDNS: "tok", ZoneID: ""}); err == nil {
		t.Fatalf("expected error for missing ZoneID")
	}
	if _, err := NewClients(Options{APITokenTunnel: "tok", AccountID: "acc"}); err == nil {
		t.Fatalf("expected error for missing TunnelID")
	}
	if _, err := NewClients(Options{APITokenAccess: "tok"}); err == nil {
		t.Fatalf("expected error for missing AccountID")
	}
	clients, err := NewClients(Options{
		APITokenDNS:    "dns-token",
		ZoneID:         "zone-1",
		APITokenTunnel: "tun-token",
		AccountID:      "acc-1",
		TunnelID:       "tun-1",
		APITokenAccess: "acc-token",
		CaddyAddr:      "http://127.0.0.1:2019",
	})
	if err != nil {
		t.Fatalf("NewClients valid: %v", err)
	}
	if clients.DNS == nil || clients.Tunnel == nil || clients.Access == nil || clients.Edge == nil {
		t.Fatalf("expected all clients non-nil, got dns=%v tun=%v acc=%v edge=%v", clients.DNS != nil, clients.Tunnel != nil, clients.Access != nil, clients.Edge != nil)
	}
	clients, err = NewClients(Options{APITokenDNS: "", ZoneID: "zone-1"})
	if err != nil {
		t.Fatalf("empty token should not error: %v", err)
	}
	if clients.DNS != nil {
		t.Fatalf("expected DNS nil for empty token")
	}
}

func TestClients_InjectableHTTPClient(t *testing.T) {
	called := false
	rt := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		called = true
		body := `{"result":[],"success":true,"errors":[],"messages":[],"result_info":{"page":1,"per_page":20}}`
		rec := httptest.NewRecorder()
		rec.Header().Set("Content-Type", "application/json")
		rec.WriteHeader(200)
		rec.Body.WriteString(body)
		return rec.Result(), nil
	})
	httpClient := &http.Client{Transport: rt}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{"result":[],"success":true,"errors":[],"messages":[],"result_info":{"page":1,"per_page":20}}`))
	}))
	defer srv.Close()
	clients, err := NewClients(Options{
		APITokenDNS: "tok",
		ZoneID:      "zone-1",
		HTTPClient:  httpClient,
		BaseURL:     srv.URL + "/",
	})
	if err != nil {
		t.Fatalf("NewClients: %v", err)
	}
	_, err = clients.DNS.ListRecords(context.Background())
	if err != nil {
		t.Fatalf("ListRecords: %v", err)
	}
	if !called {
		t.Fatalf("injected HTTP client was not used")
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestDNSClient(t *testing.T) {
	tests := []struct {
		name string
		fn   func(t *testing.T, client exposure.DNSClient, srv *httptest.Server)
	}{
		{
			name: "ListRecords",
			fn: func(t *testing.T, client exposure.DNSClient, srv *httptest.Server) {
				records, err := client.ListRecords(context.Background())
				if err != nil {
					t.Fatalf("ListRecords: %v", err)
				}
				if len(records) != 2 {
					t.Fatalf("expected 2 records, got %d", len(records))
				}
				if records[0].Name != "ai.example.com" || records[0].Type != "CNAME" {
					t.Fatalf("unexpected record %+v", records[0])
				}
			},
		},
		{
			name: "CreateRecord",
			fn: func(t *testing.T, client exposure.DNSClient, srv *httptest.Server) {
				id, err := client.CreateRecord(context.Background(), exposure.Record{
					Name: "ai.example.com", Type: "CNAME", Content: "ai.home.example.com", TTL: 300, Proxied: true,
				})
				if err != nil {
					t.Fatalf("CreateRecord: %v", err)
				}
				if id != "new-id" {
					t.Fatalf("expected new-id, got %q", id)
				}
			},
		},
		{
			name: "ReplaceRecord",
			fn: func(t *testing.T, client exposure.DNSClient, srv *httptest.Server) {
				err := client.ReplaceRecord(context.Background(), "rec-123", exposure.Record{
					Name: "ai.example.com", Type: "CNAME", Content: "tun.cfargotunnel.com", TTL: 1, Proxied: true,
				})
				if err != nil {
					t.Fatalf("ReplaceRecord: %v", err)
				}
			},
		},
		{
			name: "DeleteRecord",
			fn: func(t *testing.T, client exposure.DNSClient, srv *httptest.Server) {
				if err := client.DeleteRecord(context.Background(), "rec-123"); err != nil {
					t.Fatalf("DeleteRecord: %v", err)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var handler http.HandlerFunc
			switch tc.name {
			case "ListRecords":
				handler = func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodGet || !strings.Contains(r.URL.Path, "dns_records") {
						t.Errorf("ListRecords: unexpected %s %s", r.Method, r.URL.Path)
					}
					if ah := r.Header.Get("Authorization"); !strings.Contains(ah, "Bearer") {
						t.Errorf("missing auth header %q", ah)
					}
					w.Header().Set("Content-Type", "application/json")
					json.NewEncoder(w).Encode(map[string]any{
						"result": []map[string]any{
							{"id": "1", "name": "ai.example.com", "type": "CNAME", "content": "ai.home.example.com", "proxied": false, "ttl": 300},
							{"id": "2", "name": "ai.home.example.com", "type": "A", "content": "100.64.0.1", "proxied": false, "ttl": 300},
						},
						"success": true, "errors": []any{}, "messages": []any{},
						"result_info": map[string]any{"page": 1, "per_page": 20},
					})
				}
			case "CreateRecord":
				handler = func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodPost || !strings.Contains(r.URL.Path, "dns_records") {
						t.Errorf("Create: unexpected %s %s", r.Method, r.URL.Path)
					}
					var body map[string]any
					json.NewDecoder(r.Body).Decode(&body)
					if body["name"] != "ai.example.com" || body["type"] != "CNAME" {
						t.Errorf("Create body mismatch %+v", body)
					}
					w.Header().Set("Content-Type", "application/json")
					json.NewEncoder(w).Encode(map[string]any{
						"result": map[string]any{"id": "new-id", "name": "ai.example.com", "type": "CNAME", "content": "ai.home.example.com"},
						"success": true, "errors": []any{}, "messages": []any{},
					})
				}
			case "ReplaceRecord":
				handler = func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodPut || !strings.Contains(r.URL.Path, "dns_records/rec-123") {
						t.Errorf("Replace: unexpected %s %s", r.Method, r.URL.Path)
					}
					w.Header().Set("Content-Type", "application/json")
					json.NewEncoder(w).Encode(map[string]any{
						"result": map[string]any{"id": "rec-123"}, "success": true, "errors": []any{}, "messages": []any{},
					})
				}
			case "DeleteRecord":
				handler = func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodDelete || !strings.Contains(r.URL.Path, "dns_records/rec-123") {
						t.Errorf("Delete: unexpected %s %s", r.Method, r.URL.Path)
					}
					w.Header().Set("Content-Type", "application/json")
					json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"id": "rec-123"}, "success": true})
				}
			}
			srv := httptest.NewServer(handler)
			defer srv.Close()
			clients, err := NewClients(Options{
				APITokenDNS: "dns-token",
				ZoneID:      "zone-123",
				BaseURL:     srv.URL + "/",
			})
			if err != nil {
				t.Fatalf("NewClients: %v", err)
			}
			tc.fn(t, clients.DNS, srv)
		})
	}
}

func TestTunnelClient(t *testing.T) {
	t.Run("ListIngress", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet || !strings.Contains(r.URL.Path, "cfd_tunnel") {
				t.Errorf("ListIngress: %s %s", r.Method, r.URL.Path)
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"result": map[string]any{
					"config": map[string]any{
						"ingress": []map[string]any{
							{"hostname": "ai.example.com", "service": "http://127.0.0.1:80"},
							{"hostname": "git.example.com", "service": "http://127.0.0.1:8080"},
							{"service": "http_status:404"},
						},
					},
				},
				"success": true,
			})
		}))
		defer srv.Close()
		clients, _ := NewClients(Options{APITokenTunnel: "tok", AccountID: "acc", TunnelID: "tun", BaseURL: srv.URL + "/"})
		rules, err := clients.Tunnel.ListIngress(context.Background())
		if err != nil {
			t.Fatalf("ListIngress: %v", err)
		}
		if len(rules) != 2 {
			t.Fatalf("expected 2 rules, got %v", rules)
		}
		if rules[0].Hostname != "ai.example.com" || rules[0].Origin != "http://127.0.0.1:80" {
			t.Fatalf("unexpected rule %+v", rules[0])
		}
	})
	t.Run("SetIngress", func(t *testing.T) {
		var captured map[string]any
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPut || !strings.Contains(r.URL.Path, "cfd_tunnel") {
				t.Errorf("SetIngress: %s %s", r.Method, r.URL.Path)
			}
			json.NewDecoder(r.Body).Decode(&captured)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"success": true})
		}))
		defer srv.Close()
		clients, _ := NewClients(Options{APITokenTunnel: "tok", AccountID: "acc", TunnelID: "tun", BaseURL: srv.URL + "/"})
		err := clients.Tunnel.SetIngress(context.Background(), []exposure.IngressRule{
			{Hostname: "ai.example.com", Origin: "http://127.0.0.1:80"},
		})
		if err != nil {
			t.Fatalf("SetIngress: %v", err)
		}
		cfg, ok := captured["config"].(map[string]any)
		if !ok {
			t.Fatalf("missing config %+v", captured)
		}
		ing, ok := cfg["ingress"].([]any)
		if !ok || len(ing) != 2 {
			t.Fatalf("expected 2 ingress (rule+catch-all), got %+v", cfg["ingress"])
		}
		found404 := false
		for _, v := range ing {
			if m, ok := v.(map[string]any); ok && m["service"] == "http_status:404" {
				found404 = true
			}
		}
		if !found404 {
			t.Fatalf("catch-all not preserved")
		}
	})
}

func TestAccessClient(t *testing.T) {
	t.Run("GetApplication", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet || !strings.Contains(r.URL.Path, "access/apps") {
				t.Errorf("GetApplication List: %s %s", r.Method, r.URL.Path)
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"result": []map[string]any{
					{
						"id": "app-1", "name": "ai", "domain": "ai.example.com", "type": "self_hosted",
						"policies": []map[string]any{
							{"name": "allow", "include": []map[string]any{{"group": map[string]any{"name": "members"}}}},
						},
					},
				},
				"success": true,
			})
		}))
		defer srv.Close()
		clients, _ := NewClients(Options{APITokenAccess: "tok", AccountID: "acc", BaseURL: srv.URL + "/"})
		app, err := clients.Access.GetApplication(context.Background(), "ai.example.com")
		if err != nil {
			t.Fatalf("GetApplication: %v", err)
		}
		if app.ID != "app-1" || app.Hostname != "ai.example.com" {
			t.Fatalf("unexpected app %+v", app)
		}
		if len(app.Policies) != 1 || len(app.Policies[0].Include) != 1 || app.Policies[0].Include[0] != "group:members" {
			t.Fatalf("policy include mismatch %+v", app.Policies)
		}
	})
	t.Run("GetApplicationNotFound", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"result": []any{}, "success": true})
		}))
		defer srv.Close()
		clients, _ := NewClients(Options{APITokenAccess: "tok", AccountID: "acc", BaseURL: srv.URL + "/"})
		_, err := clients.Access.GetApplication(context.Background(), "missing.example.com")
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("expected ErrNotFound, got %v", err)
		}
	})
	t.Run("PutApplicationCreate", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				t.Errorf("expected POST, got %s", r.Method)
			}
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if body["domain"] != "new.example.com" {
				t.Errorf("domain mismatch %+v", body)
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"id": "new-app", "name": "new", "domain": "new.example.com"}, "success": true})
		}))
		defer srv.Close()
		clients, _ := NewClients(Options{APITokenAccess: "tok", AccountID: "acc", BaseURL: srv.URL + "/"})
		id, err := clients.Access.PutApplication(context.Background(), exposure.AccessApp{Name: "new", Hostname: "new.example.com", Policies: []exposure.AccessPolicy{{Name: "allow", Include: []string{"group:members"}}}})
		if err != nil {
			t.Fatalf("PutApplication: %v", err)
		}
		if id != "new-app" {
			t.Fatalf("expected new-app, got %q", id)
		}
	})
	t.Run("PutApplicationUpdate", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPut || !strings.Contains(r.URL.Path, "app-1") {
				t.Errorf("expected PUT app-1, got %s %s", r.Method, r.URL.Path)
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"id": "app-1"}, "success": true})
		}))
		defer srv.Close()
		clients, _ := NewClients(Options{APITokenAccess: "tok", AccountID: "acc", BaseURL: srv.URL + "/"})
		id, err := clients.Access.PutApplication(context.Background(), exposure.AccessApp{ID: "app-1", Name: "ai", Hostname: "ai.example.com"})
		if err != nil {
			t.Fatalf("PutApplication update: %v", err)
		}
		if id != "app-1" {
			t.Fatalf("expected app-1, got %q", id)
		}
	})
	t.Run("DeleteApplication", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodDelete || !strings.Contains(r.URL.Path, "app-1") {
				t.Errorf("Delete: %s %s", r.Method, r.URL.Path)
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"success": true})
		}))
		defer srv.Close()
		clients, _ := NewClients(Options{APITokenAccess: "tok", AccountID: "acc", BaseURL: srv.URL + "/"})
		if err := clients.Access.DeleteApplication(context.Background(), "app-1"); err != nil {
			t.Fatalf("DeleteApplication: %v", err)
		}
	})
}

func TestEdgeClient(t *testing.T) {
	t.Run("ListRoutes", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet || r.URL.Path != "/routes" {
				t.Errorf("ListRoutes: %s %s", r.Method, r.URL.Path)
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]exposure.Route{{Hostname: "ai.example.com", Upstream: "http://127.0.0.1:3000"}})
		}))
		defer srv.Close()
		clients, _ := NewClients(Options{CaddyAddr: srv.URL})
		routes, err := clients.Edge.ListRoutes(context.Background())
		if err != nil {
			t.Fatalf("ListRoutes: %v", err)
		}
		if len(routes) != 1 || routes[0].Hostname != "ai.example.com" {
			t.Fatalf("unexpected routes %+v", routes)
		}
	})
	t.Run("PutRoute", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPut || r.URL.Path != "/routes/ai.example.com" {
				t.Errorf("PutRoute: %s %s", r.Method, r.URL.Path)
			}
			var body exposure.Route
			json.NewDecoder(r.Body).Decode(&body)
			if body.Upstream != "http://127.0.0.1:3000" {
				t.Errorf("body mismatch %+v", body)
			}
			w.WriteHeader(204)
		}))
		defer srv.Close()
		clients, _ := NewClients(Options{CaddyAddr: srv.URL})
		if err := clients.Edge.PutRoute(context.Background(), exposure.Route{Hostname: "ai.example.com", Upstream: "http://127.0.0.1:3000"}); err != nil {
			t.Fatalf("PutRoute: %v", err)
		}
	})
	t.Run("DeleteRoute", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodDelete || r.URL.Path != "/routes/ai.example.com" {
				t.Errorf("DeleteRoute: %s %s", r.Method, r.URL.Path)
			}
			w.WriteHeader(204)
		}))
		defer srv.Close()
		clients, _ := NewClients(Options{CaddyAddr: srv.URL})
		if err := clients.Edge.DeleteRoute(context.Background(), "ai.example.com"); err != nil {
			t.Fatalf("DeleteRoute: %v", err)
		}
	})
	t.Run("DeleteRouteNotFoundIdempotent", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(404)
			w.Write([]byte("not found"))
		}))
		defer srv.Close()
		clients, _ := NewClients(Options{CaddyAddr: srv.URL})
		if err := clients.Edge.DeleteRoute(context.Background(), "missing.example.com"); err != nil {
			t.Fatalf("DeleteRoute 404 should be nil, got %v", err)
		}
	})
}

func TestEmailClient_EnsureAndDelete(t *testing.T) {
	t.Run("EnsureCreates", func(t *testing.T) {
		calls := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "email/routing/rules") {
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{"result": []any{}, "success": true})
				return
			}
			if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "email/routing/rules") {
				var body map[string]any
				json.NewDecoder(r.Body).Decode(&body)
				actions, _ := body["actions"].([]any)
				if len(actions) != 1 {
					t.Errorf("expected 1 action, got %v", actions)
				}
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"id": "rule-1"}, "success": true})
				return
			}
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}))
		defer srv.Close()
		ec, _ := NewEmailClient(EmailOptions{APIToken: "tok", ZoneID: "zone-1", BaseURL: srv.URL + "/"})
		if err := ec.EnsureEmailRoute(context.Background(), "ai@example.com", "ingest@example.net"); err != nil {
			t.Fatalf("EnsureEmailRoute: %v", err)
		}
		if calls != 2 {
			t.Fatalf("expected 2 calls (list+create), got %d", calls)
		}
	})
	t.Run("EnsureIdempotent", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{
					"result": []map[string]any{{
						"id": "rule-1", "name": "omahab-ai", "enabled": true,
						"matchers": []map[string]any{{"field": "to", "type": "literal", "value": "ai@example.com"}},
						"actions": []map[string]any{{"type": "forward", "value": []string{"ingest@example.net"}}},
					}},
					"success": true,
				})
				return
			}
			t.Errorf("unexpected create/update, should be idempotent: %s %s", r.Method, r.URL.Path)
		}))
		defer srv.Close()
		ec, _ := NewEmailClient(EmailOptions{APIToken: "tok", ZoneID: "zone-1", BaseURL: srv.URL + "/"})
		if err := ec.EnsureEmailRoute(context.Background(), "ai@example.com", "ingest@example.net"); err != nil {
			t.Fatalf("Ensure idempotent: %v", err)
		}
	})
	t.Run("EnsureUpdatesMismatchedDestination", func(t *testing.T) {
		updated := false
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{
					"result": []map[string]any{{
						"id": "rule-1", "name": "omahab-ai", "enabled": true,
						"matchers": []map[string]any{{"field": "to", "type": "literal", "value": "ai@example.com"}},
						"actions": []map[string]any{{"type": "forward", "value": []string{"old@example.net"}}},
					}},
					"success": true,
				})
				return
			}
			if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "rule-1") {
				updated = true
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"id": "rule-1"}, "success": true})
				return
			}
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}))
		defer srv.Close()
		ec, _ := NewEmailClient(EmailOptions{APIToken: "tok", ZoneID: "zone-1", BaseURL: srv.URL + "/"})
		if err := ec.EnsureEmailRoute(context.Background(), "ai@example.com", "ingest@example.net"); err != nil {
			t.Fatalf("Ensure update: %v", err)
		}
		if !updated {
			t.Fatalf("expected update to be called")
		}
	})
	t.Run("DeleteExisting", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{
					"result": []map[string]any{{
						"id": "rule-1",
						"matchers": []map[string]any{{"field": "to", "type": "literal", "value": "ai@example.com"}},
						"actions": []map[string]any{{"type": "forward", "value": []string{"ingest@example.net"}}},
					}},
					"success": true,
				})
				return
			}
			if r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "rule-1") {
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{"success": true})
				return
			}
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}))
		defer srv.Close()
		ec, _ := NewEmailClient(EmailOptions{APIToken: "tok", ZoneID: "zone-1", BaseURL: srv.URL + "/"})
		if err := ec.DeleteEmailRoute(context.Background(), "ai@example.com"); err != nil {
			t.Fatalf("DeleteEmailRoute: %v", err)
		}
	})
	t.Run("DeleteNotFoundIdempotent", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{"result": []any{}, "success": true})
				return
			}
			t.Errorf("unexpected delete when not found: %s %s", r.Method, r.URL.Path)
		}))
		defer srv.Close()
		ec, _ := NewEmailClient(EmailOptions{APIToken: "tok", ZoneID: "zone-1", BaseURL: srv.URL + "/"})
		if err := ec.DeleteEmailRoute(context.Background(), "missing@example.com"); err != nil {
			t.Fatalf("Delete not found should be nil, got %v", err)
		}
	})
	t.Run("EnsureAIRouteAlias", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{"result": []any{}, "success": true})
				return
			}
			if r.Method == http.MethodPost {
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"id": "rule-1"}, "success": true})
				return
			}
		}))
		defer srv.Close()
		ec, _ := NewEmailClient(EmailOptions{APIToken: "tok", ZoneID: "zone-1", BaseURL: srv.URL + "/"})
		if err := ec.EnsureAIRoute(context.Background(), "alias@example.com", "worker@example.net"); err != nil {
			t.Fatalf("EnsureAIRoute: %v", err)
		}
		if err := ec.DeleteAIRoute(context.Background(), "alias@example.com"); err != nil {
			t.Fatalf("DeleteAIRoute: %v", err)
		}
	})
}

func TestErrorMapping(t *testing.T) {
	statusTests := []struct {
		status int
		check  func(error) bool
		name   string
	}{
		{401, func(err error) bool { return errors.Is(err, ErrUnauthorized) }, "401 unauthorized"},
		{403, func(err error) bool { return errors.Is(err, ErrForbidden) }, "403 forbidden"},
		{404, func(err error) bool { return errors.Is(err, store.ErrNotFound) }, "404 not found"},
		{429, func(err error) bool { return errors.Is(err, ErrRateLimited) }, "429 rate limited"},
	}
	for _, st := range statusTests {
		t.Run("DNS "+st.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(st.status)
				json.NewEncoder(w).Encode(map[string]any{
					"success": false, "errors": []map[string]any{{"code": st.status, "message": "error"}}, "messages": []any{},
				})
			}))
			defer srv.Close()
			clients, _ := NewClients(Options{APITokenDNS: "tok", ZoneID: "zone", BaseURL: srv.URL + "/"})
			_, err := clients.DNS.ListRecords(context.Background())
			if err == nil || !st.check(err) {
				t.Fatalf("DNS ListRecords status %d: expected typed error, got %v", st.status, err)
			}
		})
		t.Run("Tunnel "+st.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(st.status)
				json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []map[string]any{{"code": st.status, "message": "error"}}})
			}))
			defer srv.Close()
			clients, _ := NewClients(Options{APITokenTunnel: "tok", AccountID: "acc", TunnelID: "tun", BaseURL: srv.URL + "/"})
			_, err := clients.Tunnel.ListIngress(context.Background())
			if err == nil || !st.check(err) {
				t.Fatalf("Tunnel ListIngress status %d: expected typed error, got %v", st.status, err)
			}
		})
		t.Run("Access "+st.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(st.status)
				json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []map[string]any{{"code": st.status, "message": "error"}}})
			}))
			defer srv.Close()
			clients, _ := NewClients(Options{APITokenAccess: "tok", AccountID: "acc", BaseURL: srv.URL + "/"})
			_, err := clients.Access.GetApplication(context.Background(), "ai.example.com")
			if st.status == 404 {
				if !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("Access Get 404: expected ErrNotFound, got %v", err)
				}
			} else {
				if err == nil || !st.check(err) {
					t.Fatalf("Access Get status %d: expected typed error, got %v", st.status, err)
				}
			}
		})
		t.Run("Edge "+st.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(st.status)
				w.Write([]byte("error"))
			}))
			defer srv.Close()
			clients, _ := NewClients(Options{CaddyAddr: srv.URL})
			_, err := clients.Edge.ListRoutes(context.Background())
			if st.status == 404 {
				if err != nil {
					t.Fatalf("Edge 404 should return empty list, not error, got %v", err)
				}
			} else {
				if err == nil || !st.check(err) {
					t.Fatalf("Edge ListRoutes status %d: expected typed error, got %v", st.status, err)
				}
			}
		})
		t.Run("Email "+st.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(st.status)
				json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []map[string]any{{"code": st.status, "message": "error"}}})
			}))
			defer srv.Close()
			ec, _ := NewEmailClient(EmailOptions{APIToken: "tok", ZoneID: "zone", BaseURL: srv.URL + "/"})
			err := ec.EnsureEmailRoute(context.Background(), "ai@example.com", "ingest@example.net")
			if err == nil || !st.check(err) {
				t.Fatalf("Email Ensure status %d: expected typed error, got %v", st.status, err)
			}
		})
	}
}

func TestNoLeak_SDKTypes(t *testing.T) {
	var _ exposure.DNSClient = (*dnsClient)(nil)
	var _ exposure.TunnelClient = (*tunnelClient)(nil)
	var _ exposure.AccessClient = (*accessClient)(nil)
	var _ exposure.EdgeClient = (*edgeClient)(nil)
}
