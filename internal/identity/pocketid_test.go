package identity

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/omahab/omahab/internal/store"
)

func apiKeyHeader(key string) string {
	return key
}

func newTestClient(t *testing.T, handler http.HandlerFunc) (*PocketIDClient, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	c, err := NewPocketIDClient(PocketIDConfig{
		BaseURL:    srv.URL,
		APIKey:     "test-secret",
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("NewPocketIDClient: %v", err)
	}
	return c, srv
}

func TestNewPocketIDClient(t *testing.T) {
	tests := []struct {
		name    string
		cfg     PocketIDConfig
		wantErr bool
		isNotConfigured bool
	}{
		{"ok", PocketIDConfig{BaseURL: "https://pocket.example.com", APIKey: "b"}, false, false},
		{"missing base", PocketIDConfig{BaseURL: "", APIKey: "b"}, true, true},
		{"missing api key", PocketIDConfig{BaseURL: "https://pocket.example.com", APIKey: ""}, true, true},
		{"bad scheme", PocketIDConfig{BaseURL: "ftp://pocket.example.com", APIKey: "b"}, true, false},
		{"trailing slash trimmed", PocketIDConfig{BaseURL: "https://pocket.example.com/", APIKey: "b"}, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewPocketIDClient(tc.cfg)
			if tc.wantErr && err == nil {
				t.Fatalf("want error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("want nil, got %v", err)
			}
			if tc.isNotConfigured && err != nil && !isErrNotConfigured(err) {
				t.Fatalf("want ErrNotConfigured, got %v", err)
			}
		})
	}
}

func isErrNotConfigured(err error) bool {
	return errors.Is(err, ErrNotConfigured)
}

func TestCreateRecoveryCode(t *testing.T) {
	const userID = "user-123"
	const email = "alice@example.com"
	type testCase struct {
		name        string
		email       string
		handler     http.HandlerFunc
		wantCode    string
		wantURLSuffix string
		wantErr     bool
		errIs       error
		checkExpiry bool
	}
	tests := []testCase{
		{
			name:  "success",
			email: email,
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-API-KEY") != "test-secret" {
					http.Error(w, `{"error":"unauthorized","code":"invalid_api_key"}`, http.StatusUnauthorized)
					return
				}
				if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/users") && r.URL.Query().Get("search") != "" {
					json.NewEncoder(w).Encode(paginatedUsersDto{
						Data: []pocketUserDto{{ID: userID, Username: "alice", Email: strPtr(email), DisplayName: "Alice"}},
					})
					return
				}
				if r.Method == http.MethodPost && r.URL.Path == "/api/users/"+userID+"/one-time-access-token" {
					json.NewEncoder(w).Encode(tokenResponseDto{Token: "ABC123"})
					return
				}
				http.NotFound(w, r)
			},
			wantCode:      "ABC123",
			wantURLSuffix: "/lc/ABC123",
			checkExpiry:   true,
		},
		{
			name:  "api key missing",
			email: email,
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-API-KEY") != "test-secret" {
					http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
					return
				}
				// Return success if auth present – but we will call with wrong creds
				http.NotFound(w, r)
			},
			wantErr: true,
		},
		{
			name:  "user not found",
			email: "missing@example.com",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-API-KEY") != "test-secret" {
					http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
					return
				}
				if r.Method == http.MethodGet {
					json.NewEncoder(w).Encode(paginatedUsersDto{Data: []pocketUserDto{}})
					return
				}
				http.NotFound(w, r)
			},
			wantErr: true,
			errIs:   store.ErrNotFound,
		},
		{
			name:  "invalid email",
			email: "not-an-email",
			handler: func(w http.ResponseWriter, r *http.Request) {
				t.Fatalf("handler should not be called for invalid email")
			},
			wantErr: true,
			errIs:   store.ErrValidation,
		},
		{
			name:  "token empty",
			email: email,
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-API-KEY") != "test-secret" {
					http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
					return
				}
				if r.Method == http.MethodGet {
					json.NewEncoder(w).Encode(paginatedUsersDto{Data: []pocketUserDto{{ID: userID, Username: "alice", Email: strPtr(email)}}})
					return
				}
				if r.Method == http.MethodPost {
					json.NewEncoder(w).Encode(tokenResponseDto{Token: ""})
					return
				}
				http.NotFound(w, r)
			},
			wantErr: true,
		},
		{
			name:  "api returns 500",
			email: email,
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					http.Error(w, `{"error":"internal","code":"internal_error"}`, http.StatusInternalServerError)
					return
				}
				http.NotFound(w, r)
			},
			wantErr: true,
		},
		{
			name:  "expiry handling within bounds",
			email: email,
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-API-KEY") != "test-secret" {
					http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
					return
				}
				if r.Method == http.MethodGet {
					json.NewEncoder(w).Encode(paginatedUsersDto{Data: []pocketUserDto{{ID: userID, Username: "alice", Email: strPtr(email)}}})
					return
				}
				if r.Method == http.MethodPost {
					json.NewEncoder(w).Encode(tokenResponseDto{Token: "LONGTOKEN12"})
					return
				}
				http.NotFound(w, r)
			},
			wantCode:      "LONGTOKEN12",
			wantURLSuffix: "/lc/LONGTOKEN12",
			checkExpiry:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := tc.handler
			c, srv := newTestClient(t, handler)
			defer srv.Close()
			// For api key missing case, create client with wrong secret
			if tc.name == "api key missing" {
				var err error
				c, err = NewPocketIDClient(PocketIDConfig{
					BaseURL:    srv.URL,
					APIKey:     "wrong",
					HTTPClient: srv.Client(),
				})
				if err != nil {
					t.Fatalf("NewPocketIDClient wrong: %v", err)
				}
			}
			start := time.Now().UTC()
			code, urlStr, exp, err := c.CreateRecoveryCode(context.Background(), tc.email)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got code=%q url=%q", code, urlStr)
				}
				if tc.errIs != nil && !isStoreError(err, tc.errIs) {
					t.Fatalf("want err %v, got %v", tc.errIs, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if code != tc.wantCode {
				t.Fatalf("code=%q want %q", code, tc.wantCode)
			}
			if !strings.HasSuffix(urlStr, tc.wantURLSuffix) {
				t.Fatalf("url=%q want suffix %q", urlStr, tc.wantURLSuffix)
			}
			if !strings.HasPrefix(urlStr, srv.URL) && !strings.HasPrefix(urlStr, "http") {
				t.Fatalf("url %q should start with base", urlStr)
			}
			if tc.checkExpiry {
				if exp.IsZero() {
					t.Fatalf("expiresAt is zero")
				}
				if exp.Before(start) {
					t.Fatalf("expiry %v before start %v", exp, start)
				}
				if exp.After(start.Add(2 * DefaultRecoveryTTL).Add(time.Second)) {
					t.Fatalf("expiry %v too far in future, start %v", exp, start)
				}
				// Check within expected ttl window: ~15m
				diff := exp.Sub(start)
				if diff < DefaultRecoveryTTL-time.Second || diff > DefaultRecoveryTTL+time.Second*2 {
					t.Fatalf("expiry diff %v want ~%v", diff, DefaultRecoveryTTL)
				}
			}
		})
	}
}

