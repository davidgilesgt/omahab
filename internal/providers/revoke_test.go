package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

// Regression: LiteLLM /key/delete matches rows by token or key_alias — never
// by the key_id returned from /key/generate. Sending only the key_id 200s a
// silent no-op, leaving a "deleted" key valid. Revoke must include the alias.
func TestRevokeVirtualKeySendsAlias(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Keys []string `json:"keys"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode delete payload: %v", err)
		}
		got = body.Keys
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	g := &litellmGateway{httpClient: srv.Client(), baseURL: srv.URL, masterKey: "test-master"}
	if err := g.RevokeVirtualKey(context.Background(), "litellm-key-id-123", "hermes-qa-probe"); err != nil {
		t.Fatalf("RevokeVirtualKey: %v", err)
	}
	if !slices.Contains(got, "hermes-qa-probe") {
		t.Fatalf("delete payload missing key_alias, got %q", got)
	}
}
