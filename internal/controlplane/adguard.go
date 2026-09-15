package controlplane

import (
	"context"
	"crypto/rand"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"time"

	"github.com/omahab/omahab/internal/apitypes"
	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"
)
// preserved; only managed bind/security fields update on network change.

const adguardStateDir = "/var/lib/AdGuardHome"
const adguardYAML = "/var/lib/AdGuardHome/AdGuardHome.yaml"
const adguardWebPort = 3002

// lanAddresses returns non-loopback LAN addresses: RFC1918 IPv4 and ULA
// IPv6. Tailnet, container, link-local and loopback interfaces are excluded.
// No LAN address means DNS stays awaiting LAN configuration (the web UI
// keeps running); never fall back to 0.0.0.0.
func lanAddresses() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		name := strings.ToLower(iface.Name)
		if strings.HasPrefix(name, "tailscale") || name == "tailscale0" ||
			strings.HasPrefix(name, "br-") || strings.HasPrefix(name, "docker") ||
			strings.HasPrefix(name, "veth") || strings.HasPrefix(name, "lo") {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
				continue
			}
			if ip.IsMulticast() || ip.IsUnspecified() {
				continue
			}
			if v4 := ip.To4(); v4 != nil {
				if !v4.IsPrivate() {
					continue
				}
				out = append(out, v4.String())
				continue
			}
			// ULA IPv6 only (fc00::/7).
			if len(ip) == net.IPv6len && ip[0]&0xfe == 0xfc {
				out = append(out, ip.String())
			}
		}
	}
	return out
}

func adguardDoc(data []byte) (map[string]any, error) {
	doc := map[string]any{}
	if len(data) == 0 {
		return doc, nil
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse AdGuardHome.yaml: %w", err)
	}
	if doc == nil {
		doc = map[string]any{}
	}
	return doc, nil
}

func writeAdGuardYAML(doc map[string]any) error {
	raw, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(adguardStateDir, 0o755); err != nil {
		return err
	}
	tmp := adguardYAML + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, adguardYAML); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func randomAdminPassword() (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	buf := make([]byte, 20)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	for i := range buf {
		buf[i] = alphabet[int(buf[i])%len(alphabet)]
	}
	return string(buf), nil
}

// ensureAdGuardHome seeds the mutable YAML on first boot and refreshes
// managed bind/security fields on network change. Fresh files get Quad9
// upstream, bootstrap resolvers, DHCP off and a bcrypt-hashed admin password
// (plaintext kept in secrets for one-time display). Existing files keep user
// filtering/upstream choices; only schema_version, web bind, DNS port, LAN
// bind_hosts and dhcp.enabled are managed.
func (b *Backend) ensureAdGuardHome(ctx context.Context) error {
	raw, err := os.ReadFile(adguardYAML)
	lan := lanAddresses()
	doc, fresh, err := renderAdGuardDoc(raw, err != nil, lan)
	if err != nil {
		return err
	}
	if fresh {
		password, err := randomAdminPassword()
		if err != nil {
			return err
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			return err
		}
		doc["users"] = []any{map[string]any{"name": "admin", "password": string(hash)}}
		if err := writeAdGuardYAML(doc); err != nil {
			return err
		}
		if err := upsertSecret(ctx, b.secrets, "platform-app", "adguard_initial_password", password); err != nil {
			log.Printf("adguard: store initial password: %v", err)
		}
		log.Printf("adguard: seeded mutable config (upstream Quad9, DHCP off, LAN binds %v)", lan)
		return nil
	}
	return writeAdGuardYAML(doc)
}

func managedBinds(lan []string) []string {
	if len(lan) == 0 {
		// No LAN: loopback-only DNS, web UI keeps running, status
		// reports awaiting LAN configuration.
		return []string{"127.0.0.1"}
	}
	return append([]string{}, lan...)
}

