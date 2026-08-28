package cloudflare

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	cloudflare "github.com/cloudflare/cloudflare-go/v2"
	"github.com/omahab/omahab/internal/store"
)

func assertNoTokenLeak(t *testing.T, err error, tokens ...string) {
	t.Helper()
	if err == nil {
		return
	}
	msg := err.Error()
	for _, tok := range tokens {
		if tok != "" && strings.Contains(msg, tok) {
			t.Fatalf("error leaks token")
		}
	}
}

func TestEnsureTunnel_CreateWhenListEmpty(t *testing.T) {
	apiToken := "api-token-b-secret-111"
	connectorToken := "connector-token-secret-111"
	accountID := "acc-123"
	getListCalls := 0
	postCalls := 0
	tokenCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "cfd_tunnel") && !strings.Contains(r.URL.Path, "/token") {
			getListCalls++
			if r.URL.Query().Get("name") != "omahab" {
				t.Errorf("list name param mismatch got %q", r.URL.Query().Get("name"))
			}
			if r.URL.Query().Get("is_deleted") != "false" || r.URL.Query().Get("per_page") != "100" {
				t.Errorf("list query params mismatch got %v", r.URL.Query())
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"result":  []any{},
			})
			return
		}
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "cfd_tunnel") && !strings.Contains(r.URL.Path, "/token") {
			postCalls++
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if body["name"] != "omahab" || body["config_src"] != "cloudflare" {
				t.Errorf("create body mismatch %+v", body)
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"result": map[string]any{
					"id":    "tun-123",
					"token": connectorToken,
				},
			})
			return
		}
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/token") {
			tokenCalls++
			t.Errorf("unexpected token fetch in create-with-token test")
			w.WriteHeader(500)
			return
		}
		t.Errorf("unexpected %s %s", r.Method, r.URL.String())
	}))
	defer srv.Close()

	creator := NewTunnelCreator(accountID, apiToken, nil, srv.URL+"/")
	id, token, err := creator.EnsureTunnel(context.Background(), "omahab")
	if err != nil {
		assertNoTokenLeak(t, err, connectorToken, apiToken)
		t.Fatalf("EnsureTunnel create failed: %v", err)
	}
	assertNoTokenLeak(t, err, connectorToken, apiToken)
	if id != "tun-123" {
		t.Fatalf("expected tun-123 got %q", id)
	}
	if token != connectorToken {
		t.Fatalf("token mismatch")
	}
	if strings.Contains(token, connectorToken) == false {
		t.Fatalf("token should equal expected")
	}
	// ensure error strings don't contain token
	// also ensure counts
	if getListCalls != 1 {
		t.Fatalf("expected 1 list call got %d", getListCalls)
	}
	if postCalls != 1 {
		t.Fatalf("expected 1 post call got %d", postCalls)
	}
	if tokenCalls != 0 {
		t.Fatalf("expected 0 token calls got %d", tokenCalls)
	}
}

func TestEnsureTunnel_ExistingNameAdoption(t *testing.T) {
	apiToken := "api-token-b-secret-222"
	connectorToken := "connector-token-secret-222"
	accountID := "acc-123"
	postCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "cfd_tunnel") && !strings.Contains(r.URL.Path, "/token") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"result": []map[string]any{
					{"id": "tun-existing", "name": "omahab", "config_src": "cloudflare"},
				},
			})
			return
		}
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "cfd_tunnel") {
			postCalls++
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]any{"success": false})
			return
		}
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/token") {
			if !strings.Contains(r.URL.Path, "tun-existing") {
				t.Errorf("token path wrong %s", r.URL.Path)
			}
			w.Header().Set("Content-Type", "application/json")
			// test string result form
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"result":  connectorToken,
			})
			return
		}
		t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	creator := NewTunnelCreator(accountID, apiToken, nil, srv.URL+"/")
	id, token, err := creator.EnsureTunnel(context.Background(), "omahab")
	if err != nil {
		assertNoTokenLeak(t, err, connectorToken, apiToken)
		t.Fatalf("adoption failed: %v", err)
	}
	if id != "tun-existing" {
		t.Fatalf("expected tun-existing got %q", id)
	}
	if token != connectorToken {
		t.Fatalf("token mismatch")
	}
	assertNoTokenLeak(t, err, connectorToken, apiToken)
	if postCalls != 0 {
		t.Fatalf("expected no POST for adoption, got %d", postCalls)
	}
}

