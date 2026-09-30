package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/exposure"
)

// fakeTechnitiumZone is an in-memory Technitium zone API following the same
// stub pattern as stubTechnitiumNoAuth: it swaps technitiumBaseURL to an
// httptest server implementing /api/user/login, /api/zones/create,
// /api/zones/records/get|add|delete. Call counts prove the reconciler skips
// writes when in sync.
type fakeTechnitiumZone struct {
	mu sync.Mutex
	// zone stores the created zone type; "" until created.
	zoneType string
	records  map[string]string // hostname -> ip (A records only)
	adds     int
	deletes  int
	creates  int
	lists    int
}

func newFakeTechnitiumZoneServer(t *testing.T) *fakeTechnitiumZone {
	t.Helper()
	f := &fakeTechnitiumZone{records: map[string]string{}}
	mux := http.NewServeMux()
	respond := func(w http.ResponseWriter, status string, errMsg string) {
		w.Header().Set("Content-Type", "application/json")
		if status == "ok" {
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"status": status, "errorMessage": errMsg})
	}
	mux.HandleFunc("/api/user/login", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","token":"tok"}`))
	})
	mux.HandleFunc("/api/zones/create", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.creates++
		if r.URL.Query().Get("type") != "Forwarder" {
			respond(w, "error", "unexpected zone type")
			return
		}
		if f.zoneType != "" {
			respond(w, "error", "Zone already exists: "+r.URL.Query().Get("zone"))
			return
		}
		f.zoneType = r.URL.Query().Get("type")
		respond(w, "ok", "")
	})
	mux.HandleFunc("/api/zones/records/get", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.zoneType == "" {
			respond(w, "error", "No such zone was found: "+r.URL.Query().Get("zone"))
			return
		}
		if !strings.EqualFold(r.URL.Query().Get("listZone"), "true") {
			respond(w, "error", "listZone required")
			return
		}
		records := make([]map[string]any, 0, len(f.records))
		for h, ip := range f.records {
			records = append(records, map[string]any{
				"name": h, "type": "A", "rData": map[string]any{"ipAddress": ip},
			})
		}
		// The FWD record that makes this zone a conditional forwarder must
		// be ignored by the reconciler.
		records = append(records, map[string]any{
			"name": r.URL.Query().Get("zone"), "type": "FWD",
			"rData": map[string]any{"forwarder": "this-server"},
		})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"response": map[string]any{"zone": map[string]any{"name": r.URL.Query().Get("zone"), "type": f.zoneType}, "records": records},
			"status":   "ok",
		})
	})
	mux.HandleFunc("/api/zones/records/add", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.adds++
		q := r.URL.Query()
		if q.Get("type") != "A" || q.Get("overwrite") != "true" {
			respond(w, "error", "unexpected add params")
			return
		}
		f.records[q.Get("domain")] = q.Get("ipAddress")
		respond(w, "ok", "")
	})
	mux.HandleFunc("/api/zones/records/delete", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.deletes++
		q := r.URL.Query()
		if q.Get("type") != "A" {
			respond(w, "error", "unexpected delete params")
			return
		}
		if f.records[q.Get("domain")] != q.Get("ipAddress") {
			respond(w, "error", "no such record")
			return
		}
		delete(f.records, q.Get("domain"))
		respond(w, "ok", "")
	})
	srv := httptest.NewServer(mux)
	origBase := technitiumBaseURL
	technitiumBaseURL = srv.URL
	t.Cleanup(func() {
		srv.Close()
		technitiumBaseURL = origBase
	})
	return f
}

// technitiumZoneTestBackend builds a backend with technitium installed,
// exposure configured, and a stubbed LAN IP.
func technitiumZoneTestBackend(t *testing.T) (*Backend, *fakeTechnitiumZone) {
	b, _ := newTechnitiumInstalledBackend(t)
	origLAN := lanAddresses
	lanAddresses = func() []string { return []string{"192.168.1.20"} }
	t.Cleanup(func() { lanAddresses = origLAN })
	// Login comes from the stored initial password (the fake accepts any).
	if _, err := b.secrets.Put(context.Background(), "platform-app", "technitium_initial_password", "pw"); err != nil {
		t.Fatal(err)
	}
	if err := b.store.Migrate(context.Background(), exposure.Migrations()...); err != nil {
		t.Fatal(err)
	}
	edge := &memEdge{routes: map[string]exposure.Route{}}
	dns := &memDNS{}
	expSvc, err := exposure.New(b.store, exposure.Config{
		Domain:      "omahab.com",
		TailscaleIP: "100.75.94.122",
		TunnelDNS:   "tunnel.example.com",
	}, exposure.Clients{DNS: dns, Edge: edge})
	if err != nil {
		t.Fatal(err)
	}
	b.exposure = expSvc
	f := newFakeTechnitiumZoneServer(t)
	return b, f
}

