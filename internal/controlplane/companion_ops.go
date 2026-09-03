package controlplane

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"github.com/omahab/omahab/internal/apitypes"
	"github.com/omahab/omahab/internal/companion"
	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/events"
	"github.com/omahab/omahab/internal/store"
)
func companionDeviceToAPI(d *companion.Device) apitypes.CompanionDevice {
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
	out := make([]apitypes.CompanionDevice, 0, len(list))
	for _, d := range list {
		out = append(out, companionDeviceToAPI(d))
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
	return companionDeviceToAPI(d), nil
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
	return companionDeviceToAPI(d), nil
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
	// Mark device revoked first.
	if _, err := b.environments.RevokeDevice(ctx, trimmed); err != nil {
		return translateError(err)
	}
	// Immediately revoke its LiteLLM virtual keys (owner_kind=device, owner_id=deviceID).
	if b.providers != nil {
		// Query provider_virtual_keys for this device.
		rows, err := b.db.QueryContext(ctx, `SELECT id FROM provider_virtual_keys WHERE owner_kind = 'device' AND owner_id = ? AND revoked_at IS NULL`, trimmed)
		if err == nil {
			defer rows.Close()
			var ids []string
			for rows.Next() {
				var vkID string
				if err := rows.Scan(&vkID); err == nil {
					ids = append(ids, vkID)
				}
			}
			for _, vkID := range ids {
				_ = b.providers.RevokeVirtualKey(ctx, domain.ID(vkID))
			}
		}
	}
	// Also revoke cached device-key secret if present.
	if b.secrets != nil {
		_ = b.secrets.DeleteByName(ctx, "platform-app", "device-key."+trimmed)
	}
	// Revoke per-device Forgejo token if present.
	if b.scm != nil {
		if fc := b.scm.ForgejoClient(); fc != nil {
			// Try stored token name, fallback to device-<id>
			tokenName := "device-" + trimmed
			if b.environments != nil {
				if tn, err := b.environments.GetForgejoTokenName(ctx, trimmed); err == nil && strings.TrimSpace(tn) != "" {
					tokenName = strings.TrimSpace(tn)
				}
			}
			_ = fc.DeleteToken(ctx, "omahab", tokenName)
			// Also try DeleteAccessToken path
			_ = fc.DeleteAccessToken(ctx, "omahab", tokenName)
			if b.environments != nil {
				_ = b.environments.SetForgejoTokenName(ctx, trimmed, "")
			}
		}
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
	return companionDeviceToAPI(d), nil
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
