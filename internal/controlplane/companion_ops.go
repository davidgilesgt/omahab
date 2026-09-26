package controlplane

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/omahab/omahab/internal/apitypes"
	"github.com/omahab/omahab/internal/companion"
	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/events"
	"github.com/omahab/omahab/internal/store"
)
func companionDeviceToAPI(d *companion.Device, granted bool) apitypes.CompanionDevice {
	return apitypes.CompanionDevice{
		ID:                 domain.ID(d.ID),
		Name:               d.Name,
		Hostname:           d.Hostname,
		Platform:           d.Platform,
		Arch:               d.Arch,
		ClientVersion:      d.ClientVersion,
		Shell:              d.Shell,
		EnvRevision:        d.EnvRevision,
		EnvVariableCount:   d.EnvVariableCount,
		BackupLastSnapshot: d.BackupLastSnapshot,
		AllowProviderOAuth: d.AllowProviderOAuth,
		Granted:            granted,
		LastSeenAt:         d.LastSeenAt,
		RevokedAt:          d.RevokedAt,
		CreatedAt:          d.CreatedAt,
		UpdatedAt:          d.UpdatedAt,
	}
}

func (b *Backend) ListCompanionDevices(ctx context.Context, p apitypes.Pagination) ([]apitypes.CompanionDevice, error) {
	if b.environments == nil {
		return []apitypes.CompanionDevice{}, nil
	}
	list, err := b.environments.ListDevices(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	grants, err := b.environments.GrantedDeviceIDs(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	out := make([]apitypes.CompanionDevice, 0, len(list))
	for _, d := range list {
		out = append(out, companionDeviceToAPI(d, grants[d.ID]))
	}
	return paginate(out, p), nil
}

func (b *Backend) GetCompanionDevice(ctx context.Context, id domain.ID) (apitypes.CompanionDevice, error) {
	if b.environments == nil {
		return apitypes.CompanionDevice{}, translateError(fmt.Errorf("%w: environments not configured", ErrNotConfigured))
	}
	d, err := b.environments.GetDevice(ctx, string(id))
	if err != nil {
		return apitypes.CompanionDevice{}, translateError(err)
	}
	granted, err := b.environments.IsDeviceGranted(ctx, d.ID)
	if err != nil {
		return apitypes.CompanionDevice{}, translateError(err)
	}
	return companionDeviceToAPI(d, granted), nil
}

func (b *Backend) UpdateCompanionDeviceInfo(ctx context.Context, deviceID domain.ID, req apitypes.UpdateCompanionDeviceRequest) (apitypes.CompanionDevice, error) {
	if b.environments == nil {
		return apitypes.CompanionDevice{}, translateError(fmt.Errorf("%w: environments not configured", ErrNotConfigured))
	}
	info := companion.DeviceInfo{
		Hostname:      req.Hostname,
		Platform:      req.Platform,
		Arch:          req.Arch,
		ClientVersion: req.ClientVersion,
		Shell:         req.Shell,
	}
	if req.EnvRevision != nil {
		info.EnvRevision = *req.EnvRevision
	}
	if req.EnvVariableCount != nil {
		info.EnvVariableCount = *req.EnvVariableCount
	}
	info.BackupLastSnapshot = req.BackupLastSnapshot
	d, err := b.environments.UpdateDeviceInfo(ctx, string(deviceID), info)
	if err != nil {
		return apitypes.CompanionDevice{}, translateError(err)
	}
	granted, err := b.environments.IsDeviceGranted(ctx, d.ID)
	if err != nil {
		return apitypes.CompanionDevice{}, translateError(err)
	}
	return companionDeviceToAPI(d, granted), nil
}


func (b *Backend) CreateCompanionEnrollment(ctx context.Context) (apitypes.CompanionEnrollment, string, error) {
	if b.environments == nil {
		return apitypes.CompanionEnrollment{}, "", translateError(fmt.Errorf("%w: environments not configured", ErrNotConfigured))
	}
	enr, code, err := b.environments.CreateEnrollment(ctx)
	if err != nil {
		return apitypes.CompanionEnrollment{}, "", translateError(err)
	}
	return apitypes.CompanionEnrollment{
		ID:        domain.ID(enr.ID),
		ExpiresAt: enr.ExpiresAt,
		CreatedAt: enr.CreatedAt,
	}, code, nil
}
func (b *Backend) EnrollCompanion(ctx context.Context, code string) (apitypes.EnrollCompanionResponse, error) {
	if b.environments == nil {
		return apitypes.EnrollCompanionResponse{}, translateError(fmt.Errorf("%w: environments not configured", ErrNotConfigured))
	}
	if strings.TrimSpace(code) == "" {
		return apitypes.EnrollCompanionResponse{}, translateError(fmt.Errorf("%w: code is required", store.ErrValidation))
	}
	dev, token, creds, err := b.environments.EnrollDevice(ctx, code)
	if err != nil {
		return apitypes.EnrollCompanionResponse{}, translateError(err)
	}
	resp := apitypes.EnrollCompanionResponse{
		Token:       token,
		TokenPrefix: token[:16],
	}
	if len(resp.Token) < 16 {
		resp.TokenPrefix = resp.Token
	}
	if creds != nil {
		resp.ResticRepo = creds.ResticRepo
		resp.ResticPassword = creds.ResticPassword
		resp.RestUser = creds.RestUser
		resp.RestPassword = creds.RestPassword
	}
	// Per-device Forgejo token (same path as workspace ws-<id> tokens).
	// Best-effort: if Forgejo is not configured, enrollment still succeeds without git token.
	if dev != nil && b.scm != nil {
		if fc := b.scm.ForgejoClient(); fc != nil {
			tokenName := "device-" + dev.ID
			scopes := []string{"read:repository", "write:repository"}
			// Device tokens are user-scoped for omahab bot user, no repo restriction.
			if tok, err := fc.CreateToken(ctx, "omahab", tokenName, scopes); err == nil && strings.TrimSpace(tok) != "" {
				resp.ForgejoToken = strings.TrimSpace(tok)
				// Attempt to resolve Forgejo host for credential helper.
				host := ""
				if inst, ierr := b.store.Instance(ctx); ierr == nil && strings.TrimSpace(inst.Domain) != "" {
					host = "git." + strings.TrimSpace(inst.Domain)
				}
				if v, serr := b.secrets.RevealByName(ctx, "platform-app", "forgejo_base_url"); serr == nil && strings.TrimSpace(v) != "" {
					host = strings.TrimSpace(v)
					// Strip scheme if present.
					host = strings.TrimPrefix(host, "https://")
					host = strings.TrimPrefix(host, "http://")
					host = strings.Split(host, "/")[0]
				} else if env := strings.TrimSpace(os.Getenv("OMAHAB_FORGEJO_URL")); env != "" {
					host = strings.TrimPrefix(env, "https://")
					host = strings.TrimPrefix(host, "http://")
					host = strings.Split(host, "/")[0]
				}
				resp.ForgejoHost = host
				_ = b.environments.SetForgejoTokenName(ctx, dev.ID, tokenName)
			}
		}
	}
	// Machine backup server is not a default bundle: nothing to serve
	// until a client exists. First enrollment installs it (best-effort;
	// enrollment succeeds regardless).
	if dev != nil {
		b.ensureResticServerApp(ctx)
	}
	return resp, nil
}

func (b *Backend) RevokeCompanionDevice(ctx context.Context, id domain.ID) error {
	if b.environments == nil {
		return translateError(fmt.Errorf("%w: environments not configured", ErrNotConfigured))
	}
	trimmed := strings.TrimSpace(string(id))
	if trimmed == "" {
		return translateError(fmt.Errorf("%w: id is required", store.ErrValidation))
	}
	// 1. Local containment first: device auth and environment bundle are
	// blocked immediately, even if an external revoke below fails.
	if _, err := b.environments.RevokeDevice(ctx, trimmed); err != nil {
		return translateError(err)
	}
	// 2. External revokes with durable confirmation. Every failure persists
	// its retry handle; nothing reports success until confirmed.
	if err := b.revokeDeviceExternals(ctx, trimmed); err != nil {
		return err
	}
	if b.events != nil {
		_, _ = b.events.Publish(ctx, events.PublishInput{
			Type:     "companion.revoked",
			Severity: "warning",
			Message:  fmt.Sprintf("companion device revoked: %s", trimmed),
			Data:     map[string]any{"device_id": trimmed},
		})
	}
	return nil
}

// pendingRevokeError reports contained-but-unconfirmed revocation.
// DELETE surfaces 503 (not 204) so callers never mistake it for full success.
func pendingRevokeError(deviceID string, pending []companion.RevocationPending) error {
	first := ""
	if len(pending) > 0 && strings.TrimSpace(pending[0].LastError) != "" {
		first = ": " + pending[0].LastError
	}
	return fmt.Errorf("%w: device %s contained but %d external revocation(s) unconfirmed, retry in progress%s",
		apitypes.ErrServiceUnavailable, deviceID, len(pending), first)
}

// revokeDeviceExternals revokes every external credential for a device,
// queueing durable pending rows for anything unconfirmed. It also drives
// pre-existing pending rows for the device, so a repeat call (or the retry
// driver) converges. Returns nil only when nothing remains unconfirmed.
func (b *Backend) revokeDeviceExternals(ctx context.Context, deviceID string) error {
	// LiteLLM virtual keys: local revoke first (marks revoked_at), then
	// strict gateway confirmation per key. Rows are read regardless of
	// revoked_at: local state never proves the gateway bearer dead.
	if b.providers != nil {
		type keyHandle struct {
			id, name, gatewayKeyID string
		}
		var handles []keyHandle
		rows, err := b.db.QueryContext(ctx, `SELECT id, name, gateway_key_id FROM provider_virtual_keys WHERE owner_kind = 'device' AND owner_id = ?`, deviceID)
		if err != nil {
			return translateError(fmt.Errorf("query device virtual keys: %w", err))
		}
		func() {
			defer rows.Close()
			for rows.Next() {
				var h keyHandle
				var gw sql.NullString
				if err := rows.Scan(&h.id, &h.name, &gw); err != nil {
					err = fmt.Errorf("scan device virtual key: %w", err)
					return
				}
				if gw.Valid {
					h.gatewayKeyID = gw.String
				}
				handles = append(handles, h)
			}
			err = rows.Err()
		}()
		if err != nil {
			return translateError(err)
		}
		for _, h := range handles {
			if err := b.providers.RevokeVirtualKey(ctx, domain.ID(h.id)); err != nil {
				// Local revoke failure: return immediately; nothing was
				// confirmed, handles persist in their rows for retry.
				return translateError(fmt.Errorf("revoke device virtual key: %w", err))
			}
			if strings.TrimSpace(h.gatewayKeyID) == "" {
				continue // no gateway handle exists: nothing to confirm.
			}
			if err := b.providers.GatewayRevokeVirtualKey(ctx, domain.ID(h.id)); err != nil {
				_ = b.environments.QueueRevocationPending(ctx, deviceID, companion.RevokeTargetGatewayKey, h.gatewayKeyID, h.name, err)
			}
		}
	}
	// Cached device-key secret: absent counts as confirmed (already gone).
	if b.secrets != nil {
		secretName := "device-key." + deviceID
		if err := b.secrets.DeleteByName(ctx, "platform-app", secretName); err != nil && !errors.Is(err, store.ErrNotFound) {
			_ = b.environments.QueueRevocationPending(ctx, deviceID, companion.RevokeTargetDeviceSecret, secretName, "", err)
		}
	}
	// Per-device Forgejo token: the stored name is the retry handle and is
	// cleared only after DeleteToken confirms (already idempotent upstream).
	if b.scm != nil {
		if fc := b.scm.ForgejoClient(); fc != nil {
			tokenName := "device-" + deviceID
			if tn, err := b.environments.GetForgejoTokenName(ctx, deviceID); err == nil && strings.TrimSpace(tn) != "" {
				tokenName = strings.TrimSpace(tn)
			}
			if err := fc.DeleteToken(ctx, "omahab", tokenName); err != nil {
				_ = b.environments.QueueRevocationPending(ctx, deviceID, companion.RevokeTargetForgejoToken, tokenName, "", err)
			} else if err := b.environments.SetForgejoTokenName(ctx, deviceID, ""); err != nil {
				return translateError(fmt.Errorf("clear device token handle: %w", err))
			}
		}
	}
	// Machine-backup htpasswd entry + restic reload, surfaced for retry.
	if err := b.environments.RemoveDeviceBackupCredentials(ctx, deviceID); err != nil {
		_ = b.environments.QueueRevocationPending(ctx, deviceID, companion.RevokeTargetHtpasswd, "dev-"+deviceID, "", err)
	}
	// Managed folder sharing: retry handle is the device itself.
	if b.syncer != nil {
		if affected, err := b.syncer.RemoveCompanionDevice(ctx, deviceID); err != nil {
			_ = b.environments.QueueRevocationPending(ctx, deviceID, "sync_folders", deviceID, "", err)
		} else if len(affected) > 0 {
			_, _ = b.ReconcileSyncFolders(ctx)
		}
	}
	// Drive any pre-existing pending rows for this device too.
	if _, err := b.RetryPendingDeviceRevocations(ctx); err != nil {
		return err
	}
	pending, err := b.environments.ListRevocationPending(ctx)
	if err != nil {
		return translateError(fmt.Errorf("list revocation pending: %w", err))
	}
	var mine []companion.RevocationPending
	for _, p := range pending {
		if p.DeviceID == deviceID {
			mine = append(mine, p)
		}
	}
	if len(mine) > 0 {
		return pendingRevokeError(deviceID, mine)
	}
	return nil
}

// RetryPendingDeviceRevocations re-attempts every unconfirmed device external
// revoke until the external confirms. Pending rows survive restarts (same
// control DB), so recovery after a gateway/Forgejo/systemd outage needs only
// this driver plus a live external — no admin action, no new handles.
// A row is deleted only on confirmed success; failures refresh attempts and
// last error. Unknown targets are left untouched. Returns completed count.
func (b *Backend) RetryPendingDeviceRevocations(ctx context.Context) (int, error) {
	if b.environments == nil {
		return 0, translateError(fmt.Errorf("%w: environments not configured", ErrNotConfigured))
	}
	pending, err := b.environments.ListRevocationPending(ctx)
	if err != nil {
		return 0, translateError(fmt.Errorf("list revocation pending: %w", err))
	}
	completed := 0
	for _, p := range pending {
		var cerr error
		switch p.Target {
		case companion.RevokeTargetGatewayKey:
			cerr = b.retryGatewayKeyRevoke(ctx, p)
		case companion.RevokeTargetForgejoToken:
			cerr = b.retryForgejoTokenRevoke(ctx, p)
		case companion.RevokeTargetHtpasswd:
			if b.environments != nil {
				cerr = b.environments.RemoveDeviceBackupCredentials(ctx, p.DeviceID)
			}
		case companion.RevokeTargetDeviceSecret:
			if b.secrets != nil {
				cerr = b.secrets.DeleteByName(ctx, "platform-app", p.Handle)
				if cerr != nil && errors.Is(cerr, store.ErrNotFound) {
					cerr = nil // already gone: confirmed.
				}
			}
		case "sync_folders":
			if b.syncer != nil {
				if affected, err := b.syncer.RemoveCompanionDevice(ctx, p.DeviceID); err != nil {
					cerr = err
				} else if len(affected) > 0 {
					_, _ = b.ReconcileSyncFolders(ctx)
				}
			}
		default:
			continue // unknown target: leave for its owner.
		}
		if cerr != nil {
			_ = b.environments.QueueRevocationPending(ctx, p.DeviceID, p.Target, p.Handle, p.Alias, cerr)
			continue
		}
		if err := b.environments.ClearRevocationPending(ctx, p.DeviceID, p.Target, p.Handle); err != nil {
			return completed, translateError(fmt.Errorf("clear revocation pending: %w", err))
		}
		// Forgejo handle bookkeeping clears only after the pending row is gone.
		if p.Target == companion.RevokeTargetForgejoToken {
			_ = b.environments.SetForgejoTokenName(ctx, p.DeviceID, "")
		}
		completed++
	}
	return completed, nil
}

// retryGatewayKeyRevoke confirms one gateway bearer dead via the strict path.
// The pending handle/alias pair is the exact stored gateway handle, so retry
// needs no local key row (it may be gone) and touches no unrelated keys.
func (b *Backend) retryGatewayKeyRevoke(ctx context.Context, p companion.RevocationPending) error {
	if b.gateway == nil {
		return fmt.Errorf("gateway not configured")
	}
	return b.gateway.RevokeVirtualKeyStrict(ctx, p.Handle, p.Alias)
}

// retryForgejoTokenRevoke confirms one Forgejo token deletion (idempotent upstream).
func (b *Backend) retryForgejoTokenRevoke(ctx context.Context, p companion.RevocationPending) error {
	if b.scm == nil {
		return fmt.Errorf("scm not configured")
	}
	fc := b.scm.ForgejoClient()
	if fc == nil {
		return fmt.Errorf("forgejo client is required")
	}
	return fc.DeleteToken(ctx, "omahab", p.Handle)
}

// GetNtfyConfig returns the ntfy phone notifications configuration (admin).
func (b *Backend) GetNtfyConfig(ctx context.Context) (apitypes.NtfyConfig, error) {
	if b.secrets == nil {
		return apitypes.NtfyConfig{Enabled: false}, translateError(fmt.Errorf("%w: secrets not configured", ErrNotConfigured))
	}
	topic, err := b.secrets.RevealByName(ctx, "platform-app", "ntfy_topic")
	if err != nil || strings.TrimSpace(topic) == "" {
		return apitypes.NtfyConfig{Enabled: false}, nil
	}
	return apitypes.NtfyConfig{Enabled: true, Topic: strings.TrimSpace(topic)}, nil
}

// SetNtfyEnabled enables or disables phone notifications; when enabling, generates a random 24-char topic if missing.
func (b *Backend) SetNtfyEnabled(ctx context.Context, enabled bool) (apitypes.NtfyConfig, error) {
	if b.secrets == nil {
		return apitypes.NtfyConfig{}, translateError(fmt.Errorf("%w: secrets not configured", ErrNotConfigured))
	}
	if !enabled {
		_ = b.secrets.DeleteByName(ctx, "platform-app", "ntfy_topic")
		return apitypes.NtfyConfig{Enabled: false}, nil
	}
	// Enable — generate if missing
	if topic, err := b.secrets.RevealByName(ctx, "platform-app", "ntfy_topic"); err == nil && strings.TrimSpace(topic) != "" {
		return apitypes.NtfyConfig{Enabled: true, Topic: strings.TrimSpace(topic)}, nil
	}
	topic, err := generateNtfyTopic()
	if err != nil {
		return apitypes.NtfyConfig{}, translateError(err)
	}
	if err := upsertSecret(ctx, b.secrets, "platform-app", "ntfy_topic", topic); err != nil {
		return apitypes.NtfyConfig{}, translateError(err)
	}
	return apitypes.NtfyConfig{Enabled: true, Topic: topic}, nil
}

func generateNtfyTopic() (string, error) {
	// 18 random bytes -> 24 chars base64url raw (18*8/6=24)
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate ntfy topic: %w", err)
	}
	s := strings.TrimRight(base64.RawURLEncoding.EncodeToString(b), "=")
	// Ensure 24 chars; base64 raw url without padding already 24.
	if len(s) < 24 {
		// fallback pad via extra byte
		s = s + strings.Repeat("A", 24-len(s))
	}
	if len(s) > 24 {
		s = s[:24]
	}
	return s, nil
}
func (b *Backend) SetDeviceAllowOAuth(ctx context.Context, id domain.ID, allow bool) (apitypes.CompanionDevice, error) {
	if b.environments == nil {
		return apitypes.CompanionDevice{}, translateError(fmt.Errorf("%w: environments not configured", ErrNotConfigured))
	}
	d, err := b.environments.SetDeviceAllowOAuth(ctx, string(id), allow)
	if err != nil {
		return apitypes.CompanionDevice{}, translateError(err)
	}
	granted, err := b.environments.IsDeviceGranted(ctx, d.ID)
	if err != nil {
		return apitypes.CompanionDevice{}, translateError(err)
	}
	return companionDeviceToAPI(d, granted), nil
}

// DeviceEnvGrantResult is the admin grant/revoke response: consent state plus
// the current environment revision. Names/booleans/revision only, never values.
type DeviceEnvGrantResult struct {
	DeviceID string `json:"device_id"`
	Granted  bool   `json:"granted"`
	Revision int    `json:"revision"`
}

// GrantDeviceEnvironment records explicit owner consent to share tool-environment
// values with a device. The device receives current values on its next fetch;
// rotations after the grant are visible without re-granting.
func (b *Backend) GrantDeviceEnvironment(ctx context.Context, id domain.ID) (DeviceEnvGrantResult, error) {
	if b.environments == nil {
		return DeviceEnvGrantResult{}, translateError(fmt.Errorf("%w: environments not configured", ErrNotConfigured))
	}
	trimmed := strings.TrimSpace(string(id))
	if trimmed == "" {
		return DeviceEnvGrantResult{}, translateError(fmt.Errorf("%w: id is required", store.ErrValidation))
	}
	if err := b.environments.GrantDevice(ctx, trimmed); err != nil {
		return DeviceEnvGrantResult{}, translateError(err)
	}
	rev, _, err := b.environments.GetRevision(ctx)
	if err != nil {
		return DeviceEnvGrantResult{}, translateError(err)
	}
	if b.events != nil {
		_, _ = b.events.Publish(ctx, events.PublishInput{
			Type:     "environment.changed",
			Severity: "info",
			Message:  fmt.Sprintf("tool environment shared with device %s", trimmed),
			Data:     map[string]any{"device_id": trimmed, "action": "grant"},
		})
	}
	return DeviceEnvGrantResult{DeviceID: trimmed, Granted: true, Revision: rev}, nil
}

// RevokeDeviceEnvironment withdraws owner consent. The device's next fetch
// returns an empty bundle so it clears managed values.
func (b *Backend) RevokeDeviceEnvironment(ctx context.Context, id domain.ID) (DeviceEnvGrantResult, error) {
	if b.environments == nil {
		return DeviceEnvGrantResult{}, translateError(fmt.Errorf("%w: environments not configured", ErrNotConfigured))
	}
	trimmed := strings.TrimSpace(string(id))
	if trimmed == "" {
		return DeviceEnvGrantResult{}, translateError(fmt.Errorf("%w: id is required", store.ErrValidation))
	}
	if err := b.environments.RevokeGrant(ctx, trimmed); err != nil {
		return DeviceEnvGrantResult{}, translateError(err)
	}
	rev, _, err := b.environments.GetRevision(ctx)
	if err != nil {
		return DeviceEnvGrantResult{}, translateError(err)
	}
	if b.events != nil {
		_, _ = b.events.Publish(ctx, events.PublishInput{
			Type:     "environment.changed",
			Severity: "info",
			Message:  fmt.Sprintf("tool environment unshared from device %s", trimmed),
			Data:     map[string]any{"device_id": trimmed, "action": "revoke"},
		})
	}
	return DeviceEnvGrantResult{DeviceID: trimmed, Granted: false, Revision: rev}, nil
}

func (b *Backend) GetCompanionEnvironment(ctx context.Context, deviceToken string) (map[string]string, string, error) {
	if b.environments == nil {
		return nil, "", translateError(fmt.Errorf("%w: environments not configured", ErrNotConfigured))
	}
	trimmed := strings.TrimSpace(deviceToken)
	if trimmed == "" {
		return nil, "", translateError(fmt.Errorf("%w: device token is required", store.ErrValidation))
	}
	// Validate token to get device ID (also checks prefix, revoked, constant-time).
	dev, err := b.environments.ValidateDeviceToken(ctx, trimmed)
	if err != nil {
		return nil, "", translateError(err)
	}
	bundle, rev, err := b.environments.GetCompanionEnvironmentBundle(ctx, dev.ID)
	if err != nil {
		return nil, "", translateError(err)
	}
	// ETag derived from revision + device ID hash (per spec: W/"rev-<revision>-<deviceIDHash>")
	h := sha256.Sum256([]byte(dev.ID))
	hashPart := hex.EncodeToString(h[:4])
	etag := fmt.Sprintf(`W/"rev-%d-%s"`, rev, hashPart)
	return bundle, etag, nil
}

// Tool environment (server authoritative singleton agent-tools)

func (b *Backend) ListToolEnvironments(ctx context.Context) ([]apitypes.ToolEnvEntry, error) {
	if b.environments == nil {
		return []apitypes.ToolEnvEntry{}, nil
	}
	list, err := b.environments.ListToolEnvs(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	out := make([]apitypes.ToolEnvEntry, 0, len(list))
	for _, m := range list {
		out = append(out, apitypes.ToolEnvEntry{Name: m.Name, Version: m.Version, CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt})
	}
	return out, nil
}

func (b *Backend) PutToolEnvironment(ctx context.Context, name, value string) (apitypes.ToolEnvEntry, error) {
	if b.environments == nil {
		return apitypes.ToolEnvEntry{}, translateError(fmt.Errorf("%w: environments not configured", ErrNotConfigured))
	}
	if strings.Contains(value, "\x00") || strings.Contains(value, "\r") || strings.Contains(value, "\n") {
		return apitypes.ToolEnvEntry{}, translateError(fmt.Errorf("%w: value must not contain NUL, CR, or LF", store.ErrValidation))
	}
	meta, err := b.environments.PutToolEnv(ctx, name, value)
	if err != nil {
		return apitypes.ToolEnvEntry{}, translateError(err)
	}
	if b.events != nil {
		_, _ = b.events.Publish(ctx, events.PublishInput{
			Type:     "environment.changed",
			Severity: "info",
			Message:  fmt.Sprintf("tool environment changed: %s updated", name),
			Data:     map[string]any{"name": name, "action": "put", "version": meta.Version},
		})
	}
	return apitypes.ToolEnvEntry{Name: meta.Name, Version: meta.Version, CreatedAt: meta.CreatedAt, UpdatedAt: meta.UpdatedAt}, nil
}
func (b *Backend) DeleteToolEnvironment(ctx context.Context, name string) error {
	if b.environments == nil {
		return translateError(fmt.Errorf("%w: environments not configured", ErrNotConfigured))
	}
	if err := b.environments.DeleteToolEnv(ctx, name); err != nil {
		return translateError(err)
	}
	if b.events != nil {
		_, _ = b.events.Publish(ctx, events.PublishInput{
			Type:     "environment.changed",
			Severity: "info",
			Message:  fmt.Sprintf("tool environment changed: %s deleted", name),
			Data:     map[string]any{"name": name, "action": "delete"},
		})
	}
	return nil
}





// Email ingestion