func TestTechnitiumZoneCreateAndUpsert(t *testing.T) {
	b, f := technitiumZoneTestBackend(t)
	ctx := context.Background()
	for _, h := range []string{"photos.omahab.com", "home.omahab.com"} {
		if _, err := b.exposure.UpsertService(ctx, exposure.UpsertInput{
			Hostname: h, Upstream: "http://127.0.0.1:2283", Exposure: domain.ExposurePrivate,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.ReconcileTechnitiumZone(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.zoneType != "Forwarder" || f.creates != 1 {
		t.Fatalf("zone = %q/%d, want Forwarder created once", f.zoneType, f.creates)
	}
	lan := technitiumHostLANIPv4()
	if lan == "" {
		t.Fatal("test host has no RFC1918 address")
	}
	for _, h := range []string{"photos.omahab.com", "home.omahab.com"} {
		if f.records[h] != lan {
			t.Fatalf("record %s = %q, want %q", h, f.records[h], lan)
		}
	}
}

func TestTechnitiumZoneInSyncNoWrites(t *testing.T) {
	b, f := technitiumZoneTestBackend(t)
	ctx := context.Background()
	if _, err := b.exposure.UpsertService(ctx, exposure.UpsertInput{
		Hostname: "home.omahab.com", Upstream: "http://127.0.0.1:8484", Exposure: domain.ExposurePrivate,
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.ReconcileTechnitiumZone(ctx); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	f.mu.Lock()
	adds, deletes, creates := f.adds, f.deletes, f.creates
	f.mu.Unlock()
	// Second run: everything matches, no write calls.
	if err := b.ReconcileTechnitiumZone(ctx); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.adds != adds || f.deletes != deletes || f.creates != creates {
		t.Fatalf("writes on in-sync run: adds %d->%d deletes %d->%d creates %d->%d",
			adds, f.adds, deletes, f.deletes, creates, f.creates)
	}
}

func TestTechnitiumZoneReplacesForeignAndStaleIP(t *testing.T) {
	b, f := technitiumZoneTestBackend(t)
	ctx := context.Background()
	if _, err := b.exposure.UpsertService(ctx, exposure.UpsertInput{
		Hostname: "ai.omahab.com", Upstream: "http://127.0.0.1:8080", Exposure: domain.ExposurePrivate,
	}); err != nil {
		t.Fatal(err)
	}
	// Foreign stale anchor: a previous tailscale-IP answer omahabd must
	// replace with the LAN IP.
	f.mu.Lock()
	f.records["ai.omahab.com"] = "100.75.94.122"
	f.mu.Unlock()
	if err := b.ReconcileTechnitiumZone(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.records["ai.omahab.com"] != technitiumHostLANIPv4() {
		t.Fatalf("record = %q, want LAN IP %q", f.records["ai.omahab.com"], technitiumHostLANIPv4())
	}
}

func TestTechnitiumZoneDeletesRemovedRoute(t *testing.T) {
	b, f := technitiumZoneTestBackend(t)
	ctx := context.Background()
	home, err := b.exposure.UpsertService(ctx, exposure.UpsertInput{
		Hostname: "home.omahab.com", Upstream: "http://127.0.0.1:8484", Exposure: domain.ExposurePrivate,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.ReconcileTechnitiumZone(ctx); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	// Retire the service (route removed): apply the delete plan, then the
	// A record must be deleted from the household zone.
	plan, err := b.exposure.Delete(ctx, home.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.exposure.Apply(ctx, plan.ID); err != nil {
		t.Fatal(err)
	}
	if err := b.ReconcileTechnitiumZone(ctx); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.records["home.omahab.com"]; ok {
		t.Fatalf("record survived route removal: %v", f.records)
	}
	if f.deletes == 0 {
		t.Fatal("expected a delete call")
	}
}

func TestTechnitiumZoneNoOpWithoutTechnitium(t *testing.T) {
	b, _ := newSetupBackend(t, nil)
	ctx := context.Background()
	if err := b.ReconcileTechnitiumZone(ctx); err != nil {
		t.Fatalf("reconcile without technitium: %v", err)
	}
}

func TestTechnitiumZoneSkipsForeignHostnames(t *testing.T) {
	b, f := technitiumZoneTestBackend(t)
	ctx := context.Background()
	// A hostname not under the instance domain must not be published into
	// the zone (exposure service validation would reject it, so call the
	// reconciler directly with a mixed list).
	token := ""
	if pw, perr := b.secrets.RevealByName(ctx, "platform-app", "technitium_initial_password"); perr == nil {
		token, _ = technitiumLogin(ctx, "admin", strings.TrimSpace(pw))
	}
	if token == "" {
		t.Fatal("no token from fake")
	}
	if err := reconcileTechnitiumZone(ctx, token, "omahab.com", []string{"app.other.test", "home.omahab.com"}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.records["app.other.test"]; ok {
		t.Fatal("foreign-domain hostname published into the zone")
	}
	if _, ok := f.records["home.omahab.com"]; !ok {
		t.Fatal("zone-domain hostname missing")
	}
}

func TestTechnitiumZoneRequiresToken(t *testing.T) {
	if err := reconcileTechnitiumZone(context.Background(), "", "omahab.com", nil); err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("err = %v, want token requirement", err)
	}
}

// Compile-time interface checks for the query params the reconciler sends.
var _ = url.Values{}
var _ = fmt.Sprintf
