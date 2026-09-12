package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omahab/omahab/internal/controlplane"
	"github.com/omahab/omahab/internal/sshkeys"
)

// stubGate implements BootstrapGate for handler-plumbing tests.
type stubGate struct {
	active   bool
	claimErr error
	lastCode string
	lastIP   string
}

func (g *stubGate) Claim(code, sourceIP string) error {
	g.lastCode, g.lastIP = code, sourceIP
	return g.claimErr
}
func (g *stubGate) SSHKeys(string, []string) (int, error) { return 0, nil }
func (g *stubGate) ListSSHKeys(context.Context) ([]sshkeys.SSHKey, error) {
	return nil, nil
}
func (g *stubGate) AdminUsername() string { return "omahab" }
func (g *stubGate) TailscaleUp() (string, error) { return "", nil }
func (g *stubGate) TailscaleStatus() (bool, string, string, error) {
	return false, "", "", nil
}
func (g *stubGate) Complete() error { return nil }
func (g *stubGate) Active() bool    { return g.active }

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

func TestBootstrapClaimEmptyCodeReachesGate(t *testing.T) {
	backend := newRealBackend(t, nil)
	gate := &stubGate{active: true}
	srv := newRealServer(t, backend, func(c *Config) { c.Bootstrap = gate })

	// Empty code from a LAN source must reach the gate (not 400).
	req := httptest.NewRequest(http.MethodPost, "/api/bootstrap/claim", strings.NewReader(`{"code":""}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.168.1.10:54321"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("lan empty claim = %d, body %s, want 200", rec.Code, rec.Body.String())
	}
	if gate.lastCode != "" || gate.lastIP != "192.168.1.10" {
		t.Fatalf("gate saw code=%q ip=%q, want empty + 192.168.1.10", gate.lastCode, gate.lastIP)
	}

	// Gate rejection maps to 401.
	gate.claimErr = errUnauthorized("invalid code")
	req2 := httptest.NewRequest(http.MethodPost, "/api/bootstrap/claim", strings.NewReader(`{"code":""}`))
	req2.Header.Set("Content-Type", "application/json")
	req2.RemoteAddr = "203.0.113.9:54321"
	rec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("rejected claim = %d, body %s, want 401", rec2.Code, rec2.Body.String())
	}
}