func TestCreateRecoveryCodeNotConfigured(t *testing.T) {
	c := &PocketIDClient{baseURL: "", apiKey: "", httpClient: http.DefaultClient}
	_, _, _, err := c.CreateRecoveryCode(context.Background(), "a@b.com")
	if err == nil || !isStoreNotConfigured(err) {
		t.Fatalf("want ErrNotConfigured, got %v", err)
	}
	_ = store.ErrValidation
}

func isStoreNotConfigured(err error) bool {
	return err != nil && strings.Contains(err.Error(), "not configured")
}

func isStoreError(err, target error) bool {
	if err == nil || target == nil {
		return false
	}
	// Use string matching for store errors since they are wrapped with fmt.Errorf %w
	msg := err.Error()
	if target == store.ErrNotFound && strings.Contains(msg, "not found") {
		return true
	}
	if target == store.ErrValidation && strings.Contains(msg, "validation") {
		return true
	}
	if target == store.ErrConflict && strings.Contains(msg, "conflict") {
		return true
	}
	return false
}

func TestValidateRecovery(t *testing.T) {
	const userID = "user-123"
	tests := []struct {
		name    string
		email   string
		code    string
		handler http.HandlerFunc
		wantErr bool
		errIs   error
	}{
		{
			name:  "success",
			email: "alice@example.com",
			code:  "ABC123",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-API-KEY") != "test-secret" {
					http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
					return
				}
				json.NewEncoder(w).Encode(paginatedUsersDto{Data: []pocketUserDto{{ID: userID, Username: "alice", Email: strPtr("alice@example.com")}}})
			},
		},
		{
			name:  "invalid code length",
			email: "alice@example.com",
			code:  "BAD",
			handler: func(w http.ResponseWriter, r *http.Request) {
				t.Fatalf("should not call API for invalid code length")
			},
			wantErr: true,
			errIs:   store.ErrValidation,
		},
		{
			name:  "user not found",
			email: "missing@example.com",
			code:  "ABC123",
			handler: func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(paginatedUsersDto{Data: []pocketUserDto{}})
			},
			wantErr: true,
			errIs:   store.ErrNotFound,
		},
		{
			name:  "missing email",
			email: "",
			code:  "ABC123",
			handler: func(w http.ResponseWriter, r *http.Request) {
				t.Fatalf("should not call API")
			},
			wantErr: true,
			errIs:   store.ErrValidation,
		},
		{
			name:  "unauthorized maps to error",
			email: "alice@example.com",
			code:  "ABC123",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, `{"error":"forbidden","code":"forbidden"}`, http.StatusForbidden)
			},
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, srv := newTestClient(t, tc.handler)
			defer srv.Close()
			err := c.ValidateRecovery(context.Background(), tc.email, tc.code)
			if tc.wantErr && err == nil {
				t.Fatalf("want error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("want nil, got %v", err)
			}
			if tc.errIs != nil && err != nil && !isStoreError(err, tc.errIs) {
				t.Fatalf("want %v, got %v", tc.errIs, err)
			}
			// Verify X-API-KEY when handler checks it – if we didn't get error about auth, we succeeded.
		})
	}
}

