package diskinstall

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CheckConnectivity verifies network by DNS+HTTPS to pinned hosts before erasure.
// Returns nil if reachable, else error. Caller should treat error as connection step failure.
func CheckConnectivity(ctx context.Context) error {
	// Check DNS and HTTPS to cache.nixos.org (pinned cache) with timeout.
	// Use context with 5s timeout per attempt.
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// DNS check: lookup cache.nixos.org
	resolver := net.Resolver{}
	addrs, err := resolver.LookupHost(cctx, "cache.nixos.org")
	if err != nil || len(addrs) == 0 {
		return fmt.Errorf("DNS lookup failed for cache.nixos.org: %w", err)
	}

	// HTTPS check: HEAD cache.nixos.org
	req, err := http.NewRequestWithContext(cctx, http.MethodHead, "https://cache.nixos.org/", nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("HTTPS check failed for cache.nixos.org: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return fmt.Errorf("cache.nixos.org returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// WriteNetworkFile writes a NetworkFile JSON to path with 0600 root-owned.
// For profile mode, keyfile should be staged path already 0600.
// Caller must ensure directory exists and clean up on exit.
func WriteNetworkFile(path string, nf NetworkFile) error {
	if nf.Mode != "profile" && nf.Mode != "wired-dhcp" {
		return fmt.Errorf("invalid network mode %q", nf.Mode)
	}
	data, err := json.MarshalIndent(nf, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	// Write with 0600
	if err := os.WriteFile(path, append(data, '\n'), 0600); err != nil {
		return err
	}
	// Ensure 0600 even if umask varied
	_ = os.Chmod(path, 0600)
	return nil
}

// ReadNetworkFile reads a NetworkFile from path.
func ReadNetworkFile(path string) (*NetworkFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var nf NetworkFile
	if err := json.Unmarshal(data, &nf); err != nil {
		return nil, err
	}
	return &nf, nil
}

// IsWiredDHCP returns true if no persistent NM profile should be copied.
func IsWiredDHCP(nf NetworkFile) bool {
	return nf.Mode == "wired-dhcp"
}

// ValidateNetworkFileForInstall checks profile invariants before erasure.
// For profile mode: connection_uuid, interface, keyfile must be non-empty and keyfile must exist and be 0600.
// For wired-dhcp: keyfile must be empty.
// Also rejects Wi-Fi lacking persistent credentials: keyfile must contain psk or persistent settings — we check file non-empty and contains "psk=" or "wifi" or "802-11" markers, but at minimum require file exists and is non-empty.
// Returns error to block proceeding.
func ValidateNetworkFileForInstall(nf NetworkFile) error {
	if nf.Mode == "wired-dhcp" {
		if nf.Keyfile != "" {
			return fmt.Errorf("wired-dhcp keyfile must be empty")
		}
		return nil
	}
	if nf.Mode == "profile" {
		if strings.TrimSpace(nf.ConnectionUUID) == "" {
			return fmt.Errorf("profile connection_uuid required")
		}
		if strings.TrimSpace(nf.Interface) == "" {
			return fmt.Errorf("profile interface required")
		}
		if strings.TrimSpace(nf.Keyfile) == "" {
			return fmt.Errorf("profile keyfile required")
		}
		fi, err := os.Stat(nf.Keyfile)
		if err != nil {
			return fmt.Errorf("profile keyfile missing: %w", err)
		}
		if fi.Mode().Perm() != 0600 {
			return fmt.Errorf("profile keyfile must be 0600")
		}
		data, err := os.ReadFile(nf.Keyfile)
		if err != nil {
			return fmt.Errorf("read profile keyfile: %w", err)
		}
		if len(data) == 0 {
			return fmt.Errorf("profile keyfile empty: Wi-Fi credentials must be persistent")
		}
		// For Wi-Fi, ensure persistent credentials marker exists; otherwise fail before erasure.
		// We accept ethernet profile without psk, but require file non-empty; Wi-Fi without psk is not persistent.
		// Heuristic: if file mentions wifi or wireless, require psk or password.
		lower := strings.ToLower(string(data))
		if strings.Contains(lower, "wifi") || strings.Contains(lower, "wireless") || strings.Contains(lower, "802-11-wireless") {
			if !strings.Contains(lower, "psk=") && !strings.Contains(lower, "password=") && !strings.Contains(lower, "wep-key") {
				return fmt.Errorf("Wi-Fi profile lacks persistent credentials (no psk/password)")
			}
		}
		return nil
	}
	return fmt.Errorf("unknown network mode %q", nf.Mode)
}
