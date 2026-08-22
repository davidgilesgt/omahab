package identity

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/omahab/omahab/internal/store"
)

func TestSeedDefaultGroups(t *testing.T) {
	t.Run("success idempotent", func(t *testing.T) {
		calls := 0
		c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != basicAuthHeader("test-id", "test-secret") {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			if r.Method == http.MethodGet && r.URL.Path == "/api/user-groups" {
				// First call returns empty, later returns seeded
				if calls == 0 {
					calls++
					json.NewEncoder(w).Encode(paginatedGroupsMinimalDto{Data: []pocketUserGroupMinimalDto{}})
					return
				}
				json.NewEncoder(w).Encode(paginatedGroupsMinimalDto{Data: []pocketUserGroupMinimalDto{
					{ID: "g1", Name: "admins", FriendlyName: "admins"},
					{ID: "g2", Name: "members", FriendlyName: "members"},
					{ID: "g3", Name: "guests", FriendlyName: "guests"},
				}})
				return
			}
			if r.Method == http.MethodPost && r.URL.Path == "/api/user-groups" {
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				name := body["name"].(string)
				json.NewEncoder(w).Encode(pocketUserGroupDto{ID: "g-" + name, Name: name, FriendlyName: name})
				return
			}
			http.NotFound(w, r)
		})
		defer srv.Close()
		if err := c.SeedDefaultGroups(context.Background()); err != nil {
			t.Fatalf("SeedDefaultGroups: %v", err)
		}
		// Second call should be idempotent and still succeed (now groups exist)
		if err := c.SeedDefaultGroups(context.Background()); err != nil {
			t.Fatalf("second SeedDefaultGroups: %v", err)
		}
	})
	t.Run("auth failure", func(t *testing.T) {
		c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		})
		defer srv.Close()
		if err := c.SeedDefaultGroups(context.Background()); err == nil {
			t.Fatalf("want error")
		}
	})
	t.Run("not configured", func(t *testing.T) {
		c := &PocketIDClient{}
		if err := c.SeedDefaultGroups(context.Background()); err == nil || !isErrNotConfigured(err) {
			t.Fatalf("want not configured, got %v", err)
		}
	})
}

