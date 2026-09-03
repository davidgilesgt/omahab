package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/skip2/go-qrcode"
	"github.com/spf13/cobra"

	"github.com/omahab/omahab/internal/tui"
)

const bootstrapCodePath = "/run/omahab/bootstrap-code"
const bootstrapDonePath = "/var/lib/omahab/bootstrap-done"

// newConsoleCmd builds `omahab console`: the tty1 first-boot display.
// Clears the screen, shows the LAN bootstrap URL + one-time code + QR,
// refreshing every 5s; after bootstrap completes, shows a live status
// screen (hostname, Tailscale IP, doctor, backup, events, exposure).
func newConsoleCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "console",
		Short: "First-boot console display (tty1)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConsole()
		},
	}
}

func runConsole() error {
	w := os.Stdout
	for {
		fmt.Fprint(w, "\033[2J\033[H") // clear + home
		fmt.Fprintln(w, "")
		fmt.Fprintln(w, "  ┌─────────────────────────────────────────────┐")
		if _, err := os.Stat(bootstrapDonePath); err == nil {
			fmt.Fprintln(w, "  │            OMAHAB  ·  live status           │")
		} else {
			fmt.Fprintln(w, "  │            OMAHAB  ·  first boot            │")
		}
		fmt.Fprintln(w, "  └─────────────────────────────────────────────┘")
		fmt.Fprintln(w, "")

		if _, err := os.Stat(bootstrapDonePath); err == nil {
			// Post-bootstrap live status, refreshing every 5s.
			renderLiveStatus(w)
			fmt.Fprintln(w, "")
			fmt.Fprintln(w, "  press Enter for shell login")
			fmt.Fprintln(w, "")
			fmt.Fprintf(w, "  Refreshing every 5s — %s\n", time.Now().Format("15:04:05"))
			time.Sleep(5 * time.Second)
			continue
		}

		// Bootstrap pending: LAN wizard URL + one-time code.
		ip := lanIPv4()
		code := readBootstrapCode()
		fmt.Fprintln(w, "  Complete setup from any device on this network:")
		fmt.Fprintln(w, "")
		if ip != "" {
			fmt.Fprintf(w, "      http://%s:8485\n", ip)
			if code != "" {
				fmt.Fprintln(w, "")
				fmt.Fprintln(w, "  One-time code:")
				fmt.Fprintf(w, "      %s\n", code)
				if qr, err := qrcode.New("http://"+ip+":8485/#code="+code, qrcode.Medium); err == nil {
					fmt.Fprintln(w, "")
					fmt.Fprint(w, qr.ToSmallString(false))
				}
			} else {
				fmt.Fprintln(w, "")
				fmt.Fprintln(w, "  (waiting for the one-time code — omahabd is starting)")
			}
		} else {
			fmt.Fprintln(w, "      (waiting for a network address)")
		}
		fmt.Fprintln(w, "")
		fmt.Fprintf(w, "  Refreshing every 5s — %s\n", time.Now().Format("15:04:05"))
		time.Sleep(5 * time.Second)
	}
}

func renderLiveStatus(w io.Writer) {
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "omahab"
	}
	tsIP := tailscaleIPv4()
	if tsIP == "" {
		tsIP = "—"
	}
	// Resolve capabilities for tui styles.
	caps := tui.ResolveCaps(isTerminal(w), os.Getenv("TERM"), os.Getenv("NO_COLOR"))

	// Dashboard URL
	dashURL := fmt.Sprintf("http://%s:8484", tsIP)
	if tsIP == "—" {
		dashURL = "http://<tailscale-ip>:8484"
	}

	fmt.Fprintf(w, "  Host:       %s\n", hostname)
	fmt.Fprintf(w, "  Tailscale:  %s\n", tsIP)
	fmt.Fprintf(w, "  Dashboard:  %s\n", dashURL)
	fmt.Fprintln(w, "")

	// Fetch live data (best-effort).
	token := readAdminToken()
	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()
	_ = ctx // used per fetch

	// Doctor summary.
	doctorSummary := fetchDoctorSummary(ctx, token, caps)
	fmt.Fprintf(w, "  Health:     %s\n", doctorSummary)

	// Backup age.
	backupLine := fetchBackupLine(ctx, token)
	fmt.Fprintf(w, "  Backup:     %s\n", backupLine)

	// Unread events.
	unreadLine := fetchUnreadEventsLine(ctx, token)
	fmt.Fprintf(w, "  Inbox:      %s\n", unreadLine)

	// Exposed public count.
	exposureLine := fetchExposureLine(ctx, token)
	fmt.Fprintf(w, "  Exposure:   %s\n", exposureLine)
}

