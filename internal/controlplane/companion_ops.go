package controlplane

import (
	"github.com/omahab/omahab/internal/apitypes"
	"context"
	"github.com/omahab/omahab/internal/domain"
	"fmt"
	"encoding/hex"

	"crypto/sha256"
	"github.com/omahab/omahab/internal/store"
	"strings"
)

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
		out = append(out, apitypes.CompanionDevice{
			ID:                 domain.ID(d.ID),
			Name:               d.Name,
			AllowProviderOAuth: d.AllowProviderOAuth,
			CreatedAt:          d.CreatedAt,
			UpdatedAt:          d.UpdatedAt,
		})
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
	return apitypes.CompanionDevice{
		ID:                 domain.ID(d.ID),
		Name:               d.Name,
		AllowProviderOAuth: d.AllowProviderOAuth,
		CreatedAt:          d.CreatedAt,
		UpdatedAt:          d.UpdatedAt,
	}, nil
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
	// Device token issued; no LiteLLM key yet — per-device key will be issued on first environment sync via ensureDeviceVirtualKey.
	_ = dev
	resp := apitypes.EnrollCompanionResponse{
		Token:       token,
		TokenPrefix: token[:16],
	}
	if creds != nil {
		resp.ResticRepo = creds.ResticRepo
		resp.ResticPassword = creds.ResticPassword
		resp.RestUser = creds.RestUser
		resp.RestPassword = creds.RestPassword
	}
	if len(resp.TokenPrefix) > len(token) {
		resp.TokenPrefix = token
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
	return nil
}

func (b *Backend) SetDeviceAllowOAuth(ctx context.Context, id domain.ID, allow bool) (apitypes.CompanionDevice, error) {
	if b.environments == nil {
		return apitypes.CompanionDevice{}, translateError(fmt.Errorf("%w: environments not configured", ErrNotConfigured))
	}
	d, err := b.environments.SetDeviceAllowOAuth(ctx, string(id), allow)
	if err != nil {
		return apitypes.CompanionDevice{}, translateError(err)
	}
	return apitypes.CompanionDevice{
		ID:                 domain.ID(d.ID),
		Name:               d.Name,
		AllowProviderOAuth: d.AllowProviderOAuth,
		CreatedAt:          d.CreatedAt,
		UpdatedAt:          d.UpdatedAt,
	}, nil
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
	return apitypes.ToolEnvEntry{Name: meta.Name, Version: meta.Version, CreatedAt: meta.CreatedAt, UpdatedAt: meta.UpdatedAt}, nil
}

func (b *Backend) DeleteToolEnvironment(ctx context.Context, name string) error {
	if b.environments == nil {
		return translateError(fmt.Errorf("%w: environments not configured", ErrNotConfigured))
	}
	if err := b.environments.DeleteToolEnv(ctx, name); err != nil {
		return translateError(err)
	}
	return nil
}





// Email ingestion
