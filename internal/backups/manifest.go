package backups

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ManifestFormatVersion is the only supported manifest format. Version 2
// replaced the version 1 shape (flat applications list) with the
// scope-centric record: every scope carries its own paths, artifacts,
// and requirements, so restores can reason about dependencies without
// consulting the capture-time catalog.
const ManifestFormatVersion = 2

// ArtifactKind classifies one staged artifact. sqlite-dump and pg-dump
// are logical database dumps; file is plain captured data.
const (
	ArtifactSQLiteDump = "sqlite-dump"
	ArtifactPGDump     = "pg-dump"
	ArtifactFile       = "file"
)

// SnapshotManifest is the structured scope record stored inside every
// encrypted restic snapshot at the fixed staging path
// <stateDir>/backup-staging/manifest.json (restic captures the staging
// tree at its absolute path, so the manifest always appears at
// /var/lib/omahab/backup-staging/manifest.json inside a snapshot).
// Snapshots without a v2 manifest are not restoreable: staging and
// manual recovery are the only paths for them.
type SnapshotManifest struct {
	FormatVersion int `json:"format_version"`
	// Instance identifies the machine that captured the snapshot.
	Instance  ManifestInstance   `json:"instance"`
	System    ManifestSystem     `json:"system"`
	CreatedAt string             `json:"created_at"`
	Scopes    []ManifestScope    `json:"scopes"`
	Storage   []ManifestDisk     `json:"storage,omitempty"`
	Omitted   []ManifestOmission `json:"omitted,omitempty"`
	// RecoveryFingerprint identifies the phrase kit that derives
	// phrase-based repository passwords. Empty when no kit existed.
	RecoveryFingerprint string `json:"recovery_fingerprint,omitempty"`
}

// ManifestInstance is the captured identity of the source machine.
type ManifestInstance struct {
	ID     string `json:"id"`
	Domain string `json:"domain"`
	// Placement is "lan" or "vps".
	Placement string `json:"placement"`
}

// ManifestSystem records the system generation the snapshot came from so
// restores can refuse cross-generation restores instead of guessing.
type ManifestSystem struct {
	// ReleaseRef is the flake ref the system was built from.
	ReleaseRef string `json:"release_ref,omitempty"`
	// NixpkgsRev is the locked nixpkgs revision.
	NixpkgsRev string `json:"nixpkgs_rev,omitempty"`
	// FlakeLockSHA256 is the hash of flake.lock.
	FlakeLockSHA256 string `json:"flake_lock_sha256,omitempty"`
	// BundlesRepoRev is the custom-bundles repo HEAD at capture time;
	// empty when no custom bundles repo exists.
	BundlesRepoRev string `json:"bundles_repo_rev,omitempty"`
}

// ManifestScope is one captured scope: everything it takes to restore
// that scope on its own, including its requirements.
type ManifestScope struct {
	// Name is the scope identifier (server-config, app:<bundle-id>,
	// server-sync, server-projects, machine-backups).
	Name string `json:"name"`
	// Requires lists scope names that must restore first. App scopes
	// require server-config (secrets and control state).
	Requires []string `json:"requires,omitempty"`
	// Paths are the live source paths captured for this scope.
	Paths []string `json:"paths,omitempty"`
	// Artifacts are the staged files with content hashes, verified
	// during staging validation.
	Artifacts []ManifestArtifact `json:"artifacts,omitempty"`
	// Bundle identifies the application for app: scopes.
	Bundle *ManifestBundle `json:"bundle,omitempty"`
	// AppVersion is the application's nix package version when exposed.
	AppVersion string `json:"app_version,omitempty"`
	// PostgresMajor is the major version of a dumped PostgreSQL
	// database, from the running cluster.
	PostgresMajor int `json:"postgres_major,omitempty"`
	// Volumes are the scope's placed data volumes.
	Volumes []ManifestDisk `json:"volumes,omitempty"`
	// Databases are the logical databases this scope restores.
	Databases []ManifestDatabase `json:"databases,omitempty"`
	// SecretNames lists the exact app secret/config subset needed to
	// run; values recover through the server-config control dump.
	SecretNames []string `json:"secret_names,omitempty"`
}

