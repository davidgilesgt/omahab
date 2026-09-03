package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// UpdaterConfig controls self-update behavior. Used for tests.
type UpdaterConfig struct {
	// ExecPath is the current binary path (os.Executable fallback).
	ExecPath string
	// HTTPClient is the client for /dl fetches (nil -> default).
	HTTPClient *http.Client
	// RestartFunc replaces systemctl restart for tests.
	RestartFunc func() error
}

// selfUpdateEnabled reports whether self-update should run on this platform.
// Linux only (systemd user unit); darwin would use launchd but we keep Linux for B2.
func selfUpdateEnabled() bool {
	return runtime.GOOS == "linux"
}

// maybeSelfUpdate checks version skew and triggers update if needed.
// Called after each successful status sync. Non-blocking.
func (d *Daemon) maybeSelfUpdate() {
	if !selfUpdateEnabled() {
		return
	}
	d.mu.RLock()
	st := d.status
	d.mu.RUnlock()
	if st == nil || strings.TrimSpace(st.Version) == "" {
		return
	}
	serverVer := strings.TrimSpace(st.Version)
	ownVer := strings.TrimSpace(Version)
	if ownVer == "" || ownVer == "dev" {
		return
	}
	if serverVer == ownVer {
		return
	}
	// Avoid spamming: already updating?
	d.mu.RLock()
	lastErr := d.lastErr
	d.mu.RUnlock()
	if strings.Contains(lastErr, "self-update") {
		return
	}
	d.log.Info("version skew detected, attempting self-update", "server", serverVer, "client", ownVer)
	go func(sv string) {
		if err := d.doSelfUpdate(sv, UpdaterConfig{}); err != nil {
			d.log.Warn("self-update failed", "err", err, "server", sv, "client", ownVer)
			d.mu.Lock()
			d.lastErr = fmt.Sprintf("self-update failed: %v", err)
			d.mu.Unlock()
			d.broadcastStatus()
		} else {
			d.log.Info("self-update completed, restarting", "server", sv)
		}
	}(serverVer)
}

// doSelfUpdate downloads the correct binary for this OS/arch, verifies SHA256 against
// /dl/SHA256SUMS, atomically replaces the running binary, and restarts via systemctl.
func (d *Daemon) doSelfUpdate(targetVersion string, cfg UpdaterConfig) error {
	if d.cfg == nil || strings.TrimSpace(d.cfg.ServerURL) == "" {
		return fmt.Errorf("no server_url configured")
	}
	serverURL := strings.TrimSuffix(strings.TrimSpace(d.cfg.ServerURL), "/")
	goos := runtime.GOOS
	goarch := runtime.GOARCH
	fileName := fmt.Sprintf("omahab-clientd-%s-%s", goos, goarch)
	dlURL := serverURL + "/dl/" + fileName
	sumsURL := serverURL + "/dl/SHA256SUMS"

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	execPath := cfg.ExecPath
	if execPath == "" {
		var err error
		execPath, err = os.Executable()
		if err != nil {
			return fmt.Errorf("os.Executable: %w", err)
		}
		// Resolve symlink (Nix store etc.) to get real file.
		if resolved, err := filepath.EvalSymlinks(execPath); err == nil && resolved != "" {
			execPath = resolved
		}
	}

	// 1. Fetch SHA256SUMS.
	ctx, cancel := context.WithTimeout(d.ctx, 30*time.Second)
	defer cancel()
	sumsData, err := fetchURL(ctx, httpClient, sumsURL)
	if err != nil {
		return fmt.Errorf("fetch SHA256SUMS: %w", err)
	}
	expectedHash, err := parseSHA256SUMS(sumsData, fileName)
	if err != nil {
		return err
	}

	// 2. Fetch binary.
	ctx2, cancel2 := context.WithTimeout(d.ctx, 60*time.Second)
	defer cancel2()
	binData, err := fetchURL(ctx2, httpClient, dlURL)
	if err != nil {
		return fmt.Errorf("fetch %s: %w", fileName, err)
	}

	// 3. Verify SHA256.
	h := sha256.Sum256(binData)
	actual := hex.EncodeToString(h[:])
	if actual != expectedHash {
		return fmt.Errorf("SHA256 mismatch for %s: expected %s, actual %s", fileName, expectedHash, actual)
	}

	// 4. Atomic replace.
	dir := filepath.Dir(execPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	tmp := filepath.Join(dir, "."+filepath.Base(execPath)+".tmp")
	if err := os.WriteFile(tmp, binData, 0o755); err != nil {
		return fmt.Errorf("write tmp: %w", err)
	}
	// Ensure executable.
	_ = os.Chmod(tmp, 0o755)
	if err := os.Rename(tmp, execPath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename %s -> %s: %w", tmp, execPath, err)
	}

	// 5. Restart via systemctl --user.
	restart := cfg.RestartFunc
	if restart == nil {
		restart = func() error {
			ctxR, cancelR := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancelR()
			cmd := exec.CommandContext(ctxR, "systemctl", "--user", "restart", "omahab-clientd.service")
			out, err := cmd.CombinedOutput()
			if err != nil {
				return fmt.Errorf("systemctl restart: %w (%s)", err, string(out))
			}
			return nil
		}
	}
	if err := restart(); err != nil {
		return err
	}
	return nil
}

func fetchURL(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("GET %s: %d %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 20<<20))
	if err != nil {
		return nil, err
	}
	return data, nil
}

func parseSHA256SUMS(data []byte, fileName string) (string, error) {
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		hash := parts[0]
		name := parts[1]
		// handle leading * or space (BSD vs GNU)
		name = strings.TrimPrefix(name, "*")
		name = strings.TrimPrefix(name, " ")
		name = filepath.Base(name)
		if name == fileName {
			if len(hash) != 64 {
				return "", fmt.Errorf("invalid hash length for %s", fileName)
			}
			// validate hex
			if _, err := hex.DecodeString(hash); err != nil {
				return "", fmt.Errorf("invalid hash for %s: %w", fileName, err)
			}
			return strings.ToLower(hash), nil
		}
	}
	return "", fmt.Errorf("%s not found in SHA256SUMS", fileName)
}