func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// readAdminToken tries OMAHAB_TOKEN env, then well-known token files.
func readAdminToken() string {
	if v := strings.TrimSpace(os.Getenv("OMAHAB_TOKEN")); v != "" {
		return v
	}
	candidates := []string{}
	if h := os.Getenv("XDG_CONFIG_HOME"); h != "" {
		candidates = append(candidates, filepath.Join(h, "omahab", "token"))
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		candidates = append(candidates, filepath.Join(home, ".config", "omahab", "token"))
	}
	// Explicit omahab user path (daemon provisions there; console runs as root)
	candidates = append(candidates, "/home/omahab/.config/omahab/token")
	candidates = append(candidates, "/root/.config/omahab/token")
	// Also check via user lookup for omahab
	if u, err := user.Lookup("omahab"); err == nil && u.HomeDir != "" {
		candidates = append(candidates, filepath.Join(u.HomeDir, ".config", "omahab", "token"))
	}
	for _, p := range candidates {
		if p == "" {
			continue
		}
		if data, err := os.ReadFile(p); err == nil {
			if t := strings.TrimSpace(string(data)); t != "" {
				return t
			}
		}
	}
	return ""
}

type doctorCheck struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Message string `json:"message"`
	Detail  string `json:"detail"`
}
type doctorResult struct {
	Healthy bool          `json:"healthy"`
	Checks  []doctorCheck `json:"checks"`
}

func fetchDoctorSummary(ctx context.Context, token string, caps tui.Caps) string {
	var res doctorResult
	if err := apiGet(ctx, "/api/v1/doctor", token, &res); err != nil {
		if token == "" {
			return "unavailable (no token — console runs without auth)"
		}
		return "unavailable (" + strings.TrimSpace(err.Error()) + ")"
	}
	if len(res.Checks) == 0 {
		if res.Healthy {
			return tui.HealthChip("healthy") + " healthy (no checks)"
		}
		return tui.HealthChip("unknown") + " unknown"
	}
	failures := 0
	for _, c := range res.Checks {
		if strings.ToLower(c.Status) != "healthy" {
			failures++
		}
	}
	if failures == 0 {
		return tui.HealthChip("healthy") + fmt.Sprintf(" healthy (%d checks)", len(res.Checks))
	}
	// Use tui for chip rendering with caps.
	chip := tui.HealthChip("unhealthy")
	if failures == 1 {
		// single failure: show its message
		for _, c := range res.Checks {
			if strings.ToLower(c.Status) != "healthy" {
				return chip + fmt.Sprintf(" %d failing — %s: %s", failures, c.Name, c.Message)
			}
		}
	}
	return chip + fmt.Sprintf(" %d of %d checks failing", failures, len(res.Checks))
}

