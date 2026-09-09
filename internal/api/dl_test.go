package api

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDL_ServesBinariesAndSHA(t *testing.T) {
	dir := t.TempDir()
	// Create fake binaries.
	files := map[string][]byte{
		"omahab-clientd-linux-amd64":  []byte("linux-amd64-binary"),
		"omahab-clientd-linux-arm64":  []byte("linux-arm64-binary"),
		"omahab-clientd-darwin-arm64": []byte("darwin-arm64-binary"),
		"omahab-clientd-darwin-amd64": []byte("darwin-amd64-binary"),
		"omarchy-plugin.tar.gz":       []byte("fake-tar-gz"),
		"install.sh":                  []byte("#!/bin/sh\necho static-installer\n"),
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	// Generate SHA256SUMS.
	var sums strings.Builder
	for name, data := range files {
		h := sha256.Sum256(data)
		sums.WriteString(hex.EncodeToString(h[:]) + "  " + name + "\n")
	}
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(sums.String()), 0o644); err != nil {
		t.Fatalf("write sums: %v", err)
	}

	backend := newRealBackend(t, nil)
	srv := newRealServer(t, backend, func(c *Config) { c.DLDir = dir })
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Check each file.
	for name, want := range files {
		resp, err := http.Get(ts.URL + "/dl/" + name)
		if err != nil {
			t.Fatalf("GET /dl/%s: %v", name, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("/dl/%s status %d want 200", name, resp.StatusCode)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if name != "install.sh" && string(body) != string(want) {
			t.Fatalf("/dl/%s body mismatch", name)
		}
	}
	// Check SHA256SUMS.
	resp, err := http.Get(ts.URL + "/dl/SHA256SUMS")
	if err != nil {
		t.Fatalf("GET SHA256SUMS: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("SHA256SUMS status %d", resp.StatusCode)
	}
	sumsBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for name, data := range files {
		h := sha256.Sum256(data)
		hexHash := hex.EncodeToString(h[:])
		if !strings.Contains(string(sumsBody), hexHash+"  "+name) {
			t.Fatalf("SHA256SUMS missing %s %s", name, hexHash)
		}
	}
	// Check install.sh is static: ?code= and Host headers do not inject.
	resp, err = http.Get(ts.URL + "/install.sh?code=do-not-echo-this")
	if err != nil {
		t.Fatalf("GET install.sh?code: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("install.sh?code status %d", resp.StatusCode)
	}
	instBytes, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	inst := string(instBytes)
	if strings.Contains(inst, "do-not-echo-this") {
		t.Fatalf("install.sh should not contain injected code, got %q", inst[:200])
	}
	if strings.Contains(inst, "__CODE__") || strings.Contains(inst, "__SERVER__") {
		t.Fatalf("install.sh should not contain templating placeholders, got %q", inst[:200])
	}
	// Without code, should be byte-identical.
	resp, err = http.Get(ts.URL + "/install.sh")
	if err != nil {
		t.Fatalf("GET install.sh no code: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("install.sh no code status %d", resp.StatusCode)
	}
	plainBytes, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(plainBytes) != inst {
		t.Fatalf("install.sh with ?code should be byte-identical to without")
	}
	// Vary Host/forwarded headers and confirm no injection.
	for _, host := range []string{"evil.example.com", "1.2.3.4:9999"} {
		req, _ := http.NewRequest("GET", ts.URL+"/install.sh?code=xx", nil)
		req.Host = host
		req.Header.Set("X-Forwarded-Host", host)
		req.Header.Set("X-Forwarded-Proto", "https")
		resp, err = http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET install.sh with host %q: %v", host, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(body) != inst {
			t.Fatalf("install.sh with Host %q should be byte-identical", host)
		}
		if strings.Contains(string(body), host) {
			t.Fatalf("install.sh should not contain injected host %q", host)
		}
	}
	// Also check /dl/install.sh path delegates to static handler and is identical.
	resp, err = http.Get(ts.URL + "/dl/install.sh?code=othercode")
	if err != nil {
		t.Fatalf("GET dl/install.sh: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("dl/install.sh status %d", resp.StatusCode)
	}
	body2, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body2) != inst {
		t.Fatalf("dl/install.sh should be byte-identical to /install.sh")
	}
	if strings.Contains(string(body2), "othercode") {
		t.Fatalf("dl/install.sh should not inject code")
	}
	// Check unknown file rejected.
	resp, err = http.Get(ts.URL + "/dl/evil.txt")
	if err != nil {
		t.Fatalf("GET evil: %v", err)
	}
	if resp.StatusCode != 404 {
		t.Fatalf("evil should be 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}
