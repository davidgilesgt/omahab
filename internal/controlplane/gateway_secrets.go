package controlplane

import (
	"strings"
)

// gatewayConfigDir mirrors the gateway wiring in backend.go: the LiteLLM
// config tree lives under <DataDir>/apps/litellm/config.
func (b *Backend) gatewayConfigDir() string {
	if strings.TrimSpace(b.cfg.DataDir) == "" {
		return "/srv/omahab/apps/litellm/config"
	}
	return b.cfg.DataDir + "/apps/litellm/config"
}
