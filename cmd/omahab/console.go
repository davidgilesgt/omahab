package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/skip2/go-qrcode"
	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"
	"golang.org/x/term"

	"github.com/omahab/omahab/internal/netenv"
	"github.com/omahab/omahab/internal/tui"
)
const bootstrapCodePath = "/run/omahab/bootstrap-code"
const bootstrapDonePath = "/var/lib/omahab/bootstrap-done"

// newConsoleCmd builds `omahab console`: the tty1 first-boot display.
// Clears the screen, shows the LAN bootstrap URL + one-time code + QR,
// refreshing every 5s; after bootstrap completes, shows a live status
// screen (hostname, Tailscale IP, doctor, backup, events, exposure).
func newConsoleCmd() *cobra.Command {
	var once bool
	cmd := &cobra.Command{
		Use:   "console",
		Short: "First-boot console display (tty1)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConsoleWithOptions(once)
		},
	}
	cmd.Flags().BoolVar(&once, "once", false, "render one frame and exit")
	return cmd
}

func runConsole() error {
	return runConsoleWithOptions(false)
}

func runConsoleWithOptions(once bool) error {
	var w io.Writer = os.Stdout
	isTTY := isTerminal(w)
	caps := tui.ResolveCaps(isTTY, os.Getenv("TERM"), os.Getenv("NO_COLOR"))
	// Only interactive console clears; --once, redirected, dumb, NO_COLOR emit plain text without escapes.
	// No forced-color-on-pipe: caps already respects isTTY/TERM/NO_COLOR.
	width := 0
	if isTTY {
		if f, ok := w.(*os.File); ok {
			if wi, _, err := term.GetSize(int(f.Fd())); err == nil {
				width = wi
			}
		} else if term.IsTerminal(int(os.Stdout.Fd())) {
			if wi, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
				width = wi
			}
		}
	}
	if width == 0 && term.IsTerminal(int(os.Stdout.Fd())) {
		if wi, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
			width = wi
		}
	}
	shouldClear := !once && isRealTTY(w) && caps.ColorEnabled
	for {
		if shouldClear {
			fmt.Fprint(w, "\033[2J\033[H")
		}
		// Banner: compact wordmark when narrow to avoid overflow.
		var banner string
		if width > 0 && width < 60 {
			// Compact plain wordmark, no border, no ANSI when narrow.
			banner = "  OMAHAB"
		} else {
			// Determine title for banner
			title := "first boot"
			if _, err := os.Stat(bootstrapDonePath); err == nil {
				title = "live status"
			}
			banner = tui.Banner(title, caps)
		}
		for _, line := range strings.Split(banner, "\n") {
			fmt.Fprintln(w, line)
		}
		fmt.Fprintln(w, "")

		snap := gatherConsoleSnapshot()
		renderConsoleSnapshot(w, caps, width, snap)

		fmt.Fprintln(w, "")
		if isRealTTY(w) {
			fmt.Fprintln(w, "  press Enter for shell login")
			fmt.Fprintln(w, "")
		}
		fmt.Fprintf(w, "  Refreshing every 5s — %s\n", time.Now().Format("15:04:05"))
		if once {
			return nil
		}
		if isRealTTY(w) {
			if waitForEnter(5 * time.Second) {
				if err := execLogin(); err != nil {
					fmt.Fprintf(w, "  login failed: %v\n", err)
					time.Sleep(2 * time.Second)
					continue
				}
				// On success, process is replaced; never returns.
			}
		} else {
			time.Sleep(5 * time.Second)
		}
	}
}

func isRealTTY(w io.Writer) bool {
	if term.IsTerminal(int(os.Stdout.Fd())) {
		return true
	}
	return isTerminal(w)
}

func execLogin() error {
	path := findLoginPath()
	if _, err := os.Stat(path); err != nil {
		return err
	}
	env := os.Environ()
	argv := []string{path}
	// Replace current process with login; preserves authenticated login via /run/wrappers/bin/login
	return syscall.Exec(path, argv, env)
}

