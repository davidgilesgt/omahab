package client

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/url"
	"os/exec"
	"strings"
	"time"
)

// DiagnosticCheck is a single named check result.
type DiagnosticCheck struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

// DiagnosticReport aggregates all Tailscale/DNS/TLS/PocketID/instance checks.
type DiagnosticReport struct {
	Checks    []DiagnosticCheck `json:"checks"`
	OverallOK bool              `json:"overall_ok"`
	At        time.Time         `json:"at"`
}

// TailscaleChecker is injectable for tests.
type TailscaleChecker interface {
	IsInstalled(ctx context.Context) (bool, string)
	IsLoggedIn(ctx context.Context) (bool, string)
	Tailnet(ctx context.Context) (string, error)
	ServerNodeVisible(ctx context.Context, expectedNode string) (bool, string)
	TailscaleIP(ctx context.Context) (string, error)
}

// DNSResolver is injectable for tests.
type DNSResolver interface {
	LookupIP(ctx context.Context, host string) ([]net.IP, error)
}

// TLSChecker probes TLS cert validity.
type TLSChecker interface {
	CheckTLS(ctx context.Context, serverURL string) (bool, string)
}

// ExecTailscaleChecker uses the local tailscale binary.
type ExecTailscaleChecker struct {
	Bin string
}

func (e *ExecTailscaleChecker) bin() string {
	if e.Bin != "" {
		return e.Bin
	}
	return "tailscale"
}

func (e *ExecTailscaleChecker) IsInstalled(ctx context.Context) (bool, string) {
	_, err := exec.LookPath(e.bin())
	if err != nil {
		return false, "tailscale not found in PATH"
	}
	return true, "tailscale installed"
}

func (e *ExecTailscaleChecker) IsLoggedIn(ctx context.Context) (bool, string) {
	out, err := exec.CommandContext(ctx, e.bin(), "status", "--json").Output()
	if err != nil {
		return false, fmt.Sprintf("tailscale status failed: %v", err)
	}
	s := string(out)
	if strings.Contains(s, `"BackendState":"Running"`) {
		return true, "tailscale running"
	}
	if strings.Contains(s, "LoggedOut") || strings.Contains(s, "NeedsLogin") {
		return false, "tailscale not logged in"
	}
	return true, "tailscale status ok"
}

func (e *ExecTailscaleChecker) Tailnet(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, e.bin(), "status", "--json").Output()
	if err != nil {
		return "", err
	}
	s := string(out)
	// crude extract of MagicDNSSuffix or tailnet name; full parsing not needed for check
	// Look for `"MagicDNSSuffix":"<name>"`
	idx := strings.Index(s, "MagicDNSSuffix")
	if idx == -1 {
		return "", fmt.Errorf("tailnet not found")
	}
	seg := s[idx:]
	q1 := strings.Index(seg, "\"")
	if q1 == -1 {
		return "", fmt.Errorf("tailnet parse fail")
	}
	// find value after colon
	colon := strings.Index(seg, ":")
	if colon == -1 {
		return "", fmt.Errorf("tailnet parse fail")
	}
	rest := strings.TrimSpace(seg[colon+1:])
	rest = strings.Trim(rest, "\"")
	end := strings.Index(rest, "\"")
	if end != -1 {
		rest = rest[:end]
	}
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return "", fmt.Errorf("empty tailnet")
	}
	return rest, nil
}

func (e *ExecTailscaleChecker) ServerNodeVisible(ctx context.Context, expectedNode string) (bool, string) {
	if expectedNode == "" {
		return true, "no expected server node configured"
	}
	out, err := exec.CommandContext(ctx, e.bin(), "status", "--json").Output()
	if err != nil {
		return false, fmt.Sprintf("tailscale status failed: %v", err)
	}
	if strings.Contains(string(out), expectedNode) {
		return true, fmt.Sprintf("server node %s visible", expectedNode)
	}
	return false, fmt.Sprintf("server node %s not visible", expectedNode)
}

func (e *ExecTailscaleChecker) TailscaleIP(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, e.bin(), "ip", "-4").Output()
	if err != nil {
		return "", err
	}
	ip := strings.TrimSpace(string(out))
	if ip == "" {
		return "", fmt.Errorf("no tailscale ip")
	}
	return strings.Split(ip, "\n")[0], nil
}

// NetDNSResolver uses net.DefaultResolver.
type NetDNSResolver struct{}

func (n *NetDNSResolver) LookupIP(ctx context.Context, host string) ([]net.IP, error) {
	return net.DefaultResolver.LookupIP(ctx, "ip4", host)
}

// DefaultTLSChecker does a TLS handshake without sending credentials.
type DefaultTLSChecker struct{}

func (d *DefaultTLSChecker) CheckTLS(ctx context.Context, serverURL string) (bool, string) {
	u, err := url.Parse(serverURL)
	if err != nil {
		return false, fmt.Sprintf("invalid url: %v", err)
	}
	if u.Scheme != "https" {
		return true, "http (no TLS check)"
	}
	host := u.Host
	if !strings.Contains(host, ":") {
		host += ":443"
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	conn, err := tls.DialWithDialer(dialer, "tcp", host, &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: u.Hostname(),
	})
	if err != nil {
		return false, fmt.Sprintf("tls handshake failed: %v", err)
	}
	_ = conn.Close()
	return true, "tls certificate valid"
}