func TestConfigureDefaults(t *testing.T) {
	t.Run("success disables email OTP and enforces webauthn", func(t *testing.T) {
		var gotPut map[string]string
		c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != basicAuthHeader("test-id", "test-secret") {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			if r.Method == http.MethodGet && r.URL.Path == "/api/application-configuration/all" {
				json.NewEncoder(w).Encode([]appConfigVarDto{
					{Key: "appName", Value: "Pocket ID"},
					{Key: "sessionDuration", Value: "60"},
					{Key: "emailOneTimeAccessAsAdminEnabled", Value: "true"},
					{Key: "emailOneTimeAccessAsUnauthenticatedEnabled", Value: "true"},
					{Key: "webauthnUserVerification", Value: "preferred"},
					{Key: "allowUserSignups", Value: "disabled"},
				})
				return
			}
			if r.Method == http.MethodPut && r.URL.Path == "/api/application-configuration" {
				_ = json.NewDecoder(r.Body).Decode(&gotPut)
				// Respond with updated vars
				var out []appConfigVarDto
				for k, v := range gotPut {
					out = append(out, appConfigVarDto{Key: k, Value: v})
				}
				json.NewEncoder(w).Encode(out)
				return
			}
			http.NotFound(w, r)
		})
		defer srv.Close()
		if err := c.ConfigureDefaults(context.Background()); err != nil {
			t.Fatalf("ConfigureDefaults: %v", err)
		}
		if gotPut["emailOneTimeAccessAsAdminEnabled"] != "false" {
			t.Fatalf("want admin OTP false, got %q", gotPut["emailOneTimeAccessAsAdminEnabled"])
		}
		if gotPut["emailOneTimeAccessAsUnauthenticatedEnabled"] != "false" {
			t.Fatalf("want unauth OTP false, got %q", gotPut["emailOneTimeAccessAsUnauthenticatedEnabled"])
		}
		if gotPut["webauthnUserVerification"] != "required" {
			t.Fatalf("want webauthn required, got %q", gotPut["webauthnUserVerification"])
		}
		if gotPut["appName"] == "" {
			t.Fatalf("want appName preserved")
		}
	})
	t.Run("fetch failure propagates", func(t *testing.T) {
		c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				http.Error(w, `{"error":"internal"}`, http.StatusInternalServerError)
				return
			}
			http.NotFound(w, r)
		})
		defer srv.Close()
		if err := c.ConfigureDefaults(context.Background()); err == nil {
			t.Fatalf("want error")
		}
	})
	t.Run("update failure maps correctly", func(t *testing.T) {
		c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				json.NewEncoder(w).Encode([]appConfigVarDto{{Key: "appName", Value: "Pocket ID"}})
				return
			}
			if r.Method == http.MethodPut {
				http.Error(w, `{"error":"validation failed","code":"validation_failed"}`, http.StatusBadRequest)
				return
			}
			http.NotFound(w, r)
		})
		defer srv.Close()
		err := c.ConfigureDefaults(context.Background())
		if err == nil || !isStoreError(err, store.ErrValidation) {
			t.Fatalf("want validation, got %v", err)
		}
	})
	t.Run("not configured", func(t *testing.T) {
		c := &PocketIDClient{}
		if err := c.ConfigureDefaults(context.Background()); err == nil || !isErrNotConfigured(err) {
			t.Fatalf("want not configured, got %v", err)
		}
	})
	t.Run("basic auth verified", func(t *testing.T) {
		c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != basicAuthHeader("test-id", "test-secret") {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			// succeed for both endpoints
			if r.Method == http.MethodGet {
				json.NewEncoder(w).Encode([]appConfigVarDto{{Key: "appName", Value: "Pocket ID"}})
				return
			}
			if r.Method == http.MethodPut {
				json.NewEncoder(w).Encode([]appConfigVarDto{})
				return
			}
			http.NotFound(w, r)
		})
		defer srv.Close()
		if err := c.ConfigureDefaults(context.Background()); err != nil {
			t.Fatalf("ConfigureDefaults auth: %v", err)
		}
	})
}

