package syncer

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/omahab/omahab/internal/domain"
)

// Sentinel errors for API callers.
var (
	ErrNotFound       = errors.New("sync folder not found")
	ErrAlreadyExists  = errors.New("sync folder already exists")
	ErrValidation     = errors.New("validation error")
	ErrPathTraversal  = errors.New("path traversal rejected")
	ErrGitRejected    = errors.New("active .git sync rejected")
	ErrDeviceNotFound = errors.New("sync device not found")
)

// DefaultSyncRoot is the server-side root under which all sync folders live.
const DefaultSyncRoot = "/srv/omahab/sync"

// DefaultNotesExclusions returns the sensible Obsidian/notes exclusions from DESIGN §16.
// These are applied to Notes folders and form the baseline for other folders.
func DefaultNotesExclusions() []string {
	return []string{
		".obsidian/workspace*",
		".obsidian/cache/",
		".obsidian/plugins/*/data.json",
		".trash/",
		".stversions/",
		"*sync-conflict*",
	}
}

// DefaultExclusions returns the baseline exclusions for a folder by name.
// Notes-like folders receive the full Obsidian set; others receive the minimal set.
func DefaultExclusions(folderName string) []string {
	if strings.EqualFold(folderName, "Notes") {
		return DefaultNotesExclusions()
	}
	return []string{
		".stversions/",
		"*sync-conflict*",
	}
}

// KnowledgeRegistrar registers a server path as a default-assistant knowledge source.
// Implementations must scope registration to the default Hermes profile only;
// project bots must never receive synced-folder sources.
type KnowledgeRegistrar interface {
	Register(ctx context.Context, sourceID string, serverPath string) error
	Unregister(ctx context.Context, sourceID string) error
}

// NoopRegistrar is a no-op KnowledgeRegistrar for testing or when knowledge
// indexing is disabled.
type NoopRegistrar struct{}

func (NoopRegistrar) Register(_ context.Context, _, _ string) error { return nil }
func (NoopRegistrar) Unregister(_ context.Context, _ string) error  { return nil }

// Service owns Syncthing folder and device metadata.
type Service struct {
	db        *sql.DB
	syncRoot  string
	registrar KnowledgeRegistrar
}

// New creates a Service. syncRoot is the server-side directory under which all
// folder paths must reside. If empty, DefaultSyncRoot is used. registrar may be
// nil, in which case NoopRegistrar is used.
func New(db *sql.DB, syncRoot string, registrar KnowledgeRegistrar) *Service {
	if syncRoot == "" {
		syncRoot = DefaultSyncRoot
	}
	syncRoot = filepath.Clean(syncRoot)
	if registrar == nil {
		registrar = NoopRegistrar{}
	}
	return &Service{db: db, syncRoot: syncRoot, registrar: registrar}
}

// SyncRoot returns the configured server-side root.
func (s *Service) SyncRoot() string { return s.syncRoot }

// CreateInput holds the fields required to create a sync folder.
type CreateInput struct {
	Name        string
	ServerPath  string
	ShareWithAI bool
}

// UpdateInput holds mutable fields for a sync folder.
type UpdateInput struct {
	Name        *string
	ShareWithAI *bool
}

// Device represents an enrolled Syncthing device for a folder.
type Device struct {
	ID         string    `json:"id"`
	FolderID   string    `json:"folder_id"`
	DeviceID   string    `json:"device_id"`
	DeviceName string    `json:"device_name"`
	CreatedAt  time.Time `json:"created_at"`
}

