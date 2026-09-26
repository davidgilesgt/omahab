package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func doRotateHermes(t *testing.T, srv *Server, body *strings.Reader, contentType bool) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *strings.Reader
	if body == nil {
		rdr = strings.NewReader("")
	} else {
		rdr = body
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/hermes/mcp-token/rotate", rdr)
	req.Header.Set("Authorization", "Bearer test-token")
	if contentType || (body != nil && body.Len() > 0) {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func decodeHermesToken(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode token: %v: %s", err, rec.Body.String())
	}
	if strings.TrimSpace(out.Token) == "" {
		t.Fatalf("empty token in %s", rec.Body.String())
	}
	return out.Token
}

// TestHermesRotateRejectsMalformedBody pins F-hermes-rotate-no-validation:
// a malformed body fails with 400 and leaves the active token untouched,
// while {} and an empty body rotate normally.
func TestHermesRotateRejectsMalformedBody(t *testing.T) {
	backend := newRealBackend(t, nil)
	srv := newRealServer(t, backend)

	tok1 := ""
	if rec := doRotateHermes(t, srv, strings.NewReader("{}"), true); rec.Code != http.StatusOK {
		t.Fatalf("rotate {{}} = %d, body %s, want 200", rec.Code, rec.Body.String())
	} else {
		tok1 = decodeHermesToken(t, rec)
	}

	if rec := doRotateHermes(t, srv, strings.NewReader("{bad"), true); rec.Code != http.StatusBadRequest {
		t.Fatalf("rotate {{bad}} = %d, body %s, want 400", rec.Code, rec.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/hermes/mcp-token", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get token = %d, body %s, want 200", rec.Code, rec.Body.String())
	}
	if got := decodeHermesToken(t, rec); got != tok1 {
		t.Fatalf("token changed on malformed rotate: got %q want %q", got, tok1)
	}

	if rec := doRotateHermes(t, srv, nil, false); rec.Code != http.StatusOK {
		t.Fatalf("rotate empty body = %d, body %s, want 200", rec.Code, rec.Body.String())
	} else if tok2 := decodeHermesToken(t, rec); tok2 == tok1 {
		t.Fatalf("empty-body rotation did not change token")
	}
}