func TestEnsureTunnel_TokenRetrievalWhenCreateOmitsToken(t *testing.T) {
	apiToken := "api-token-b-secret-333"
	connectorToken := "connector-token-secret-333"
	accountID := "acc-123"
	getListCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "cfd_tunnel") && !strings.Contains(r.URL.Path, "/token") {
			getListCalls++
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"success": true, "result": []any{}})
			return
		}
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "cfd_tunnel") {
			w.Header().Set("Content-Type", "application/json")
			// omit token
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"result": map[string]any{
					"id": "tun-new",
				},
			})
			return
		}
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/token") {
			if !strings.Contains(r.URL.Path, "tun-new") {
				t.Errorf("token fetch for wrong id %s", r.URL.Path)
			}
			w.Header().Set("Content-Type", "application/json")
			// test object form with tunnel_token
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"result": map[string]any{
					"tunnel_token": connectorToken,
				},
			})
			return
		}
		t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	creator := NewTunnelCreator(accountID, apiToken, nil, srv.URL+"/")
	id, token, err := creator.EnsureTunnel(context.Background(), "omahab")
	if err != nil {
		assertNoTokenLeak(t, err, connectorToken, apiToken)
		t.Fatalf("token retrieval failed: %v", err)
	}
	if id != "tun-new" {
		t.Fatalf("id mismatch got %q", id)
	}
	if token != connectorToken {
		t.Fatalf("token mismatch after fetch")
	}
	assertNoTokenLeak(t, err, connectorToken, apiToken)
	if getListCalls != 1 {
		t.Fatalf("expected 1 list call got %d", getListCalls)
	}
}

func TestEnsureTunnel_409RelistRace(t *testing.T) {
	apiToken := "api-token-b-secret-444"
	connectorToken := "connector-token-secret-444"
	accountID := "acc-123"
	listCalls := 0
	postCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "cfd_tunnel") && !strings.Contains(r.URL.Path, "/token") {
			listCalls++
			w.Header().Set("Content-Type", "application/json")
			if listCalls == 1 {
				json.NewEncoder(w).Encode(map[string]any{"success": true, "result": []any{}})
				return
			}
			// second list after 409: return existing remotely managed
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"result": []map[string]any{
					{"id": "tun-raced", "name": "omahab", "config_src": "cloudflare"},
				},
			})
			return
		}
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "cfd_tunnel") {
			postCalls++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(409)
			json.NewEncoder(w).Encode(map[string]any{
				"success": false,
				"errors":  []map[string]any{{"code": 409, "message": "tunnel already exists"}},
			})
			return
		}
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/token") {
			if !strings.Contains(r.URL.Path, "tun-raced") {
				t.Errorf("token fetch wrong %s", r.URL.Path)
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"result":  connectorToken,
			})
			return
		}
		t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	creator := NewTunnelCreator(accountID, apiToken, nil, srv.URL+"/")
	id, token, err := creator.EnsureTunnel(context.Background(), "omahab")
	if err != nil {
		assertNoTokenLeak(t, err, connectorToken, apiToken)
		t.Fatalf("409 race failed: %v", err)
	}
	if id != "tun-raced" {
		t.Fatalf("expected tun-raced got %q", id)
	}
	if token != connectorToken {
		t.Fatalf("token mismatch in race")
	}
	assertNoTokenLeak(t, err, connectorToken, apiToken)
	if listCalls != 2 {
		t.Fatalf("expected 2 list calls got %d", listCalls)
	}
	if postCalls != 1 {
		t.Fatalf("expected 1 post call got %d", postCalls)
	}
	// ensure error not containing token
	if err != nil && strings.Contains(err.Error(), connectorToken) {
		t.Fatalf("error leaks token")
	}
}