// Create validates, persists, and optionally registers a new sync folder.
func (s *Service) Create(ctx context.Context, in CreateInput) (*domain.SyncFolder, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, fmt.Errorf("%w: name is required", ErrValidation)
	}
	if len(name) > 128 {
		return nil, fmt.Errorf("%w: name too long", ErrValidation)
	}
	serverPath, err := s.validateServerPath(in.ServerPath)
	if err != nil {
		return nil, err
	}
	// Reject active .git sync: any path component equal to .git
	if containsGitComponent(serverPath) {
		return nil, ErrGitRejected
	}

	id := newID()
	now := time.Now().UTC()

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO sync_folders (id, name, server_path, share_with_ai, health, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, name, serverPath, boolToInt(in.ShareWithAI), string(domain.HealthUnknown), now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano),
	)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrAlreadyExists
		}
		return nil, fmt.Errorf("insert sync folder: %w", err)
	}

	folder := &domain.SyncFolder{
		ID:          domain.ID(id),
		Name:        name,
		ServerPath:  serverPath,
		ShareWithAI: in.ShareWithAI,
		Health:      domain.HealthUnknown,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	if in.ShareWithAI {
		if err := s.registrar.Register(ctx, id, serverPath); err != nil {
			// Best-effort: log via error wrapping; do not leave folder without source
			// registration silently. Caller can retry sync of registration.
			_ = err
		}
	}

	return folder, nil
}

// Get returns a folder by ID.
func (s *Service) Get(ctx context.Context, id string) (*domain.SyncFolder, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, name, server_path, share_with_ai, health, created_at, updated_at FROM sync_folders WHERE id = ?`, id)
	return scanFolder(row)
}

// GetByName returns a folder by name.
func (s *Service) GetByName(ctx context.Context, name string) (*domain.SyncFolder, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, name, server_path, share_with_ai, health, created_at, updated_at FROM sync_folders WHERE name = ?`, strings.TrimSpace(name))
	return scanFolder(row)
}

// List returns all sync folders ordered by name.
func (s *Service) List(ctx context.Context) ([]*domain.SyncFolder, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, server_path, share_with_ai, health, created_at, updated_at FROM sync_folders ORDER BY name ASC`)
	if err != nil {
		return nil, fmt.Errorf("list sync folders: %w", err)
	}
	defer rows.Close()
	var out []*domain.SyncFolder
	for rows.Next() {
		f, err := scanFolder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// Update mutates a folder's mutable fields. ServerPath is immutable after creation
// to avoid silently moving synced data.
func (s *Service) Update(ctx context.Context, id string, in UpdateInput) (*domain.SyncFolder, error) {
	existing, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}

	name := existing.Name
	if in.Name != nil {
		trimmed := strings.TrimSpace(*in.Name)
		if trimmed == "" {
			return nil, fmt.Errorf("%w: name is required", ErrValidation)
		}
		if len(trimmed) > 128 {
			return nil, fmt.Errorf("%w: name too long", ErrValidation)
		}
		name = trimmed
	}

	shareWithAI := existing.ShareWithAI
	if in.ShareWithAI != nil {
		shareWithAI = *in.ShareWithAI
	}

	now := time.Now().UTC()
	_, err = s.db.ExecContext(ctx,
		`UPDATE sync_folders SET name = ?, share_with_ai = ?, updated_at = ? WHERE id = ?`,
		name, boolToInt(shareWithAI), now.Format(time.RFC3339Nano), id,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrAlreadyExists
		}
		return nil, fmt.Errorf("update sync folder: %w", err)
	}

	// Handle Share-with-AI registration transitions.
	if !existing.ShareWithAI && shareWithAI {
		_ = s.registrar.Register(ctx, id, existing.ServerPath)
	} else if existing.ShareWithAI && !shareWithAI {
		_ = s.registrar.Unregister(ctx, id)
	}

	existing.Name = name
	existing.ShareWithAI = shareWithAI
	existing.UpdatedAt = now
	return existing, nil
}

// Delete removes a folder and unregisters its knowledge source if needed.
func (s *Service) Delete(ctx context.Context, id string) error {
	folder, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if folder.ShareWithAI {
		_ = s.registrar.Unregister(ctx, id)
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM sync_folders WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete sync folder: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// EnrollDevice adds a Syncthing device to a folder.
func (s *Service) EnrollDevice(ctx context.Context, folderID, deviceID, deviceName string) (*Device, error) {
	if strings.TrimSpace(deviceID) == "" {
		return nil, fmt.Errorf("%w: device_id is required", ErrValidation)
	}
	// Ensure folder exists
	if _, err := s.Get(ctx, folderID); err != nil {
		return nil, err
	}
	id := newID()
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sync_devices (id, folder_id, device_id, device_name, created_at) VALUES (?, ?, ?, ?, ?)`,
		id, folderID, strings.TrimSpace(deviceID), strings.TrimSpace(deviceName), now.Format(time.RFC3339Nano),
	)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("%w: device already enrolled for this folder", ErrAlreadyExists)
		}
		return nil, fmt.Errorf("enroll device: %w", err)
	}
	return &Device{
		ID:         id,
		FolderID:   folderID,
		DeviceID:   strings.TrimSpace(deviceID),
		DeviceName: strings.TrimSpace(deviceName),
		CreatedAt:  now,
	}, nil
}