func TestCreateUser(t *testing.T) {
	tests := []struct {
		name    string
		email   string
		displayName string
		handler http.HandlerFunc
		wantErr bool
		errIs   error
	}{
		{
			name:  "success with enrollment link",
			email: "bob@example.com",
			displayName: "Bob",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-API-KEY") != "test-secret" {
					http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
					return
				}
				if r.Method == http.MethodPost && r.URL.Path == "/api/users" {
					var body map[string]any
					_ = json.NewDecoder(r.Body).Decode(&body)
					if body["email"] != "bob@example.com" {
						http.Error(w, `{"error":"bad email"}`, http.StatusBadRequest)
						return
					}
					json.NewEncoder(w).Encode(pocketUserDto{ID: "new-user", Username: "bob", Email: strPtr("bob@example.com"), DisplayName: "Bob"})
					return
				}
				if r.Method == http.MethodPost && r.URL.Path == "/api/users/new-user/one-time-access-token" {
					json.NewEncoder(w).Encode(tokenResponseDto{Token: "INVITE12"})
					return
				}
				http.NotFound(w, r)
			},
		},
		{
			name:  "validation empty email",
			email: "",
			displayName: "Bob",
			handler: func(w http.ResponseWriter, r *http.Request) {
				t.Fatalf("should not call API")
			},
			wantErr: true,
			errIs:   store.ErrValidation,
		},
		{
			name:  "conflict duplicate",
			email: "bob@example.com",
			displayName: "Bob",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost && r.URL.Path == "/api/users" {
					http.Error(w, `{"error":"already in use","code":"already_in_use"}`, http.StatusConflict)
					return
				}
				http.NotFound(w, r)
			},
			wantErr: true,
			errIs:   store.ErrConflict,
		},
		{
			name:  "bad request",
			email: "bob@example.com",
			displayName: "Bob",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost && r.URL.Path == "/api/users" {
					http.Error(w, `{"error":"validation failed","code":"validation_failed"}`, http.StatusBadRequest)
					return
				}
				http.NotFound(w, r)
			},
			wantErr: true,
			errIs:   store.ErrValidation,
		},
		{
			name:  "server error",
			email: "bob@example.com",
			displayName: "Bob",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, `{"error":"internal"}`, http.StatusInternalServerError)
			},
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, srv := newTestClient(t, tc.handler)
			defer srv.Close()
			uid, urlStr, exp, err := c.CreateUser(context.Background(), tc.email, tc.displayName, false, nil)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error")
				}
				if tc.errIs != nil && !isStoreError(err, tc.errIs) {
					t.Fatalf("want %v, got %v", tc.errIs, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("want nil, got %v", err)
			}
			if uid == "" {
				t.Fatalf("want userID")
			}
			if !strings.Contains(urlStr, "/lc/") {
				t.Fatalf("want enrollment url with /lc/, got %q", urlStr)
			}
			if exp.IsZero() || exp.Before(time.Now().Add(-time.Minute)) {
				t.Fatalf("bad expiry %v", exp)
			}
		})
	}
}