// ManifestArtifact is one staged file with its content hash and kind.
type ManifestArtifact struct {
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
	// Kind is sqlite-dump, pg-dump, or file.
	Kind string `json:"kind"`
}

// ManifestBundle records where an app scope's bundle definition lives so
// custom bundles can travel with their snapshots.
type ManifestBundle struct {
	// ID is the bundle id.
	ID string `json:"id"`
	// Source is catalog or custom.
	Source string `json:"source"`
	// DefinitionPath is where the definition was captured from: the
	// staging path (bundles/<id>/ for custom, present in the snapshot)
	// or empty for curated catalog bundles.
	DefinitionPath string `json:"definition_path,omitempty"`
	// DefinitionRev is the custom-bundles repo commit the definition
	// was read at; empty for catalog bundles.
	DefinitionRev string `json:"definition_rev,omitempty"`
}

// ManifestDatabase is one logical database dump in the scope.
type ManifestDatabase struct {
	Name   string `json:"name"`
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
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

// ManifestPath returns the fixed manifest location inside a captured or
// restored tree: <root>/manifest.json, where root is the backup staging
// directory path as it existed at capture time.
func ManifestPath(stagingRoot string) string {
	return filepath.Join(stagingRoot, manifestFileName)
}

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

// Scope returns the manifest scope with the given name.
func (m SnapshotManifest) Scope(name string) (ManifestScope, bool) {
	for _, s := range m.Scopes {
		if s.Name == name {
			return s, true
		}
	}
	return ManifestScope{}, false
}

// backupStagingDir is the fixed, system-level staging directory whose
// absolute path is captured inside every snapshot: the manifest always
// lands at /var/lib/omahab/backup-staging/manifest.json.
const backupStagingDir = "backup-staging"

// backupStagingDirPath is the absolute staging path stamped into
// restore-facing error messages.
const backupStagingDirPath = "/var/lib/omahab/backup-staging"

// backupStagingDirPathParent is the production state directory: captures
// stage at <stateDir>/backup-staging, so an extracted snapshot reproduces
// that absolute path below the extraction target.
const backupStagingDirPathParent = "/var/lib/omahab"

// ScopeNames lists the captured scope names in manifest order.
func (m SnapshotManifest) ScopeNames() []string {
	out := make([]string, 0, len(m.Scopes))
	for _, s := range m.Scopes {
		out = append(out, s.Name)
	}
	return out
}

func (m SnapshotManifest) Validate() error {
	var problems []string
	if m.FormatVersion != ManifestFormatVersion {
		problems = append(problems, fmt.Sprintf("format_version %d unsupported (want %d)", m.FormatVersion, ManifestFormatVersion))
	}
	if strings.TrimSpace(m.Instance.ID) == "" {
		problems = append(problems, "instance.id is required")
	}
	if len(m.Scopes) == 0 {
		problems = append(problems, "scopes is required")
	}
	for _, s := range m.Scopes {
		if strings.TrimSpace(s.Name) == "" {
			problems = append(problems, "scope name is required")
		}
		for _, a := range s.Artifacts {
			if strings.TrimSpace(a.File) == "" {
				problems = append(problems, fmt.Sprintf("scope %s: artifact file is required", s.Name))
			}
			if !looksLikeSHA256(a.SHA256) {
				problems = append(problems, fmt.Sprintf("scope %s: artifact %s needs a sha256", s.Name, a.File))
			}
		}
		for _, d := range s.Databases {
			if !looksLikeSHA256(d.SHA256) {
				problems = append(problems, fmt.Sprintf("scope %s: database %s needs a sha256", s.Name, d.Name))
			}
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("%w: %s", ErrInvalid, strings.Join(problems, "; "))
	}
	return nil
}

func looksLikeSHA256(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// SHA256File hashes one file's content.
func SHA256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
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