func fetchBackupLine(ctx context.Context, token string) string {
	type backup struct {
		ID         string     `json:"id"`
		Status     string     `json:"status"`
		StartedAt  time.Time  `json:"started_at"`
		FinishedAt *time.Time `json:"finished_at"`
		VerifiedAt *time.Time `json:"verified_at"`
		Error      string     `json:"error"`
	}
	type envelope struct {
		Items []backup `json:"items"`
	}
	var env envelope
	// Try envelope, fallback to array.
	if err := apiGet(ctx, "/api/v1/backups", token, &env); err != nil {
		// attempt raw array fallback
		var arr []backup
		if err2 := apiGet(ctx, "/api/v1/backups", token, &arr); err2 != nil {
			if token == "" {
				return "unavailable (no token)"
			}
			return "unavailable"
		} else {
			env.Items = arr
		}
	}
	if len(env.Items) == 0 {
		return "never"
	}
	// Find latest by started_at.
	latest := env.Items[0]
	for _, b := range env.Items[1:] {
		if b.StartedAt.After(latest.StartedAt) {
			latest = b
		}
	}
	if strings.TrimSpace(latest.Error) != "" {
		return "error: " + strings.TrimSpace(latest.Error)
	}
	t := latest.StartedAt
	if latest.FinishedAt != nil {
		t = *latest.FinishedAt
	}
	age := time.Since(t)
	if age < time.Minute {
		return "just now"
	}
	if age < time.Hour {
		return fmt.Sprintf("%dm ago", int(age.Minutes()))
	}
	if age < 24*time.Hour {
		return fmt.Sprintf("%dh ago", int(age.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(age.Hours()/24))
}

func fetchUnreadEventsLine(ctx context.Context, token string) string {
	type event struct {
		ID     string     `json:"id"`
		ReadAt *time.Time `json:"read_at"`
	}
	type envelope struct {
		Items []event `json:"items"`
	}
	var env envelope
	if err := apiGet(ctx, "/api/v1/events", token, &env); err != nil {
		var arr []event
		if err2 := apiGet(ctx, "/api/v1/events", token, &arr); err2 != nil {
			if token == "" {
				return "unavailable"
			}
			return "unavailable"
		} else {
			env.Items = arr
		}
	}
	unread := 0
	for _, e := range env.Items {
		if e.ReadAt == nil {
			unread++
		}
	}
	if unread == 0 {
		return "0 unread"
	}
	return fmt.Sprintf("%d unread", unread)
}

func fetchExposureLine(ctx context.Context, token string) string {
	type app struct {
		Exposure string `json:"exposure"`
	}
	type envelope struct {
		Items []app `json:"items"`
	}
	var env envelope
	if err := apiGet(ctx, "/api/v1/applications", token, &env); err != nil {
		var arr []app
		if err2 := apiGet(ctx, "/api/v1/applications", token, &arr); err2 != nil {
			if token == "" {
				return "unavailable"
			}
			return "unavailable"
		} else {
			env.Items = arr
		}
	}
	public := 0
	for _, a := range env.Items {
		if strings.ToLower(a.Exposure) == "public" {
			public++
		}
	}
	if public == 0 {
		return "nothing public"
	}
	if public == 1 {
		return "1 app public"
	}
	return fmt.Sprintf("%d apps public", public)
}

func apiGet(ctx context.Context, path, token string, out any) error {
	urlStr := "http://127.0.0.1:8484" + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		msg := strings.TrimSpace(string(b))
		if msg == "" {
			msg = resp.Status
		}
		return fmt.Errorf("%s: %s", resp.Status, msg)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	// Handle envelope vs bare array: try direct unmarshal, fallback to items wrapper already handled by callers.
	if err := json.Unmarshal(data, out); err != nil {
		return err
	}
	return nil
}

// lanIPv4 returns the first non-loopback, non-tailscale IPv4 address.
func lanIPv4() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 || !strings.HasPrefix(iface.Name, "e") {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipnet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipnet.IP.To4()
			if ip4 == nil || ip4.IsLoopback() || ip4[0] == 100 {
				continue
			}
			return ip4.String()
		}
	}
	return ""
}

// tailscaleIPv4 returns the tailscale IPv4 via `tailscale ip -4`.
func tailscaleIPv4() string {
	out, err := exec.Command("tailscale", "ip", "-4").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// readBootstrapCode reads the one-time code from tmpfs.
func readBootstrapCode() string {
	data, err := os.ReadFile(bootstrapCodePath)
	if err != nil {
		return ""
	}
	code := strings.TrimSpace(string(data))
	if len(code) > 10 {
		code = code[:10]
	}
	return code
}