// Diagnose runs all required checks: Tailscale/DNS/TLS/PocketID/instance.
// It never falls back from private Tailscale route to a public route.
func Diagnose(ctx context.Context, cfg *Config, remote *RemoteClient, checker TailscaleChecker, resolver DNSResolver, tlsChecker TLSChecker) *DiagnosticReport {
	if checker == nil {
		checker = &ExecTailscaleChecker{}
	}
	if resolver == nil {
		resolver = &NetDNSResolver{}
	}
	if tlsChecker == nil {
		tlsChecker = &DefaultTLSChecker{}
	}
	var checks []DiagnosticCheck
	add := func(name string, ok bool, msg string) {
		checks = append(checks, DiagnosticCheck{Name: name, OK: ok, Message: msg})
	}

	// 1. Tailscale installed
	ok, msg := checker.IsInstalled(ctx)
	add("tailscale_installed", ok, msg)

	// 2. Tailscale logged in
	if ok {
		lok, lmsg := checker.IsLoggedIn(ctx)
		add("tailscale_logged_in", lok, lmsg)
		ok = lok
	} else {
		add("tailscale_logged_in", false, "skipped: tailscale not installed")
	}

	// 3. Tailnet identity pinning
	if cfg.ExpectedTailnet != "" {
		tn, err := checker.Tailnet(ctx)
		if err != nil {
			add("tailscale_tailnet", false, fmt.Sprintf("tailnet check failed: %v", err))
		} else if tn != cfg.ExpectedTailnet {
			add("tailscale_tailnet", false, fmt.Sprintf("tailnet mismatch: got %s want %s", tn, cfg.ExpectedTailnet))
		} else {
			add("tailscale_tailnet", true, fmt.Sprintf("tailnet %s", tn))
		}
	} else {
		add("tailscale_tailnet", true, "no expected tailnet pinned")
	}

	// 4. Server node visible
	vis, vmsg := checker.ServerNodeVisible(ctx, cfg.ExpectedServerNode)
	add("server_node_visible", vis, vmsg)

	// 5. DNS resolves to expected Tailscale IP in private mode
	u, _ := url.Parse(cfg.ServerURL)
	host := ""
	if u != nil {
		host = u.Hostname()
	}
	tip, tipErr := checker.TailscaleIP(ctx)
	if u != nil && u.Scheme == "https" && host != "" && tipErr == nil && tip != "" {
		ips, err := resolver.LookupIP(ctx, host)
		if err != nil {
			add("dns_resolves_to_tailscale_ip", false, fmt.Sprintf("dns lookup failed for %s: %v", host, err))
		} else {
			match := false
			for _, ip := range ips {
				if ip.String() == tip {
					match = true
					break
				}
			}
			if match {
				add("dns_resolves_to_tailscale_ip", true, fmt.Sprintf("%s resolves to tailscale ip %s", host, tip))
			} else {
				// Public fallback detection: resolved but not to tailscale IP.
				add("dns_resolves_to_tailscale_ip", false, fmt.Sprintf("dns for %s does not resolve to tailscale ip %s (got %v) — public fallback rejected", host, tip, ips))
			}
		}
	} else if tipErr != nil {
		add("dns_resolves_to_tailscale_ip", false, fmt.Sprintf("tailscale ip unavailable: %v", tipErr))
	} else {
		add("dns_resolves_to_tailscale_ip", true, "skipped (http or no host)")
	}

	// 6. TLS certificate valid
	tok, tmsg := tlsChecker.CheckTLS(ctx, cfg.ServerURL)
	add("tls_certificate_valid", tok, tmsg)

	// 7. Pocket ID reachable (via server health; pocketid is proxied behind omahabd edge)
	if remote != nil {
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		// Use a lightweight endpoint; if server is reachable, pocket-id config is implied.
		// We probe /api/instance which the edge should gate via Pocket ID session.
		err := remote.CheckPocketID(pctx)
		if err != nil {
			add("pocketid_reachable", false, fmt.Sprintf("pocket-id check failed: %v", err))
		} else {
			add("pocketid_reachable", true, "pocket-id reachable via omahabd")
		}
	} else {
		add("pocketid_reachable", false, "no remote client")
	}

	// 8. Instance ID match (pinning)
	if cfg.PinnedInstanceID == "" {
		add("instance_id_match", false, "no pinned instance id (enrollment required)")
	} else if remote != nil {
		rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		inst, err := remote.GetInstance(rctx)
		if err != nil {
			// Distinguish mismatch from other errors
			if isInstanceMismatch(err) {
				add("instance_id_match", false, fmt.Sprintf("instance mismatch: %v", err))
			} else {
				add("instance_id_match", false, fmt.Sprintf("instance fetch failed: %v", err))
			}
		} else if string(inst.ID) != cfg.PinnedInstanceID {
			add("instance_id_match", false, fmt.Sprintf("instance mismatch: got %s want %s", inst.ID, cfg.PinnedInstanceID))
		} else {
			add("instance_id_match", true, fmt.Sprintf("instance %s", inst.ID))
		}
	} else {
		add("instance_id_match", false, "no remote client")
	}

	overall := true
	for _, c := range checks {
		// tailscale_tailnet with no pin is not a failure; treat as ok already
		// instance_id_match with no pin is expected to be false until enrollment
		// but for overall, require it when pinned
		if !c.OK {
			// Allow "no expected tailnet pinned" and "skipped http" as ok (already marked ok)
			// So any !OK is a real failure.
			overall = false
			// Do not break: collect all
		}
	}
	// If not yet enrolled (no pinned ID), overall is false but report still shows checks.
	return &DiagnosticReport{Checks: checks, OverallOK: overall, At: time.Now().UTC()}
}

func isInstanceMismatch(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), ErrInstanceMismatch.Error())
}
