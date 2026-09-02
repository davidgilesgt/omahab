// Package tailnet drives Tailscale enrollment for the bootstrap wizard
// and the `omahab setup` CLI: run `tailscale up` capturing the auth URL,
// poll status until Running with a 100.x address.
package tailnet

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"time"
)

// Up runs `tailscale up --timeout=110s`, returning the printed auth URL
// (empty when already enrolled).
func Up(ctx context.Context) (authURL string, err error) {
	cctx, cancel := context.WithTimeout(ctx, 110*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, "tailscale", "up", "--timeout=110s").CombinedOutput()
	combined := string(out)
	if u := extractAuthURL(combined); u != "" {
		return u, nil
	}
	if err != nil {
		return "", fmt.Errorf("tailscale up: %v: %s", err, strings.TrimSpace(combined))
	}
	// No URL and no error: likely already logged in.
	return "", nil
}

// extractAuthURL finds the login.tailscale.com URL in command output.
func extractAuthURL(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if !strings.Contains(line, "https://login.tailscale.com") {
			continue
		}
		for _, field := range strings.Fields(line) {
			if strings.HasPrefix(field, "https://login.tailscale.com") {
				return strings.TrimRight(field, ".,;)")
			}
		}
		idx := strings.Index(line, "https://")
		if idx >= 0 {
			rest := line[idx:]
			if sp := strings.IndexAny(rest, " \t\r\n\"'"); sp >= 0 {
				return rest[:sp]
			}
			return rest
		}
	}
	return ""
}

// StatusResult reports whether tailscale is Running and its IPv4.
type StatusResult struct {
	Running bool
	IP      string
	State   string
	Detail  string
}

// Status polls `tailscale status --json` once.
func Status(ctx context.Context) (StatusResult, error) {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, "tailscale", "status", "--json").CombinedOutput()
	res := StatusResult{Detail: truncate(string(out), 600)}
	if err != nil && strings.TrimSpace(string(out)) == "" {
		res.Detail = err.Error()
		return res, nil
	}
	var st struct {
		BackendState string `json:"BackendState"`
		Self         struct {
			TailscaleIPs []string `json:"TailscaleIPs"`
		} `json:"Self"`
	}
	if jerr := json.Unmarshal(out, &st); jerr != nil {
		// Fall back to substring heuristics for older clients.
		if strings.Contains(string(out), "Running") {
			res.Running = true
			res.IP = firstTailscaleIPv4(ctx)
			res.State = "Running"
		}
		return res, nil
	}
	res.State = st.BackendState
	res.Running = st.BackendState == "Running"
	if res.Running {
		for _, ip := range st.Self.TailscaleIPs {
			if strings.HasPrefix(ip, "100.") {
				res.IP = ip
				break
			}
		}
		if res.IP == "" {
			res.IP = firstTailscaleIPv4(ctx)
		}
	}
	return res, nil
}

func firstTailscaleIPv4(ctx context.Context) string {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, "tailscale", "ip", "-4").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// IsTailscaleIPv4 reports whether ip is a 100.64.0.0/10 CGNAT address.
func IsTailscaleIPv4(ip string) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil || parsed.To4() == nil {
		return false
	}
	return parsed.To4()[0] == 100
}