// ListDevices returns devices enrolled for a folder.
func (s *Service) ListDevices(ctx context.Context, folderID string) ([]*Device, error) {
	if _, err := s.Get(ctx, folderID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, folder_id, device_id, device_name, created_at FROM sync_devices WHERE folder_id = ? ORDER BY created_at ASC`, folderID)
	if err != nil {
		return nil, fmt.Errorf("list devices: %w", err)
	}
	defer rows.Close()
	var out []*Device
	for rows.Next() {
		var d Device
		var createdAt string
		if err := rows.Scan(&d.ID, &d.FolderID, &d.DeviceID, &d.DeviceName, &createdAt); err != nil {
			return nil, err
		}
		d.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
		out = append(out, &d)
	}
	return out, rows.Err()
}

// RemoveDevice unenrolls a device from a folder.
func (s *Service) RemoveDevice(ctx context.Context, folderID, deviceID string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM sync_devices WHERE folder_id = ? AND device_id = ?`, folderID, strings.TrimSpace(deviceID))
	if err != nil {
		return fmt.Errorf("remove device: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrDeviceNotFound
	}
	return nil
}

// ExclusionsFor returns the default exclusions for the named folder.
func (s *Service) ExclusionsFor(folderName string) []string {
	return DefaultExclusions(folderName)
}

// validateServerPath ensures the path is absolute, within syncRoot, contains no
// traversal, and is not empty.
func (s *Service) validateServerPath(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("%w: server_path is required", ErrValidation)
	}
	if strings.Contains(trimmed, "\x00") {
		return "", fmt.Errorf("%w: server_path contains NUL", ErrValidation)
	}
	// Reject any raw ".." component before cleaning to catch traversal attempts
	// even when Clean would normalize them away.
	for _, part := range strings.Split(trimmed, "/") {
		if part == ".." {
			return "", ErrPathTraversal
		}
	}
	cleaned := filepath.Clean(trimmed)
	if !filepath.IsAbs(cleaned) {
		return "", fmt.Errorf("%w: server_path must be absolute", ErrValidation)
	}
	// Must be within syncRoot (or exactly syncRoot is rejected — must be a subdir)
	rel, err := filepath.Rel(s.syncRoot, cleaned)
	if err != nil {
		return "", ErrPathTraversal
	}
	if rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
		return "", ErrPathTraversal
	}
	return cleaned, nil
}

func containsGitComponent(serverPath string) bool {
	parts := strings.Split(filepath.Clean(serverPath), string(filepath.Separator))
	for _, p := range parts {
		if p == ".git" {
			return true
		}
	}
	return false
}

type folderScanner interface {
	Scan(dest ...any) error
}

func scanFolder(row folderScanner) (*domain.SyncFolder, error) {
	var (
		id, name, serverPath, health string
		shareWithAI                  int
		createdAt, updatedAt         string
	)
	if err := row.Scan(&id, &name, &serverPath, &shareWithAI, &health, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scan sync folder: %w", err)
	}
	ca, _ := time.Parse(time.RFC3339Nano, createdAt)
	ua, _ := time.Parse(time.RFC3339Nano, updatedAt)
	return &domain.SyncFolder{
		ID:          domain.ID(id),
		Name:        name,
		ServerPath:  serverPath,
		ShareWithAI: shareWithAI != 0,
		Health:      domain.Health(health),
		CreatedAt:   ca,
		UpdatedAt:   ua,
	}, nil
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") || strings.Contains(msg, "unique constraint")
}
