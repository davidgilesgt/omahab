package controlplane

import (
	"os"
	"strings"
)

// Placement returns the daemon placement: "lan" or "vps".
// It reads OMAHAB_PLACEMENT; unset or anything besides "vps" means
// "lan" — the LAN stays open by default (ISO-installer path).
// On lan placement, panel requests from LAN source addresses and tailnet
// addresses (100.64.0.0/10) bypass the bearer token; on vps placement every
// source needs the panel token.
func Placement() string {
	if strings.TrimSpace(strings.ToLower(os.Getenv("OMAHAB_PLACEMENT"))) == "vps" {
		return "vps"
	}
	return "lan"
}
