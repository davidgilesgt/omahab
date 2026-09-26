package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omahab/omahab/internal/controlplane"
)

// TestBearerAuthRequiresTokenEverywhere pins the fail-closed boundary: LAN,
// tailnet, and WAN sources all receive 401 on admin routes without a token.
// There is no network-based bypass; the panel bearer token is required from
// every source address.
func TestBearerAuthRequiresTokenEverywhere(t *testing.T) {
	backend := newRealBackend(t, nil)
	srv := newRealServer(t, backend)

	anonPaths := []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/network/status", ""},
		{http.MethodGet, "/api/v1/secrets", ""},
		{http.MethodGet, "/api/v1/users", ""},
		{http.MethodGet, "/api/v1/model-keys", ""},
		{http.MethodGet, "/api/v1/hermes/mcp-token", ""},
		{http.MethodPost, "/api/v1/hermes/mcp-token/rotate", "{}"},
		{http.MethodGet, "/api/v1/applications", ""},
		{http.MethodGet, "/api/v1/setup", ""},
	}
	for _, remote := range []string{"192.168.1.10:54321", "100.89.20.15:54321", "100.0.0.1:54321", "203.0.113.9:54321"} {
		for _, tc := range anonPaths {
			var body *strings.Reader
			if tc.body == "" {
				body = strings.NewReader("")
			} else {
				body = strings.NewReader(tc.body)
			}
			req := httptest.NewRequest(tc.method, tc.path, body)
			req.RemoteAddr = remote
			if tc.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s %s %s without token = %d, body %s, want 401", remote, tc.method, tc.path, rec.Code, rec.Body.String())
			}
		}
	}

	// The same callers with the panel bearer token get through.
	for _, tc := range anonPaths {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req.RemoteAddr = "192.168.1.10:54321"
		req.Header.Set("Authorization", "Bearer test-token")
		if tc.body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code == http.StatusUnauthorized {
			t.Errorf("%s %s with token = 401, body %s, want authorized", tc.method, tc.path, rec.Body.String())
		}
	}

	bad := httptest.NewRequest(http.MethodGet, "/api/v1/network/status", nil)
	bad.RemoteAddr = "192.168.1.10:54321"
	bad.Header.Set("Authorization", "Bearer wrong-token")
	badRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(badRec, bad)
	if badRec.Code != http.StatusUnauthorized {
		t.Fatalf("lan wrong token = %d, body %s, want 401", badRec.Code, badRec.Body.String())
	}
}

// TestBearerAuthFailsClosedWithoutToken pins fail-closed startup: when no
// panel token is configured, admin routes reject every caller with 401
// instead of serving openly.
func TestBearerAuthFailsClosedWithoutToken(t *testing.T) {
	backend := newRealBackend(t, nil)
	srv, err := New(Config{Backend: backend})
	if err != nil {
		t.Fatal(err)
	}
	for _, hdr := range []string{"", "Bearer test-token"} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/secrets", nil)
		req.RemoteAddr = "192.168.1.10:54321"
		if hdr != "" {
			req.Header.Set("Authorization", hdr)
		}
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("no-token server, auth %q = %d, body %s, want 401", hdr, rec.Code, rec.Body.String())
		}
	}
}

// TestBearerAuthVPSRequiresToken pins the VPS side: no LAN/tailnet bypass,
// every source needs the panel token.
func TestBearerAuthVPSRequiresToken(t *testing.T) {
	t.Setenv("OMAHAB_PLACEMENT", "vps")
	backend := newRealBackend(t, nil)
	srv := newRealServer(t, backend)
	for _, remote := range []string{"192.168.1.10:54321", "100.89.20.15:54321", "203.0.113.9:54321"} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/network/status", nil)
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s without token = %d, body %s, want 401", remote, rec.Code, rec.Body.String())
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/network/status", nil)
	req.RemoteAddr = "100.89.20.15:54321"
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("tailnet with token = %d, body %s, want 200", rec.Code, rec.Body.String())
	}
}

func TestNetworkStatusShape(t *testing.T) {
	backend := newRealBackend(t, nil)
	srv := newRealServer(t, backend)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/network/status", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Placement        string `json:"placement"`
		LANClosed        bool   `json:"lan_closed"`
		TailscaleRunning bool   `json:"tailscale_running"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Placement != "lan" && out.Placement != "vps" {
		t.Fatalf("placement = %q, want lan or vps", out.Placement)
	}
}

func TestNetworkAuthRequired(t *testing.T) {
	backend := newRealBackend(t, nil)
	srv := newRealServer(t, backend)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/network/status"},
		{http.MethodPost, "/api/v1/network/close-lan"},
		{http.MethodPost, "/api/v1/network/open-lan"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without token = %d, want 401", tc.method, tc.path, rec.Code)
		}
	}
}

func TestCloseOpenLANHandler(t *testing.T) {
	backend := newRealBackend(t, nil)
	srv := newRealServer(t, backend)
	// Fake the firewall seams: no LAN rules present, restarts are no-ops.
	// The handler test must never exec nft/systemctl or touch the host
	// sentinel, so it runs without escalation on any machine.
	sentinel := filepath.Join(t.TempDir(), "lan-closed")
	restore := controlplane.SetLANSeamsForTest(sentinel,
		func(args ...string) (string, error) { return "", nil },
		func() error { return nil })
	defer restore()
	post := func(path string) (int, bool) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.Header.Set("Authorization", "Bearer test-token")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		var out struct {
			Closed bool `json:"closed"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("POST %s: decode %v: %s", path, err, rec.Body.String())
		}
		return rec.Code, out.Closed
	}
	if code, closed := post("/api/v1/network/close-lan"); code != http.StatusOK || !closed {
		t.Fatalf("close-lan = %d closed=%v, want 200 true", code, closed)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("close-lan did not write sentinel: %v", err)
	}
	if code, closed := post("/api/v1/network/open-lan"); code != http.StatusOK || closed {
		t.Fatalf("open-lan = %d closed=%v, want 200 false", code, closed)
	}
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatalf("open-lan did not clear sentinel: %v", err)
	}
}