func waitForEnter(timeout time.Duration) bool {
	pfd := []unix.PollFd{{Fd: int32(os.Stdin.Fd()), Events: unix.POLLIN}}
	ms := int(timeout.Milliseconds())
	n, err := unix.Poll(pfd, ms)
	if err != nil || n == 0 {
		return false
	}
	// Data available; consume the line. Use buffered reader to clear input.
	r := bufio.NewReader(os.Stdin)
	// Set a short read deadline if possible? Instead, just read with timeout via poll already ensured data.
	// ReadString may block if no newline, but Enter sends newline. If user typed partial without Enter, poll would have returned but ReadString would block. Mitigate by reading available bytes.
	// Use non-blocking read: try to read one byte peek, then drain.
	_, _ = r.ReadString('\n')
	return true
}

// consoleSnapshot holds bounded status for console and welcome rendering.
type consoleSnapshot struct {
	LANIP           string
	Hostname        string
	MDNSURL         string
	DashboardURL    string
	Code            string
	ServiceActive   string
	ServiceSub      string
	ServiceResult   string
	ServiceErr      error
	UpOK            bool
	UpErr           error
	BootstrapActive *bool
	BootstrapErr    error
}

func gatherConsoleSnapshot() consoleSnapshot {
	snap := consoleSnapshot{}
	snap.LANIP = netenv.LANIPv4()
	h, _ := os.Hostname()
	snap.Hostname = h
	short := h
	if idx := strings.Index(short, "."); idx != -1 {
		short = short[:idx]
	}
	if short == "" {
		short = "omahab"
	}
	snap.MDNSURL = fmt.Sprintf("http://%s.local:8484", short)
	if snap.LANIP != "" {
		snap.DashboardURL = fmt.Sprintf("http://%s:8484", snap.LANIP)
	} else {
		snap.DashboardURL = snap.MDNSURL
	}
	snap.Code = readBootstrapCode()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	active, sub, result, err := queryServiceStatus(ctx)
	cancel()
	snap.ServiceActive = active
	snap.ServiceSub = sub
	snap.ServiceResult = result
	snap.ServiceErr = err
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	upOK, upErr := probeUpWithContext(ctx2, "http://127.0.0.1:8484")
	cancel2()
	snap.UpOK = upOK
	snap.UpErr = upErr
	if snap.UpOK {
		ctx3, cancel3 := context.WithTimeout(context.Background(), time.Second)
		bActive, bErr := probeBootstrapStatusWithContext(ctx3, "http://127.0.0.1:8484")
		cancel3()
		snap.BootstrapActive = bActive
		snap.BootstrapErr = bErr
		if bActive == nil && bErr != nil {
			_, statErr := os.Stat(bootstrapDonePath)
			var active bool
			if statErr == nil {
				active = false
			} else if os.IsNotExist(statErr) {
				active = true
			} else {
				active = true
			}
			snap.BootstrapActive = &active
		}
	} else {
		_, statErr := os.Stat(bootstrapDonePath)
		var active bool
		if statErr == nil {
			active = false
		} else if os.IsNotExist(statErr) {
			active = true
		} else {
			active = true
		}
		snap.BootstrapActive = &active
	}
	return snap
}

func queryServiceStatus(ctx context.Context) (active, sub, result string, err error) {
	cmd := exec.CommandContext(ctx, "systemctl", "show", "omahabd", "-p", "ActiveState", "-p", "SubState", "-p", "Result")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", "", "", err
	}
	lines := strings.Split(string(out), "\n")
	for _, l := range lines {
		if strings.HasPrefix(l, "ActiveState=") {
			active = strings.TrimPrefix(l, "ActiveState=")
		} else if strings.HasPrefix(l, "SubState=") {
			sub = strings.TrimPrefix(l, "SubState=")
		} else if strings.HasPrefix(l, "Result=") {
			result = strings.TrimPrefix(l, "Result=")
		}
	}
	return active, sub, result, nil
}

