package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/omahab/omahab/internal/apitypes"
)

func doAuthed(srv *Server, method, target, body string) *httptest.ResponseRecorder {
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, target, nil)
	} else {
		req = httptest.NewRequest(method, target, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func genRecovery(t *testing.T, srv *Server) apitypes.RecoveryKeyMaterial {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/recovery/generate", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("generate status %d, body %s", rec.Code, rec.Body.String())
	}
	var mat apitypes.RecoveryKeyMaterial
	if err := json.Unmarshal(rec.Body.Bytes(), &mat); err != nil {
		t.Fatal(err)
	}
	return mat
}

func TestRecoveryConfirmSavedAckAndChallenge(t *testing.T) {
	backend := newRealBackend(t, nil)
	srv := newRealServer(t, backend)

	// --- saved-ack mode ---
	mat := genRecovery(t, srv)
	ackBody := `{"fingerprint":` + strconv.Quote(mat.Fingerprint) + `,"saved_ack":true}`
	if rec := doAuthed(srv, http.MethodPost, "/api/v1/recovery/confirm", ackBody); rec.Code != http.StatusOK {
		t.Fatalf("saved-ack confirm status %d, body %s", rec.Code, rec.Body.String())
	}

	// --- legacy challenge mode ---
	mat2 := genRecovery(t, srv)
	chalBody := `{"fingerprint":` + strconv.Quote(mat2.Fingerprint) + `,"challenge":{"0":` +
		strconv.Quote(mat2.Phrase[0]) + `,"1":` + strconv.Quote(mat2.Phrase[1]) +
		`,"2":` + strconv.Quote(mat2.Phrase[2]) + `}}`
	if rec := doAuthed(srv, http.MethodPost, "/api/v1/recovery/confirm", chalBody); rec.Code != http.StatusOK {
		t.Fatalf("challenge confirm status %d, body %s", rec.Code, rec.Body.String())
	}

	// --- neither mode: must be rejected ---
	mat3 := genRecovery(t, srv)
	emptyBody := `{"fingerprint":` + strconv.Quote(mat3.Fingerprint) + `}`
	if rec := doAuthed(srv, http.MethodPost, "/api/v1/recovery/confirm", emptyBody); rec.Code == http.StatusOK {
		t.Fatalf("empty confirm should fail, got 200 %s", rec.Body.String())
	}
}

func TestTailscaleV1RoutesAuthAndPresence(t *testing.T) {
	backend := newRealBackend(t, nil)
	srv := newRealServer(t, backend)

	// No bearer -> 401 (proves the routes live inside the bearerAuth group).
	anon := httptest.NewRequest(http.MethodGet, "/api/v1/tailscale/status", nil)
	anonRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(anonRec, anon)
	if anonRec.Code != http.StatusUnauthorized {
		t.Fatalf("anon status = %d, want 401", anonRec.Code)
	}
	anonUp := httptest.NewRequest(http.MethodPost, "/api/v1/tailscale/up", nil)
	anonUpRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(anonUpRec, anonUp)
	if anonUpRec.Code != http.StatusUnauthorized {
		t.Fatalf("anon up = %d, want 401", anonUpRec.Code)
	}

	// With bearer the routes must exist (never 404/405).
	// The live result depends on the machine (200 enrolled-or-URL vs 502
	// when tailscaled is absent), so accept either success or gateway error.
	statusReq := httptest.NewRequest(http.MethodGet, "/api/v1/tailscale/status", nil)
	statusReq.Header.Set("Authorization", "Bearer test-token")
	statusRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(statusRec, statusReq)
	if statusRec.Code != http.StatusOK && statusRec.Code != http.StatusBadGateway {
		t.Fatalf("authed status = %d (%s), want 200 or 502", statusRec.Code, statusRec.Body.String())
	}
	if statusRec.Code == http.StatusOK {
		var st struct {
			Running bool   `json:"running"`
			IP      string `json:"ip"`
			State   string `json:"state"`
		}
		if err := json.Unmarshal(statusRec.Body.Bytes(), &st); err != nil {
			t.Fatalf("status shape: %v (%s)", err, statusRec.Body.String())
		}
	}

	upReq := httptest.NewRequest(http.MethodPost, "/api/v1/tailscale/up", nil)
	upReq.Header.Set("Authorization", "Bearer test-token")
	upRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(upRec, upReq)
	if upRec.Code != http.StatusOK && upRec.Code != http.StatusBadGateway {
		t.Fatalf("authed up = %d (%s), want 200 or 502", upRec.Code, upRec.Body.String())
	}
	if upRec.Code == http.StatusOK && !strings.Contains(upRec.Body.String(), "auth_url") {
		t.Fatalf("up body missing auth_url: %s", upRec.Body.String())
	}
}
