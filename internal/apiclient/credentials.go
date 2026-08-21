package apiclient

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// CredentialStore abstracts bearer credential retrieval.
// The CLI config file (~/.config/omahab/client.json) MUST NOT store secrets.
// Tokens come from this interface or the OMAHAB_TOKEN environment variable.
type CredentialStore interface {
	// Token returns the bearer token or empty string if not configured.
	Token() (string, error)
	// SetToken persists a token (keyring/file). Optional; some stores are read-only.
	SetToken(token string) error
	// Clear removes any persisted token.
	Clear() error
}

// EnvCredentialStore reads OMAHAB_TOKEN.
type EnvCredentialStore struct{}

func (EnvCredentialStore) Token() (string, error) {
	return strings.TrimSpace(os.Getenv("OMAHAB_TOKEN")), nil
}
func (EnvCredentialStore) SetToken(_ string) error { return errors.New("env store is read-only") }
func (EnvCredentialStore) Clear() error            { return errors.New("env store is read-only") }

// FileCredentialStore reads a token from a file on disk (fallback for
// environments without a desktop keyring). The default path is
// ~/.config/omahab/credentials.json or a plain token file, but the file
// itself is distinct from client.json. This is intentionally separate to
// uphold "never that file" for credentials.
type FileCredentialStore struct {
	Path string
}

func (f FileCredentialStore) Token() (string, error) {
	if f.Path == "" {
		p, err := DefaultCredentialsPath()
		if err != nil {
			return "", err
		}
		f.Path = p
	}
	b, err := os.ReadFile(f.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func (f FileCredentialStore) SetToken(token string) error {
	if f.Path == "" {
		p, err := DefaultCredentialsPath()
		if err != nil {
			return err
		}
		f.Path = p
	}
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o700); err != nil {
		return err
	}
	// Write with 0600; never world-readable.
	return os.WriteFile(f.Path, []byte(strings.TrimSpace(token)+"\n"), 0o600)
}

func (f FileCredentialStore) Clear() error {
	if f.Path == "" {
		p, err := DefaultCredentialsPath()
		if err != nil {
			return err
		}
		f.Path = p
	}
	err := os.Remove(f.Path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// DefaultCredentialsPath returns the fallback token file path.
// Prefer XDG_CONFIG_HOME, then ~/.config.
func DefaultCredentialsPath() (string, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".config", "omahab")
	} else {
		dir = filepath.Join(dir, "omahab")
	}
	return filepath.Join(dir, "token"), nil
}

// CompositeCredentialStore tries each store in order.
type CompositeCredentialStore struct {
	Stores []CredentialStore
}

func (c CompositeCredentialStore) Token() (string, error) {
	for _, s := range c.Stores {
		tok, err := s.Token()
		if err != nil {
			continue
		}
		if strings.TrimSpace(tok) != "" {
			return strings.TrimSpace(tok), nil
		}
	}
	return "", nil
}

func (c CompositeCredentialStore) SetToken(token string) error {
	// Prefer the last writable store; fallback to first that succeeds.
	var lastErr error
	for i := len(c.Stores) - 1; i >= 0; i-- {
		if err := c.Stores[i].SetToken(token); err == nil {
			return nil
		} else {
			lastErr = err
		}
	}
	if lastErr != nil {
		return lastErr
	}
	return errors.New("no writable credential store")
}

func (c CompositeCredentialStore) Clear() error {
	var lastErr error
	for _, s := range c.Stores {
		if err := s.Clear(); err != nil {
			lastErr = err
		}
	}
	return lastErr
}

// ResolveToken returns the first non-empty token from OMAHAB_TOKEN or the
// provided store. It never returns a token sourced from client.json.
func ResolveToken(store CredentialStore) (string, error) {
	// Env always wins, even if a store is provided.
	env := strings.TrimSpace(os.Getenv("OMAHAB_TOKEN"))
	if env != "" {
		return env, nil
	}
	if store == nil {
		return "", nil
	}
	return store.Token()
}
