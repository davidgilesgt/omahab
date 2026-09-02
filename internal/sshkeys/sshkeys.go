package sshkeys

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// SSHKey is a parsed public key with display metadata.
type SSHKey struct {
	Raw         string `json:"raw"`
	Type        string `json:"type"`
	Base64      string `json:"base64"`
	Comment     string `json:"comment,omitempty"`
	Source      string `json:"source"`
	Fingerprint string `json:"fingerprint"`
}

// ParseAuthorizedKeysLine parses one authorized_keys line.
func ParseAuthorizedKeysLine(line, source string) (*SSHKey, error) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return nil, fmt.Errorf("empty or comment line")
	}
	// Handle options prefix (e.g. "restrict,command=\"...\" ssh-ed25519 AAAA...")
	// For simplicity we extract the key type token.
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return nil, fmt.Errorf("invalid key line: too few fields")
	}
	// Find the key type field: it is one of ssh-*, ecdsa-*, sk-*
	typeIdx := -1
	for i, f := range fields {
		if strings.HasPrefix(f, "ssh-") || strings.HasPrefix(f, "ecdsa-") || strings.HasPrefix(f, "sk-ssh-") || strings.HasPrefix(f, "sk-ecdsa-") {
			typeIdx = i
			break
		}
	}
	if typeIdx == -1 || typeIdx+1 >= len(fields) {
		return nil, fmt.Errorf("cannot find key type in line: %q", line)
	}
	keyType := fields[typeIdx]
	b64 := fields[typeIdx+1]
	comment := ""
	if typeIdx+2 < len(fields) {
		comment = strings.Join(fields[typeIdx+2:], " ")
	}
	// Validate base64
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		// Try RawStdEncoding (no padding)
		raw, err = base64.RawStdEncoding.DecodeString(b64)
		if err != nil {
			return nil, fmt.Errorf("invalid base64 in key: %w", err)
		}
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty key data")
	}
	fp := fingerprintSHA256(raw)
	return &SSHKey{
		Raw:         line,
		Type:        keyType,
		Base64:      b64,
		Comment:     comment,
		Source:      source,
		Fingerprint: fp,
	}, nil
}
func fingerprintSHA256(raw []byte) string {
	sum := sha256.Sum256(raw)
	b64 := base64.StdEncoding.EncodeToString(sum[:])
	// OpenSSH style: SHA256:<base64 without padding>
	b64 = strings.TrimRight(b64, "=")
	return "SHA256:" + b64
}

// ImportKeysFromGitHub fetches https://github.com/<username>.keys and
// parses each line.
func ImportKeysFromGitHub(ctx context.Context, username string) ([]SSHKey, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return nil, fmt.Errorf("username required")
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	url := "https://github.com/" + username + ".keys"
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch github keys: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("github keys for %q: HTTP %d", username, resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 128*1024))
	if err != nil {
		return nil, err
	}
	var keys []SSHKey
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, err := ParseAuthorizedKeysLine(line, "github:"+username)
		if err != nil {
			continue
		}
		keys = append(keys, *k)
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("no usable keys found for github user %q", username)
	}
	return keys, nil
}

// ParsePastedKeys parses keys pasted as a multiline string.
func ParsePastedKeys(raw string) ([]SSHKey, error) {
	var keys []SSHKey
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, err := ParseAuthorizedKeysLine(line, "pasted")
		if err != nil {
			return nil, fmt.Errorf("invalid pasted key %q: %w", line, err)
		}
		keys = append(keys, *k)
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("no valid keys pasted")
	}
	return keys, nil
}

// MergeAuthorizedKeys performs additive merge: existing keys are kept, new keys
// are appended only if their base64 data is not already present.
func MergeAuthorizedKeys(existing []string, newKeys []SSHKey) ([]string, int) {
	seen := map[string]bool{}
	for _, line := range existing {
		// Normalize to base64 for dedup
		k, err := ParseAuthorizedKeysLine(line, "existing")
		if err != nil {
			// Keep unparseable lines as-is and mark their raw as seen
			seen[strings.TrimSpace(line)] = true
			continue
		}
		seen[k.Base64] = true
	}
	merged := append([]string{}, existing...)
	added := 0
	for _, k := range newKeys {
		if seen[k.Base64] {
			continue
		}
		merged = append(merged, k.Raw)
		seen[k.Base64] = true
		added++
	}
	return merged, added
}

// EnsureAuthorizedKeys merges newKeys into the user's authorized_keys file
// additively (existing keys kept; duplicates skipped) and writes it back
// with 0600 and correct ownership when running as root.
func EnsureAuthorizedKeys(username string, newKeys []SSHKey) (added int, path string, err error) {
	u, err := user.Lookup(username)
	if err != nil {
		return 0, "", fmt.Errorf("lookup user %q: %w", username, err)
	}
	home := u.HomeDir
	if home == "" {
		return 0, "", fmt.Errorf("user %q has no home directory", username)
	}
	akPath := filepath.Join(home, ".ssh", "authorized_keys")
	var existing []string
	if data, rerr := os.ReadFile(akPath); rerr == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.TrimSpace(line) != "" {
				existing = append(existing, line)
			}
		}
	} else if !os.IsNotExist(rerr) {
		return 0, "", rerr
	}
	merged, n := MergeAuthorizedKeys(existing, newKeys)
	if n == 0 && len(existing) > 0 {
		return 0, akPath, nil
	}
	if err := os.MkdirAll(filepath.Dir(akPath), 0o700); err != nil {
		return 0, "", err
	}
	content := strings.Join(merged, "\n") + "\n"
	if werr := os.WriteFile(akPath, []byte(content), 0o600); werr != nil {
		return 0, "", werr
	}
	if uid, e1 := strconv.Atoi(u.Uid); e1 == nil {
		if gid, e2 := strconv.Atoi(u.Gid); e2 == nil {
			_ = os.Chown(akPath, uid, gid)
			_ = os.Chown(filepath.Dir(akPath), uid, gid)
		}
	}
	return n, akPath, nil
}
