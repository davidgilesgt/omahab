package cloudflare

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)
// the deployed path delivers ai@domain mail to omahab-email-worker, which
// HMAC-posts the raw message to omahabd. Forwarding to an email address is
// kept for non-worker destinations.
func TestEnsureEmailRoute_WorkerAction(t *testing.T) {
	var posts []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			json.NewEncoder(w).Encode(map[string]any{"success": true, "result": []any{}})
			return
		}
		if r.Method == http.MethodPost {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode body: %v", err)
			}
			posts = append(posts, body)
			json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]any{"id": "rule-1"}})
			return
		}
		t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	c, err := NewEmailClient(EmailOptions{APIToken: "tok", ZoneID: "zone", BaseURL: srv.URL + "/"})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.EnsureEmailRoute(context.Background(), "ai@example.com", "omahab-email-worker"); err != nil {
		t.Fatalf("ensure worker route: %v", err)
	}
	if len(posts) != 1 {
		t.Fatalf("posts = %d, want 1", len(posts))
	}
	actions, _ := posts[0]["actions"].([]any)
	if len(actions) != 1 {
		t.Fatalf("actions = %v", posts[0]["actions"])
	}
	action, _ := actions[0].(map[string]any)
	if action["type"] != "worker" {
		t.Fatalf("action type = %v, want worker", action["type"])
	}
	values, _ := action["value"].([]any)
	if len(values) != 1 || values[0] != "omahab-email-worker" {
		t.Fatalf("action value = %v", action["value"])
	}
}

// An existing Send-to-Worker rule is idempotent: no update is issued.
func TestEnsureEmailRoute_WorkerIdempotent(t *testing.T) {
	puts := 0
	posts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			json.NewEncoder(w).Encode(map[string]any{"success": true, "result": []map[string]any{
				{
					"id": "rule-1", "name": "omahab-ai", "enabled": true,
					"matchers": []map[string]any{{"field": "to", "type": "literal", "value": "ai@example.com"}},
					"actions":  []map[string]any{{"type": "worker", "value": []string{"omahab-email-worker"}}},
				},
			}})
		case http.MethodPost:
			posts++
			json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]any{"id": "rule-1"}})
		case http.MethodPut:
			puts++
			json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]any{"id": "rule-1"}})
		}
	}))
	defer srv.Close()
	c, err := NewEmailClient(EmailOptions{APIToken: "tok", ZoneID: "zone", BaseURL: srv.URL + "/"})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.EnsureEmailRoute(context.Background(), "ai@example.com", "omahab-email-worker"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if posts != 0 || puts != 0 {
		t.Fatalf("posts = %d puts = %d, want no writes", posts, puts)
	}
}

// A stale forward rule for the same recipient is migrated to the Worker.
func TestEnsureEmailRoute_MigratesForwardToWorker(t *testing.T) {
	var putBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			json.NewEncoder(w).Encode(map[string]any{"success": true, "result": []map[string]any{
				{
					"id": "rule-1", "name": "omahab-ai", "enabled": true,
					"matchers": []map[string]any{{"field": "to", "type": "literal", "value": "ai@example.com"}},
					"actions":  []map[string]any{{"type": "forward", "value": []string{"user@example.com"}}},
				},
			}})
			return
		}
		if r.Method == http.MethodPut {
			json.NewDecoder(r.Body).Decode(&putBody)
			json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]any{"id": "rule-1"}})
			return
		}
		t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	c, err := NewEmailClient(EmailOptions{APIToken: "tok", ZoneID: "zone", BaseURL: srv.URL + "/"})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.EnsureEmailRoute(context.Background(), "ai@example.com", "omahab-email-worker"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	actions, _ := putBody["actions"].([]any)
	action, _ := actions[0].(map[string]any)
	if action["type"] != "worker" {
		t.Fatalf("migrated action type = %v, want worker", action["type"])
	}
}

// Empty destinations fail closed; an email forward still works.
func TestEnsureEmailRoute_Validation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			json.NewEncoder(w).Encode(map[string]any{"success": true, "result": []any{}})
			return
		}
		if r.Method == http.MethodPost {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode: %v", err)
			}
			actions, _ := body["actions"].([]any)
			action, _ := actions[0].(map[string]any)
			if action["type"] != "forward" {
				t.Errorf("email destination action = %v, want forward", action["type"])
			}
			json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]any{"id": "rule-1"}})
			return
		}
	}))
	defer srv.Close()
	c, err := NewEmailClient(EmailOptions{APIToken: "tok", ZoneID: "zone", BaseURL: srv.URL + "/"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, dest := range []string{"", "has space", "UPPER-CASE!"} {
		if err := c.EnsureEmailRoute(ctx, "ai@example.com", dest); err == nil {
			t.Fatalf("destination %q must fail", dest)
		}
	}
	if err := c.EnsureEmailRoute(ctx, "ai@example.com", "user@example.com"); err != nil {
		t.Fatalf("email forward: %v", err)
	}
	if err := c.EnsureEmailRoute(ctx, "not-an-email", "omahab-email-worker"); err == nil {
		t.Fatal("bad recipient must fail")
	}
}
