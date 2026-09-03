package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/omahab/omahab/internal/api"
	"github.com/omahab/omahab/internal/backups"
	"github.com/omahab/omahab/internal/events"
	"github.com/omahab/omahab/internal/secrets"
	"github.com/omahab/omahab/internal/setupguide"
	"github.com/omahab/omahab/internal/store"
)

// ---------------------------------------------------------------------------
// Cloudflare token verification (wizard live check).
// ---------------------------------------------------------------------------

// VerifyCloudflareToken runs the live token check server-side (the browser
// cannot call the CF API cross-origin).
func (b *Backend) VerifyCloudflareToken(ctx context.Context, token string) (api.VerifyCloudflareTokenResult, error) {
	if err := setupguide.ValidateCloudflareToken(token); err != nil {
		return api.VerifyCloudflareTokenResult{}, fmt.Errorf("%w: %v", store.ErrValidation, err)
	}
	ok, status, detail := setupguide.VerifyCloudflareTokenLive(ctx, token)
	return api.VerifyCloudflareTokenResult{
		OK:     ok,
		Status: status,
		Detail: detail,
	}, nil
}

// ---------------------------------------------------------------------------
// Recovery key (DESIGN §9): 24-word BIP39 phrase + recovery.kit.
// ---------------------------------------------------------------------------

var (
	recoveryMu   sync.Mutex
	recoveryPend = map[string]recoveryPending{}
)

type recoveryPending struct {
	phrase  []string
	seed    [32]byte
	expires time.Time
}

// GenerateRecoveryKey creates a fresh 24-word phrase and holds the seed in
// memory keyed by fingerprint (first 8 hex of SHA-256(seed)) for 15 minutes.
// The phrase is shown once and never persisted until ConfirmRecoveryKey.
func (b *Backend) GenerateRecoveryKey(ctx context.Context) (api.RecoveryKeyMaterial, error) {
	words, seed, err := secrets.GenerateRecoveryPhrase()
	if err != nil {
		return api.RecoveryKeyMaterial{}, fmt.Errorf("generate phrase: %w", err)
	}
	sum := sha256.Sum256(seed[:])
	fingerprint := hex.EncodeToString(sum[:4])
	now := time.Now()
	recoveryMu.Lock()
	// purge expired
	for k, v := range recoveryPend {
		if now.After(v.expires) {
			delete(recoveryPend, k)
		}
	}
	phraseCopy := make([]string, len(words))
	copy(phraseCopy, words)
	recoveryPend[fingerprint] = recoveryPending{phrase: phraseCopy, seed: seed, expires: now.Add(15 * time.Minute)}
	recoveryMu.Unlock()
	return api.RecoveryKeyMaterial{
		Phrase:      phraseCopy,
		Fingerprint: fingerprint,
	}, nil
}

// ConfirmRecoveryKey verifies the 3-word challenge, wraps the master key with
// the derived recovery key, and persists recovery.kit (0600) plus the
// platform-app/recovery_fingerprint secret.
func (b *Backend) ConfirmRecoveryKey(ctx context.Context, fingerprint string, challenge map[int]string) error {
	fingerprint = strings.TrimSpace(strings.ToLower(fingerprint))
	if fingerprint == "" {
		return fmt.Errorf("%w: fingerprint is required", store.ErrValidation)
	}
	if len(challenge) != 3 {
		return fmt.Errorf("%w: challenge must have exactly 3 entries", store.ErrValidation)
	}
	recoveryMu.Lock()
	pend, ok := recoveryPend[fingerprint]
	if ok && time.Now().After(pend.expires) {
		delete(recoveryPend, fingerprint)
		ok = false
	}
	recoveryMu.Unlock()
	if !ok {
		return fmt.Errorf("%w: recovery phrase expired or not found; generate again", store.ErrValidation)
	}
	for idx, word := range challenge {
		if idx < 0 || idx >= 24 {
			return fmt.Errorf("%w: challenge index %d out of range (0-23)", store.ErrValidation, idx)
		}
		expected := pend.phrase[idx]
		if strings.TrimSpace(strings.ToLower(word)) != strings.TrimSpace(strings.ToLower(expected)) {
			return fmt.Errorf("%w: challenge word mismatch at position %d", store.ErrValidation, idx)
		}
	}
	recoveryKey := secrets.DeriveRecoveryKey(pend.seed)
	wrapped := secrets.WrapMasterKey(b.masterKey, recoveryKey)
	kit := struct {
		Version       int    `json:"version"`
		Fingerprint   string `json:"fingerprint"`
		MasterWrapped string `json:"master_wrapped"`
		CreatedAt     string `json:"created_at"`
	}{
		Version:       1,
		Fingerprint:   fingerprint,
		MasterWrapped: base64.StdEncoding.EncodeToString(wrapped),
		CreatedAt:     time.Now().UTC().Format(time.RFC3339),
	}
	data, err := json.MarshalIndent(kit, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal recovery.kit: %w", err)
	}
	if err := os.MkdirAll(b.cfg.StateDir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(b.cfg.StateDir, "recovery.kit")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write recovery.kit: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("persist recovery.kit: %w", err)
	}
	_ = os.Chmod(path, 0o600)
	if err := upsertSecret(ctx, b.secrets, "platform-app", "recovery_fingerprint", fingerprint); err != nil {
		return fmt.Errorf("store recovery fingerprint: %w", err)
	}
	recoveryMu.Lock()
	delete(recoveryPend, fingerprint)
	recoveryMu.Unlock()
	return nil
}


