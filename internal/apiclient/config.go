package apiclient

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// clientIfaceAddrs mirrors cmd/omahab ifaceAddrs for lan discovery without import cycle.
type clientIfaceAddrs struct {
	Name  string
	Flags net.Flags
	Addrs []net.Addr
}

// ClientConfig is stored at ~/.config/omahab/client.json.
// It intentionally contains no secrets. Bearer credentials go through
// CredentialStore and never this file.
type ClientConfig struct {
	Server string `json:"server,omitempty"`
}

// DefaultClientConfigPath returns ~/.config/omahab/client.json (or XDG variant).
func DefaultClientConfigPath() (string, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".config", "omahab")
	} else {
		dir = filepath.Join(dir, "omahab")
	}
	return filepath.Join(dir, "client.json"), nil
}

// LoadClientConfig loads the client config file if present; returns empty
// config when absent.
func LoadClientConfig(path string) (ClientConfig, error) {
	if path == "" {
		p, err := DefaultClientConfigPath()
		if err != nil {
			return ClientConfig{}, err
		}
		path = p
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ClientConfig{}, nil
		}
		return ClientConfig{}, err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return ClientConfig{}, nil
	}
	var cfg ClientConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return ClientConfig{}, err
	}
	cfg.Server = strings.TrimSpace(cfg.Server)
	return cfg, nil
}

// ResolveServer resolves the server URL with precedence:
//  1. explicit flag value
//  2. OMAHAB_SERVER env
//  3. client.json server
//  4. LAN IP (e.g. http://192.168.1.12:8484) or <hostname>.local, never 127.0.0.1
//     Falls back to http://127.0.0.1:8484 only when no private address is found.
func ResolveServer(flagValue string, cfg ClientConfig) string {
	if s := strings.TrimSpace(flagValue); s != "" {
		return s
	}
	if s := strings.TrimSpace(os.Getenv("OMAHAB_SERVER")); s != "" {
		return s
	}
	if s := strings.TrimSpace(cfg.Server); s != "" {
		return s
	}
	return defaultServerURL()
}

// defaultServerURL prefers the device LAN IP / <hostname>.local for
// first-boot surfaces, never 127.0.0.1 when a LAN address is available.
// Pattern mirrors cmd/omahab/console.go lanIPv4() / tailscaleIPv4().
func defaultServerURL() string {
	if ip := lanIPv4ForClient(); ip != "" {
		return "http://" + ip + ":8484"
	}
	if host := hostnameLocal(); host != "" {
		return "http://" + host + ":8484"
	}
	return "http://127.0.0.1:8484"
}

func hostnameLocal() string {
	h, err := os.Hostname()
	if err != nil || strings.TrimSpace(h) == "" {
		return ""
	}
	h = strings.TrimSpace(h)
	if idx := strings.Index(h, "."); idx != -1 {
		h = h[:idx]
	}
	if h == "" {
		return ""
	}
	return h + ".local"
}

func lanIPv4ForClient() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	var list []clientIfaceAddrs
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		list = append(list, clientIfaceAddrs{Name: iface.Name, Flags: iface.Flags, Addrs: addrs})
	}
	return pickLANIPv4ForClient(list)
}

func pickLANIPv4ForClient(ifaces []clientIfaceAddrs) string {
	// Use non-loopback, up, private IPv4, skipping docker/tailscale/virtual interfaces.
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if strings.HasPrefix(iface.Name, "tailscale") ||
			strings.HasPrefix(iface.Name, "docker") ||
			strings.HasPrefix(iface.Name, "br-") ||
			strings.HasPrefix(iface.Name, "veth") ||
			strings.HasPrefix(iface.Name, "podman") ||
			strings.HasPrefix(iface.Name, "virbr") ||
			strings.HasPrefix(iface.Name, "cni") {
			continue
		}
		for _, addr := range iface.Addrs {
			ipnet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipnet.IP.To4()
			if ip4 == nil || ip4.IsLoopback() {
				continue
			}
			if !ip4.IsPrivate() {
				continue
			}
			return ip4.String()
		}
	}
	return ""
}
