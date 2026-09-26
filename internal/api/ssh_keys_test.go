package api

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/omahab/omahab/internal/config"
)

func doDeleteSSHKey(srv *Server, auth, body string, contentType bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/system/ssh-keys", strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if contentType {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// TestSystemSSHKeyDeleteRoute pins F-ssh-revoke-route-missing at the auth
// layer: the DELETE handler is registered behind bearer auth with strict
// JSON validation before any mutation. Before registration every case below
// answered 405 method not allowed.
func TestSystemSSHKeyDeleteRoute(t *testing.T) {
	backend := newRealBackend(t, nil)
	srv := newRealServer(t, backend)

	if rec := doDeleteSSHKey(srv, "", `{"fingerprint":"SHA256:test"}`, true); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous DELETE = %d, body %s, want 401", rec.Code, rec.Body.String())
	}
	if rec := doDeleteSSHKey(srv, "Bearer wrong-token", `{"fingerprint":"SHA256:test"}`, true); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong-token DELETE = %d, body %s, want 401", rec.Code, rec.Body.String())
	}
	if rec := doDeleteSSHKey(srv, "Bearer test-token", "{bad", true); rec.Code != http.StatusBadRequest {
		t.Errorf("malformed DELETE = %d, body %s, want 400", rec.Code, rec.Body.String())
	}
	if rec := doDeleteSSHKey(srv, "Bearer test-token", "{}", true); rec.Code != http.StatusBadRequest {
		t.Errorf("empty-fingerprint DELETE = %d, body %s, want 400", rec.Code, rec.Body.String())
	}
}

// mintSSHKeyLine generates a fresh ed25519 authorized_keys line plus its
// SHA256 fingerprint. The private half is discarded immediately.
func mintSSHKeyLine(t *testing.T, comment string) (line, fingerprint string) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	line = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " " + comment
	return line, ssh.FingerprintSHA256(sshPub)
}

// isolatedAdminUser returns the existing unprivileged "nobody" account with
// OMAHAB_SSH_HOME redirecting all admin-account files (authorized_keys and
// the best-effort provisioned panel token) into a temp dir. Nothing is
// created or deleted on the host: no useradd/userdel, no writes to real
// homes or /etc/passwd, as root or otherwise.
func isolatedAdminUser(t *testing.T, home string) string {
	t.Helper()
	t.Setenv("OMAHAB_SSH_HOME", home)
	if _, err := user.Lookup("nobody"); err != nil {
		t.Skipf("no isolated OS user available: %v", err)
	}
	return "nobody"
}

func postSSHKeys(t *testing.T, srv *Server, lines []string) int {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"keys": lines})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/system/ssh-keys", strings.NewReader(string(raw)))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST ssh-keys = %d, body %s, want 200", rec.Code, rec.Body.String())
	}
	var out struct {
		Added int `json:"added"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode add response: %v: %s", err, rec.Body.String())
	}
	return out.Added
}

func listSSHKeyFingerprints(t *testing.T, srv *Server) []string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/system/ssh-keys", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET ssh-keys = %d, body %s, want 200", rec.Code, rec.Body.String())
	}
	var out struct {
		Items []struct {
			Fingerprint string `json:"fingerprint"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode list response: %v: %s", err, rec.Body.String())
	}
	fps := make([]string, 0, len(out.Items))
	for _, it := range out.Items {
		fps = append(fps, it.Fingerprint)
	}
	return fps
}

func deleteSSHKey(t *testing.T, srv *Server, fingerprint string, confirmLast bool) int {
	t.Helper()
	body := fmt.Sprintf(`{"fingerprint":%q`, fingerprint)
	if confirmLast {
		body += `,"confirm_last":true`
	}
	body += `}`
	rec := doDeleteSSHKey(srv, "Bearer test-token", body, true)
	return rec.Code
}

// TestSystemSSHKeyDeleteFlow is the end-to-end revocation proof for
// F-ssh-revoke-route-missing: add two keys through POST, delete one through
// the API, verify authorized_keys no longer contains it while the other
// survives, then verify re-delete 404s, last-key-without-confirm 409s, and
// confirmed last-key removal empties the file.
func TestSystemSSHKeyDeleteFlow(t *testing.T) {
	home := t.TempDir()
	admin := isolatedAdminUser(t, home)
	backend := newRealBackend(t, func(cfg *config.Config) { cfg.AdminUser = admin })
	srv := newRealServer(t, backend)

	lineA, fpA := mintSSHKeyLine(t, "flow-a")
	lineB, fpB := mintSSHKeyLine(t, "flow-b")
	if added := postSSHKeys(t, srv, []string{lineA, lineB}); added != 2 {
		t.Fatalf("added = %d, want 2", added)
	}

	akPath := filepath.Join(home, ".ssh", "authorized_keys")
	readAuthorizedKeys := func() string {
		t.Helper()
		data, err := os.ReadFile(akPath)
		if err != nil {
			t.Fatalf("read authorized_keys: %v", err)
		}
		return string(data)
	}

	if code := deleteSSHKey(t, srv, fpA, false); code != http.StatusNoContent {
		t.Fatalf("DELETE fpA = %d, want 204", code)
	}
	if fps := listSSHKeyFingerprints(t, srv); len(fps) != 1 || fps[0] != fpB {
		t.Fatalf("fingerprints after delete = %q, want [%q]", fps, fpB)
	}
	if content := readAuthorizedKeys(); strings.Contains(content, strings.TrimSpace(lineA)) || !strings.Contains(content, strings.TrimSpace(lineB)) {
		t.Fatalf("authorized_keys not revoked correctly:\n%s", content)
	}

	if code := deleteSSHKey(t, srv, fpA, false); code != http.StatusNotFound {
		t.Fatalf("re-DELETE fpA = %d, want 404", code)
	}
	if code := deleteSSHKey(t, srv, fpB, false); code != http.StatusConflict {
		t.Fatalf("DELETE last key without confirm = %d, want 409", code)
	}
	if code := deleteSSHKey(t, srv, fpB, true); code != http.StatusNoContent {
		t.Fatalf("DELETE last key with confirm = %d, want 204", code)
	}
	if fps := listSSHKeyFingerprints(t, srv); len(fps) != 0 {
		t.Fatalf("fingerprints after full revoke = %q, want empty", fps)
	}
}
