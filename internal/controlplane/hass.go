package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/omahab/omahab/internal/apitypes"
	"github.com/omahab/omahab/internal/integrations"
)

// hassScopeName is the secret scope holding HASS_SERVER/HASS_TOKEN for the
// default assistant. The token never appears in API responses: it is
// projected only through the secret store into the Hermes runtime.
func (b *Backend) hassService() (*integrations.Service, error) {
	if b.integrations == nil {
		return nil, translateError(fmt.Errorf("%w: home assistant not configured", ErrNotConfigured))
	}
	return b.integrations, nil
}

// GetHomeAssistant returns the Home Assistant connection state (no token).
// A missing row is not an error: callers render "not connected".
func (b *Backend) GetHomeAssistant(ctx context.Context) (*apitypes.HomeAssistant, error) {
	svc, err := b.hassService()
	if err != nil {
		return nil, err
	}
	cfg, err := svc.Get(ctx)
	if err != nil {
		if errors.Is(err, integrations.ErrNotFound) {
			return &apitypes.HomeAssistant{Status: "not_connected"}, nil
		}
		return nil, translateError(err)
	}
	return toAPIHomeAssistant(cfg), nil
}

// ConfigureHomeAssistant validates the server live via hass-cli, projects
// HASS_SERVER/HASS_TOKEN to the default assistant scope, and installs the
// hass-cli Hermes skill (DESIGN §17). The token travels in the request body
// and never persists outside the secret store.
func (b *Backend) ConfigureHomeAssistant(ctx context.Context, serverURL, token string) (*apitypes.HomeAssistant, error) {
	svc, err := b.hassService()
	if err != nil {
		return nil, err
	}
	cfg, err := svc.Configure(ctx, strings.TrimSpace(serverURL), token)
	if err != nil {
		return nil, translateError(err)
	}
	return toAPIHomeAssistant(cfg), nil
}

// RemoveHomeAssistant disconnects Home Assistant and drops the projected
// credentials. The installed skill file stays (harmless without credentials);
// reconfigure reinstalls it.
func (b *Backend) RemoveHomeAssistant(ctx context.Context) error {
	svc, err := b.hassService()
	if err != nil {
		return err
	}
	return translateError(svc.Remove(ctx))
}

func toAPIHomeAssistant(cfg *integrations.HomeAssistantConfig) *apitypes.HomeAssistant {
	return &apitypes.HomeAssistant{
		ServerURL:       cfg.ServerURL,
		Status:          cfg.Status,
		LastValidatedAt: cfg.LastValidatedAt,
		UpdatedAt:       cfg.UpdatedAt,
	}
}
