package backups

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SnapshotManifest is the structured scope record stored inside every
// encrypted restic snapshot at <dump-dir>/manifest.json. Legacy snapshots
// without a manifest remain accessible as staging/manual recovery only: no
// safe live scope boundary is inferred from an old directory listing.
type SnapshotManifest struct {
	FormatVersion int            `json:"format_version"`
	InstanceID    string         `json:"instance_id"`
	Domain        string         `json:"domain"`
	ReleaseRef    string         `json:"release_ref"`
	CreatedAt     string         `json:"created_at"`
	Scopes        []string       `json:"scopes"`
	Applications  []ManifestApp  `json:"applications"`
	Storage       []ManifestDisk `json:"storage,omitempty"`
	// RecoveryFingerprint identifies the phrase kit that derives
	// phrase-based repository passwords. Empty when no kit existed.
	RecoveryFingerprint string `json:"recovery_fingerprint,omitempty"`
	// Omitted records scopes that were selected but explicitly skipped
	// (not installed, or paused with empty data).
	Omitted []ManifestOmission `json:"omitted,omitempty"`
}

// ManifestApp records one application's captured identity.
type ManifestApp struct {
	BundleID  string         `json:"bundle_id"`
	Version   string         `json:"version"`
	Volumes   []ManifestDisk `json:"volumes,omitempty"`
	Databases []string       `json:"databases,omitempty"`
	// Secrets lists the exact app secret/config subset needed to run; values
	// recover through the server-config control dump and are re-encrypted
	// under the current master key on single-app restore.
	Secrets []string `json:"secrets,omitempty"`
}

// ManifestOmission records one selected-but-skipped scope and why.
type ManifestOmission struct {
	Scope  string `json:"scope"`
	Reason string `json:"reason"`
}

// ManifestDisk is one placed volume identity with its filesystem.
type ManifestDisk struct {
	Volume string  `json:"volume"`
	Path   string  `json:"path,omitempty"`
	FSUUID *string `json:"fs_uuid,omitempty"`
}

const manifestFileName = "manifest.json"

// ScopeServerConfig is the control-plane checkpoint: control DB,
// secrets/kit/configuration and the inseparable identity checkpoint
// (PocketID DB/key/OIDC client configuration). It excludes photos,
// documents and other app payloads.
const ScopeServerConfig = "server-config"

// Server-owned data scopes are explicit entries, never hidden under config.
const (
	ScopeServerSync     = "server-sync"
	ScopeServerProjects = "server-projects"
	ScopeMachineBackups = "machine-backups"
)

// AppScope returns the scope name for one bundle.
func AppScope(bundleID string) string { return "app:" + bundleID }

// ScopeBundleID splits an app: scope back into its bundle id.
func ScopeBundleID(scope string) (string, bool) {
	if !strings.HasPrefix(scope, "app:") {
		return "", false
	}
	id := strings.TrimPrefix(scope, "app:")
	if id == "" {
		return "", false
	}
	return id, true
}

func (m SnapshotManifest) Validate() error {
	var problems []string
	if m.FormatVersion != 1 {
		problems = append(problems, fmt.Sprintf("format_version %d unsupported", m.FormatVersion))
	}
	if strings.TrimSpace(m.InstanceID) == "" {
		problems = append(problems, "instance_id is required")
	}
	if len(m.Scopes) == 0 {
		problems = append(problems, "scopes is required")
	}
	if len(problems) > 0 {
		return fmt.Errorf("%w: %s", ErrInvalid, strings.Join(problems, "; "))
	}
	return nil
}

// WriteManifest stores the manifest in dir and fsyncs it for rename
// durability before restic captures the tree.
func WriteManifest(dir string, m SnapshotManifest) error {
	if m.CreatedAt == "" {
		m.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if err := m.Validate(); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := filepath.Join(dir, manifestFileName+".tmp")
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	if f, err := os.OpenFile(tmp, os.O_RDWR, 0o600); err == nil {
		_ = f.Sync()
		_ = f.Close()
	}
	if err := os.Rename(tmp, filepath.Join(dir, manifestFileName)); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// ReadManifest loads and validates a staged manifest. It never executes
// anything embedded in an imported manifest; callers use the installed
// trusted catalog operations.
func ReadManifest(dir string) (SnapshotManifest, error) {
	var m SnapshotManifest
	raw, err := os.ReadFile(filepath.Join(dir, manifestFileName))
	if err != nil {
		return m, err
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return m, fmt.Errorf("parse manifest: %w", err)
	}
	if err := m.Validate(); err != nil {
		return m, err
	}
	return m, nil
}