// renderAdGuardDoc merges managed fields into the mutable YAML document.
// Fresh documents get Quad9 upstream, bootstrap resolvers, DHCP off and web
// bind; existing documents keep user filtering/upstream choices while
// schema_version, web bind, DNS port, LAN bind_hosts and dhcp.enabled
// refresh.
func renderAdGuardDoc(raw []byte, missing bool, lan []string) (doc map[string]any, fresh bool, err error) {
	if !missing {
		if doc, err = adguardDoc(raw); err != nil {
			return nil, false, err
		}
	} else {
		doc = map[string]any{}
	}
	fresh = missing
	doc["schema_version"] = 34
	doc["bind_host"] = "127.0.0.1"
	doc["bind_port"] = adguardWebPort
	if fresh {
		doc["dns"] = map[string]any{
			"bind_hosts":         managedBinds(lan),
			"port":               53,
			"upstream_dns":       []string{"https://dns.quad9.net/dns-query"},
			"bootstrap_dns":      []string{"9.9.9.9", "149.112.112.112"},
			"protection_enabled": true,
		}
		doc["dhcp"] = map[string]any{"enabled": false}
		return doc, true, nil
	}
	// Refresh managed fields only; preserve user filtering/upstream.
	dns, _ := doc["dns"].(map[string]any)
	if dns == nil {
		dns = map[string]any{}
		doc["dns"] = dns
	}
	dns["bind_hosts"] = managedBinds(lan)
	dns["port"] = 53
	dhcp, _ := doc["dhcp"].(map[string]any)
	if dhcp == nil {
		dhcp = map[string]any{}
		doc["dhcp"] = dhcp
	}
	dhcp["enabled"] = false
	return doc, false, nil
}

// checkAdGuardPortConflict refuses existing port-53 conflicts with an
// actionable message instead of disabling another resolver. It never touches
// the conflicting service.
func checkAdGuardPortConflict() error {
	if systemctlActive("adguardhome.service") {
		return nil
	}
	for _, network := range []string{"tcp", "udp"} {
		addr := "0.0.0.0:53"
		var lc net.ListenConfig
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		var conn net.Listener
		var perr error
		if network == "tcp" {
			conn, perr = lc.Listen(ctx, "tcp", addr)
		} else {
			var pc net.PacketConn
			pc, perr = lc.ListenPacket(ctx, "udp", addr)
			if perr == nil {
				_ = pc.Close()
			}
		}
		cancel()
		if perr != nil {
			return fmt.Errorf("port 53 (%s) is already held by another resolver (often systemd-resolved or a router service): free it or disable that resolver before enabling household DNS; nothing was changed", network)
		}
		if conn != nil {
			_ = conn.Close()
		}
	}
	return nil
}

// adguardDNSReady is the DNS query readiness check: a real query against the
// LAN listener. A login page alone never means DNS works. Timeouts and
// connection refusals are not ready; any DNS response (even NXDOMAIN) is.
func adguardDNSReady(ctx context.Context) error {
	lan := lanAddresses()
	target := "127.0.0.1"
	if len(lan) > 0 {
		target = lan[0]
	}
	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: 3 * time.Second}
			return d.DialContext(ctx, "udp", net.JoinHostPort(target, "53"))
		},
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := resolver.LookupHost(lookupCtx, "example.com")
	if err != nil {
		if dnsErr, ok := err.(*net.DNSError); ok && dnsErr.Timeout() {
			return fmt.Errorf("no DNS response from %s:53 (timeout)", target)
		}
		if strings.Contains(strings.ToLower(err.Error()), "refused") {
			return fmt.Errorf("DNS on %s:53 refused", target)
		}
		// Any other DNS error (NXDOMAIN, SERVFAIL, no records) proves the
		// server answers queries.
		return nil
	}
	return nil
}

// AdGuardSetup aliases the API view type.
type AdGuardSetup = apitypes.AdGuardSetup

// GetAdGuardSetup serves the admin setup view with show-once semantics.
func (b *Backend) GetAdGuardSetup(ctx context.Context) (AdGuardSetup, error) {
	view := AdGuardSetup{LANAddresses: lanAddresses(), DNSPort: 53, Upstream: "https://dns.quad9.net/dns-query"}
	if pw, err := b.secrets.RevealByName(ctx, "platform-app", "adguard_initial_password"); err == nil && strings.TrimSpace(pw) != "" {
		view.InitialAdminPassword = strings.TrimSpace(pw)
		_ = b.secrets.DeleteByName(ctx, "platform-app", "adguard_initial_password")
	} else {
		view.PasswordShown = true
	}
	if err := adguardDNSReady(ctx); err != nil {
		view.DNSDetail = err.Error()
	} else {
		view.DNSReady = true
	}
	if len(view.LANAddresses) == 0 {
		view.DNSDetail = firstNonEmpty(view.DNSDetail, "awaiting LAN configuration")
	}
	return view, nil
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}


func adguardInstalled(ctx context.Context, b *Backend) bool {
	if b.apps == nil {
		return false
	}
	list, err := b.apps.List(ctx)
	if err != nil {
		return false
	}
	for _, st := range list {
		if st.BundleID == "adguard-home" {
			return true
		}
	}
	return false
}
