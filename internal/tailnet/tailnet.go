// Package tailnet drives Tailscale enrollment for the bootstrap wizard
// and the `omahab setup` CLI: run `tailscale up` capturing the auth URL,
// poll status until Running with a 100.x address.
package tailnet

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// upMu serializes concurrent `tailscale up` runs: the CLI mints one auth
// URL per run and concurrent runs race for the login session.
var upMu sync.Mutex

// urlWait bounds how long Up waits for the auth URL. `tailscale up`
// --timeout=110s prints the URL within seconds but the process only exits
// on login or timeout; waiting for exit would hang the bootstrap wizard
// for ~110s looking dead.
const urlWait = 30 * time.Second

// Up runs `tailscale up --timeout=110s`, returning the printed auth URL
// (empty when already enrolled). It returns as soon as the URL appears
// instead of waiting for login/timeout; the child keeps running in the
// background so the pending login stays valid, and is reaped on exit.
func Up(ctx context.Context) (authURL string, err error) {
	if st, serr := Status(ctx); serr == nil && st.Running {
		return "", nil
	}
	upMu.Lock()
	defer upMu.Unlock()
	pr, pw, perr := os.Pipe()
	if perr != nil {
		return "", fmt.Errorf("tailscale up: pipe: %w", perr)
	}
	cmd := exec.Command("tailscale", "up", "--timeout=110s")
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		_ = pw.Close()
		_ = pr.Close()
		return "", fmt.Errorf("tailscale up: %w", err)
	}
	_ = pw.Close() // child holds its own copy; EOF arrives on exit/kill
	urlCh := make(chan string, 1)
	doneCh := make(chan struct{})
	var sb strings.Builder
	go func() {
		defer close(doneCh)
		sc := bufio.NewScanner(pr)
		for sc.Scan() {
			line := sc.Text()
			if sb.Len() < 4096 {
				sb.WriteString(line)
				sb.WriteByte('\n')
			}
			if u := extractAuthURL(line); u != "" {
				select {
				case urlCh <- u:
				default:
				}
			}
		}
	}()
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	timer := time.NewTimer(urlWait)
	defer timer.Stop()
	select {
	case u := <-urlCh:
		// Leave the child running so the pending login stays valid;
		// reap it when it exits on its own.
		go func() {
			<-waitCh
			<-doneCh
			_ = pr.Close()
		}()
		return u, nil
	case err := <-waitCh:
		<-doneCh
		_ = pr.Close()
		out := strings.TrimSpace(sb.String())
		if err != nil {
			return "", fmt.Errorf("tailscale up: %v: %s", err, out)
		}
		// Clean exit with no URL: already enrolled (or nothing to do).
		if u := extractAuthURL(out); u != "" {
			return u, nil
		}
		return "", nil
	case <-timer.C:
		_ = cmd.Process.Kill()
		<-waitCh
		<-doneCh
		_ = pr.Close()
		out := strings.TrimSpace(sb.String())
		if u := extractAuthURL(out); u != "" {
			return u, nil
		}
		return "", fmt.Errorf("tailscale up: timed out waiting for auth URL: %s", truncate(out, 300))
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		<-waitCh
		<-doneCh
		_ = pr.Close()
		return "", fmt.Errorf("tailscale up: %w", ctx.Err())
	}
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
