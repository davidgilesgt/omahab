package controlplane

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/omahab/omahab/internal/apitypes"
)

// Technitium DNS — household filtering on LAN :53, web/API on 127.0.0.1:5380.
// Managed via HTTP API (APIDOCS): /api/status, /api/user/login,
// /api/user/changePassword, /api/settings/get|set, /api/admin/sso/get|set.
// First-start env seeding is not used; omahabd converges steady-state via API.

const technitiumStateDir = "/var/lib/technitium-dns-server"
const technitiumWebPort = 5380
const technitiumUpstream = "https://dns.quad9.net/dns-query"

var technitiumBaseURL = "http://127.0.0.1:5380"

func technitiumBlocklists() []string {
	return []string{
		"https://raw.githubusercontent.com/StevenBlack/hosts/master/hosts",
		"https://cdn.jsdelivr.net/gh/hagezi/dns-blocklists-legacy@latest/hosts/pro.txt",
	}
}

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

// technitiumDNSEndpoints builds :53 listen endpoints: LAN addresses plus
// loopback for local probes. Empty LAN yields loopback-only (awaiting LAN).
func technitiumDNSEndpoints(lan []string) []string {
	if len(lan) == 0 {
		return []string{"127.0.0.1:53"}
	}
	seen := map[string]bool{}
	var out []string
	for _, ip := range append([]string{"127.0.0.1"}, lan...) {
		ep := net.JoinHostPort(ip, "53")
		if !seen[ep] {
			seen[ep] = true
			out = append(out, ep)
		}
	}
	return out
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

type technitiumAPIResponse struct {
	Status       string `json:"status"`
	ErrorMessage string `json:"errorMessage"`
	Token        string `json:"token"`
}

func technitiumCall(ctx context.Context, path string, params url.Values, token string) ([]byte, error) {
	u := technitiumBaseURL + path
	if params == nil {
		params = url.Values{}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u+"?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("technitium %s: http %d", path, resp.StatusCode)
	}
	return body, nil
}

func technitiumStatus(ctx context.Context) (hasDefaultCreds, ssoEnabled bool, err error) {
	body, err := technitiumCall(ctx, "/api/status", nil, "")
	if err != nil {
		return false, false, err
	}
	var v struct {
		HasDefaultCredentials bool   `json:"hasDefaultCredentials"`
		SsoEnabled            bool   `json:"ssoEnabled"`
		Status                string `json:"status"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return false, false, err
	}
	return v.HasDefaultCredentials, v.SsoEnabled, nil
}

func technitiumLogin(ctx context.Context, user, pass string) (string, error) {
	body, err := technitiumCall(ctx, "/api/user/login", url.Values{
		"user": {user},
		"pass": {pass},
	}, "")
	if err != nil {
		return "", err
	}
	var v technitiumAPIResponse
	if err := json.Unmarshal(body, &v); err != nil {
		return "", err
	}
	if v.Status != "ok" || v.Token == "" {
		return "", fmt.Errorf("technitium login failed: %s", firstNonEmpty(v.ErrorMessage, v.Status))
	}
	return v.Token, nil
}

func technitiumChangePassword(ctx context.Context, token, user, oldPass, newPass string) error {
	body, err := technitiumCall(ctx, "/api/user/changePassword", url.Values{
		"user":    {user},
		"pass":    {oldPass},
		"newPass": {newPass},
	}, token)
	if err != nil {
		return err
	}
	var v technitiumAPIResponse
	if err := json.Unmarshal(body, &v); err != nil {
		return err
	}
	if v.Status != "ok" {
		return fmt.Errorf("technitium change password: %s", firstNonEmpty(v.ErrorMessage, v.Status))
	}
	return nil
}

func technitiumApplySettings(ctx context.Context, token string, lan []string) error {
	params := url.Values{
		"webServiceLocalAddresses": {"127.0.0.1"},
		"webServiceHttpPort":       {fmt.Sprint(technitiumWebPort)},
		"dnsServerLocalEndPoints":  {strings.Join(technitiumDNSEndpoints(lan), ",")},
		"forwarders":               {technitiumUpstream},
		"forwarderProtocol":        {"Https"},
		"enableBlocking":           {"true"},
		"blockListUrls":            {strings.Join(technitiumBlocklists(), ",")},
		"recursion":                {"AllowOnlyForPrivateNetworks"},
	}
	body, err := technitiumCall(ctx, "/api/settings/set", params, token)
	if err != nil {
		return err
	}
	var v technitiumAPIResponse
	if err := json.Unmarshal(body, &v); err != nil {
		return err
	}
	if v.Status != "ok" {
		return fmt.Errorf("technitium settings: %s", firstNonEmpty(v.ErrorMessage, v.Status))
	}
	return nil
}

// ensureTechnitium converges Technitium via API: default-credential rotation
// on first boot (password kept once in secrets), then idempotent settings
// (Quad9 DoH forwarders, blocklists, loopback web, LAN :53 binds,
// private-only recursion) on every run.
func (b *Backend) ensureTechnitium(ctx context.Context) error {
	waitCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	var lastErr error
	for {
		_, _, err := technitiumStatus(waitCtx)
		if err == nil {
			lastErr = nil
			break
		}
		lastErr = err
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("technitium web not ready on :%d: %w", technitiumWebPort, lastErr)
		case <-time.After(2 * time.Second):
		}
	}

	hasDefault, _, err := technitiumStatus(ctx)
	if err != nil {
		return err
	}
	token := ""
	if hasDefault {
		token, err = technitiumLogin(ctx, "admin", "admin")
		if err != nil {
			return err
		}
		password, err := randomAdminPassword()
		if err != nil {
			return err
		}
		if err := technitiumChangePassword(ctx, token, "admin", "admin", password); err != nil {
			return err
		}
		if err := upsertSecret(ctx, b.secrets, "platform-app", "technitium_initial_password", password); err != nil {
			log.Printf("technitium: store initial password: %v", err)
		}
		// Re-login with the new password for the settings pass below.
		token, err = technitiumLogin(ctx, "admin", password)
		if err != nil {
			return err
		}
		log.Printf("technitium: rotated default admin credentials")
	} else if pw, err := b.secrets.RevealByName(ctx, "platform-app", "technitium_initial_password"); err == nil && strings.TrimSpace(pw) != "" {
		if t, lerr := technitiumLogin(ctx, "admin", strings.TrimSpace(pw)); lerr == nil {
			token = t
		}
	}
	if token == "" {
		// Steady state: API token or session managed outside first-boot.
		// Settings convergence needs auth; without it report clearly.
		return fmt.Errorf("technitium auth required: initial password already shown and no API token configured")
	}
	lan := lanAddresses()
	if err := technitiumApplySettings(ctx, token, lan); err != nil {
		return err
	}
	log.Printf("technitium: converged settings (Quad9 DoH, blocking on, web 127.0.0.1:%d, DNS %v)", technitiumWebPort, technitiumDNSEndpoints(lan))
	return nil
}

// technitiumSSOParams renders the Pocket ID OIDC settings for the Technitium
// console. Signup stays open to every Pocket ID-authenticated user: Pocket ID
// accounts are enrollment-gated, so an authenticated user is already trusted,
// while a mapped-only restriction turns IdP group-claim gaps (missing groups
// scope delivery, unmapped membership) into a dead-end login error. The group
// map still syncs admins/members to local roles on every login.
func technitiumSSOParams(domain, clientID, clientSecret string) url.Values {
	authority := "https://id." + strings.TrimSpace(domain)
	return url.Values{
		"ssoEnabled":                       {"true"},
		"ssoAuthority":                     {authority},
		"ssoClientId":                      {strings.TrimSpace(clientID)},
		"ssoClientSecret":                  {strings.TrimSpace(clientSecret)},
		"ssoScopes":                        {"openid|profile|email|groups"},
		"ssoAllowSignup":                   {"true"},
		"ssoAllowSignupOnlyForMappedUsers": {"false"},
		"ssoGroupMap":                      {"admins:Administrators|members:DNS Administrators"},
	}
}

// ensureTechnitiumSSO configures Pocket ID OIDC for the Technitium console.
// Group map syncs Pocket ID groups to local roles on every login.
func (b *Backend) ensureTechnitiumSSO(ctx context.Context, domain, clientID, clientSecret string) error {
	domain = strings.TrimSpace(domain)
	if domain == "" || clientID == "" || clientSecret == "" {
		return fmt.Errorf("domain, clientID and clientSecret required")
	}
	token, err := technitiumLoginFromSecrets(ctx, b)
	if err != nil {
		return err
	}
	params := technitiumSSOParams(domain, clientID, clientSecret)
	body, err := technitiumCall(ctx, "/api/admin/sso/set", params, token)
	if err != nil {
		return err
	}
	var v technitiumAPIResponse
	if err := json.Unmarshal(body, &v); err != nil {
		return err
	}
	if v.Status != "ok" {
		return fmt.Errorf("technitium sso: %s", firstNonEmpty(v.ErrorMessage, v.Status))
	}
	log.Printf("technitium: SSO enabled via %s", params.Get("ssoAuthority"))
	return nil
}

func technitiumLoginFromSecrets(ctx context.Context, b *Backend) (string, error) {
	if pw, err := b.secrets.RevealByName(ctx, "platform-app", "technitium_initial_password"); err == nil && strings.TrimSpace(pw) != "" {
		if t, lerr := technitiumLogin(ctx, "admin", strings.TrimSpace(pw)); lerr == nil {
			return t, nil
		}
	}
	if tok, err := b.secrets.RevealByName(ctx, "platform-app", "technitium_api_token"); err == nil && strings.TrimSpace(tok) != "" {
		return strings.TrimSpace(tok), nil
	}
	return "", fmt.Errorf("technitium auth required: no initial password or API token in secrets")
}

// checkTechnitiumPortConflict refuses existing port-53 conflicts with an
// actionable message. It never touches the conflicting service.
func checkTechnitiumPortConflict() error {
	if systemctlActive("technitium-dns-server.service") {
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

// technitiumDNSReady is the DNS query readiness check: a real query against
// the LAN listener. Timeouts and refusals are not ready; any DNS response
// (even NXDOMAIN) is.
func technitiumDNSReady(ctx context.Context) error {
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
		return nil
	}
	return nil
}

// TechnitiumSetup aliases the API view type.
type TechnitiumSetup = apitypes.TechnitiumSetup

// GetTechnitiumSetup serves the admin setup view with show-once semantics.
func (b *Backend) GetTechnitiumSetup(ctx context.Context) (TechnitiumSetup, error) {
	view := TechnitiumSetup{LANAddresses: lanAddresses(), DNSPort: 53, Upstream: technitiumUpstream}
	if pw, err := b.secrets.RevealByName(ctx, "platform-app", "technitium_initial_password"); err == nil && strings.TrimSpace(pw) != "" {
		view.InitialAdminPassword = strings.TrimSpace(pw)
		_ = b.secrets.DeleteByName(ctx, "platform-app", "technitium_initial_password")
	} else {
		view.PasswordShown = true
	}
	if _, sso, err := technitiumStatus(ctx); err == nil {
		view.SSOEnabled = sso
	}
	if err := technitiumDNSReady(ctx); err != nil {
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

func technitiumInstalled(ctx context.Context, b *Backend) bool {
	if b.apps == nil {
		return false
	}
	list, err := b.apps.List(ctx)
	if err != nil {
		return false
	}
	for _, st := range list {
		if st.BundleID == "technitium" {
			return true
		}
	}
	return false
}
