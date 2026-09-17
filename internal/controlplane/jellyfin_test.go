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

// jellyfinFake is a scripted Jellyfin server for setup tests.
type jellyfinFake struct {
	wizardDone    bool
	libraryExists bool
	authFails     bool
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
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.calls = append(f.calls, "user:"+body["Name"])
		f.wizardDone = true
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/Startup/Complete", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls = append(f.calls, "complete")
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/Users/AuthenticateByName", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls = append(f.calls, "auth")
		fail := f.authFails
		f.mu.Unlock()
		if fail {
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
	if !fake.has("user:admin") || !fake.has("complete") || !fake.has("create-library") {
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
}

func TestEnsureJellyfinSetupAlreadyConverged(t *testing.T) {
	fake := &jellyfinFake{wizardDone: true, libraryExists: true}
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
