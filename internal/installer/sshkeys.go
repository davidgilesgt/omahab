package installer

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
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

// ImportKeysFromGitHub fetches keys for a GitHub username and parses them.
func ImportKeysFromGitHub(ctx context.Context, probes Probes, username string) ([]SSHKey, error) {
	if username == "" {
		return nil, &ValidationError{Field: "github_user", Message: "username is empty"}
	}
	if strings.Contains(username, "/") || strings.Contains(username, " ") {
		return nil, &ValidationError{Field: "github_user", Message: "invalid username"}
	}
	if probes.FetchGitHubKeys == nil {
		return nil, fmt.Errorf("github fetch probe not configured")
	}
	lines, err := probes.FetchGitHubKeys(ctx, username)
	if err != nil {
		return nil, err
	}
	var keys []SSHKey
	for _, line := range lines {
		k, err := ParseAuthorizedKeysLine(line, fmt.Sprintf("github:%s", username))
		if err != nil {
			continue // skip malformed lines but don't fail whole import
		}
		keys = append(keys, *k)
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("no valid keys found for github user %q", username)
	}
	return keys, nil
}

// ParseKeysFromFile reads keys from a file path via probes.
func ParseKeysFromFile(probes Probes, path string) ([]SSHKey, error) {
	if probes.ReadFile == nil {
		return nil, fmt.Errorf("read file probe not configured")
	}
	data, err := probes.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var keys []SSHKey
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, err := ParseAuthorizedKeysLine(line, fmt.Sprintf("file:%s", path))
		if err != nil {
			continue
		}
		keys = append(keys, *k)
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("no valid keys in %s", path)
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

// EnsureAuthorizedKeys merges newKeys into the user's authorized_keys file additively
// and writes it back via probes.
func EnsureAuthorizedKeys(probes Probes, user string, newKeys []SSHKey) (added int, path string, err error) {
	if probes.AuthorizedKeys == nil || probes.WriteAuthorizedKeys == nil {
		return 0, "", fmt.Errorf("authorized_keys probes not configured")
	}
	akPath, existing, err := probes.AuthorizedKeys(user)
	if err != nil {
		return 0, "", fmt.Errorf("read authorized_keys: %w", err)
	}
	merged, n := MergeAuthorizedKeys(existing, newKeys)
	if n == 0 {
		return 0, akPath, nil // nothing to do
	}
	if err := probes.WriteAuthorizedKeys(user, akPath, merged); err != nil {
		return 0, akPath, fmt.Errorf("write authorized_keys: %w", err)
	}
	return n, akPath, nil
}
