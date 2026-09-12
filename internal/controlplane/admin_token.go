package controlplane

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
)

// ProvisionUserToken writes the panel API token to
// <home>/.config/omahab/token (0600, correct ownership) when the user
// exists and the file is absent. Idempotent.
func ProvisionUserToken(username, token string) error {
	if strings.TrimSpace(token) == "" {
		return fmt.Errorf("token is required")
	}
	u, err := user.Lookup(username)
	if err != nil {
		// User not created yet (module creates it); retry on next start.
		return fmt.Errorf("lookup user %q: %w", username, err)
	}
	dir := filepath.Join(u.HomeDir, ".config", "omahab")
	path := filepath.Join(dir, "token")
	if data, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(data)) == strings.TrimSpace(token) {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".token-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if _, err := tmp.WriteString(token + "\n"); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	uid, err1 := strconv.Atoi(u.Uid)
	gid, err2 := strconv.Atoi(u.Gid)
	if err1 != nil || err2 != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("invalid uid/gid for %q: %q/%q", username, u.Uid, u.Gid)
	}
	if err := os.Chown(tmpName, uid, gid); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("chown token temp: %w", err)
	}
	if err := os.Chown(dir, uid, gid); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("chown config dir: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}