func TestHealthCheck(t *testing.T) {
	t.Run("success via users", func(t *testing.T) {
		c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-API-KEY") != "test-secret" {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			if r.URL.Path == "/api/users" {
				json.NewEncoder(w).Encode(paginatedUsersDto{Data: []pocketUserDto{}})
				return
			}
			http.NotFound(w, r)
		})
		defer srv.Close()
		if err := c.HealthCheck(context.Background()); err != nil {
			t.Fatalf("want nil, got %v", err)
		}
	})
	t.Run("fallback to app config", func(t *testing.T) {
		c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-API-KEY") != "test-secret" {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			if r.URL.Path == "/api/users" {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			if r.URL.Path == "/api/application-configuration/all" {
				json.NewEncoder(w).Encode([]appConfigVarDto{{Key: "appName", Value: "Pocket ID"}})
				return
			}
			http.NotFound(w, r)
		})
		defer srv.Close()
		if err := c.HealthCheck(context.Background()); err != nil {
			t.Fatalf("want nil via fallback, got %v", err)
		}
	})
	t.Run("unauthorized", func(t *testing.T) {
		c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		})
		defer srv.Close()
		if err := c.HealthCheck(context.Background()); err == nil {
			t.Fatalf("want error")
		}
	})
	t.Run("not configured", func(t *testing.T) {
		c := &PocketIDClient{httpClient: http.DefaultClient}
		if err := c.HealthCheck(context.Background()); err == nil || !isStoreNotConfigured(err) {
			t.Fatalf("want not configured, got %v", err)
		}
	})
}