func probeUpWithContext(ctx context.Context, base string) (bool, error) {
	url := strings.TrimRight(base, "/") + "/up"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, err
	}
	client := &http.Client{Timeout: time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, fmt.Errorf("up status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return false, err
	}
	var data map[string]any
	if err := json.Unmarshal(body, &data); err != nil {
		return false, fmt.Errorf("invalid up json: %w", err)
	}
	if st, ok := data["status"].(string); ok && st == "up" {
		return true, nil
	}
	return false, fmt.Errorf("up status not up: %s", string(body))
}

func probeBootstrapStatusWithContext(ctx context.Context, base string) (*bool, error) {
	url := strings.TrimRight(base, "/") + "/api/bootstrap/status"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("bootstrap status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if err != nil {
		return nil, err
	}
	var out struct {
		Active bool `json:"active"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return &out.Active, nil
}

func renderConsoleSnapshot(w io.Writer, caps tui.Caps, width int, snap consoleSnapshot) {
	isFailed := snap.ServiceActive == "failed" || snap.ServiceResult == "failed" || snap.ServiceResult == "failure" || (snap.ServiceErr != nil)
	if isFailed {
		fmt.Fprintln(w, "  Control panel could not start")
		fmt.Fprintln(w, "")
		fmt.Fprintln(w, "    Run systemctl status omahabd --no-pager")
		fmt.Fprintln(w, "    Run journalctl -u omahabd -b --no-pager")
		return
	}
	if snap.LANIP == "" {
		if consolePlacement() == "vps" {
			fmt.Fprintln(w, "  No LAN address (tailscale-only box)")
			fmt.Fprintln(w, "")
			fmt.Fprintln(w, "    SSH in and run: sudo tailscale up")
			fmt.Fprintln(w, "    Then open the dashboard from a device on the same tailnet.")
			return
		}
		fmt.Fprintln(w, "  Waiting for a network address")
		fmt.Fprintln(w, "")
		fmt.Fprintln(w, "    Run nmcli device status")
		fmt.Fprintln(w, "")
		fmt.Fprintln(w, "    Local dashboard (when ready):")
		fmt.Fprintf(w, "      %s\n", snap.MDNSURL)
		return
	}
	if snap.ServiceActive == "activating" || snap.ServiceActive == "reloading" || (!snap.UpOK && !isFailed) {
		if snap.UpErr != nil {
			fmt.Fprintln(w, "  Starting the control panel...")
			return
		}
	}
	if snap.BootstrapActive != nil && *snap.BootstrapActive {
		fmt.Fprintln(w, "  Finish setup")
		fmt.Fprintln(w, "")
		fmt.Fprintln(w, "    Open this URL on another device:")
		fmt.Fprintf(w, "      %s\n", snap.DashboardURL)
		fmt.Fprintf(w, "      also: %s (if mDNS is available)\n", snap.MDNSURL)
		if snap.Code != "" {
			fmt.Fprintln(w, "")
			fmt.Fprintln(w, "    One-time code (from this console):")
			if caps.ColorEnabled && width >= 60 {
				style := lipgloss.NewStyle().Foreground(tui.NeutralFG).Background(tui.NeutralBG).Padding(0, 2).Bold(true)
				rendered := style.Render(snap.Code)
				if rendered == snap.Code {
					rendered = "  " + snap.Code + "  "
				}
				fmt.Fprintf(w, "      %s\n", rendered)
			} else {
				fmt.Fprintf(w, "      %s\n", snap.Code)
			}
			if width >= 60 && caps.ColorEnabled {
				if qr, err := qrcode.New(snap.DashboardURL+"#code="+snap.Code, qrcode.Medium); err == nil {
					fmt.Fprintln(w, "")
					fmt.Fprint(w, qr.ToSmallString(false))
				}
			}
			fmt.Fprintln(w, "")
			fmt.Fprintln(w, "    After claiming, the token is at ~/.config/omahab/token (XDG-aware, 0600)")
		} else {
			fmt.Fprintln(w, "")
			fmt.Fprintln(w, "    One-time code:")
			fmt.Fprintln(w, "      Run sudo omahab console --once to view")
		}
		return
	}
	if snap.BootstrapActive != nil && !*snap.BootstrapActive {
		fmt.Fprintln(w, "  Control panel ready")
		fmt.Fprintln(w, "")
		fmt.Fprintln(w, "    Dashboard:")
		fmt.Fprintf(w, "      %s\n", snap.DashboardURL)
		fmt.Fprintf(w, "      also: %s\n", snap.MDNSURL)
		fmt.Fprintln(w, "")
		fmt.Fprintln(w, "    Run omahab status to check")
		return
	}
	if snap.UpOK {
		fmt.Fprintln(w, "  Control panel ready")
		fmt.Fprintln(w, "")
		fmt.Fprintf(w, "    %s\n", snap.DashboardURL)
		return
	}
	fmt.Fprintln(w, "  Starting the control panel...")
}

func findLoginPath() string {
	candidates := []string{"/bin/login", "/run/wrappers/bin/login", "/run/current-system/sw/bin/login"}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if p, err := exec.LookPath("login"); err == nil && filepath.IsAbs(p) {
		return p
	}
	return "/bin/login"
}

func renderFirstBoot(w io.Writer, caps tui.Caps, ip, code string) {
	if consolePlacement() == "vps" {
		renderFirstBootVPS(w, caps, code)
		return
	}
	fmt.Fprintln(w, "  Complete setup from any device on this network:")
	fmt.Fprintln(w, "")
	if ip != "" {
		url := fmt.Sprintf("http://%s:8484", ip)
		if caps.ColorEnabled {
			style := lipgloss.NewStyle().Foreground(tui.AccentAdaptive).Bold(true)
			rendered := style.Render(url)
			if rendered == url {
				rendered = "\x1b[1m" + url + "\x1b[0m"
			}
			fmt.Fprintf(w, "      %s\n", rendered)
		} else {
			fmt.Fprintf(w, "      %s\n", url)
		}
		// Always show mDNS alternative with actual hostname
		hostname, _ := os.Hostname()
		if hostname == "" {
			hostname = "omahab"
		}
		// Use short hostname before first dot for mDNS.
		if idx := strings.Index(hostname, "."); idx != -1 {
			hostname = hostname[:idx]
		}
		fmt.Fprintf(w, "      also: http://%s.local:8484\n", hostname)
		renderFirstBootCode(w, caps, ip, code)
	} else {
		fmt.Fprintln(w, "      (waiting for a network address)")
	}
}

// renderFirstBootVPS is the first-boot screen on tailscale-only boxes:
// there is no LAN URL, so it points at SSH + `tailscale up` + the
// tailnet dashboard instead of a "waiting for a network address" dead-end.
func renderFirstBootVPS(w io.Writer, caps tui.Caps, code string) {
	fmt.Fprintln(w, "  Complete setup from a device on your tailnet:")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "      1) SSH into this machine (or use your cloud console)")
	fmt.Fprintln(w, "      2) Run: sudo tailscale up")
	fmt.Fprintln(w, "      3) Open the dashboard from a device on the same tailnet")
	renderFirstBootCode(w, caps, "", code)
}

// renderFirstBootCode shows the one-time code (and QR when a LAN URL
// exists). With empty ip there is no URL to encode, so the QR is skipped.
func renderFirstBootCode(w io.Writer, caps tui.Caps, ip, code string) {
	if code == "" {
		fmt.Fprintln(w, "")
		fmt.Fprintln(w, "  (waiting for the one-time code — omahabd is starting)")
		return
	}
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "  One-time code:")
	if caps.ColorEnabled {
		codeStyle := lipgloss.NewStyle().Foreground(tui.NeutralFG).Background(tui.NeutralBG).Padding(0, 2).Bold(true)
		rendered := codeStyle.Render(code)
		if rendered == code {
			rendered = "\x1b[1m  " + code + "  \x1b[0m"
		}
		fmt.Fprintf(w, "      %s\n", rendered)
	} else {
		fmt.Fprintf(w, "      %s\n", code)
	}
	if ip != "" {
		if qr, err := qrcode.New("http://"+ip+":8484/#code="+code, qrcode.Medium); err == nil {
			fmt.Fprintln(w, "")
			fmt.Fprint(w, qr.ToSmallString(false))
		}
	}
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "  After Complete, CLI token at ~/.config/omahab/token (XDG-aware, 0600)")
}

func renderLiveStatus(w io.Writer, caps tui.Caps) {
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "omahab"
	}
	tsIP := tailscaleIPv4()
	lanIP := netenv.LANIPv4()
	// Dashboard URL: prefer Tailscale, then LAN IP / mDNS, never 127.0.0.1.
	// Reuses netenv.LANIPv4() per DISTRO-FIX-PLAN FIRST-BOOT surfaces.
	dashURL := ""
	shortHost := hostname
	if idx := strings.Index(shortHost, "."); idx != -1 {
		shortHost = shortHost[:idx]
	}
	mdnsURL := fmt.Sprintf("http://%s.local:8484", shortHost)
	if tsIP != "" {
		dashURL = fmt.Sprintf("http://%s:8484", tsIP)
	} else if lanIP != "" {
		dashURL = fmt.Sprintf("http://%s:8484 (also %s)", lanIP, mdnsURL)
	} else {
		dashURL = mdnsURL
	}
	if tsIP == "" {
		tsIP = "—"
	}

	labelStyle := lipgloss.NewStyle().Foreground(tui.NeutralFG)
	renderLabel := func(label, value string) {
		if caps.ColorEnabled {
			fmt.Fprintf(w, "  %s %s\n", labelStyle.Render(label), value)
		} else {
			fmt.Fprintf(w, "  %s %s\n", label, value)
		}
	}

	renderLabel("Host:      ", hostname)
	renderLabel("Tailscale: ", tsIP)
	renderLabel("Dashboard: ", dashURL)
	fmt.Fprintln(w, "")

	// Fetch live data (best-effort).
	token := readAdminToken()
	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()
	_ = ctx // used per fetch

	// Doctor summary.
	doctorSummary := fetchDoctorSummary(ctx, token, caps)
	renderLabel("Health:    ", doctorSummary)

	// Backup age.
	backupLine := fetchBackupLine(ctx, token)
	renderLabel("Backup:    ", backupLine)

	// Unread events.
	unreadLine := fetchUnreadEventsLine(ctx, token)
	renderLabel("Inbox:     ", unreadLine)

	// Exposed public count.
	exposureLine := fetchExposureLine(ctx, token)
	renderLabel("Exposure:  ", exposureLine)

	// Update line.
	updateLine := fetchUpdateLine()
	renderLabel("Update:    ", updateLine)
}

func fetchUpdateLine() string {
	data, err := os.ReadFile(updateAvailablePath)
	if err != nil {
		return "up to date"
	}
	v := strings.TrimSpace(string(data))
	if v == "" {
		return "up to date"
	}
	// Ensure leading v.
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	return tui.WarnChip.Render(" UPDATE ") + " " + v + " — sudo omahab system upgrade"
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
	adminUser := consoleAdminUser()
	candidates = append(candidates, filepath.Join("/home", adminUser, ".config", "omahab", "token"))
	candidates = append(candidates, "/root/.config/omahab/token")
	if u, err := user.Lookup(adminUser); err == nil && u.HomeDir != "" {
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

func consoleAdminUser() string {
	if v := strings.TrimSpace(os.Getenv("OMAHAB_ADMIN_USER")); v != "" {
		return v
	}
	return "omahab"
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
			return "—  (sign in on the dashboard to see live health)"
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
				return "—  (sign in on the dashboard to see live health)"
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
				return "—  (sign in on the dashboard to see live health)"
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
				return "—  (sign in on the dashboard to see live health)"
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

// consolePlacement reports where this machine serves the dashboard.
// Mirrors services.omahab.placement, exported to the console environment
// as OMAHAB_PLACEMENT. LAN stays open by default; vps means
// tailscale-only with no LAN URL on first boot.
func consolePlacement() string {
	if strings.TrimSpace(os.Getenv("OMAHAB_PLACEMENT")) == "vps" {
		return "vps"
	}
	return "lan"
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
