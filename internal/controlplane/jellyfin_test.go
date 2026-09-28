package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestJellyfinOIDCCallback(t *testing.T) {
	got := jellyfinOIDCCallback("omahab.com")
	want := "https://media.omahab.com/sso/OID/redirect/pocketid"
	if got != want {
		t.Fatalf("callback = %q, want %q", got, want)
	}
}

func TestJellyfinProviderDocPocketID(t *testing.T) {
	raw, err := jellyfinProviderDoc("omahab.com", "cid", "/srv/omahab/apps/jellyfin/sso/pocketid-secret")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		FormatVersion int `json:"FormatVersion"`
		Configuration struct {
			OidConfigs map[string]struct {
				OidEndpoint         string   `json:"OidEndpoint"`
				OidClientId         string   `json:"OidClientId"`
				OidSecretFile       string   `json:"OidSecretFile"`
				OidSecret           string   `json:"OidSecret"`
				Enabled             bool     `json:"Enabled"`
				EnableAuthorization bool     `json:"EnableAuthorization"`
				RoleClaim           string   `json:"RoleClaim"`
				Roles               []string `json:"Roles"`
				AdminRoles          []string `json:"AdminRoles"`
				EnableAllFolders    bool     `json:"EnableAllFolders"`
			} `json:"OidConfigs"`
		} `json:"Configuration"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.FormatVersion != 1 {
		t.Fatalf("FormatVersion = %d, want 1", doc.FormatVersion)
	}
	p, ok := doc.Configuration.OidConfigs["pocketid"]
	if !ok {
		t.Fatal("missing pocketid provider")
	}
	if p.OidEndpoint != "https://id.omahab.com/.well-known/openid-configuration" {
		t.Fatalf("OidEndpoint = %q", p.OidEndpoint)
	}
	if p.OidClientId != "cid" {
		t.Fatalf("OidClientId = %q", p.OidClientId)
	}
	if p.OidSecretFile != "/srv/omahab/apps/jellyfin/sso/pocketid-secret" {
		t.Fatalf("OidSecretFile = %q", p.OidSecretFile)
	}
	if p.OidSecret != "" {
		t.Fatal("secret must be a file reference, never inline")
	}
	if !p.Enabled || !p.EnableAuthorization || !p.EnableAllFolders {
		t.Fatalf("provider not fully enabled: %+v", p)
	}
	if p.RoleClaim != "groups" {
		t.Fatalf("RoleClaim = %q, want groups", p.RoleClaim)
	}
	if len(p.Roles) != 2 || len(p.AdminRoles) != 1 || p.AdminRoles[0] != "admins" {
		t.Fatalf("role map = %v/%v, want [admins members]/[admins]", p.Roles, p.AdminRoles)
	}
}

func TestWriteJellyfinSSOFileIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sso", "providers.json")
	changed, err := writeJellyfinSSOFile(path, []byte("{}\n"), 0o640)
	if err != nil || !changed {
		t.Fatalf("first write changed=%v err=%v", changed, err)
	}
	changed, err = writeJellyfinSSOFile(path, []byte("{}\n"), 0o640)
	if err != nil || changed {
		t.Fatalf("second write changed=%v err=%v, want unchanged", changed, err)
	}
}

// jellyfinFake models upstream startup behavior: no first user exists
// initially, GET /Startup/User initializes it without completing the wizard,
// POST /Startup/User returns 404 until GET succeeds, only
// /Startup/Complete flips wizardDone, and authentication checks the
// configured username and password.
type jellyfinFake struct {
	wizardDone      bool
	userInitialized bool
	adminPassword   string
	libraryExists   bool
	authFails       bool
	getInitFails    bool
	// getInit503s makes the first N GET /Startup/User calls answer 503, as
	// upstream does while core startup is still running.
	getInit503s   int
	completeFails bool
	mu            sync.Mutex
	calls         []string
	libraryQuery  string
}

func (f *jellyfinFake) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/System/Info/Public", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls = append(f.calls, "public")
		done := f.wizardDone
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"StartupWizardCompleted": done})
	})
	mux.HandleFunc("/Startup/User", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Method == http.MethodGet {
			f.calls = append(f.calls, "init-user")
			if f.getInit503s > 0 {
				f.getInit503s--
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			if f.getInitFails {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			f.userInitialized = true
			_ = json.NewEncoder(w).Encode(map[string]any{"Name": "admin"})
			return
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.calls = append(f.calls, "user:"+body["Name"])
		if !f.userInitialized {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		f.adminPassword = body["Password"]
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/Startup/Complete", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls = append(f.calls, "complete")
		fail := f.completeFails
		if !fail {
			f.wizardDone = true
		}
		f.mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/Users/AuthenticateByName", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.calls = append(f.calls, "auth")
		fail := f.authFails
		wantUser := "admin"
		wantPw := f.adminPassword
		f.mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if strings.TrimSpace(body["Username"]) != wantUser || body["Pw"] != wantPw || strings.TrimSpace(wantPw) == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "tok"})
	})
	mux.HandleFunc("/Library/VirtualFolders", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			f.mu.Lock()
			f.calls = append(f.calls, "create-library")
			f.libraryQuery = r.URL.RawQuery
			f.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		f.mu.Lock()
		f.calls = append(f.calls, "list-libraries")
		exists := f.libraryExists
		f.mu.Unlock()
		folders := []any{}
		if exists {
			folders = append(folders, map[string]any{
				"Name": "Media", "CollectionType": "mixed",
				"Locations": []string{"/srv/omahab/apps/jellyfin/library"},
			})
		}
		_ = json.NewEncoder(w).Encode(folders)
	})
	return mux
}

func (f *jellyfinFake) has(call string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == call || strings.HasPrefix(c, call+":") {
			return true
		}
	}
	return false
}

func setupJellyfinTest(t *testing.T, fake *jellyfinFake) *Backend {
	t.Helper()
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)
	old := jellyfinBaseURL
	jellyfinBaseURL = srv.URL
	t.Cleanup(func() { jellyfinBaseURL = old })
	b, _ := newSetupBackend(t, nil)
	b.cfg.DataDir = "/srv/omahab"
	return b
}

func TestEnsureJellyfinSetupFirstRun(t *testing.T) {
	fake := &jellyfinFake{}
	b := setupJellyfinTest(t, fake)
	if err := b.ensureJellyfinSetup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !fake.has("init-user") || !fake.has("user:admin") || !fake.has("complete") || !fake.has("create-library") {
		t.Fatalf("missing setup calls: %v", fake.calls)
	}
	pw, err := b.secrets.RevealByName(context.Background(), "platform-app", "jellyfin_breakglass_password")
	if err != nil || strings.TrimSpace(pw) == "" {
		t.Fatalf("breakglass password not stored: %v", err)
	}
	if !strings.Contains(fake.libraryQuery, "collectionType=mixed") || !strings.Contains(fake.libraryQuery, "name=Media") {
		t.Fatalf("library query = %q", fake.libraryQuery)
	}
	if !strings.Contains(fake.libraryQuery, "paths=") {
		t.Fatalf("library query missing paths: %q", fake.libraryQuery)
	}
	// Second reconciliation must not change the password or duplicate the library.
	fake.mu.Lock()
	fake.libraryExists = true
	fake.calls = nil
	fake.mu.Unlock()
	if err := b.ensureJellyfinSetup(context.Background()); err != nil {
		t.Fatal(err)
	}
	pw2, err := b.secrets.RevealByName(context.Background(), "platform-app", "jellyfin_breakglass_password")
	if err != nil || pw2 != pw {
		t.Fatalf("password changed on reconverge: %q vs %q", pw2, pw)
	}
	if fake.has("user") || fake.has("complete") || fake.has("create-library") {
		t.Fatalf("second run must not duplicate writes: %v", fake.calls)
	}
}

// Jellyfin answers /System/Info/Public before startup finishes and 503s
// /Startup/User meanwhile; setup must wait it out, not abort.
func TestEnsureJellyfinSetupWaitsOutStartup503(t *testing.T) {
	fake := &jellyfinFake{getInit503s: 2}
	b := setupJellyfinTest(t, fake)
	if err := b.ensureJellyfinSetup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !fake.has("user:admin") || !fake.has("complete") {
		t.Fatalf("setup did not finish after 503s: %v", fake.calls)
	}
}

func TestEnsureJellyfinSetupAlreadyConverged(t *testing.T) {
	fake := &jellyfinFake{wizardDone: true, libraryExists: true, adminPassword: "pw", userInitialized: true}
	b := setupJellyfinTest(t, fake)
	ctx := context.Background()
	if err := upsertSecret(ctx, b.secrets, "platform-app", "jellyfin_breakglass_password", "pw"); err != nil {
		t.Fatal(err)
	}
	if err := b.ensureJellyfinSetup(ctx); err != nil {
		t.Fatal(err)
	}
	if fake.has("user") || fake.has("complete") || fake.has("create-library") {
		t.Fatalf("unexpected writes: %v", fake.calls)
	}
}

func TestEnsureJellyfinSetupInitFailureLeavesIncomplete(t *testing.T) {
	fake := &jellyfinFake{getInitFails: true}
	b := setupJellyfinTest(t, fake)
	if err := b.ensureJellyfinSetup(context.Background()); err == nil {
		t.Fatal("expected init failure")
	}
	if fake.has("complete") || fake.has("create-library") {
		t.Fatalf("failed init must leave wizard incomplete: %v", fake.calls)
	}
	fake.mu.Lock()
	done := fake.wizardDone
	fake.mu.Unlock()
	if done {
		t.Fatal("wizard must remain incomplete after init failure")
	}
}

func TestEnsureJellyfinSetupAuthFailureLeavesIncomplete(t *testing.T) {
	fake := &jellyfinFake{authFails: true}
	b := setupJellyfinTest(t, fake)
	if err := b.ensureJellyfinSetup(context.Background()); err == nil {
		t.Fatal("expected auth failure")
	}
	if fake.has("complete") || fake.has("create-library") {
		t.Fatalf("failed auth must leave wizard incomplete: %v", fake.calls)
	}
	fake.mu.Lock()
	done := fake.wizardDone
	fake.mu.Unlock()
	if done {
		t.Fatal("wizard must remain incomplete after auth failure")
	}
	// Password is stored before auth so a retry can reuse it.
	pw, err := b.secrets.RevealByName(context.Background(), "platform-app", "jellyfin_breakglass_password")
	if err != nil || strings.TrimSpace(pw) == "" {
		t.Fatalf("password must be stored before auth for retry: %v", err)
	}
}

func TestEnsureJellyfinSetupSecretStoreFailure(t *testing.T) {
	fake := &jellyfinFake{}
	b := setupJellyfinTest(t, fake)
	b.secrets = nil
	if err := b.ensureJellyfinSetup(context.Background()); err == nil {
		t.Fatal("expected secret-store failure")
	}
	if fake.has("user") || fake.has("complete") || fake.has("create-library") {
		t.Fatalf("secret failure must precede remote updates: %v", fake.calls)
	}
	fake.mu.Lock()
	done := fake.wizardDone
	fake.mu.Unlock()
	if done {
		t.Fatal("wizard must remain incomplete after secret failure")
	}
}

func TestEnsureJellyfinSetupInterruptionReusesPassword(t *testing.T) {
	fake := &jellyfinFake{completeFails: true}
	b := setupJellyfinTest(t, fake)
	ctx := context.Background()
	if err := b.ensureJellyfinSetup(ctx); err == nil {
		t.Fatal("expected complete failure")
	}
	pw1, err := b.secrets.RevealByName(ctx, "platform-app", "jellyfin_breakglass_password")
	if err != nil || strings.TrimSpace(pw1) == "" {
		t.Fatalf("interrupted run must store password: %v", err)
	}
	fake.mu.Lock()
	if fake.wizardDone {
		fake.mu.Unlock()
		t.Fatal("wizard must remain incomplete after complete failure")
	}
	fake.completeFails = false
	fake.calls = nil
	fake.mu.Unlock()
	if err := b.ensureJellyfinSetup(ctx); err != nil {
		t.Fatalf("retry: %v", err)
	}
	pw2, err := b.secrets.RevealByName(ctx, "platform-app", "jellyfin_breakglass_password")
	if err != nil || pw2 != pw1 {
		t.Fatalf("retry must reuse stored password: %q vs %q", pw2, pw1)
	}
	if !fake.has("complete") || !fake.has("create-library") {
		t.Fatalf("retry must complete wizard and library: %v", fake.calls)
	}
}

func TestEnsureJellyfinSetupUnknownAdminSkips(t *testing.T) {
	fake := &jellyfinFake{wizardDone: true, authFails: true}
	b := setupJellyfinTest(t, fake)
	ctx := context.Background()
	if err := upsertSecret(ctx, b.secrets, "platform-app", "jellyfin_breakglass_password", "stale"); err != nil {
		t.Fatal(err)
	}
	// A healthy pre-automation server with an unknown local password must
	// not fail setup.
	if err := b.ensureJellyfinSetup(ctx); err != nil {
		t.Fatalf("expected skip, got error: %v", err)
	}
	if fake.has("create-library") {
		t.Fatalf("library must not converge without auth: %v", fake.calls)
	}
}

func TestEnsureJellyfinSetupNoSecretSkips(t *testing.T) {
	fake := &jellyfinFake{wizardDone: true}
	b := setupJellyfinTest(t, fake)
	if err := b.ensureJellyfinSetup(context.Background()); err != nil {
		t.Fatalf("expected skip, got error: %v", err)
	}
	if fake.has("auth") || fake.has("create-library") {
		t.Fatalf("no auth attempted without secret: %v", fake.calls)
	}
}

func TestJellyfinPathsDefault(t *testing.T) {
	b, _ := newSetupBackend(t, nil)
	if got := jellyfinLibraryPath(b); got != "/srv/omahab/apps/jellyfin/library" {
		t.Fatalf("library = %q", got)
	}
	if got := jellyfinSSODir(b); got != "/srv/omahab/apps/jellyfin/sso" {
		t.Fatalf("sso dir = %q", got)
	}
}

// Regression for F-jellyfin-sso-discovery: the Flowfin plugin reads the
// discovery document server-side over a transport that refuses non-public
// addresses by default, and Pocket ID only emits the groups claim when the
// groups scope is requested. Without both, /SSO/OID/start/pocketid fails
// with "the authorization server's discovery document could not be read"
// and role authorization denies every login.
func TestJellyfinProviderDocPrivateNetworkAndScopes(t *testing.T) {
	raw, err := jellyfinProviderDoc("omahab.com", "cid", "/srv/omahab/apps/jellyfin/sso/pocketid-secret")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Configuration struct {
			OidConfigs map[string]struct {
				OidEndpoint                  string   `json:"OidEndpoint"`
				OidScopes                    []string `json:"OidScopes"`
				RoleClaim                    string   `json:"RoleClaim"`
				AllowPrivateNetworkAddresses bool     `json:"AllowPrivateNetworkAddresses"`
			} `json:"OidConfigs"`
		} `json:"Configuration"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	p, ok := doc.Configuration.OidConfigs["pocketid"]
	if !ok {
		t.Fatal("missing pocketid provider")
	}
	if !p.AllowPrivateNetworkAddresses {
		t.Fatal("AllowPrivateNetworkAddresses must be true: id.<domain> resolves to a tailnet/LAN address the plugin otherwise refuses")
	}
	if p.RoleClaim != "groups" {
		t.Fatalf("RoleClaim = %q, want groups", p.RoleClaim)
	}
	want := map[string]bool{"openid": false, "profile": false, "email": false, "groups": false}
	for _, s := range p.OidScopes {
		if _, ok := want[s]; !ok {
			t.Fatalf("unexpected scope %q", s)
		}
		want[s] = true
	}
	for s, seen := range want {
		if !seen {
			t.Fatalf("OidScopes missing %q (Pocket ID omits the groups claim without it): %v", s, p.OidScopes)
		}
	}
}
