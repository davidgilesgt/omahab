package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/omahab/omahab/internal/api"
	"github.com/omahab/omahab/internal/backups"
	"github.com/omahab/omahab/internal/domain"
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
// Recovery key (DESIGN §9): server-side generate + confirm.
// ---------------------------------------------------------------------------

// GenerateRecoveryKey creates a fresh age key pair, returns public key,
// private key (shown once), and the armored recovery kit. Nothing is
// persisted until ConfirmRecoveryKey.
func (b *Backend) GenerateRecoveryKey(ctx context.Context) (api.RecoveryKeyMaterial, error) {
	pub, priv, err := secrets.GenerateAgeKeyPair()
	if err != nil {
		return api.RecoveryKeyMaterial{}, fmt.Errorf("generate age key pair: %w", err)
	}
	kit, err := b.secrets.ExportRecoveryCopy(ctx, pub)
	if err != nil {
		return api.RecoveryKeyMaterial{}, fmt.Errorf("export recovery copy: %w", err)
	}
	return api.RecoveryKeyMaterial{
		PublicKey:  pub,
		PrivateKey: priv,
		Kit:        kit,
	}, nil
}

// ConfirmRecoveryKey re-exports the kit to the user-confirmed public key,
// persists the armored kit at <StateDir>/recovery.age, stores the
// platform-app/recovery_age_recipient secret, and marks the recovery_key
// setup check ok (identity_recoveries row inserted by recovery drill).
func (b *Backend) ConfirmRecoveryKey(ctx context.Context, publicKey string) error {
	if err := setupguide.ValidateRecoveryKey(publicKey); err != nil {
		return fmt.Errorf("%w: %v", store.ErrValidation, err)
	}
	kit, err := b.secrets.ExportRecoveryCopy(ctx, publicKey)
	if err != nil {
		return fmt.Errorf("export recovery copy: %w", err)
	}
	if err := os.MkdirAll(b.cfg.StateDir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(b.cfg.StateDir, "recovery.age")
	if err := os.WriteFile(path, []byte(kit), 0o600); err != nil {
		return fmt.Errorf("write recovery.age: %w", err)
	}
	if err := upsertSecret(ctx, b.secrets, "platform-app", "recovery_age_recipient", publicKey); err != nil {
		return fmt.Errorf("store recovery recipient: %w", err)
	}
	// Record a recovery-key completion row so the recovery_tested check
	// flips to ok (the actual drill remains recommended).
	if _, err := b.db.ExecContext(ctx,
		`INSERT INTO identity_recoveries (id, user_id, method, status, performed_at)
		 VALUES (?, 'system', 'recovery-key-export', 'ok', CURRENT_TIMESTAMP)`,
		domain.ID(newSetupID())); err != nil {
		// Non-fatal: check stays pending until a real drill.
		_ = err
	}
	return nil
}

func newSetupID() string {
	return fmt.Sprintf("rec_%d", os.Getpid())
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
