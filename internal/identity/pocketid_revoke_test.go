package identity

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/omahab/omahab/internal/store"
)
func TestGetUserByEmail(t *testing.T) {
	t.Run("returns disabled flag", func(t *testing.T) {
		c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-API-KEY") != "test-secret" {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/users") && r.URL.Query().Get("search") != "" {
				json.NewEncoder(w).Encode(paginatedUsersDto{
					Data: []pocketUserDto{{ID: "u9", Username: "bob", Email: strPtr("Bob@Example.com"), DisplayName: "Bob", Disabled: true}},
				})
				return
			}
			http.NotFound(w, r)
		})
		defer srv.Close()
		u, err := c.GetUserByEmail(context.Background(), "bob@example.com")
		if err != nil {
			t.Fatalf("GetUserByEmail: %v", err)
		}
		if !u.Disabled {
			t.Fatalf("want disabled, got %+v", u)
		}
		if u.Email != "bob@example.com" {
			t.Fatalf("want lowercased email, got %q", u.Email)
		}
	})
t.Run("not found maps", func(t *testing.T) {
		c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(paginatedUsersDto{})
		})
		defer srv.Close()
		if _, err := c.GetUserByEmail(context.Background(), "missing@example.com"); err == nil || !isStoreError(err, store.ErrNotFound) {
			t.Fatalf("want not found, got %v", err)
		}
	})
t.Run("validation", func(t *testing.T) {
		c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			http.NotFound(w, r)
		})
		defer srv.Close()
		if _, err := c.GetUserByEmail(context.Background(), ""); err == nil {
			t.Fatalf("want error for empty email")
		}
		bare := &PocketIDClient{}
		if _, err := bare.GetUserByEmail(context.Background(), "a@b.c"); err == nil || !isErrNotConfigured(err) {
			t.Fatalf("want not configured, got %v", err)
		}
	})
}

func TestConsumeOneTimeToken(t *testing.T) {
	newBurnClient := func(t *testing.T, status int, body string, gotKey *string) (*PocketIDClient, *httptest.Server) {
		t.Helper()
		return newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/one-time-access-token/") {
				if gotKey != nil {
					*gotKey = r.Header.Get("X-API-KEY")
				}
				w.WriteHeader(status)
				_, _ = w.Write([]byte(body))
				return
			}
			http.NotFound(w, r)
		})
	}
	t.Run("consumed on 2xx without api key", func(t *testing.T) {
		var gotKey string
		c, srv := newBurnClient(t, http.StatusOK, `{"token":"session-abc"}`, &gotKey)
		defer srv.Close()
		if err := c.ConsumeOneTimeToken(context.Background(), "CODEA123456"); err != nil {
			t.Fatalf("ConsumeOneTimeToken: %v", err)
		}
		if gotKey != "" {
			t.Fatalf("redeem must not send the admin api key, sent %q", gotKey)
		}
	})
	t.Run("already-dead token is nil", func(t *testing.T) {
		for name, tc := range map[string]struct {
			status int
			body   string
		}{
			"unknown token 404": {http.StatusNotFound, `{"error":"one-time access token not found"}`},
			"expired token 410": {http.StatusGone, `{"error":"token expired"}`},
			"invalid token 400": {http.StatusBadRequest, `{"error":"invalid token"}`},
		} {
			t.Run(name, func(t *testing.T) {
				c, srv := newBurnClient(t, tc.status, tc.body, nil)
				defer srv.Close()
				if err := c.ConsumeOneTimeToken(context.Background(), "OLDCODE12345"); err != nil {
					t.Fatalf("dead token must yield nil, got %v", err)
				}
			})
		}
	})
	t.Run("unconfirmed on generic 404 and server errors", func(t *testing.T) {
		c, srv := newBurnClient(t, http.StatusNotFound, `{"error":"not found"}`, nil)
		defer srv.Close()
		if err := c.ConsumeOneTimeToken(context.Background(), "CODEA123456"); err == nil {
			t.Fatalf("generic 404 must not be mistaken for a dead token")
		}
		c2, srv2 := newBurnClient(t, http.StatusInternalServerError, `boom`, nil)
		defer srv2.Close()
		if err := c2.ConsumeOneTimeToken(context.Background(), "CODEA123456"); err == nil {
			t.Fatalf("want error on 500")
		}
	})
	t.Run("validation and not configured", func(t *testing.T) {
		c, srv := newBurnClient(t, http.StatusOK, `{}`, nil)
		defer srv.Close()
		if err := c.ConsumeOneTimeToken(context.Background(), ""); err == nil {
			t.Fatalf("want error for empty code")
		}
		bare := &PocketIDClient{}
		if err := bare.ConsumeOneTimeToken(context.Background(), "CODEA123456"); err == nil || !isErrNotConfigured(err) {
			t.Fatalf("want not configured, got %v", err)
		}
	})
}

func TestRevokeUserSessions(t *testing.T) {
	c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	defer srv.Close()
	err := c.RevokeUserSessions(context.Background(), "u1")
	if err == nil || !errors.Is(err, ErrRevocationNotSupported) {
		t.Fatalf("want ErrRevocationNotSupported, got %v", err)
	}
	if err := c.RevokeUserSessions(context.Background(), ""); err == nil {
		t.Fatalf("want error for empty user")
	}
	bare := &PocketIDClient{}
	if err := bare.RevokeUserSessions(context.Background(), "u1"); err == nil || !isErrNotConfigured(err) {
		t.Fatalf("want not configured, got %v", err)
	}
}
