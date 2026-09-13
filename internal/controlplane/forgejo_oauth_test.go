package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Woodpecker authenticates with a client secret and has no PKCE support, so
// its Forgejo OAuth2 application must be a confidential client. Forgejo's API
// defaults confidential_client to false when omitted, which produced public
// apps and broke CI login with "invalid_request: PKCE is required for public
// clients" (live 2026-09-12).
func TestEnsureWoodpeckerOAuthAppCreatesConfidentialClient(t *testing.T) {
	t.Parallel()
	var gotCreate map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/user/applications/oauth2":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/user/applications/oauth2":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "bad body", http.StatusBadRequest)
				return
			}
			gotCreate = body
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":1,"name":"Woodpecker","client_id":"cid-1","client_secret":"sec-1","confidential_client":true,"redirect_uris":["https://ci.example.com/authorize"]}`))
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	b, _ := newSetupBackend(t, nil)
	cid, sec, err := b.ensureWoodpeckerOAuthApp(context.Background(), srv.URL, "tok", "example.com")
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if cid != "cid-1" || sec != "sec-1" {
		t.Fatalf("credentials = %q %q, want cid-1 sec-1", cid, sec)
	}
	conf, ok := gotCreate["confidential_client"]
	if !ok || conf != true {
		t.Fatalf("create payload confidential_client = %v (%v), want true", conf, gotCreate)
	}
	uris, _ := gotCreate["redirect_uris"].([]any)
	if len(uris) != 1 || uris[0] != "https://ci.example.com/authorize" {
		t.Fatalf("create payload redirect_uris = %v", gotCreate["redirect_uris"])
	}
}

// An existing public Woodpecker app (created before the confidential fix)
// must be patched back to confidential, not reused as-is.
func TestEnsureWoodpeckerOAuthAppMigratesPublicClient(t *testing.T) {
	t.Parallel()
	var gotPatch map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/user/applications/oauth2":
			// List hides the secret, as Forgejo does.
			_, _ = w.Write([]byte(`[{"id":7,"name":"Woodpecker","client_id":"cid-7","client_secret":"","confidential_client":false,"redirect_uris":["https://ci.example.com/authorize"]}]`))
		case (r.Method == http.MethodPatch || r.Method == http.MethodPut) && strings.HasPrefix(r.URL.Path, "/api/v1/user/applications/oauth2/7"):
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "bad body", http.StatusBadRequest)
				return
			}
			gotPatch = body
			_, _ = w.Write([]byte(`{"id":7,"name":"Woodpecker","client_id":"cid-7","client_secret":"sec-new","confidential_client":true,"redirect_uris":["https://ci.example.com/authorize"]}`))
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	b, _ := newSetupBackend(t, nil)
	cid, sec, err := b.ensureWoodpeckerOAuthApp(context.Background(), srv.URL, "tok", "example.com")
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if cid != "cid-7" || sec != "sec-new" {
		t.Fatalf("credentials = %q %q, want cid-7 sec-new", cid, sec)
	}
	if gotPatch == nil {
		t.Fatal("public app was reused without patching to confidential")
	}
	if conf, ok := gotPatch["confidential_client"]; !ok || conf != true {
		t.Fatalf("patch payload confidential_client = %v (%v), want true", conf, gotPatch)
	}
}

// A converged Woodpecker app (redirect + confidential already correct) must
// reuse the stored secret without PATCHing: Forgejo's list response omits
// client_secret, and every update bumps updated_unix and rotates the secret
// (Forgejo 15), which orphaned the running server on each setup re-run (live
// 2026-09-13: "invalid client secret" until manual restart).
func TestEnsureWoodpeckerOAuthAppReusesStoredSecret(t *testing.T) {
	t.Parallel()
	patched := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/user/applications/oauth2":
			// Converged app, secret hidden as Forgejo does.
			_, _ = w.Write([]byte(`[{"id":7,"name":"Woodpecker","client_id":"cid-7","client_secret":"","confidential_client":true,"redirect_uris":["https://ci.example.com/authorize"]}]`))
		default:
			patched = true
			http.Error(w, "must not patch a converged app", http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)

	b, _ := newSetupBackend(t, nil)
	if err := upsertSecret(context.Background(), b.secrets, "platform-app", "woodpecker_forgejo_client_secret", "stored-sec"); err != nil {
		t.Fatalf("store secret: %v", err)
	}
	cid, sec, err := b.ensureWoodpeckerOAuthApp(context.Background(), srv.URL, "tok", "example.com")
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if cid != "cid-7" || sec != "stored-sec" {
		t.Fatalf("credentials = %q %q, want cid-7 stored-sec", cid, sec)
	}
	if patched {
		t.Fatal("converged app was patched; stored secret must be reused without update")
	}
}
