package controlplane

import (
	"os"
	"strings"
)

// Placement returns the daemon placement: "lan" or "vps".
// It reads OMAHAB_PLACEMENT; unset or anything besides "vps" means
// "lan" — the LAN stays open by default (ISO-installer path).
// On lan placement, panel requests from LAN source addresses bypass the
// bearer token; the panel token guards tailnet/remote access.
func Placement() string {
	if strings.TrimSpace(strings.ToLower(os.Getenv("OMAHAB_PLACEMENT"))) == "vps" {
		return "vps"
	}
	return "lan"
}
