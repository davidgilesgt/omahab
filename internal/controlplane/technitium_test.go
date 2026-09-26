package controlplane

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/omahab/omahab/internal/apps"
	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/events"
)

func TestTechnitiumDNSEndpointsLoopbackWhenNoLAN(t *testing.T) {
	got := technitiumDNSEndpoints(nil)
	if len(got) != 1 || got[0] != "127.0.0.1:53" {
		t.Fatalf("endpoints = %v, want [127.0.0.1:53]", got)
	}
}

func TestTechnitiumDNSEndpointsLANPlusLoopback(t *testing.T) {
	got := technitiumDNSEndpoints([]string{"192.168.8.106", "192.168.8.106"})
	want := []string{"127.0.0.1:53", "192.168.8.106:53"}
	if len(got) != len(want) {
		t.Fatalf("endpoints = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("endpoints = %v, want %v", got, want)
		}
	}
}

func TestTechnitiumUpstreamIsQuad9DoH(t *testing.T) {
	if technitiumUpstream != "https://dns.quad9.net/dns-query" {
		t.Fatalf("upstream = %q", technitiumUpstream)
	}
	if technitiumWebPort != 5380 {
		t.Fatalf("web port = %d, want 5380", technitiumWebPort)
	}
}

func TestTechnitiumBlocklistsDefault(t *testing.T) {
	lists := technitiumBlocklists()
	if len(lists) == 0 {
		t.Fatal("blocklists empty")
	}
	joined := strings.Join(lists, ",")
	if !strings.Contains(joined, "StevenBlack") || !strings.Contains(joined, "hagezi") {
		t.Fatalf("blocklists = %v, want StevenBlack + HaGeZi Pro hosts", lists)
	}
}

func TestTechnitiumSSOParamsOpenSignupWithRoleSync(t *testing.T) {
	p := technitiumSSOParams("omahab.com", "cid", "csecret")
	if p.Get("ssoAuthority") != "https://id.omahab.com" {
		t.Fatalf("authority = %q", p.Get("ssoAuthority"))
	}
	if p.Get("ssoAllowSignup") != "true" {
		t.Fatalf("ssoAllowSignup = %q, want true", p.Get("ssoAllowSignup"))
	}
	if p.Get("ssoAllowSignupOnlyForMappedUsers") != "false" {
		t.Fatalf("ssoAllowSignupOnlyForMappedUsers = %q, want false (no mapped-only dead-end)", p.Get("ssoAllowSignupOnlyForMappedUsers"))
	}
	got := p.Get("ssoGroupMap")
	if !strings.Contains(got, "admins:Administrators") || !strings.Contains(got, "members:DNS Administrators") {
		t.Fatalf("ssoGroupMap = %q, want admins+members role sync", got)
	}
	scopes := strings.Split(p.Get("ssoScopes"), "|")
	for _, want := range []string{"openid", "profile", "email", "groups"} {
		found := false
		for _, s := range scopes {
			if s == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("ssoScopes = %q, missing %q", p.Get("ssoScopes"), want)
		}
	}
}

