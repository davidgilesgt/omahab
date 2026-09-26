package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/omahab/omahab/internal/apitypes"
)

// TestListGroupsRoute pins the Pocket ID group directory contract: admin
// bearer required, and with Pocket ID configured the response carries the
// actual group IDs, names, and friendly names in server order.
func TestListGroupsRoute(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/user-groups" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"id": "g-admins", "name": "admins", "friendlyName": "Administrators"},
				{"id": "g-members", "name": "members", "friendlyName": "Members"},
			},
		})
	}))
	defer fake.Close()

	t.Setenv("OMAHAB_POCKETID_API_KEY", "test-secret")
	t.Setenv("OMAHAB_POCKETID_URL", fake.URL)
	backend := newRealBackend(t, nil)
	srv := newRealServer(t, backend)

	anon := httptest.NewRequest(http.MethodGet, "/api/v1/groups", nil)
	anonRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(anonRec, anon)
	if anonRec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous groups = %d, body %s, want 401", anonRec.Code, anonRec.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/groups", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("authorized groups = %d, body %s, want 200", rec.Code, rec.Body.String())
	}
	var groups []apitypes.Group
	if err := json.Unmarshal(rec.Body.Bytes(), &groups); err != nil {
		t.Fatalf("decode groups: %v: %s", err, rec.Body.String())
	}
	want := []apitypes.Group{
		{ID: "g-admins", Name: "admins", FriendlyName: "Administrators"},
		{ID: "g-members", Name: "members", FriendlyName: "Members"},
	}
	if len(groups) != len(want) {
		t.Fatalf("groups = %+v, want %+v", groups, want)
	}
	for i := range want {
		if groups[i] != want[i] {
			t.Fatalf("groups[%d] = %+v, want %+v", i, groups[i], want[i])
		}
	}
}
