package controlplane

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/secrets"
	"github.com/omahab/omahab/internal/store"
)

// EnsureInstance initializes the singleton instance identity.
// It is idempotent and never overwrites a differing ID.
func EnsureInstance(ctx context.Context, st *store.Store) (domain.Instance, error) {
	if inst, err := st.Instance(ctx); err == nil {
		return inst, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return domain.Instance{}, fmt.Errorf("load instance: %w", err)
	}
	// Not found: create with defaults from env or stable generation.
	id := strings.TrimSpace(os.Getenv("OMAHAB_INSTANCE_ID"))
	if id == "" {
		id = store.NewID()
	}
	domainEnv := strings.TrimSpace(os.Getenv("OMAHAB_DOMAIN"))
	if domainEnv == "" {
		domainEnv = "example.com"
	}
	name := strings.TrimSpace(os.Getenv("OMAHAB_ASSISTANT_NAME"))
	if name == "" {
		name = "AI"
	}
	slug := strings.TrimSpace(os.Getenv("OMAHAB_ASSISTANT_SLUG"))
	if slug == "" {
		slug = "ai"
	}
	tailnet := strings.TrimSpace(os.Getenv("OMAHAB_TAILNET"))
	tailscaleIP := strings.TrimSpace(os.Getenv("OMAHAB_TAILSCALE_IP"))

	inst := domain.Instance{
		ID:            domain.ID(id),
		Domain:        domainEnv,
		Tailnet:       tailnet,
		TailscaleIP:   tailscaleIP,
		AssistantName: name,
		AssistantSlug: slug,
		CreatedAt:     time.Now().UTC(),
	}
	saved, err := st.SaveInstance(ctx, inst)
	if err != nil {
		return domain.Instance{}, fmt.Errorf("save instance: %w", err)
	}
	return saved, nil
}

// EnsureMasterKey loads or creates the 32-byte master key at path with 0600.
// For daemon startup we bypass strict root requirement to allow temp directories.
func EnsureMasterKey(path string) ([32]byte, error) {
	loader := secrets.Loader{
		Path:   path,
		IsRoot: func() bool { return true }, // allow creation in test/temp dirs; real prod still validates perms on load
	}
	k, err := loader.Ensure()
	if err != nil {
		return [32]byte{}, fmt.Errorf("master key: %w", err)
	}
	return k, nil
}

// EnsureAPIToken loads or creates a 32-random-byte token at path with 0600.
// Returns the raw token string (hex encoded 64 chars). File contains raw token.
func EnsureAPIToken(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("api token path is required")
	}
	// Try to load existing
	if data, err := os.ReadFile(path); err == nil {
		tok := strings.TrimSpace(string(data))
		if tok != "" {
			// enforce 0600
			_ = os.Chmod(path, 0600)
			return tok, nil
		}
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("read api token: %w", err)
	}
	// Generate 32 random bytes -> hex 64 chars
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate api token: %w", err)
	}
	tok := hex.EncodeToString(b[:])
	// ensure parent dir
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("create token dir: %w", err)
	}
	// atomic write via temp file
	tmp, err := os.CreateTemp(dir, ".api.token.*")
	if err != nil {
		return "", fmt.Errorf("create temp token: %w", err)
	}
	tmpName := tmp.Name()
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", err
	}
	if _, err := tmp.WriteString(tok + "\n"); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return "", err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return "", err
	}
	_ = os.Chmod(path, 0600)
	return tok, nil
}

// HashToken returns sha256 hex of token for logging-safe comparison.
func HashToken(tok string) string {
	h := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(h[:])
}

// LoadEmailHMACKey loads the email webhook HMAC key from env or file.
// It never reuses the API token. Returns empty if not configured.
func LoadEmailHMACKey(apiToken string) []byte {
	// Separate env/file setting
	if v := strings.TrimSpace(os.Getenv("OMAHAB_EMAIL_HMAC_KEY")); v != "" {
		if v != apiToken {
			return []byte(v)
		}
		// if same as api token, treat as not configured to avoid reuse
		return nil
	}
	path := strings.TrimSpace(os.Getenv("OMAHAB_EMAIL_HMAC_KEY_FILE"))
	if path == "" {
		path = strings.TrimSpace(os.Getenv("OMAHAB_EMAIL_WEBHOOK_SECRET_FILE"))
	}
	if path != "" {
		if data, err := os.ReadFile(path); err == nil {
			k := strings.TrimSpace(string(data))
			if k != "" && k != apiToken {
				return []byte(k)
			}
		}
	}
	return nil
}