// ---------------------------------------------------------------------------
// Storage placement: candidate disks + media/data placement.
// ---------------------------------------------------------------------------

// ListDisks parses lsblk -J and returns candidate filesystems (excluding
// root/boot/tailscale devices).
func (b *Backend) ListDisks(ctx context.Context) ([]api.Disk, error) {
	out, err := exec.CommandContext(ctx, "lsblk", "-J", "-o", "NAME,SIZE,TYPE,FSTYPE,UUID,MOUNTPOINT").Output()
	if err != nil {
		return nil, fmt.Errorf("lsblk: %w", err)
	}
	disks, err := parseLSBlock(out)
	if err != nil {
		return nil, err
	}
	var out2 []api.Disk
	for _, d := range disks {
		if d.FSType == "" {
			continue // unformatted candidates still listed below
		}
		out2 = append(out2, d)
	}
	return out2, nil
}

// ConfigureStorage appends a volume placement to <StateDir>/storage.json.
// The omahab-storage oneshot unit consumes it at boot.
func (b *Backend) ConfigureStorage(ctx context.Context, req api.ConfigureStorageRequest) error {
	volume := strings.TrimSpace(req.Volume)
	uuid := strings.TrimSpace(req.FSUUID)
	if volume != "media" && volume != "data" {
		return fmt.Errorf("%w: volume must be media or data", store.ErrValidation)
	}
	if uuid == "" {
		return fmt.Errorf("%w: fs_uuid is required", store.ErrValidation)
	}
	path := filepath.Join(b.cfg.StateDir, "storage.json")
	var entries []storageEntry
	if data, err := os.ReadFile(path); err == nil {
		_ = jsonUnmarshalStorage(data, &entries)
	}
	// Reject a change once the target dir is non-empty.
	target := "/srv/omahab"
	if volume == "media" {
		target = "/srv/omahab/apps/immich"
	}
	alreadyConfigured := false
	for _, e := range entries {
		if e.Volume == volume {
			alreadyConfigured = true
			break
		}
	}
	if !alreadyConfigured {
		if items, rerr := os.ReadDir(target); rerr == nil && len(items) > 0 {
			return fmt.Errorf("%w: %s already holds data; changing placement after data exists is unsupported", store.ErrValidation, target)
		}
	}
	// Replace any prior entry for the same volume.
	for i := range entries {
		if entries[i].Volume == volume {
			entries = append(entries[:i], entries[i+1:]...)
			break
		}
	}
	entries = append(entries, storageEntry{Volume: volume, FSUUID: uuid})
	raw, err := jsonMarshalStorage(entries)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(b.cfg.StateDir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

// ---------------------------------------------------------------------------
// Backup repositories (wizard): HTTP bridge to backups.Service.Configure,
// storing credential material via secrets (value never accepted by
// Configure itself).
// ---------------------------------------------------------------------------

// ListBackupRepositories lists configured repositories.
func (b *Backend) ListBackupRepositories(ctx context.Context) ([]backups.Repository, error) {
	return b.backups.Repositories(ctx)
}

// CreateBackupRepository stores the credentials via the secrets service
// and configures the repository via SecretRef; enables the timers.
func (b *Backend) CreateBackupRepository(ctx context.Context, req api.CreateBackupRepositoryRequest) (backups.Repository, error) {
	label := strings.TrimSpace(req.Label)
	if label == "" {
		return backups.Repository{}, fmt.Errorf("%w: label is required", store.ErrValidation)
	}
	location := strings.TrimSpace(req.Location)
	if location == "" {
		return backups.Repository{}, fmt.Errorf("%w: location is required", store.ErrValidation)
	}
	// Credential material -> secrets store.
	secretName := "backup_repo_credentials"
	value := strings.TrimSpace(req.Password)
	if value == "" {
		return backups.Repository{}, fmt.Errorf("%w: password is required", store.ErrValidation)
	}
	sec, err := b.secrets.Put(ctx, "platform-app", secretName, value)
	if err != nil {
		if _, err2 := b.secrets.RotateByName(ctx, "platform-app", secretName, value); err2 != nil {
			return backups.Repository{}, fmt.Errorf("store credentials: %w", err)
		}
		sec, err = b.secrets.GetByName(ctx, "platform-app", secretName)
		if err != nil {
			return backups.Repository{}, fmt.Errorf("load credential ref: %w", err)
		}
	}
	repo, err := b.backups.Configure(ctx, backups.ConfigureRequest{
		Label: label,
		Location: location,
		SecretRef: backups.SecretRef{
			ID:      string(sec.ID),
			Version: sec.Version,
		},
	})
	if err != nil {
		return backups.Repository{}, err
	}
	b.enableBackupTimers(ctx)
	return repo, nil
}

// DeleteBackupRepository removes a repository (audit-retained when runs exist).
func (b *Backend) DeleteBackupRepository(ctx context.Context, id string) error {
	return b.backups.DeleteRepository(ctx, id)
}

// enableBackupTimers turns on the daily backup + weekly verify timers.
func (b *Backend) enableBackupTimers(ctx context.Context) {
	for _, unit := range []string{"omahab-backup.timer", "omahab-verify.timer"} {
		if out, err := exec.CommandContext(ctx, "systemctl", "enable", "--now", unit).CombinedOutput(); err != nil {
			_, _ = b.events.Publish(ctx, events.PublishInput{
				Type:     "backups.timer_enable_failed",
				Severity: "warning",
				Message:  fmt.Sprintf("enable %s: %v: %s", unit, err, strings.TrimSpace(string(out))),
			})
		}
	}
}


type storageEntry struct {
	Volume string `json:"volume"`
	FSUUID string `json:"fs_uuid"`
}

func jsonMarshalStorage(entries []storageEntry) ([]byte, error) {
	return json.MarshalIndent(entries, "", "  ")
}

func jsonUnmarshalStorage(data []byte, out *[]storageEntry) error {
	return json.Unmarshal(data, out)
}

// lsblkDevices models the lsblk -J output.
type lsblkOutput struct {
	BlockDevices []lsblkDevice `json:"blockdevices"`
}

type lsblkDevice struct {
	Name       string        `json:"name"`
	Size       string        `json:"size"`
	Type       string        `json:"type"`
	FSType     string        `json:"fstype"`
	UUID       string        `json:"uuid"`
	Mountpoint string        `json:"mountpoint"`
	Children   []lsblkDevice `json:"children,omitempty"`
}

// parseLSBlock flattens lsblk JSON into candidate disks, excluding the
// root/boot filesystems and tailscale devices.
func parseLSBlock(raw []byte) ([]api.Disk, error) {
	var out lsblkOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse lsblk: %w", err)
	}
	var disks []api.Disk
	var walk func(ds []lsblkDevice)
	walk = func(ds []lsblkDevice) {
		for _, d := range ds {
			if d.Type == "disk" || d.Type == "part" {
				if d.FSType != "" && d.UUID != "" {
					mp := d.Mountpoint
					if mp == "/" || mp == "/boot" || mp == "/nix" || mp == "/tmp" || strings.HasPrefix(d.Name, "ts") {
						// skip system volumes
					} else {
						disks = append(disks, api.Disk{
							Name:       d.Name,
							Size:       d.Size,
							Type:       d.Type,
							FSType:     d.FSType,
							UUID:       d.UUID,
							Mountpoint: mp,
						})
					}
				}
			}
			walk(d.Children)
		}
	}
	walk(out.BlockDevices)
	return disks, nil
}