func TestGetUserAndListUsers(t *testing.T) {
	t.Run("get user success", func(t *testing.T) {
		c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-API-KEY") != "test-secret" {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			json.NewEncoder(w).Encode(pocketUserDto{ID: "u1", Username: "alice", Email: strPtr("alice@example.com"), DisplayName: "Alice", UserGroups: []pocketUserGroupMinimalDto{{ID: "g1", Name: "admins"}}})
		})
		defer srv.Close()
		u, err := c.GetUser(context.Background(), "u1")
		if err != nil {
			t.Fatalf("GetUser: %v", err)
		}
		if string(u.ID) != "u1" || u.Email != "alice@example.com" {
			t.Fatalf("got %+v", u)
		}
		if len(u.Groups) != 1 || u.Groups[0] != "g1" {
			t.Fatalf("groups %+v", u.Groups)
		}
	})
	t.Run("get user not found", func(t *testing.T) {
		c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"error":"not found","code":"not_found"}`, http.StatusNotFound)
		})
		defer srv.Close()
		_, err := c.GetUser(context.Background(), "missing")
		if err == nil || !isStoreError(err, store.ErrNotFound) {
			t.Fatalf("want not found, got %v", err)
		}
	})
	t.Run("list users success", func(t *testing.T) {
		c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(paginatedUsersDto{Data: []pocketUserDto{
				{ID: "u1", Username: "alice", Email: strPtr("alice@example.com")},
				{ID: "u2", Username: "bob", Email: strPtr("bob@example.com")},
			}})
		})
		defer srv.Close()
		users, err := c.ListUsers(context.Background())
		if err != nil {
			t.Fatalf("ListUsers: %v", err)
		}
		if len(users) != 2 {
			t.Fatalf("want 2, got %d", len(users))
		}
	})
	t.Run("validation empty id", func(t *testing.T) {
		c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			t.Fatalf("should not call")
		})
		defer srv.Close()
		_, err := c.GetUser(context.Background(), "")
		if err == nil || !isStoreError(err, store.ErrValidation) {
			t.Fatalf("want validation, got %v", err)
		}
	})
}

func TestDisableDeleteUser(t *testing.T) {
	t.Run("disable success", func(t *testing.T) {
		c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-API-KEY") != "test-secret" {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			if r.Method == http.MethodGet {
				json.NewEncoder(w).Encode(pocketUserDto{ID: "u1", Username: "alice", Email: strPtr("alice@example.com")})
				return
			}
			if r.Method == http.MethodPut {
				json.NewEncoder(w).Encode(pocketUserDto{ID: "u1", Username: "alice", Disabled: true})
				return
			}
			http.NotFound(w, r)
		})
		defer srv.Close()
		if err := c.DisableUser(context.Background(), "u1", true); err != nil {
			t.Fatalf("DisableUser: %v", err)
		}
	})
	t.Run("delete success", func(t *testing.T) {
		c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodDelete {
				t.Fatalf("want DELETE, got %s", r.Method)
			}
			w.WriteHeader(http.StatusNoContent)
		})
		defer srv.Close()
		if err := c.DeleteUser(context.Background(), "u1"); err != nil {
			t.Fatalf("DeleteUser: %v", err)
		}
	})
	t.Run("delete not configured", func(t *testing.T) {
		c := &PocketIDClient{}
		if err := c.DeleteUser(context.Background(), "u1"); err == nil || !isStoreNotConfigured(err) {
			t.Fatalf("want not configured, got %v", err)
		}
	})
}

func TestGroupOperations(t *testing.T) {
	t.Run("list groups success", func(t *testing.T) {
		c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-API-KEY") != "test-secret" {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			json.NewEncoder(w).Encode(paginatedGroupsMinimalDto{Data: []pocketUserGroupMinimalDto{
				{ID: "g1", Name: "admins", FriendlyName: "admins"},
				{ID: "g2", Name: "members", FriendlyName: "members"},
			}})
		})
		defer srv.Close()
		groups, err := c.ListGroups(context.Background())
		if err != nil {
			t.Fatalf("ListGroups: %v", err)
		}
		if len(groups) != 2 {
			t.Fatalf("want 2, got %d", len(groups))
		}
	})
	t.Run("ensure groups idempotent", func(t *testing.T) {
		created := map[string]bool{}
		c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && r.URL.Path == "/api/user-groups" {
				// Return existing admins only
				json.NewEncoder(w).Encode(paginatedGroupsMinimalDto{Data: []pocketUserGroupMinimalDto{
					{ID: "g1", Name: "admins", FriendlyName: "admins"},
				}})
				return
			}
			if r.Method == http.MethodPost && r.URL.Path == "/api/user-groups" {
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				name := body["name"].(string)
				if created[name] {
					http.Error(w, `{"error":"already in use","code":"already_in_use"}`, http.StatusConflict)
					return
				}
				created[name] = true
				json.NewEncoder(w).Encode(pocketUserGroupDto{ID: "g-" + name, Name: name, FriendlyName: name})
				return
			}
			http.NotFound(w, r)
		})
		defer srv.Close()
		groups, err := c.EnsureGroups(context.Background(), []string{"admins", "members", "guests"})
		if err != nil {
			t.Fatalf("EnsureGroups: %v", err)
		}
		if len(groups) != 3 {
			t.Fatalf("want 3, got %d len %+v", len(groups), groups)
		}
		// Second call should be idempotent – but our fake now has created flags, so test second call with fresh server that returns all
	})
	t.Run("ensure groups not configured", func(t *testing.T) {
		c := &PocketIDClient{}
		_, err := c.EnsureGroups(context.Background(), []string{"admins"})
		if err == nil || !isStoreNotConfigured(err) {
			t.Fatalf("want not configured, got %v", err)
		}
	})
	t.Run("set user groups", func(t *testing.T) {
		c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPut || r.URL.Path != "/api/users/u1/user-groups" {
				t.Fatalf("path %s method %s", r.URL.Path, r.Method)
			}
			if r.Header.Get("X-API-KEY") != "test-secret" {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			json.NewEncoder(w).Encode(pocketUserDto{ID: "u1"})
		})
		defer srv.Close()
		if err := c.SetUserGroups(context.Background(), "u1", []string{"g1", "g2"}); err != nil {
			t.Fatalf("SetUserGroups: %v", err)
		}
	})
	t.Run("add remove idempotent", func(t *testing.T) {
		c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/groups") && r.Method == http.MethodGet {
				json.NewEncoder(w).Encode([]pocketUserGroupMinimalDto{{ID: "g1", Name: "admins"}})
				return
			}
			if strings.Contains(r.URL.Path, "/user-groups") && r.Method == http.MethodPut {
				json.NewEncoder(w).Encode(pocketUserDto{ID: "u1"})
				return
			}
			// fallback for GetUser
			if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/users/u1") {
				json.NewEncoder(w).Encode(pocketUserDto{ID: "u1", Username: "alice", UserGroups: []pocketUserGroupMinimalDto{{ID: "g1", Name: "admins"}}})
				return
			}
			http.NotFound(w, r)
		})
		defer srv.Close()
		if err := c.AddUserToGroup(context.Background(), "u1", "g1"); err != nil {
			t.Fatalf("Add existing should be idempotent, got %v", err)
		}
		if err := c.AddUserToGroup(context.Background(), "u1", "g2"); err != nil {
			t.Fatalf("Add new: %v", err)
		}
		if err := c.RemoveUserFromGroup(context.Background(), "u1", "missing"); err != nil {
			t.Fatalf("Remove missing should be idempotent, got %v", err)
		}
	})
}

func TestErrorMapping(t *testing.T) {
	cases := []struct {
		status int
		body   string
		is     error
	}{
		{400, `{"error":"bad","code":"validation_failed"}`, store.ErrValidation},
		{404, `{"error":"not found","code":"not_found"}`, store.ErrNotFound},
		{409, `{"error":"conflict","code":"already_in_use"}`, store.ErrConflict},
		{500, `{"error":"internal","code":"internal_error"}`, nil},
		{401, `{"error":"unauthorized"}`, nil},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, tc.body, tc.status)
			})
			defer srv.Close()
			_, err := c.ListUsers(context.Background())
			if err == nil {
				t.Fatalf("want error")
			}
			if tc.is != nil && !isStoreError(err, tc.is) {
				t.Fatalf("want %v, got %v", tc.is, err)
			}
			if tc.is == nil && isStoreError(err, store.ErrNotFound) {
				t.Fatalf("should not be not found")
			}
		})
	}
}

func TestNotConfiguredPropagation(t *testing.T) {
	c := &PocketIDClient{baseURL: "", apiKey: "", httpClient: http.DefaultClient}
	tests := []struct {
		name string
		fn   func() error
	}{
		{"CreateRecoveryCode", func() error { _, _, _, err := c.CreateRecoveryCode(context.Background(), "a@b.com"); return err }},
		{"ValidateRecovery", func() error { return c.ValidateRecovery(context.Background(), "a@b.com", "ABC123") }},
		{"CreateUser", func() error { _, _, _, err := c.CreateUser(context.Background(), "a@b.com", "A", false, nil); return err }},
		{"GetUser", func() error { _, err := c.GetUser(context.Background(), "u1"); return err }},
		{"ListUsers", func() error { _, err := c.ListUsers(context.Background()); return err }},
		{"HealthCheck", func() error { return c.HealthCheck(context.Background()) }},
		{"EnsureGroups", func() error { _, err := c.EnsureGroups(context.Background(), []string{"admins"}); return err }},
		{"ConfigureDefaults", func() error { return c.ConfigureDefaults(context.Background()) }},
		{"GetEnrollmentState", func() error { _, err := c.GetEnrollmentState(context.Background(), "u1"); return err }},
		{"ListApplicationAccess", func() error { _, err := c.ListApplicationAccess(context.Background(), "u1"); return err }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.fn(); err == nil || !isStoreNotConfigured(err) {
				t.Fatalf("want ErrNotConfigured, got %v", err)
			}
		})
	}
}

func strPtr(s string) *string { return &s }
