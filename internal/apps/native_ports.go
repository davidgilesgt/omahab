package apps

// Loopback port map for natively-placed bundles (NixOS systemd services).
// Replaces the docker network `omahab` (172.30.0.0/24): every native
// service is reachable on 127.0.0.1 at its listed port, and Caddy
// upstreams target these addresses.
//
// Keep in sync with nix/apps.nix service definitions.
const (
	NativePortCaddyAdmin   = 2019 // caddy admin API (loopback)
	NativePortPocketID     = 1411
	NativePortForgejo      = 3000
	NativePortWoodpecker   = 8000 // woodpecker server
	NativePortWoodpeckerGr = 9000 // woodpecker grpc (agent)
	NativePortImmich       = 2283
	NativePortPaperless    = 28981
	NativePortKarakeep     = 3010
	NativePortSyncthingGUI = 8384
	NativePortNtfy         = 2586
	NativePortLiteLLM      = 4000
	NativePortHermes       = 8085 // oci-container published to loopback
)

// NativePort returns the loopback port for a bundle ID, if the bundle is
// natively placed. Second return is false for compose-only or unknown
// bundles.
func NativePort(bundleID string) (int, bool) {
	switch bundleID {
	case "caddy":
		return NativePortCaddyAdmin, true
	case "pocket-id":
		return NativePortPocketID, true
	case "forgejo":
		return NativePortForgejo, true
	case "woodpecker":
		return NativePortWoodpecker, true
	case "immich":
		return NativePortImmich, true
	case "paperless-ngx":
		return NativePortPaperless, true
	case "karakeep":
		return NativePortKarakeep, true
	case "syncthing":
		return NativePortSyncthingGUI, true
	case "ntfy":
		return NativePortNtfy, true
	case "litellm":
		return NativePortLiteLLM, true
	case "hermes":
		return NativePortHermes, true
	default:
		return 0, false
	}
}