func TestEnsureTunnel_MalformedEmptyID(t *testing.T) {
	apiToken := "api-token-b-secret-555"
	connectorToken := "connector-token-secret-555"
	accountID := "acc-123"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "cfd_tunnel") && !strings.Contains(r.URL.Path, "/token") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"success": true, "result": []any{}})
			return
		}
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "cfd_tunnel") {
			w.Header().Set("Content-Type", "application/json")
			// empty id, token present but should error on empty id
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"result": map[string]any{
					"id":    "",
					"token": connectorToken,
				},
			})
			return
		}
		// token fetch should not be reached if id empty, but handle second list for race
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "cfd_tunnel") && strings.Contains(r.URL.Path, "/token") {
			t.Errorf("unexpected token fetch")
			w.WriteHeader(500)
			return
		}
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/token") {
			t.Errorf("unexpected token fetch")
			w.WriteHeader(500)
			return
		}
		// For second list after 409? Not needed; but if our impl does re-list on empty id, handle it
		t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	creator := NewTunnelCreator(accountID, apiToken, nil, srv.URL+"/")
	_, _, err := creator.EnsureTunnel(context.Background(), "omahab")
	if err == nil {
		t.Fatalf("expected error for empty id")
	}
	assertNoTokenLeak(t, err, connectorToken, apiToken)
	if !errors.Is(err, store.ErrConflict) {
		// Empty ID should be conflict (redacted). Accept generic error that wraps conflict or plain conflict.
		// If implementation returns generic not conflict, still check error exists, but prefer conflict.
		// We assert conflict to enforce spec.
		t.Fatalf("expected ErrConflict for empty id, got %v", err)
	}
	if strings.Contains(err.Error(), connectorToken) {
		t.Fatalf("error leaks token")
	}
}

func TestEnsureTunnel_LocalConfigConflict(t *testing.T) {
	apiToken := "api-token-b-secret-666"
	connectorToken := "connector-token-secret-666"
	accountID := "acc-123"
	postCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "cfd_tunnel") && !strings.Contains(r.URL.Path, "/token") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"result": []map[string]any{
					{"id": "tun-local", "name": "omahab", "config_src": "local"},
				},
			})
			return
		}
		if r.Method == http.MethodPost {
			postCalls++
			w.WriteHeader(500)
			return
		}
		if strings.Contains(r.URL.Path, "/token") {
			t.Errorf("should not fetch token for local-managed")
			w.WriteHeader(500)
			return
		}
		t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	creator := NewTunnelCreator(accountID, apiToken, nil, srv.URL+"/")
	_, _, err := creator.EnsureTunnel(context.Background(), "omahab")
	if err == nil {
		t.Fatalf("expected conflict for local-managed")
	}
	assertNoTokenLeak(t, err, connectorToken, apiToken)
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("expected ErrConflict for local-managed, got %v", err)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "local-managed") && !strings.Contains(strings.ToLower(err.Error()), "conflicting") {
		t.Fatalf("expected local-managed or conflicting in error, got %q", err.Error())
	}
	if strings.Contains(err.Error(), connectorToken) {
		t.Fatalf("error leaks token")
	}
	if postCalls != 0 {
		t.Fatalf("expected no POST for local-managed, got %d", postCalls)
	}
}

func TestEnsureTunnel_MultipleExactMatchesConflict(t *testing.T) {
	apiToken := "api-token-b-secret-777"
	connectorToken := "connector-token-secret-777"
	accountID := "acc-123"
	postCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "cfd_tunnel") && !strings.Contains(r.URL.Path, "/token") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"result": []map[string]any{
					{"id": "tun-1", "name": "omahab", "config_src": "cloudflare"},
					{"id": "tun-2", "name": "omahab", "config_src": "cloudflare"},
				},
			})
			return
		}
		if r.Method == http.MethodPost {
			postCalls++
			w.WriteHeader(500)
			return
		}
		if strings.Contains(r.URL.Path, "/token") {
			t.Errorf("should not fetch token for multiple matches")
			w.WriteHeader(500)
			return
		}
		t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	creator := NewTunnelCreator(accountID, apiToken, nil, srv.URL+"/")
	_, _, err := creator.EnsureTunnel(context.Background(), "omahab")
	if err == nil {
		t.Fatalf("expected conflict for multiple matches")
	}
	assertNoTokenLeak(t, err, connectorToken, apiToken)
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("expected ErrConflict for multiple, got %v", err)
	}
	if strings.Contains(err.Error(), connectorToken) {
		t.Fatalf("error leaks token")
	}
	if postCalls != 0 {
		t.Fatalf("expected no POST for multiple, got %d", postCalls)
	}
}

