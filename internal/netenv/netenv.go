// Package netenv detects the hosting environment of the daemon: the
// LAN IPv4 address, whether a private NIC exists, whether cloud
// metadata is reachable, and the resulting placement ("lan" or "vps").
//
// The interface enumeration rules were moved verbatim from
// cmd/omahab/console.go pickLANIPv4 so the daemon and the console agree
// on what counts as LAN.
package netenv

import (
	"context"
	"net"
	"net/http"
	"strings"
	"time"
)

// MetadataURL is the cloud metadata endpoint probed by
// CloudMetadataPresent. It is a var so tests can point it at an
// httptest server; the real endpoint is a bare IP (169.254.169.254).
var MetadataURL = "http://169.254.169.254/"

// MetadataTimeout bounds the metadata probe.
var MetadataTimeout = 2 * time.Second

// IfaceAddrs is the minimal interface view needed for LAN selection.
// It mirrors net.Interface plus its addresses so tests can inject fakes.
type IfaceAddrs struct {
	Name  string
	Flags net.Flags
	Addrs []net.Addr
}

// skipIface reports whether iface is virtual/tunnel and never LAN.
// Same skip rules as the original console pickLANIPv4.
func skipIface(name string) bool {
	for _, p := range []string{
		"tailscale", "docker", "br-", "veth", "podman", "virbr", "cni",
	} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// PickLANIPv4 returns the first private IPv4 address on an up,
// non-loopback, non-virtual interface, or "" when there is none.
func PickLANIPv4(ifaces []IfaceAddrs) string {
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if skipIface(iface.Name) {
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

// LANIPv4 returns the first non-loopback, private IPv4 address, or "".
func LANIPv4() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	list := make([]IfaceAddrs, 0, len(ifaces))
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		list = append(list, IfaceAddrs{Name: iface.Name, Flags: iface.Flags, Addrs: addrs})
	}
	return PickLANIPv4(list)
}

// HasPrivateNIC reports whether the host has a private LAN NIC.
func HasPrivateNIC() bool {
	return LANIPv4() != ""
}

// CloudMetadataPresent reports whether a cloud metadata service answers.
// Any 2xx/4xx response counts as present; 5xx, timeouts, and network
// errors count as absent.
func CloudMetadataPresent(ctx context.Context) bool {
	return CloudMetadataPresentURL(ctx, MetadataURL)
}

// CloudMetadataPresentURL probes an explicit metadata URL (for tests).
func CloudMetadataPresentURL(ctx context.Context, url string) bool {
	ctx, cancel := context.WithTimeout(ctx, MetadataTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSpace(url), nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	c := resp.StatusCode
	return (c >= 200 && c < 300) || (c >= 400 && c < 500)
}

// AutoPlacement returns "lan" when the host has a private NIC and no
// cloud metadata service is reachable, else "vps".
func AutoPlacement() string {
	if HasPrivateNIC() && !CloudMetadataPresent(context.Background()) {
		return "lan"
	}
	return "vps"
}

// IsLANAddr reports whether s is a LAN source address: RFC1918 or ULA
// private, link-local unicast, or loopback. Empty/garbage (and bare
// ports) are not LAN. A trailing :port is tolerated.
func IsLANAddr(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	if h, _, err := net.SplitHostPort(s); err == nil {
		s = h
	}
	s = strings.Trim(s, "[]")
	ip := net.ParseIP(strings.TrimSpace(s))
	if ip == nil {
		return false
	}
	return ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast()
}
