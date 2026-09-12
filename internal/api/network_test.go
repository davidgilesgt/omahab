package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/omahab/omahab/internal/controlplane"
)

// TestBearerAuthLANBypass pins the LAN trust boundary: on lan placement,
// LAN sources reach admin routes without a token while WAN sources get 401.
func TestBearerAuthLANBypass(t *testing.T) {
	backend := newRealBackend(t, nil)
	srv := newRealServer(t, backend)

	lan := httptest.NewRequest(http.MethodGet, "/api/v1/network/status", nil)
	lan.RemoteAddr = "192.168.1.10:54321"
	lanRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(lanRec, lan)
	if lanRec.Code != http.StatusOK {
		t.Fatalf("lan without token = %d, body %s, want 200", lanRec.Code, lanRec.Body.String())
	}

	wan := httptest.NewRequest(http.MethodGet, "/api/v1/network/status", nil)
	wan.RemoteAddr = "203.0.113.9:54321"
	wanRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(wanRec, wan)
	if wanRec.Code != http.StatusUnauthorized {
		t.Fatalf("wan without token = %d, body %s, want 401", wanRec.Code, wanRec.Body.String())
	}

	bad := httptest.NewRequest(http.MethodGet, "/api/v1/network/status", nil)
	bad.RemoteAddr = "203.0.113.9:54321"
	bad.Header.Set("Authorization", "Bearer wrong-token")
	badRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(badRec, bad)
	if badRec.Code != http.StatusUnauthorized {
		t.Fatalf("wan wrong token = %d, body %s, want 401", badRec.Code, badRec.Body.String())
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
