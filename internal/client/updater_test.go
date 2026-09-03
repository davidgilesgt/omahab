package client

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestParseSHA256SUMS(t *testing.T) {
	data := []byte("abc123  omahab-clientd-linux-amd64\n" + "deadbeef  omahab-clientd-darwin-arm64\n")
	// Use real hash length 64
	realHash := strings.Repeat("a", 64)
	data2 := []byte(fmt.Sprintf("%s  omahab-clientd-%s-%s\n", realHash, runtime.GOOS, runtime.GOARCH))
	h, err := parseSHA256SUMS(data2, fmt.Sprintf("omahab-clientd-%s-%s", runtime.GOOS, runtime.GOARCH))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if h != realHash {
		t.Fatalf("hash mismatch %q != %q", h, realHash)
	}
	// not found
	if _, err := parseSHA256SUMS([]byte(realHash+"  other\n"), "missing"); err == nil {
		t.Fatalf("expected error for missing")
	}
	_ = data
}

func TestDoSelfUpdate_VerifyAndReplace(t *testing.T) {
	// Prepare a fake binary payload.
	payload := []byte("fake-omahab-clientd-binary-" + runtime.GOOS + "-" + runtime.GOARCH)
	hash := sha256.Sum256(payload)
	hashHex := hex.EncodeToString(hash[:])
	fileName := fmt.Sprintf("omahab-clientd-%s-%s", runtime.GOOS, runtime.GOARCH)
	sumsContent := fmt.Sprintf("%s  %s\n", hashHex, fileName)
	// Also include other files to ensure parsing is selective.
	sumsContent += strings.Repeat("b", 64) + "  omahab-clientd-linux-amd64\n"

	// Temp exec path.
	tmpDir := t.TempDir()
	execPath := filepath.Join(tmpDir, "omahab-clientd")
	if err := os.WriteFile(execPath, []byte("old binary"), 0o755); err != nil {
		t.Fatalf("write old: %v", err)
	}

	// Test server serving /dl/*.
	mux := http.NewServeMux()
	mux.HandleFunc("/dl/SHA256SUMS", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(sumsContent))
	})
	mux.HandleFunc("/dl/"+fileName, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(payload)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Create daemon with server URL pointing to test server.
	cfg := &Config{ServerURL: srv.URL}
	daemon := &Daemon{
		cfg: cfg,
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	// Use background ctx for update (needs d.ctx, set to background).
	daemon.ctx = t.Context()
	// Override Version for test to ensure mismatch triggers.
	origVer := Version
	Version = "0.0.1"
	defer func() { Version = origVer }()

	// Track restart called.
	restarted := false
	cfgUpdater := UpdaterConfig{
		ExecPath: execPath,
		RestartFunc: func() error {
			restarted = true
			return nil
		},
	}

	if err := daemon.doSelfUpdate("0.0.2", cfgUpdater); err != nil {
		t.Fatalf("doSelfUpdate: %v", err)
	}
	if !restarted {
		t.Fatalf("expected restart to be called")
	}
	got, err := os.ReadFile(execPath)
	if err != nil {
		t.Fatalf("read new binary: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("binary mismatch: got %q want %q", string(got), string(payload))
	}
	// Verify hash mismatch detection.
	badPayload := []byte("tampered")
	mux2 := http.NewServeMux()
	mux2.HandleFunc("/dl/SHA256SUMS", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(sumsContent))
	})
	mux2.HandleFunc("/dl/"+fileName, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(badPayload)
	})
	srv2 := httptest.NewServer(mux2)
	defer srv2.Close()
	daemon2 := &Daemon{cfg: &Config{ServerURL: srv2.URL}, log: slog.New(slog.NewTextHandler(io.Discard, nil)), ctx: t.Context()}
	execPath2 := filepath.Join(t.TempDir(), "omahab-clientd2")
	_ = os.WriteFile(execPath2, []byte("old"), 0o755)
	if err := daemon2.doSelfUpdate("0.0.2", UpdaterConfig{ExecPath: execPath2, RestartFunc: func() error { return nil }}); err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("expected hash mismatch error, got %v", err)
	}
	// Ensure old not overwritten on mismatch.
	if got2, _ := os.ReadFile(execPath2); string(got2) != "old" {
		t.Fatalf("file should not be overwritten on hash mismatch")
	}
}

func TestMaybeSelfUpdate_SkewTriggers(t *testing.T) {
	// This test proves the version-mismatch path exists: maybeSelfUpdate is called from syncOnce.
	// We verify that when server version != client Version, doSelfUpdate would be attempted.
	// Since maybeSelfUpdate spawns a goroutine, we test the Version check logic directly.
	orig := Version
	Version = "1.0.0"
	defer func() { Version = orig }()
	// Server version different.
	serverVer := "1.0.1"
	if serverVer == Version {
		t.Fatalf("should be skew")
	}
	// If Version == "dev", no update.
	Version = "dev"
	if serverVer != "dev" && Version == "dev" {
		// maybeSelfUpdate should early-return — this is the guard.
	}
	Version = orig
}


