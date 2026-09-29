package controlplane

import (
	"context"
	"crypto/rand"
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

	"github.com/omahab/omahab/internal/apitypes"
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
func (b *Backend) VerifyCloudflareToken(ctx context.Context, token string) (apitypes.VerifyCloudflareTokenResult, error) {
	if err := setupguide.ValidateCloudflareToken(token); err != nil {
		return apitypes.VerifyCloudflareTokenResult{}, fmt.Errorf("%w: %v", store.ErrValidation, err)
	}
	ok, status, detail := setupguide.VerifyCloudflareTokenLive(ctx, token)
	return apitypes.VerifyCloudflareTokenResult{
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
	// lastConfirmedSeed holds the seed of the most recently confirmed
	// recovery phrase for Hetzner backup password derivation.
	lastConfirmedSeed     *[32]byte
	lastConfirmedSeedTime time.Time
	// lastConfirmedFingerprint binds the cached seed to the kit it
	// confirmed, so a stale cache never authenticates a rotated kit.
	lastConfirmedFingerprint string
)

type recoveryPending struct {
	phrase  []string
	seed    [32]byte
	expires time.Time
}

// GenerateRecoveryKey creates a fresh 24-word phrase and holds the seed in
// memory keyed by fingerprint (first 8 hex of SHA-256(seed)) for 15 minutes.
// The phrase is shown once and never persisted until ConfirmRecoveryKey.
func (b *Backend) GenerateRecoveryKey(ctx context.Context) (apitypes.RecoveryKeyMaterial, error) {
	words, seed, err := secrets.GenerateRecoveryPhrase()
	if err != nil {
		return apitypes.RecoveryKeyMaterial{}, fmt.Errorf("generate phrase: %w", err)
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
	return apitypes.RecoveryKeyMaterial{
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
		return fmt.Errorf("%w: recovery phrase expired or not found; generate again", store.ErrNotFound)
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
	return b.persistRecoveryKit(ctx, fingerprint, pend.seed)
}

// ConfirmRecoveryKeySavedAck persists recovery.kit for the new client ritual:
// the user ticks an explicit "I saved the phrase" ack and the client has
// already verified a paste-back locally. No challenge words are sent; the
// pending seed (held in memory for 15 minutes after generate) proves the
// phrase was freshly generated in this session. Back-compat: the 3-word
// challenge path via ConfirmRecoveryKey keeps working unchanged.
func (b *Backend) ConfirmRecoveryKeySavedAck(ctx context.Context, fingerprint string) error {
	fingerprint = strings.TrimSpace(strings.ToLower(fingerprint))
	if fingerprint == "" {
		return fmt.Errorf("%w: fingerprint is required", store.ErrValidation)
	}
	recoveryMu.Lock()
	pend, ok := recoveryPend[fingerprint]
	if ok && time.Now().After(pend.expires) {
		delete(recoveryPend, fingerprint)
		ok = false
	}
	seed := pend.seed
	recoveryMu.Unlock()
	if !ok {
		return fmt.Errorf("%w: recovery phrase expired or not found; generate again", store.ErrNotFound)
	}
	return b.persistRecoveryKit(ctx, fingerprint, seed)
}

// persistRecoveryKit wraps the master key with the derived recovery key and
// persists recovery.kit (0600) plus the platform-app/recovery_fingerprint
// secret. It consumes the pending entry and caches the seed for Hetzner
// backup password derivation shortly after confirm.
func (b *Backend) persistRecoveryKit(ctx context.Context, fingerprint string, seed [32]byte) error {
	recoveryKey := secrets.DeriveRecoveryKey(seed)
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
	recoveryMu.Lock()
	// Cache seed for Hetzner backup password derivation shortly after confirm.
	seedCopy := seed
	lastConfirmedSeed = &seedCopy
	lastConfirmedSeedTime = time.Now()
	lastConfirmedFingerprint = strings.ToLower(strings.TrimSpace(fingerprint))
	delete(recoveryPend, fingerprint)
	recoveryMu.Unlock()
	return nil
}

// ---------------------------------------------------------------------------
// Storage placement: candidate disks + media/data placement.
// ---------------------------------------------------------------------------

// ListDisks parses lsblk -J and returns candidate filesystems (excluding
// root/boot/tailscale devices).
func (b *Backend) ListDisks(ctx context.Context) ([]apitypes.Disk, error) {
	out, err := exec.CommandContext(ctx, "lsblk", "-J", "-o", "NAME,SIZE,TYPE,FSTYPE,UUID,MOUNTPOINT").Output()
	if err != nil {
		return nil, fmt.Errorf("lsblk: %w", err)
	}
	disks, err := parseLSBlock(out)
	if err != nil {
		return nil, err
	}
	var out2 []apitypes.Disk
	for _, d := range disks {
		if d.FSType == "" {
			continue // unformatted candidates still listed below
		}
		out2 = append(out2, d)
	}
	return out2, nil
}

// ConfigureStorage lives in storage.go (v2 versioned placements with a
// journaled migration operation).

// ---------------------------------------------------------------------------
// Backup repositories (wizard): HTTP bridge to backups.Service.Configure,
// storing credential material via secrets (value never accepted by
// Configure itself).
// ---------------------------------------------------------------------------

// ListBackupRepositories lists configured repositories.
func (b *Backend) ListBackupRepositories(ctx context.Context) ([]backups.Repository, error) {
	return b.backups.Repositories(ctx)
}

// validBackupUsername reports whether s is safe to embed in the sftp URL
// userinfo (Hetzner Storage Box account, e.g. u123456 or u123456-sub1).
func validBackupUsername(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

// validBackupHost reports whether s is a bare hostname/IP safe to embed in
// the sftp URL host (no userinfo, port, path, or query smuggling).
func validBackupHost(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '-' {
			continue
		}
		return false
	}
	return true
}

// CreateBackupRepository stores the credentials via the secrets service
// and configures the repository via SecretRef; enables the timers.
// It supports two forms:
//   - Hetzner Storage Box (recommended): kind=hetzner_storagebox, username, host, sub_account_password
//   - Generic/Advanced: location, password
//
// For Hetzner it generates an ed25519 key, uploads the pubkey via SFTP (port 23, password auth),
// records the host key, derives the restic password from the recovery phrase seed, and sets
// location = sftp://<user>@<host>:23/./omahab/<instanceID>.
func (b *Backend) CreateBackupRepository(ctx context.Context, req apitypes.CreateBackupRepositoryRequest) (backups.Repository, error) {
	label := strings.TrimSpace(req.Label)
	if label == "" {
		return backups.Repository{}, fmt.Errorf("%w: label is required", store.ErrValidation)
	}
	kind := strings.TrimSpace(strings.ToLower(req.Kind))
	isHetzner := kind == "hetzner_storagebox" || kind == "hetzner" || kind == "hetzner_storage_box"
	// Also treat as Hetzner if username/host/password present and location empty.
	if !isHetzner && req.Username != "" && req.Host != "" && req.SubAccountPassword != "" && strings.TrimSpace(req.Location) == "" {
		isHetzner = true
	}
	var location string
	var password string
	mode := strings.ToLower(strings.TrimSpace(req.Mode))
	if mode == "" {
		mode = "create"
	}
	if mode != "create" && mode != "attach" {
		return backups.Repository{}, fmt.Errorf("%w: mode must be create|attach", store.ErrValidation)
	}
	if isHetzner {
		// Authenticate the recovery phrase BEFORE generating/uploading an SSH
		// key or mutating repository configuration. A valid supplied phrase
		// authenticates against the saved kit; otherwise the confirmed seed
		// still held in memory and bound to that kit applies. There is no
		// random fallback: an unrecoverable repository must never be created
		// silently.
		var seed [32]byte
		if strings.TrimSpace(req.Phrase) != "" {
			s, err := b.authenticateRecoverySeed(req.Phrase)
			if err != nil {
				return backups.Repository{}, err
			}
			seed = s
		} else if s, err := b.confirmedRecoverySeed(); err == nil {
			seed = s
		} else {
			return backups.Repository{}, ErrRecoveryPhraseRequired
		}
		username := strings.TrimSpace(req.Username)
		host := strings.TrimSpace(req.Host)
		subPass := req.SubAccountPassword // keep as is, may contain spaces
		if username == "" {
			return backups.Repository{}, fmt.Errorf("%w: username is required for Hetzner", store.ErrValidation)
		}
		if host == "" {
			return backups.Repository{}, fmt.Errorf("%w: host is required for Hetzner", store.ErrValidation)
		}
		if !validBackupUsername(username) {
			return backups.Repository{}, fmt.Errorf("%w: invalid username for Hetzner (letters, digits, '.', '_', '-' only)", store.ErrValidation)
		}
		if !validBackupHost(host) {
			return backups.Repository{}, fmt.Errorf("%w: invalid host for Hetzner (bare hostname or IP only)", store.ErrValidation)
		}
		if strings.TrimSpace(subPass) == "" {
			return backups.Repository{}, fmt.Errorf("%w: sub_account_password is required for Hetzner", store.ErrValidation)
		}
		// 1. Ensure ed25519 key pair
		_, pubLine, err := ensureBackupSSHKey(b.cfg.StateDir)
		if err != nil {
			return backups.Repository{}, fmt.Errorf("generate backup ssh key: %w", err)
		}
		// 2. Upload pubkey via SFTP. Failure aborts setup before any
		// repository is configured or persisted; the password is never
		// stored and never appears in the error.
		if err := uploadHetznerKey(ctx, b.cfg.StateDir, host, username, subPass, pubLine); err != nil {
			subPass = ""
			return backups.Repository{}, fmt.Errorf("upload backup ssh key: %w", err)
		}
		// Discard password (never stored)
		subPass = ""
		if mode == "attach" {
			// Attach preserves the caller's exact repository URL: no
			// instance suffix is appended and no empty namespace is created.
			location = strings.TrimSpace(req.Location)
			if location == "" {
				return backups.Repository{}, fmt.Errorf("%w: location is required for attach (exact source repository URL)", store.ErrValidation)
			}
		} else {
			// 3. Instance ID for location suffix
			instanceID := ""
			if inst, err := b.store.Instance(ctx); err == nil {
				instanceID = string(inst.ID)
			}
			if instanceID == "" {
				instanceID = "omahab"
			}
			// Sanitize instanceID to be filesystem-safe (alphanumeric + -)
			safeID := strings.Map(func(r rune) rune {
				if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
					return r
				}
				return '-'
			}, instanceID)
			location = fmt.Sprintf("sftp://%s@%s:23/./omahab/%s", username, host, safeID)
		}
		// 4. Derive restic password from the authenticated phrase seed.
		// Existing HKDF (omahab-recovery-v1, restic-password) is kept so
		// valid repositories remain readable.
		password = secrets.DeriveResticPassword(seed)
	} else {
		location = strings.TrimSpace(req.Location)
		if location == "" {
			return backups.Repository{}, fmt.Errorf("%w: location is required", store.ErrValidation)
		}
		password = strings.TrimSpace(req.Password)
		if password == "" {
			return backups.Repository{}, fmt.Errorf("%w: password is required", store.ErrValidation)
		}
		if mode == "attach" {
			// Preserve the exact source repository URL/instance suffix;
			// attach never appends a new machine ID.
			location = strings.TrimSpace(req.Location)
		}
		// Allow location with Env overrides; keep as is.
	}
	// Each repository gets its own secret record instead of rotating the
	// shared backup_repo_credentials name. Existing references migrate
	// without changing their actual passwords (see migrateBackupCredentialShapes).
	// The per-repo name needs the repository ID, so credentials stage under
	// a provisional name first and move once the repository exists.
	putRepoSecret := func(value string, key string) (backups.SecretRef, error) {
		encoded, err := json.Marshal(backups.Credentials{Password: value})
		if err != nil {
			return backups.SecretRef{}, fmt.Errorf("encode credentials: %w", err)
		}
		name := "backup_repo_" + sanitizeSecretName(key)
		sec, err := b.secrets.Put(ctx, "platform-app", name, string(encoded))
		if err != nil {
			if _, err2 := b.secrets.RotateByName(ctx, "platform-app", name, string(encoded)); err2 != nil {
				return backups.SecretRef{}, fmt.Errorf("store credentials: %w", err)
			}
			sec, err = b.secrets.GetByName(ctx, "platform-app", name)
			if err != nil {
				return backups.SecretRef{}, fmt.Errorf("load credential ref: %w", err)
			}
		}
		return backups.SecretRef{ID: string(sec.ID), Version: sec.Version}, nil
	}
	stageKey := "staging"
	var rnd [4]byte
	if _, err := rand.Read(rnd[:]); err == nil {
		stageKey = fmt.Sprintf("staging-%s", hex.EncodeToString(rnd[:]))
	}
	stageRef, err := putRepoSecret(password, stageKey)
	if err != nil {
		return backups.Repository{}, err
	}
	var repo backups.Repository
	if mode == "attach" {
		// Attach runs `restic snapshots`, never init, against the exact
		// source URL (no new machine-ID suffix).
		repo, err = b.backups.AttachRepository(ctx, backups.ConfigureRequest{
			Label: label, Location: location, SecretRef: stageRef,
		})
	} else {
		repo, err = b.backups.Configure(ctx, backups.ConfigureRequest{
			Label: label, Location: location, SecretRef: stageRef,
		})
	}
	if err != nil {
		return backups.Repository{}, translateError(err)
	}
	// Move credentials to the per-repo record with the value unchanged, point
	// the repository at it, and drop the staging record (best-effort).
	repoRef, err := putRepoSecret(password, repo.ID)
	password = ""
	if err != nil {
		return backups.Repository{}, err
	}
	if repo, err = b.backups.Configure(ctx, backups.ConfigureRequest{
		ID: repo.ID, Label: label, Location: repo.Location, SecretRef: repoRef,
	}); err != nil {
		return backups.Repository{}, translateError(err)
	}
	_ = b.secrets.DeleteByName(ctx, "platform-app", "backup_repo_"+sanitizeSecretName(stageKey))
	b.enableBackupTimers(ctx)
	// After creation run first backup immediately then verify so recovery_tested passes day one.
	// Fire in background to not block the API response.
	go func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		if _, err := b.backups.RunBackup(bgCtx, backups.RunRequest{RepositoryID: repo.ID}); err != nil {
			_, _ = b.events.Publish(bgCtx, events.PublishInput{
				Type:     "backups.first_backup_failed",
				Severity: "warning",
				Message:  fmt.Sprintf("first backup after configure failed: %v", err),
			})
			return
		}
		if _, _, err := b.backups.Verify(bgCtx, backups.VerifyRequest{RepositoryID: repo.ID}); err != nil {
			_, _ = b.events.Publish(bgCtx, events.PublishInput{
				Type:     "backups.first_verify_failed",
				Severity: "warning",
				Message:  fmt.Sprintf("first verify after configure failed: %v", err),
			})
		}
	}()
	return repo, nil
}

// DeleteBackupRepository removes a repository (audit-retained when runs exist).
func (b *Backend) DeleteBackupRepository(ctx context.Context, id string) error {
	return b.backups.DeleteRepository(ctx, id)
}

// sanitizeSecretName maps arbitrary keys into the secrets name alphabet.
func sanitizeSecretName(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-._")
	if out == "" {
		out = "repo"
	}
	if len(out) > 100 {
		out = out[:100]
	}
	return out
}

// migrateBackupCredentialShapes converts legacy raw-password backup secrets into
// backups.Credentials JSON objects without changing password bytes. Valid
// credential objects are left unchanged. Shared backup_repo_credentials
// references each receive a separate backup_repo_<sanitized-repository-id>
// record. Per-repository backup_repo_* records rotate in place. Repository
// references update through Configure without reinitializing restic data.
// Empty legacy values and credential objects with invalid field types are
// migration errors. Already converted objects survive a retry.
func (b *Backend) migrateBackupCredentialShapes(ctx context.Context) error {
	if b.backups == nil || b.secrets == nil {
		return nil
	}
	repos, err := b.backups.Repositories(ctx)
	if err != nil {
		return fmt.Errorf("list backup repositories: %w", err)
	}
	for _, repo := range repos {
		meta, err := b.secrets.Get(ctx, domain.ID(repo.SecretRef.ID))
		if err != nil {
			return fmt.Errorf("load backup credential metadata for repository %q: %w", repo.ID, err)
		}
		if meta.Scope != "platform-app" {
			continue
		}
		shared := meta.Name == "backup_repo_credentials"
		if !shared && !strings.HasPrefix(meta.Name, "backup_repo_") {
			continue
		}
		value, err := b.secrets.Reveal(ctx, domain.ID(repo.SecretRef.ID))
		if err != nil {
			return fmt.Errorf("reveal backup credential for repository %q: %w", repo.ID, err)
		}
		if value == "" {
			return fmt.Errorf("backup credential for repository %q is empty", repo.ID)
		}
		var creds backups.Credentials
		if err := json.Unmarshal([]byte(value), &creds); err == nil && strings.TrimSpace(creds.Password) != "" {
			if !shared {
				continue
			}
			// Shared valid object still needs a separate per-repository record.
			name := "backup_repo_" + sanitizeSecretName(repo.ID)
			sec, err := b.secrets.Put(ctx, "platform-app", name, value)
			if err != nil {
				return fmt.Errorf("store backup credential for repository %q: %w", repo.ID, err)
			}
			if _, err := b.backups.Configure(ctx, backups.ConfigureRequest{
				ID: repo.ID, Label: repo.Label, Location: repo.Location,
				SecretRef: backups.SecretRef{ID: string(sec.ID), Version: sec.Version},
			}); err != nil {
				return fmt.Errorf("point repository %q at migrated credential: %w", repo.ID, err)
			}
			continue
		}
		if looksLikeCredentialObject(value) {
			return fmt.Errorf("backup credential for repository %q has invalid credential fields", repo.ID)
		}
		encoded, err := json.Marshal(backups.Credentials{Password: value})
		if err != nil {
			return fmt.Errorf("encode backup credential for repository %q: %w", repo.ID, err)
		}
		if shared {
			name := "backup_repo_" + sanitizeSecretName(repo.ID)
			sec, err := b.secrets.Put(ctx, "platform-app", name, string(encoded))
			if err != nil {
				return fmt.Errorf("store backup credential for repository %q: %w", repo.ID, err)
			}
			if _, err := b.backups.Configure(ctx, backups.ConfigureRequest{
				ID: repo.ID, Label: repo.Label, Location: repo.Location,
				SecretRef: backups.SecretRef{ID: string(sec.ID), Version: sec.Version},
			}); err != nil {
				return fmt.Errorf("point repository %q at migrated credential: %w", repo.ID, err)
			}
			continue
		}
		sec, err := b.secrets.Rotate(ctx, domain.ID(repo.SecretRef.ID), string(encoded))
		if err != nil {
			return fmt.Errorf("rotate backup credential for repository %q: %w", repo.ID, err)
		}
		if _, err := b.backups.Configure(ctx, backups.ConfigureRequest{
			ID: repo.ID, Label: repo.Label, Location: repo.Location,
			SecretRef: backups.SecretRef{ID: string(sec.ID), Version: sec.Version},
		}); err != nil {
			return fmt.Errorf("point repository %q at migrated credential: %w", repo.ID, err)
		}
	}
	return nil
}

// looksLikeCredentialObject reports whether value parses as a JSON object
// containing credential field names. Such values must decode as valid
// credentials; otherwise migration must fail instead of treating the object
// text as a raw password.
func looksLikeCredentialObject(value string) bool {
	trimmed := strings.TrimSpace(value)
	if !strings.HasPrefix(trimmed, "{") {
		return false
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(value), &obj); err != nil {
		return false
	}
	for k := range obj {
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "password", "username", "accesskey", "access_key", "access-key":
			return true
		}
	}
	return false
}

// enableBackupTimers turns on the daily backup, weekly verify and
// weekly retention-prune timers.
func (b *Backend) enableBackupTimers(ctx context.Context) {
	for _, unit := range []string{"omahab-backup.timer", "omahab-verify.timer", "omahab-backup-prune.timer"} {
		if out, err := exec.CommandContext(ctx, "systemctl", "enable", "--now", unit).CombinedOutput(); err != nil {
			_, _ = b.events.Publish(ctx, events.PublishInput{
				Type:     "backups.timer_enable_failed",
				Severity: "warning",
				Message:  fmt.Sprintf("enable %s: %v: %s", unit, err, strings.TrimSpace(string(out))),
			})
		}
	}
}

// storageEntry is the legacy v1 placement record (kept for migration in
// storage.go).
type storageEntry struct {
	Volume string `json:"volume"`
	FSUUID string `json:"fs_uuid"`
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
func parseLSBlock(raw []byte) ([]apitypes.Disk, error) {
	var out lsblkOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse lsblk: %w", err)
	}
	var disks []apitypes.Disk
	var walk func(ds []lsblkDevice)
	walk = func(ds []lsblkDevice) {
		for _, d := range ds {
			if d.Type == "disk" || d.Type == "part" {
				if d.FSType != "" && d.UUID != "" {
					mp := d.Mountpoint
					if mp == "/" || mp == "/boot" || mp == "/nix" || mp == "/tmp" || strings.HasPrefix(d.Name, "ts") {
						// skip system volumes
					} else {
						disks = append(disks, apitypes.Disk{
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