func TestEnsureTunnel_Authorization401(t *testing.T) {
	apiToken := "api-token-b-secret-888"
	connectorToken := "connector-token-secret-888"
	accountID := "acc-123"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		json.NewEncoder(w).Encode(map[string]any{
			"success": false,
			"errors":  []map[string]any{{"code": 401, "message": "unauthorized"}},
		})
	}))
	defer srv.Close()

	creator := NewTunnelCreator(accountID, apiToken, nil, srv.URL+"/")
	_, _, err := creator.EnsureTunnel(context.Background(), "omahab")
	if err == nil {
		t.Fatalf("expected 401 error")
	}
	assertNoTokenLeak(t, err, connectorToken, apiToken)
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized for 401, got %v", err)
	}
	if strings.Contains(err.Error(), connectorToken) || strings.Contains(err.Error(), apiToken) {
		t.Fatalf("error leaks token")
	}
}

func TestEnsureTunnel_Validation(t *testing.T) {
	creator := NewTunnelCreator("acc-123", "tok", nil, "http://example.com/")
	if _, _, err := creator.EnsureTunnel(context.Background(), ""); !errors.Is(err, store.ErrValidation) {
		t.Fatalf("empty name should be validation, got %v", err)
	}
	creator2 := NewTunnelCreator("", "tok", nil, "http://example.com/")
	if _, _, err := creator2.EnsureTunnel(context.Background(), "omahab"); !errors.Is(err, store.ErrValidation) {
		t.Fatalf("empty accountID should be validation, got %v", err)
	}
	// nil client
	tc := &tunnelClient{accountID: "acc-123", client: nil}
	if _, _, err := tc.EnsureTunnel(context.Background(), "omahab"); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("nil client should error, got %v", err)
	}
}

func TestEnsureTunnel_EscapedName(t *testing.T) {
	apiToken := "api-token-b-secret-999"
	connectorToken := "connector-token-secret-999"
	accountID := "acc-123"
	name := "my tunnel"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "cfd_tunnel") && !strings.Contains(r.URL.Path, "/token") {
			// QueryEscape should encode space as %20 or + ; we check decoded value equals name
			if r.URL.Query().Get("name") != name {
				t.Errorf("escaped name mismatch got %q want %q raw %q", r.URL.Query().Get("name"), name, r.URL.RawQuery)
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"success": true, "result": []any{}})
			return
		}
		if r.Method == http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]any{"id": "tun-999", "token": connectorToken}})
			return
		}
		t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	creator := NewTunnelCreator(accountID, apiToken, nil, srv.URL+"/")
	id, token, err := creator.EnsureTunnel(context.Background(), name)
	if err != nil {
		assertNoTokenLeak(t, err, connectorToken, apiToken)
		t.Fatalf("escaped name failed: %v", err)
	}
	if id != "tun-999" || token != connectorToken {
		t.Fatalf("unexpected result %q %q", id, token)
	}
}

