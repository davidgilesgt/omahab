package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// stubLiteLLMGateway stands in for the LiteLLM control plane at the fixed
// loopback address the backend dials (http://127.0.0.1:4000). It issues real
// sk- bearers for POST /key/generate and records POST /key/delete
// revocations, so the test exercises the genuine gateway-issued key lifecycle
// — never a local-only omk_ token — without network access.
type stubLiteLLMGateway struct {
	t       *testing.T
	mu      sync.Mutex
	revoked []string
}

func newStubLiteLLMGateway(t *testing.T) *stubLiteLLMGateway {
	t.Helper()
	gw := &stubLiteLLMGateway{t: t}
	ln, err := net.Listen("tcp", "127.0.0.1:4000")
	if err != nil {
		t.Skipf("stub gateway port 127.0.0.1:4000 unavailable: %v (never touch a real gateway from tests)", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(gw.serve)}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return gw
}

func (g *stubLiteLLMGateway) serve(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/key/generate":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 1<<20))
		token := "sk-test-" + randHex(g.t, 16)
		id := "key-test-" + randHex(g.t, 8)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"key": token, "key_id": id})
	case "/key/delete":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Keys []string `json:"keys"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body)
		g.mu.Lock()
		g.revoked = append(g.revoked, body.Keys...)
		g.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	default:
		http.NotFound(w, r)
	}
}

func (g *stubLiteLLMGateway) sawRevoke(key string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, k := range g.revoked {
		if k == key {
			return true
		}
	}
	return false
}

func randHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(b)
}

// DELETE must remove the row: the key disappears from the list and a second
// DELETE of the same id is 404, not a repeated 204.
func TestDeleteModelKey_RemovesRow(t *testing.T) {
	// The API issues only real LiteLLM gateway virtual keys (sk- bearers).
	// Give the backend a master key and a stub gateway on its fixed loopback
	// address so creation succeeds end to end.
	t.Setenv("LITELLM_MASTER_KEY", "test-master-key")
	gw := newStubLiteLLMGateway(t)

	backend := newRealBackend(t, nil)
	srv := newRealServer(t, backend)

	do := func(method, path, body string) *httptest.ResponseRecorder {
		var req *http.Request
		if body == "" {
			req = httptest.NewRequest(method, path, nil)
		} else {
			req = httptest.NewRequest(method, path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Authorization", "Bearer test-token")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}

	createBody := `{"name":"test-key","owner_kind":"harness","owner_id":"test-owner","scopes":["omahab/fast"]}`
	rec := do(http.MethodPost, "/api/v1/model-keys", createBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201, body = %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID        string   `json:"id"`
		Name      string   `json:"name"`
		Key       string   `json:"key"`
		KeyPrefix string   `json:"key_prefix"`
		Scopes    []string `json:"scopes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created: %v", err)
	}
	if created.ID == "" {
		t.Fatalf("created key has empty id: %s", rec.Body.String())
	}
	// The issued bearer must be a gateway-accepted sk- key, never a
	// local-only omk_ token; only metadata (hash/prefix) is persisted.
	if !strings.HasPrefix(created.Key, "sk-") {
		t.Fatalf("created key = %q, want gateway-issued sk- bearer", created.Key)
	}
	if strings.HasPrefix(created.Key, "omk_") {
		t.Fatalf("created key = %q, must not be a local omk_ token", created.Key)
	}
	if len(created.Key) < 12 || created.KeyPrefix != created.Key[:12] {
		t.Fatalf("key_prefix = %q, want first 12 chars of issued key", created.KeyPrefix)
	}

	listRec := do(http.MethodGet, "/api/v1/model-keys", "")
	if listRec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200, body = %s", listRec.Code, listRec.Body.String())
	}
	if !strings.Contains(listRec.Body.String(), created.ID) {
		t.Fatalf("created key %q missing from list: %s", created.ID, listRec.Body.String())
	}

	delPath := "/api/v1/model-keys/" + created.ID
	if rec := do(http.MethodDelete, delPath, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("first delete status = %d, want 204, body = %s", rec.Code, rec.Body.String())
	}
	// The gateway row itself must be revoked by alias, not just the local
	// metadata row deleted.
	if !gw.sawRevoke("test-key") {
		t.Fatalf("gateway never revoked alias %q", "test-key")
	}

	listRec = do(http.MethodGet, "/api/v1/model-keys", "")
	if listRec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200, body = %s", listRec.Code, listRec.Body.String())
	}
	if strings.Contains(listRec.Body.String(), created.ID) {
		t.Fatalf("deleted key %q still listed: %s", created.ID, listRec.Body.String())
	}

	if rec := do(http.MethodDelete, delPath, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("second delete status = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}
}
