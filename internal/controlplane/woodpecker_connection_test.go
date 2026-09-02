package controlplane

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omahab/omahab/internal/store"
)

func TestWoodpeckerConnectionPendingWithoutSecrets(t *testing.T) {
	ctx := context.Background()
	b, _ := newSetupBackend(t, nil)
	st, err := b.GetSetupStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c := checkByID(t, st.Checks, "woodpecker_connection")
	if c.Status != "pending" {
		t.Fatalf("want pending without secrets, got %s %q", c.Status, c.Detail)
	}
	if c.Owner != "operator" {
		t.Fatalf("owner want operator got %q", c.Owner)
	}
	if !strings.Contains(c.Label, "Woodpecker") {
		t.Fatalf("label want Woodpecker got %q", c.Label)
	}
}

func TestWoodpeckerConnectionOKWithMocks(t *testing.T) {
	ctx := context.Background()
	b, _ := newSetupBackend(t, nil)

	var userResp = map[string]any{"login": "alice", "admin": true}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.Contains(auth, "Bearer") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/api/user" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(userResp)
			return
		}
		if r.URL.Path == "/api/agents" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 1, "name": "local", "last_contact": int64(9999999999)}})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	b.woodpeckerBaseURLOverride = srv.URL
	b.woodpeckerHTTPClient = srv.Client()

	sock := filepath.Join(os.TempDir(), "omahab-podman-test.sock")
	_ = os.Remove(sock)
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	defer os.Remove(sock)
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 4096)
				_, _ = c.Read(buf)
				resp := "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nContent-Type: text/plain\r\n\r\nOK"
				_, _ = c.Write([]byte(resp))
			}(conn)
		}
	}()
	b.podmanSocketPath = sock

	if err := upsertSecret(ctx, b.secrets, "platform-app", "woodpecker_token", "test-token"); err != nil {
		t.Fatal(err)
	}
	if err := upsertSecret(ctx, b.secrets, "platform-app", "woodpecker_admin_username", "alice"); err != nil {
		t.Fatal(err)
	}
	if v, err := b.secrets.RevealByName(ctx, "platform-app", "woodpecker_token"); err != nil || v != "test-token" {
		t.Fatalf("token not stored %v %q", err, v)
	}

	st, err := b.GetSetupStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c := checkByID(t, st.Checks, "woodpecker_connection")
	if c.Status != "ok" {
		t.Fatalf("want ok with mocks, got %s %q", c.Status, c.Detail)
	}
	if strings.Contains(c.Detail, "test-token") {
		t.Fatalf("detail leaked token: %q", c.Detail)
	}

	// username mismatch
	userResp["login"] = "bob"
	st, _ = b.GetSetupStatus(ctx)
	c = checkByID(t, st.Checks, "woodpecker_connection")
	if c.Status != "failed" {
		t.Fatalf("want failed on username mismatch, got %s", c.Status)
	}
	if strings.Contains(c.Detail, "test-token") {
		t.Fatalf("leaked token on mismatch")
	}
	userResp["login"] = "alice"

	// admin false
	userResp["admin"] = false
	st, _ = b.GetSetupStatus(ctx)
	c = checkByID(t, st.Checks, "woodpecker_connection")
	if c.Status != "failed" {
		t.Fatalf("want failed on not admin, got %s", c.Status)
	}
	userResp["admin"] = true

	// socket failure
	b.podmanSocketPath = "/nonexistent/socket"
	st, _ = b.GetSetupStatus(ctx)
	c = checkByID(t, st.Checks, "woodpecker_connection")
	if c.Status != "failed" {
		t.Fatalf("want failed on socket missing, got %s", c.Status)
	}
	b.podmanSocketPath = sock

	// agent missing
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/user" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(userResp)
			return
		}
		if r.URL.Path == "/api/agents" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("[]"))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv2.Close()
	b.woodpeckerBaseURLOverride = srv2.URL
	b.woodpeckerHTTPClient = srv2.Client()
	st, _ = b.GetSetupStatus(ctx)
	c = checkByID(t, st.Checks, "woodpecker_connection")
	if c.Status != "failed" {
		t.Fatalf("want failed on no agents, got %s %q", c.Status, c.Detail)
	}
}

func TestWoodpeckerConnectionRedactsTokenDetail(t *testing.T) {
	ctx := context.Background()
	b, _ := newSetupBackend(t, nil)
	if err := upsertSecret(ctx, b.secrets, "platform-app", "woodpecker_token", "supersecrettoken1234567890thatislongandcontainssecretword"); err != nil {
		t.Fatalf("put token: %v", err)
	}
	if err := upsertSecret(ctx, b.secrets, "platform-app", "woodpecker_admin_username", "alice"); err != nil {
		t.Fatalf("put username: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()
	b.woodpeckerBaseURLOverride = srv.URL
	b.woodpeckerHTTPClient = srv.Client()
	b.podmanSocketPath = "/tmp/nonexistent"

	st, err := b.GetSetupStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c := checkByID(t, st.Checks, "woodpecker_connection")
	if c.Status != "failed" {
		t.Fatalf("want failed, got %s", c.Status)
	}
	if strings.Contains(c.Detail, "supersecrettoken") {
		t.Fatalf("detail leaked token: %q", c.Detail)
	}
	if strings.Contains(strings.ToLower(c.Detail), "supersecrettoken") {
		t.Fatalf("redaction failed")
	}
}

var _ = store.ErrNotFound
