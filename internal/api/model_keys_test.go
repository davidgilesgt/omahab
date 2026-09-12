package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// DELETE must remove the row: the key disappears from the list and a second
// DELETE of the same id is 404, not a repeated 204.
func TestDeleteModelKey_RemovesRow(t *testing.T) {
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
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created: %v", err)
	}
	if created.ID == "" {
		t.Fatalf("created key has empty id: %s", rec.Body.String())
	}

	delPath := "/api/v1/model-keys/" + created.ID
	if rec := do(http.MethodDelete, delPath, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("first delete status = %d, want 204, body = %s", rec.Code, rec.Body.String())
	}

	listRec := do(http.MethodGet, "/api/v1/model-keys", "")
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
