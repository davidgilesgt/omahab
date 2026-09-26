package controlplane

import (
	"os"
	"strings"
)

// Placement returns the daemon placement: "lan" or "vps".
// It reads OMAHAB_PLACEMENT; unset or anything besides "vps" means
// "lan" — the LAN stays open by default (ISO-installer path).
// Placement controls network admission. Both placements require a bearer
// token for every administrator API route; neither trusts source IP alone.
func Placement() string {
	if strings.TrimSpace(strings.ToLower(os.Getenv("OMAHAB_PLACEMENT"))) == "vps" {
		return "vps"
	}
	return "lan"
}