// stubTechnitiumNoAuth points the Technitium API at a fake whose admin login
// is rotated away from the defaults with no stored credential to match. It
// counts settings convergences so tests can prove a deferred run performs no
// DNS side effects.
func stubTechnitiumNoAuth(t *testing.T) *atomic.Int32 {
	t.Helper()
	var settingsHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/status":
			_, _ = io.WriteString(w, `{"hasDefaultCredentials":false,"ssoEnabled":false}`)
		case "/api/user/login":
			_, _ = io.WriteString(w, `{"status":"error","errorMessage":"invalid credentials"}`)
		case "/api/settings/set":
			settingsHits.Add(1)
			_, _ = io.WriteString(w, `{"status":"ok"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	origBase := technitiumBaseURL
	technitiumBaseURL = srv.URL
	t.Cleanup(func() {
		srv.Close()
		technitiumBaseURL = origBase
	})
	return &settingsHits
}

// newTechnitiumInstalledBackend builds a backend with caddy and technitium
// installed through the scripted runner.
func newTechnitiumInstalledBackend(t *testing.T) (*Backend, *events.Service) {
	t.Helper()
	ctx := context.Background()
	digest := "sha256:" + strings.Repeat("a", 64)
	runner := &scriptedRunner{health: domain.HealthHealthy}
	b, ev := newSetupBackend(t, runner)
	cat, err := apps.NewCatalog(
		testSetupBundle("caddy", digest, domain.ExposurePublic, "", nil),
		testSetupBundle("technitium", digest, domain.ExposurePrivate, "dns", nil),
	)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := apps.NewService(b.db, apps.Options{Catalog: cat, Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	b.apps = svc
	if err := b.ensureDefaultApp(ctx, testSetupBundle("caddy", digest, domain.ExposurePublic, "", nil), "omahab.com"); err != nil {
		t.Fatalf("install caddy: %v", err)
	}
	if err := b.ensureDefaultApp(ctx, testSetupBundle("technitium", digest, domain.ExposurePrivate, "dns", nil), "omahab.com"); err != nil {
		t.Fatalf("install technitium: %v", err)
	}
	if !technitiumInstalled(ctx, b) {
		t.Fatal("technitium should report installed")
	}
	return b, ev
}

func TestEnsureTechnitiumMissingCredentialsIsOperatorPrerequisite(t *testing.T) {
	stubTechnitiumNoAuth(t)
	ctx := context.Background()
	b, _ := newSetupBackend(t, nil)
	err := b.ensureTechnitium(ctx)
	if !errors.Is(err, errTechnitiumAuthRequired) {
		t.Fatalf("ensureTechnitium = %v, want errTechnitiumAuthRequired", err)
	}
}

func TestTechnitiumLoginFromSecretsMissingIsOperatorPrerequisite(t *testing.T) {
	ctx := context.Background()
	b, _ := newSetupBackend(t, nil)
	if _, err := technitiumLoginFromSecrets(ctx, b); !errors.Is(err, errTechnitiumAuthRequired) {
		t.Fatalf("technitiumLoginFromSecrets = %v, want errTechnitiumAuthRequired", err)
	}
}

func TestConvergeTechnitiumOrDeferRecordsOperatorStepAndContinues(t *testing.T) {
	settingsHits := stubTechnitiumNoAuth(t)
	ctx := context.Background()
	b, ev := newTechnitiumInstalledBackend(t)
	origConflict := checkTechnitiumPortConflictFunc
	checkTechnitiumPortConflictFunc = func() error { return nil }
	defer func() { checkTechnitiumPortConflictFunc = origConflict }()
	if err := b.convergeTechnitiumOrDefer(ctx); err != nil {
		t.Fatalf("converge should defer, not fail: %v", err)
	}
	if settingsHits.Load() != 0 {
		t.Fatalf("deferred run converged settings %d times, want no DNS side effects", settingsHits.Load())
	}
	list, _, err := ev.List(ctx, events.ListOptions{Limit: 10, Filter: events.ListFilter{Type: "setup.step_failed"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("step_failed events = %d, want exactly the operator pending step", len(list))
	}
	got := list[0]
	if string(got.ResourceID) != "setup:technitium_auth" {
		t.Fatalf("resource = %q, want setup:technitium_auth", string(got.ResourceID))
	}
	if got.Data["code"] != "technitium_auth" || got.Data["owner"] != "operator" {
		t.Fatalf("data = %v, want code technitium_auth with owner operator", got.Data)
	}
	if !strings.Contains(got.Message, "Technitium setup view") {
		t.Fatalf("message should carry the recovery step, got %q", got.Message)
	}
}

func TestTechnitiumDNSCheckPendingWhileReconcileSucceeds(t *testing.T) {
	stubTechnitiumNoAuth(t)
	ctx := context.Background()
	b, _ := newTechnitiumInstalledBackend(t)
	origConflict := checkTechnitiumPortConflictFunc
	checkTechnitiumPortConflictFunc = func() error { return nil }
	defer func() { checkTechnitiumPortConflictFunc = origConflict }()
	// The reconcile defers household DNS but otherwise completes.
	if err := b.convergeTechnitiumOrDefer(ctx); err != nil {
		t.Fatalf("converge: %v", err)
	}
	if err := b.finishSetupSuccess(ctx); err != nil {
		t.Fatalf("finishSetupSuccess: %v", err)
	}
	st, err := b.GetSetupStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	auto := checkByID(t, st.Checks, "automatic_reconciliation")
	if auto.Status != "ok" {
		t.Fatalf("automatic_reconciliation = %+v, want ok (operator step must not fail verification)", auto)
	}
	tech := checkByID(t, st.Checks, "technitium_dns")
	if tech.Status != "pending" {
		t.Fatalf("technitium_dns = %+v, want pending operator step", tech)
	}
	if tech.Owner != "operator" || !strings.Contains(tech.Action, "Technitium setup view") {
		t.Fatalf("technitium_dns should name the operator recovery step, got %+v", tech)
	}
}

func TestTechnitiumDNSCheckSkippedWhenNotInstalled(t *testing.T) {
	ctx := context.Background()
	b, _ := newSetupBackend(t, nil)
	st, err := b.GetSetupStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tech := checkByID(t, st.Checks, "technitium_dns")
	if tech.Status != "skipped" {
		t.Fatalf("technitium_dns = %+v, want skipped", tech)
	}
}

func TestTechnitiumDNSCheckOKWithStoredCredentials(t *testing.T) {
	ctx := context.Background()
	b, _ := newTechnitiumInstalledBackend(t)
	if _, err := b.secrets.Put(ctx, "platform-app", "technitium_initial_password", "one-time-pw"); err != nil {
		t.Fatal(err)
	}
	st, err := b.GetSetupStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tech := checkByID(t, st.Checks, "technitium_dns")
	if tech.Status != "ok" {
		t.Fatalf("technitium_dns = %+v, want ok", tech)
	}
	if tech.Owner != "operator" {
		t.Fatalf("technitium_dns owner = %q, want operator", tech.Owner)
	}
}