func TestEnsureTunnel_FilterTrimsAndIgnoresDeleted(t *testing.T) {
	apiToken := "api-token-b-secret-filter"
	connectorToken := "connector-token-secret-filter"
	accountID := "acc-123"
	postCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "cfd_tunnel") && !strings.Contains(r.URL.Path, "/token") {
			w.Header().Set("Content-Type", "application/json")
			deleted := "2024-01-01T00:00:00Z"
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"result": []map[string]any{
					{"id": "tun-deleted", "name": "omahab", "config_src": "cloudflare", "deleted_at": deleted},
					{"id": "tun-other", "name": "other", "config_src": "cloudflare"},
					{"id": "tun-good", "name": "omahab", "config_src": "cloudflare"},
				},
			})
			return
		}
		if r.Method == http.MethodPost {
			postCalls++
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]any{"id": "tun-new", "token": connectorToken}})
			return
		}
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/token") {
			// Should not be called if filtered to one? Actually filtered will have 1 match (tun-good) after ignoring deleted and other name.
			// So adoption should happen, no POST.
			// But if we return multiple filtered? Let's see: after filtering, we have one exact match (tun-good) => adoption path, token fetch.
			if !strings.Contains(r.URL.Path, "tun-good") {
				t.Errorf("token fetch for wrong id %s", r.URL.Path)
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"success": true, "result": connectorToken})
			return
		}
		t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	creator := NewTunnelCreator(accountID, apiToken, nil, srv.URL+"/")
	id, token, err := creator.EnsureTunnel(context.Background(), "omahab")
	if err != nil {
		assertNoTokenLeak(t, err, connectorToken, apiToken)
		t.Fatalf("filter failed: %v", err)
	}
	if id != "tun-good" {
		t.Fatalf("expected tun-good got %q", id)
	}
	if token != connectorToken {
		t.Fatalf("token mismatch")
	}
	if postCalls != 0 {
		t.Fatalf("expected no POST when exact active match exists, got %d", postCalls)
	}
}

func TestMapAPIError409(t *testing.T) {
	req, _ := http.NewRequest("POST", "https://api.cloudflare.com/client/v4/accounts/acc/cfd_tunnel", nil)
	sdkErr := &cloudflare.Error{
		StatusCode: 409,
		Request:    req,
		Response:   &http.Response{StatusCode: 409},
	}
	mapped := mapAPIError(sdkErr)
	if !errors.Is(mapped, store.ErrConflict) {
		t.Fatalf("mapAPIError 409 should be ErrConflict, got %v", mapped)
	}
}

func TestMapHTTPStatus409(t *testing.T) {
	err := mapHTTPStatus(409, "tunnel already exists")
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("mapHTTPStatus 409 should be ErrConflict, got %v", err)
	}
	// ensure 409 with empty body also
	err2 := mapHTTPStatus(409, "")
	if !errors.Is(err2, store.ErrConflict) {
		t.Fatalf("mapHTTPStatus 409 empty should be ErrConflict, got %v", err2)
	}
}

func TestEnsureTunnel_EmptyTokenAfterFetch(t *testing.T) {
	apiToken := "api-token-b-secret-emptytok"
	connectorToken := "connector-token-secret-emptytok"
	accountID := "acc-123"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "cfd_tunnel") && !strings.Contains(r.URL.Path, "/token") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"success": true, "result": []any{}})
			return
		}
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "cfd_tunnel") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"result":  map[string]any{"id": "tun-emptytok"},
			})
			return
		}
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/token") {
			w.Header().Set("Content-Type", "application/json")
			// empty token string
			json.NewEncoder(w).Encode(map[string]any{"success": true, "result": ""})
			return
		}
		t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	creator := NewTunnelCreator(accountID, apiToken, nil, srv.URL+"/")
	_, _, err := creator.EnsureTunnel(context.Background(), "omahab")
	if err == nil {
		t.Fatalf("expected error for empty token")
	}
	assertNoTokenLeak(t, err, connectorToken, apiToken)
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("expected ErrConflict for empty token, got %v", err)
	}
	if strings.Contains(err.Error(), connectorToken) {
		t.Fatalf("error leaks token")
	}
}

func TestEnsureTunnel_TokenObjectFields(t *testing.T) {
	apiToken := "api-token-b-secret-obj"
	connectorToken := "connector-token-secret-obj"
	accountID := "acc-123"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "cfd_tunnel") && !strings.Contains(r.URL.Path, "/token") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"result": []map[string]any{
					{"id": "tun-obj", "name": "omahab", "config_src": "cloudflare"},
				},
			})
			return
		}
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/token") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"result": map[string]any{
					"token": connectorToken,
				},
			})
			return
		}
		t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	creator := NewTunnelCreator(accountID, apiToken, nil, srv.URL+"/")
	id, token, err := creator.EnsureTunnel(context.Background(), "omahab")
	if err != nil {
		assertNoTokenLeak(t, err, connectorToken, apiToken)
		t.Fatalf("object token failed: %v", err)
	}
	if id != "tun-obj" || token != connectorToken {
		t.Fatalf("unexpected id/token")
	}
}