func TestGetEnrollmentState(t *testing.T) {
	t.Run("has passkey", func(t *testing.T) {
		c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != basicAuthHeader("test-id", "test-secret") {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			json.NewEncoder(w).Encode([]webauthnCredentialDto{
				{ID: "cred1", CreatedAt: "2024-01-01T00:00:00Z"},
				{ID: "cred2", CreatedAt: "2024-01-02T00:00:00Z"},
			})
		})
		defer srv.Close()
		st, err := c.GetEnrollmentState(context.Background(), "user-123")
		if err != nil {
			t.Fatalf("GetEnrollmentState: %v", err)
		}
		if !st.HasPasskey || st.CredentialCount != 2 {
			t.Fatalf("want has passkey 2, got %+v", st)
		}
		if st.UserID != "user-123" {
			t.Fatalf("userID %q", st.UserID)
		}
	})
	t.Run("no passkey", func(t *testing.T) {
		c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode([]webauthnCredentialDto{})
		})
		defer srv.Close()
		st, err := c.GetEnrollmentState(context.Background(), "user-123")
		if err != nil {
			t.Fatalf("GetEnrollmentState: %v", err)
		}
		if st.HasPasskey || st.CredentialCount != 0 {
			t.Fatalf("want no passkey, got %+v", st)
		}
	})
	t.Run("not found maps", func(t *testing.T) {
		c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"error":"not found","code":"not_found"}`, http.StatusNotFound)
		})
		defer srv.Close()
		_, err := c.GetEnrollmentState(context.Background(), "missing")
		if err == nil || !isStoreError(err, store.ErrNotFound) {
			t.Fatalf("want not found, got %v", err)
		}
	})
	t.Run("validation empty id", func(t *testing.T) {
		c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			t.Fatalf("should not call")
		})
		defer srv.Close()
		_, err := c.GetEnrollmentState(context.Background(), "")
		if err == nil || !isStoreError(err, store.ErrValidation) {
			t.Fatalf("want validation, got %v", err)
		}
	})
	t.Run("auth header required", func(t *testing.T) {
		c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != basicAuthHeader("test-id", "test-secret") {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			json.NewEncoder(w).Encode([]webauthnCredentialDto{})
		})
		defer srv.Close()
		if _, err := c.GetEnrollmentState(context.Background(), "u1"); err != nil {
			t.Fatalf("want success with auth, got %v", err)
		}
	})
	t.Run("not configured", func(t *testing.T) {
		c := &PocketIDClient{}
		_, err := c.GetEnrollmentState(context.Background(), "u1")
		if err == nil || !isErrNotConfigured(err) {
			t.Fatalf("want not configured, got %v", err)
		}
	})
}

func TestListApplicationAccess(t *testing.T) {
	t.Run("deduplicates clients across groups", func(t *testing.T) {
		c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != basicAuthHeader("test-id", "test-secret") {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			// GetUserGroups fallback via GetUser
			if r.Method == http.MethodGet && r.URL.Path == "/api/users/u1/groups" {
				// Simulate 404 so client falls back to user fetch
				http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
				return
			}
			if r.Method == http.MethodGet && r.URL.Path == "/api/users/u1" {
				json.NewEncoder(w).Encode(pocketUserDto{
					ID: "u1", Username: "alice",
					UserGroups: []pocketUserGroupMinimalDto{
						{ID: "g1", Name: "admins"},
						{ID: "g2", Name: "members"},
					},
				})
				return
			}
			if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/user-groups/") {
				if strings.HasSuffix(r.URL.Path, "g1") {
					json.NewEncoder(w).Encode(pocketUserGroupDto{
						ID: "g1", Name: "admins",
						AllowedOidcClients: []pocketOidcClientDto{
							{ID: "c1", Name: "Forgejo"},
							{ID: "c2", Name: "Immich"},
						},
					})
					return
				}
				if strings.HasSuffix(r.URL.Path, "g2") {
					json.NewEncoder(w).Encode(pocketUserGroupDto{
						ID: "g2", Name: "members",
						AllowedOidcClients: []pocketOidcClientDto{
							{ID: "c1", Name: "Forgejo"}, // duplicate
							{ID: "c3", Name: "Paperless"},
						},
					})
					return
				}
			}
			http.NotFound(w, r)
		})
		defer srv.Close()
		access, err := c.ListApplicationAccess(context.Background(), "u1")
		if err != nil {
			t.Fatalf("ListApplicationAccess: %v", err)
		}
		if len(access) != 3 {
			t.Fatalf("want 3 deduped, got %d %+v", len(access), access)
		}
		// Check all expected IDs present
		ids := map[string]bool{}
		for _, a := range access {
			ids[a.ID] = true
		}
		for _, want := range []string{"c1", "c2", "c3"} {
			if !ids[want] {
				t.Fatalf("want %s in %+v", want, ids)
			}
		}
	})
	t.Run("empty groups returns empty", func(t *testing.T) {
		c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/users/u2" {
				json.NewEncoder(w).Encode(pocketUserDto{ID: "u2", Username: "bob", UserGroups: []pocketUserGroupMinimalDto{}})
				return
			}
			if r.URL.Path == "/api/users/u2/groups" {
				json.NewEncoder(w).Encode([]pocketUserGroupMinimalDto{})
				return
			}
			http.NotFound(w, r)
		})
		defer srv.Close()
		access, err := c.ListApplicationAccess(context.Background(), "u2")
		if err != nil {
			t.Fatalf("ListApplicationAccess empty: %v", err)
		}
		if len(access) != 0 {
			t.Fatalf("want 0, got %d", len(access))
		}
	})
	t.Run("not configured", func(t *testing.T) {
		c := &PocketIDClient{}
		_, err := c.ListApplicationAccess(context.Background(), "u1")
		if err == nil || !isErrNotConfigured(err) {
			t.Fatalf("want not configured, got %v", err)
		}
	})
	t.Run("validation", func(t *testing.T) {
		c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			t.Fatalf("should not call")
		})
		defer srv.Close()
		_, err := c.ListApplicationAccess(context.Background(), "")
		if err == nil || !isStoreError(err, store.ErrValidation) {
			t.Fatalf("want validation, got %v", err)
		}
	})
}

func TestProvisioningGroupAddRemove(t *testing.T) {
	t.Run("add and remove via provisioning", func(t *testing.T) {
		c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != basicAuthHeader("test-id", "test-secret") {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			// GetUserGroups via fallback
			if r.Method == http.MethodGet && r.URL.Path == "/api/users/u1/groups" {
				json.NewEncoder(w).Encode([]pocketUserGroupMinimalDto{{ID: "g1", Name: "admins"}})
				return
			}
			if r.Method == http.MethodGet && r.URL.Path == "/api/users/u1" {
				json.NewEncoder(w).Encode(pocketUserDto{ID: "u1", UserGroups: []pocketUserGroupMinimalDto{{ID: "g1", Name: "admins"}}})
				return
			}
			if r.Method == http.MethodPut && r.URL.Path == "/api/users/u1/user-groups" {
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				// Return success
				json.NewEncoder(w).Encode(pocketUserDto{ID: "u1"})
				return
			}
			http.NotFound(w, r)
		})
		defer srv.Close()
		if err := c.AddUserToGroup(context.Background(), "u1", "g2"); err != nil {
			t.Fatalf("AddUserToGroup: %v", err)
		}
		if err := c.RemoveUserFromGroup(context.Background(), "u1", "g1"); err != nil {
			t.Fatalf("RemoveUserFromGroup: %v", err)
		}
	})
}

func TestEnsureGroupAndUpdateAppAccess(t *testing.T) {
	t.Run("ensure single group", func(t *testing.T) {
		c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && r.URL.Path == "/api/user-groups" {
				json.NewEncoder(w).Encode(paginatedGroupsMinimalDto{Data: []pocketUserGroupMinimalDto{}})
				return
			}
			if r.Method == http.MethodPost {
				json.NewEncoder(w).Encode(pocketUserGroupDto{ID: "gid", Name: "admins", FriendlyName: "admins"})
				return
			}
			http.NotFound(w, r)
		})
		defer srv.Close()
		g, err := c.EnsureGroup(context.Background(), "admins")
		if err != nil {
			t.Fatalf("EnsureGroup: %v", err)
		}
		if g.Name != "admins" {
			t.Fatalf("want admins, got %+v", g)
		}
	})
	t.Run("update app access", func(t *testing.T) {
		c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != basicAuthHeader("test-id", "test-secret") {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			if r.Method == http.MethodPut && r.URL.Path == "/api/user-groups/g1/allowed-oidc-clients" {
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				json.NewEncoder(w).Encode(pocketUserGroupDto{ID: "g1"})
				return
			}
			http.NotFound(w, r)
		})
		defer srv.Close()
		if err := c.UpdateApplicationAccessForGroup(context.Background(), "g1", []string{"c1", "c2"}); err != nil {
			t.Fatalf("UpdateApplicationAccessForGroup: %v", err)
		}
	})
}

func TestCreateUserWithInvite(t *testing.T) {
	c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/users" {
			json.NewEncoder(w).Encode(pocketUserDto{ID: "uid", Username: "alice", Email: strPtr("alice@example.com")})
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/api/users/uid/one-time-access-token" {
			json.NewEncoder(w).Encode(tokenResponseDto{Token: "INVITE123456"})
			return
		}
		http.NotFound(w, r)
	})
	defer srv.Close()
	uid, urlStr, expStr, err := c.CreateUserWithInvite(context.Background(), "alice@example.com", "Alice", false, nil)
	if err != nil {
		t.Fatalf("CreateUserWithInvite: %v", err)
	}
	if uid != "uid" {
		t.Fatalf("uid %q", uid)
	}
	if !strings.Contains(urlStr, "/lc/") {
		t.Fatalf("url %q", urlStr)
	}
	if expStr == "" {
		t.Fatalf("want expiry string")
	}
}
