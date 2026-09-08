package sshkeys

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
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

// ErrLastKeyConfirmation is returned when attempting to delete the last valid key
// without explicit confirmation. It is returned while holding the mutation lock.
var ErrLastKeyConfirmation = errors.New("This is the last SSH key. Confirm removal to disable remote SSH access.")

var (
	githubUsernameRe = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`)
	maxGitHubResp    = 128 * 1024
)

func isCertificateType(t string) bool {
	return strings.HasSuffix(t, "-cert-v01@openssh.com")
}

// ParseAuthorizedKeysLine parses one authorized_keys line for NEW keys.
// It uses ssh.ParseAuthorizedKey for wire-format validation, rejects private keys,
// certificates, authorization options and trailing garbage, and canonicalizes the
// encoded key bytes.
func ParseAuthorizedKeysLine(line, source string) (*SSHKey, error) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return nil, fmt.Errorf("empty or comment line")
	}
	if strings.HasPrefix(trimmed, "-----BEGIN") || strings.Contains(trimmed, "PRIVATE KEY") {
		return nil, fmt.Errorf("private key not allowed")
	}
	pub, comment, options, rest, err := ssh.ParseAuthorizedKey([]byte(trimmed))
	if err != nil {
		return nil, fmt.Errorf("invalid key: %w", err)
	}
	if len(rest) != 0 && len(bytes.TrimSpace(rest)) != 0 {
		return nil, fmt.Errorf("trailing garbage after key")
	}
	if len(options) != 0 {
		return nil, fmt.Errorf("authorization options not allowed: %v", options)
	}
	if isCertificateType(pub.Type()) {
		return nil, fmt.Errorf("certificate keys not allowed")
	}
	canonicalB64 := base64.StdEncoding.EncodeToString(pub.Marshal())
	fingerprint := ssh.FingerprintSHA256(pub)
	raw := pub.Type() + " " + canonicalB64
	if comment != "" {
		raw += " " + comment
	}
	return &SSHKey{
		Raw:         raw,
		Type:        pub.Type(),
		Base64:      canonicalB64,
		Comment:     comment,
		Source:      source,
		Fingerprint: fingerprint,
	}, nil
}

// isValidGitHubUsername validates GitHub username form: alphanumeric and hyphen,
// 1-39 chars, cannot start/end with hyphen, no consecutive hyphens.
func isValidGitHubUsername(s string) bool {
	if len(s) == 0 || len(s) > 39 {
		return false
	}
	if !githubUsernameRe.MatchString(s) {
		return false
	}
	if strings.Contains(s, "--") {
		return false
	}
	return true
}

// ImportKeysFromGitHub fetches https://github.com/<username>.keys and parses each line.
func ImportKeysFromGitHub(ctx context.Context, username string) ([]SSHKey, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return nil, fmt.Errorf("username required")
	}
	if !isValidGitHubUsername(username) {
		return nil, fmt.Errorf("invalid github username %q", username)
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
	// Reject oversized rather than truncating.
	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxGitHubResp+1)))
	if err != nil {
		return nil, err
	}
	if len(data) > maxGitHubResp {
		return nil, fmt.Errorf("github response too large (>%d bytes)", maxGitHubResp)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, fmt.Errorf("empty github keys response for %q", username)
	}
	var keys []SSHKey
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			// GitHub should not return comments, but skip if present.
			continue
		}
		k, err := ParseAuthorizedKeysLine(trimmed, "github:"+username)
		if err != nil {
			return nil, fmt.Errorf("malformed github key %q: %w", trimmed, err)
		}
		keys = append(keys, *k)
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("no usable keys found for github user %q", username)
	}
	// Deduplicate canonical keys.
	seen := make(map[string]bool)
	deduped := make([]SSHKey, 0, len(keys))
	for _, k := range keys {
		if seen[k.Fingerprint] {
			continue
		}
		seen[k.Fingerprint] = true
		deduped = append(deduped, k)
	}
	return deduped, nil
}

// ParsePastedKeys parses keys pasted as a multiline string.
func ParsePastedKeys(raw string) ([]SSHKey, error) {
	var keys []SSHKey
	lines := strings.Split(raw, "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		k, err := ParseAuthorizedKeysLine(trimmed, "pasted")
		if err != nil {
			return nil, fmt.Errorf("invalid pasted key %q: %w", trimmed, err)
		}
		keys = append(keys, *k)
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("no valid keys pasted")
	}
	seen := make(map[string]bool)
	deduped := make([]SSHKey, 0, len(keys))
	for _, k := range keys {
		if seen[k.Fingerprint] {
			continue
		}
		seen[k.Fingerprint] = true
		deduped = append(deduped, k)
	}
	return deduped, nil
}

// parseLenient parses an existing authorized_keys line allowing options.
// It returns fingerprint and validity. Invalid, comment, empty, or certificate
// lines are not valid and should be preserved but not advertised.
func parseLenient(line string) (fingerprint string, valid bool, pub ssh.PublicKey, comment string, options []string) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return "", false, nil, "", nil
	}
	if strings.HasPrefix(trimmed, "-----BEGIN") || strings.Contains(trimmed, "PRIVATE KEY") {
		return "", false, nil, "", nil
	}
	p, c, opts, rest, err := ssh.ParseAuthorizedKey([]byte(trimmed))
	if err != nil {
		return "", false, nil, "", nil
	}
	if len(rest) != 0 && len(bytes.TrimSpace(rest)) != 0 {
		return "", false, nil, "", nil
	}
	if isCertificateType(p.Type()) {
		return "", false, nil, "", nil
	}
	fp := ssh.FingerprintSHA256(p)
	return fp, true, p, c, opts
}

// MergeAuthorizedKeys performs additive merge: existing keys are kept, new keys
// are appended only if their canonical fingerprint is not already present.
func MergeAuthorizedKeys(existing []string, newKeys []SSHKey) ([]string, int) {
	seen := map[string]bool{}
	for _, line := range existing {
		fp, valid, _, _, _ := parseLenient(line)
		if valid {
			seen[fp] = true
		} else {
			// For unparseable lines, mark raw trimmed as seen to avoid exact duplicate.
			trimmed := strings.TrimSpace(line)
			if trimmed != "" {
				seen[trimmed] = true
			}
		}
	}
	merged := append([]string{}, existing...)
	added := 0
	localSeen := map[string]bool{}
	for _, k := range newKeys {
		if k.Fingerprint == "" {
			// Fallback: compute from Raw if needed (should not happen).
			continue
		}
		if seen[k.Fingerprint] || localSeen[k.Fingerprint] {
			continue
		}
		merged = append(merged, k.Raw)
		seen[k.Fingerprint] = true
		localSeen[k.Fingerprint] = true
		added++
	}
	return merged, added
}

func isSymlink(path string) (bool, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return fi.Mode()&os.ModeSymlink != 0, nil
}

func acquireLock(sshDir string) (*os.File, error) {
	lockPath := filepath.Join(sshDir, ".authorized_keys.lock")
	if isLink, err := isSymlink(lockPath); err != nil {
		return nil, err
	} else if isLink {
		return nil, fmt.Errorf("lock file is symlink")
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func releaseLock(f *os.File) error {
	if f == nil {
		return nil
	}
	defer f.Close()
	return unix.Flock(int(f.Fd()), unix.LOCK_UN)
}

func writeAuthorizedKeysAtomic(sshDir, akPath string, data []byte, uid, gid int) error {
	if isLink, err := isSymlink(sshDir); err != nil {
		return err
	} else if isLink {
		return fmt.Errorf(".ssh is symlink")
	}
	if isLink, err := isSymlink(akPath); err != nil {
		return err
	} else if isLink {
		return fmt.Errorf("authorized_keys is symlink")
	}
	tmp, err := os.CreateTemp(sshDir, ".authorized_keys.tmp.*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	success := false
	defer func() {
		tmp.Close()
		if !success {
			os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Chmod(0600); err != nil {
		return err
	}
	if os.Geteuid() == 0 {
		if err := tmp.Chown(uid, gid); err != nil {
			return err
		}
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, akPath); err != nil {
		return err
	}
	success = true
	// Ensure ownership after rename if needed (already done via tmp).
	if dirF, err := os.Open(sshDir); err == nil {
		dirF.Sync()
		dirF.Close()
	}
	return nil
}

func ensureSSHDir(sshDir string, uid, gid int) error {
	isLink, err := isSymlink(sshDir)
	if err != nil {
		return err
	}
	if isLink {
		return fmt.Errorf(".ssh is symlink")
	}
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		return err
	}
	// Re-check after mkdir
	if isLink, err := isSymlink(sshDir); err != nil {
		return err
	} else if isLink {
		return fmt.Errorf(".ssh is symlink")
	}
	if err := os.Chmod(sshDir, 0700); err != nil {
		return err
	}
	if os.Geteuid() == 0 {
		if err := os.Chown(sshDir, uid, gid); err != nil {
			return err
		}
	}
	return nil
}

// EnsureAuthorizedKeysAt merges newKeys into the target authorized_keys file at
// home/.ssh/authorized_keys additively, using uid/gid for ownership. It shares
// the atomic writer for not-yet-booted targets and never uses live user lookup.
func EnsureAuthorizedKeysAt(home string, uid, gid int, newKeys []SSHKey) (int, string, error) {
	if strings.TrimSpace(home) == "" {
		return 0, "", fmt.Errorf("home directory required")
	}
	if uid < 0 || gid < 0 {
		return 0, "", fmt.Errorf("invalid uid/gid")
	}
	sshDir := filepath.Join(home, ".ssh")
	akPath := filepath.Join(sshDir, "authorized_keys")
	if err := ensureSSHDir(sshDir, uid, gid); err != nil {
		return 0, "", err
	}
	lockFile, err := acquireLock(sshDir)
	if err != nil {
		return 0, "", err
	}
	defer releaseLock(lockFile)

	if isLink, err := isSymlink(akPath); err != nil {
		return 0, "", err
	} else if isLink {
		return 0, "", fmt.Errorf("authorized_keys is symlink")
	}

	var existingData []byte
	var existingLines []string
	seen := make(map[string]bool)

	if data, err := os.ReadFile(akPath); err == nil {
		existingData = data
		lines := strings.Split(string(existingData), "\n")
		for i, line := range lines {
			if i == len(lines)-1 && line == "" {
				continue
			}
			existingLines = append(existingLines, line)
			if fp, valid, _, _, _ := parseLenient(line); valid {
				seen[fp] = true
			} else {
				trimmed := strings.TrimSpace(line)
				if trimmed != "" {
					// Track raw for dedup fallback (not fingerprint).
					// Do not treat as fingerprint; just track raw to avoid confusion.
				}
			}
		}
	} else if !os.IsNotExist(err) {
		return 0, "", err
	}

	// Deduplicate newKeys input and against existing.
	toAddLines := []string{}
	addedSeen := make(map[string]bool)
	addedCount := 0
	for _, k := range newKeys {
		if k.Fingerprint == "" {
			continue
		}
		if seen[k.Fingerprint] || addedSeen[k.Fingerprint] {
			continue
		}
		line := k.Raw
		if strings.TrimSpace(line) == "" {
			line = k.Type + " " + k.Base64
			if k.Comment != "" {
				line += " " + k.Comment
			}
		}
		toAddLines = append(toAddLines, line)
		seen[k.Fingerprint] = true
		addedSeen[k.Fingerprint] = true
		addedCount++
	}

	if addedCount == 0 {
		return 0, akPath, nil
	}

	merged := append([]string{}, existingLines...)
	merged = append(merged, toAddLines...)
	content := strings.Join(merged, "\n") + "\n"
	if err := writeAuthorizedKeysAtomic(sshDir, akPath, []byte(content), uid, gid); err != nil {
		return 0, "", err
	}
	return addedCount, akPath, nil
}

// EnsureAuthorizedKeys merges newKeys into the user's authorized_keys file
// additively (existing keys kept; duplicates skipped) and writes it back
// with 0600 and correct ownership when running as root.
// It is a thin wrapper over EnsureAuthorizedKeysAt for existing callers.
func EnsureAuthorizedKeys(username string, newKeys []SSHKey) (added int, path string, err error) {
	u, err := user.Lookup(username)
	if err != nil {
		return 0, "", fmt.Errorf("lookup user %q: %w", username, err)
	}
	home := u.HomeDir
	if home == "" {
		return 0, "", fmt.Errorf("user %q has no home directory", username)
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return 0, "", fmt.Errorf("invalid uid for %q: %w", username, err)
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return 0, "", fmt.Errorf("invalid gid for %q: %w", username, err)
	}
	return EnsureAuthorizedKeysAt(home, uid, gid, newKeys)
}

// ReadAuthorizedKeys lists valid public keys from the user's authorized_keys file.
// Missing file means empty list. Comments, blank and unparseable lines are not
// advertised as valid keys. Symlinked managed directories/files are rejected.
func ReadAuthorizedKeys(username string) ([]SSHKey, error) {
	u, err := user.Lookup(username)
	if err != nil {
		return nil, fmt.Errorf("lookup user %q: %w", username, err)
	}
	home := u.HomeDir
	if home == "" {
		return nil, fmt.Errorf("user %q has no home directory", username)
	}
	sshDir := filepath.Join(home, ".ssh")
	akPath := filepath.Join(sshDir, "authorized_keys")
	if isLink, err := isSymlink(sshDir); err != nil {
		return nil, err
	} else if isLink {
		return nil, fmt.Errorf(".ssh is symlink")
	}
	if isLink, err := isSymlink(akPath); err != nil {
		return nil, err
	} else if isLink {
		return nil, fmt.Errorf("authorized_keys is symlink")
	}
	data, err := os.ReadFile(akPath)
	if err != nil {
		if os.IsNotExist(err) {
			return []SSHKey{}, nil
		}
		return nil, err
	}
	var keys []SSHKey
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		fp, valid, pub, comment, _ := parseLenient(line)
		if !valid {
			continue
		}
		canonicalB64 := base64.StdEncoding.EncodeToString(pub.Marshal())
		raw := strings.TrimSpace(line)
		keys = append(keys, SSHKey{
			Raw:         raw,
			Type:        pub.Type(),
			Base64:      canonicalB64,
			Comment:     comment,
			Source:      "authorized_keys",
			Fingerprint: fp,
		})
	}
	if keys == nil {
		keys = []SSHKey{}
	}
	return keys, nil
}

// RemoveAuthorizedKey removes all key records matching fingerprint from the
// user's authorized_keys file. It returns true if any key was removed.
// If the removal would delete the last valid key and confirmLast is false,
// it returns ErrLastKeyConfirmation while holding the mutation lock and makes
// no changes. Missing file means empty list and returns false. Symlinked
// managed directories/files are rejected. Comments and unparseable lines are
// preserved. Invalid existing records are not counted as valid keys.
func RemoveAuthorizedKey(username, fingerprint string, confirmLast bool) (bool, error) {
	fingerprint = strings.TrimSpace(fingerprint)
	if fingerprint == "" {
		return false, fmt.Errorf("fingerprint required")
	}
	u, err := user.Lookup(username)
	if err != nil {
		return false, fmt.Errorf("lookup user %q: %w", username, err)
	}
	home := u.HomeDir
	sshDir := filepath.Join(home, ".ssh")
	akPath := filepath.Join(sshDir, "authorized_keys")

	if fi, err := os.Lstat(sshDir); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	} else {
		if fi.Mode()&os.ModeSymlink != 0 {
			return false, fmt.Errorf(".ssh is symlink")
		}
	}

	lockFile, err := acquireLock(sshDir)
	if err != nil {
		return false, err
	}
	defer releaseLock(lockFile)

	if isLink, err := isSymlink(akPath); err != nil {
		return false, err
	} else if isLink {
		return false, fmt.Errorf("authorized_keys is symlink")
	}

	data, err := os.ReadFile(akPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}

	lines := strings.Split(string(data), "\n")
	type lineInfo struct {
		raw         string
		fingerprint string
		valid       bool
	}
	var infos []lineInfo
	validCount := 0
	for i, line := range lines {
		if i == len(lines)-1 && line == "" {
			continue
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			infos = append(infos, lineInfo{raw: line, valid: false})
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			infos = append(infos, lineInfo{raw: line, valid: false})
			continue
		}
		fp, valid, _, _, _ := parseLenient(line)
		if valid {
			validCount++
			infos = append(infos, lineInfo{raw: line, fingerprint: fp, valid: true})
		} else {
			infos = append(infos, lineInfo{raw: line, valid: false})
		}
	}

	matches := 0
	for _, info := range infos {
		if info.valid && info.fingerprint == fingerprint {
			matches++
		}
	}
	if matches == 0 {
		return false, nil
	}
	if validCount-matches == 0 && !confirmLast {
		return false, ErrLastKeyConfirmation
	}

	var newLines []string
	for _, info := range infos {
		if info.valid && info.fingerprint == fingerprint {
			continue
		}
		newLines = append(newLines, info.raw)
	}

	var newData []byte
	if len(newLines) > 0 {
		newData = []byte(strings.Join(newLines, "\n") + "\n")
	} else {
		newData = []byte{}
	}

	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	if err := writeAuthorizedKeysAtomic(sshDir, akPath, newData, uid, gid); err != nil {
		return false, err
	}
	return true, nil
}
