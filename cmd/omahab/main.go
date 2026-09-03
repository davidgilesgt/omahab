package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/charmbracelet/x/term"

	"github.com/omahab/omahab/internal/apiclient"
	"github.com/omahab/omahab/internal/apps"
	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/tui"
)

var (
	version = "0.1.0"

	flagServer         string
	flagJSON           bool
	flagNonInteractive bool
	flagTimeout        time.Duration
	flagForce          bool
	flagYes            bool
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		if _, ok := err.(*printedError); ok {
			os.Exit(1)
		}
		// Fallback for errors not via handleFailure (flag parse, PersistentPreRunE, validation)
		_ = handleFailure(err)
		os.Exit(1)
	}
}

type printedError struct{ err error }

func (e *printedError) Error() string { return e.err.Error() }
func (e *printedError) Unwrap() error { return e.err }

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "omahab",
		Short: "The opinionated home server.",
		Long: `Omahab CLI — control plane for the Omahab home server.

Every operation has a JSON equivalent via --json.
Credentials are never passed as arguments; set OMAHAB_TOKEN or use the credential store.
Use --server to target a different control plane (env OMAHAB_SERVER, then ~/.config/omahab/client.json).`,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			if flagTimeout < time.Second || flagTimeout > 5*time.Minute {
				return fmt.Errorf("--timeout must be between 1s and 5m")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			// Bare `omahab` with no subcommand: welcome card. Help flag is handled by cobra before RunE.
			if len(args) == 0 {
				printWelcomeCard()
				return nil
			}
			return cmd.Help()
		},
	}
	// Persistent flags (structured output, server selection, interactivity, timeouts)
	root.PersistentFlags().StringVar(&flagServer, "server", "", "control plane URL (env OMAHAB_SERVER, then ~/.config/omahab/client.json, default http://127.0.0.1:8484)")
	root.PersistentFlags().BoolVar(&flagJSON, "json", false, "output JSON (structured, stable for agents)")
	root.PersistentFlags().BoolVar(&flagNonInteractive, "non-interactive", false, "disable prompts; destructive/public operations require --force")
	root.PersistentFlags().DurationVar(&flagTimeout, "timeout", 30*time.Second, "per-request timeout")
	// NO_COLOR is env-driven; no flag needed but we respect it
	// Also support --force as persistent for convenience (per-command will also define)
	root.PersistentFlags().BoolVar(&flagForce, "force", false, "force destructive or public operations without confirmation (required in --non-interactive)")
	// Also --yes alias for force (common UX)
	root.PersistentFlags().BoolVarP(&flagYes, "yes", "y", false, "alias for --force")
	// Core commands
	root.AddCommand(newLoginCmd())
	root.AddCommand(newStatusCmd())
	root.AddCommand(newUpCmd())
	root.AddCommand(newDoctorCmd())
	root.AddCommand(newAppCmd())
	root.AddCommand(newProjectCmd())
	root.AddCommand(newExposureCmd())
	root.AddCommand(newBackupCmd())
	root.AddCommand(newBackupDriveCmd())
	root.AddCommand(newEventCmd())
	root.AddCommand(newRunnerCmd())
	root.AddCommand(newConsoleCmd())
	root.AddCommand(newSetupCmd())
	root.AddCommand(newSystemCmd())
	root.AddCommand(newWorkspaceCmd()) // alias
	root.AddCommand(newIdentityCmd())
	root.AddCommand(newRecoveryCmd())
	root.AddCommand(newHermesCmd())
	root.AddCommand(newSecretsCmd())
	root.AddCommand(newProviderCmd())
	root.AddCommand(newEnvCmd())
	root.AddCommand(newVersionCmd())
	root.AddCommand(newCatalogCmd())
	// Completion
	root.AddCommand(newCompletionCmd())
	// Host/admin: expose doctor already, status, identity recover
	// Ensure help is discoverable
	root.InitDefaultHelpFlag()
	return root
}

// ---------- helpers ----------

func newContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), flagTimeout)
}

func isNonInteractive() bool {
	if flagNonInteractive {
		return true
	}
	if os.Getenv("TERM") == "dumb" {
		return true
	}
	if fi, err := os.Stdin.Stat(); err == nil {
		if (fi.Mode() & os.ModeCharDevice) == 0 {
		}
	}
	return false
}
func needForceGuard(isDestructiveOrPublic bool) error {
	if isDestructiveOrPublic && isNonInteractive() && !flagForce && !flagYes {
		return errors.New("refusing destructive/public operation without --force in --non-interactive mode")
	}
	return nil
}

func cobraFlagBool(name string) (bool, bool) { return false, false }

func tokenFromClientJSON() string {
	p, err := apiclient.DefaultClientConfigPath()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return ""
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		return ""
	}
	if tok, ok := raw["token"].(string); ok {
		return strings.TrimSpace(tok)
	}
	return ""
}

func resolveClient() (*apiclient.Client, error) {
	cfg, err := apiclient.LoadClientConfig("")
	if err != nil {
		return nil, fmt.Errorf("load client config: %w", err)
	}
	server := apiclient.ResolveServer(flagServer, cfg)
	envTok := strings.TrimSpace(os.Getenv("OMAHAB_TOKEN"))
	if envTok != "" {
		c := apiclient.New(server, envTok)
		c.HTTPClient.Timeout = flagTimeout
		return c, nil
	}
	if tok := tokenFromClientJSON(); tok != "" {
		c := apiclient.New(server, tok)
		c.HTTPClient.Timeout = flagTimeout
		return c, nil
	}
	store := apiclient.CompositeCredentialStore{
		Stores: []apiclient.CredentialStore{
			apiclient.EnvCredentialStore{},
			apiclient.FileCredentialStore{},
		},
	}
	token, err := apiclient.ResolveToken(store)
	if err != nil {
		return nil, err
	}
	c := apiclient.New(server, token)
	c.HTTPClient.Timeout = flagTimeout
	return c, nil
}

func clientd() *apiclient.ClientdClient {
	return apiclient.NewClientdClient("")
}

func isColorEnabled() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	return true
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func printErrorJSON(err error) {
	code := "internal"
	msg := err.Error()
	status := 0
	if apiErr, ok := err.(*apiclient.APIError); ok {
		code = apiErr.Code
		if code == "" {
			code = fmt.Sprintf("http_%d", apiErr.StatusCode)
		}
		msg = apiErr.Message
		if msg == "" {
			msg = apiErr.Error()
		}
		status = apiErr.StatusCode
	}
	env := map[string]any{
		"error": map[string]any{
			"code":    code,
			"message": msg,
		},
	}
	if status != 0 {
		env["error"].(map[string]any)["status"] = status
	}
	_ = printJSON(env)
}

func isTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	if os.IsTimeout(err) {
		return true
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline exceeded") || strings.Contains(msg, "context deadline") {
		return true
	}
	return false
}

func hintForError(err error) string {
	if err == nil {
		return ""
	}
	if apiErr, ok := err.(*apiclient.APIError); ok {
		switch apiErr.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return "hint: authentication failed (401) — set OMAHAB_TOKEN or run `omahab login`"
		case http.StatusNotFound:
			return "hint: not found (404) — run `omahab <resource> list` to see available resources"
		}
	}
	if isTimeoutError(err) {
		return "hint: request timed out — check --server / OMAHAB_SERVER and Tailscale connectivity"
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "connection refused") || strings.Contains(msg, "no such host") || strings.Contains(msg, "dial tcp") || strings.Contains(msg, "connect:") || strings.Contains(msg, "network is unreachable") {
		return "hint: check --server / OMAHAB_SERVER and Tailscale connectivity"
	}
	if strings.Contains(msg, "401") || strings.Contains(msg, "unauthorized") || strings.Contains(msg, "unauthenticated") {
		return "hint: set OMAHAB_TOKEN or run `omahab login`"
	}
	if strings.Contains(msg, "404") || strings.Contains(msg, "not found") {
		return "hint: run `omahab <resource> list`"
	}
	return ""
}

func handleFailure(err error) error {
	if err == nil {
		return nil
	}
	hint := hintForError(err)
	if flagJSON {
		printErrorJSON(err)
		if hint != "" {
			fmt.Fprintln(os.Stderr, hint)
		}
		return &printedError{err: err}
	}
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
	if hint != "" {
		fmt.Fprintln(os.Stderr, hint)
	}
	return &printedError{err: err}
}

func readTokenHidden() (string, error) {
	if term.IsTerminal(os.Stdin.Fd()) {
		b, err := term.ReadPassword(os.Stdin.Fd())
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func printWelcomeCard() {
	cfg, _ := apiclient.LoadClientConfig("")
	server := apiclient.ResolveServer(flagServer, cfg)
	serverSource := "default"
	if strings.TrimSpace(flagServer) != "" {
		serverSource = "--server flag"
	} else if strings.TrimSpace(os.Getenv("OMAHAB_SERVER")) != "" {
		serverSource = "OMAHAB_SERVER"
	} else if strings.TrimSpace(cfg.Server) != "" {
		serverSource = "~/.config/omahab/client.json"
	}
	tokenStatus := "not set"
	hint := "export OMAHAB_TOKEN or run `omahab login`"
	if tok := strings.TrimSpace(os.Getenv("OMAHAB_TOKEN")); tok != "" {
		tokenStatus = "set (via OMAHAB_TOKEN)"
		hint = ""
	} else if tok := tokenFromClientJSON(); tok != "" {
		tokenStatus = "set (via ~/.config/omahab/client.json)"
		hint = ""
	} else if tok, _ := (apiclient.FileCredentialStore{}).Token(); strings.TrimSpace(tok) != "" {
		tokenStatus = "set (via credentials file)"
		hint = ""
	}
	if flagJSON {
		_ = printJSON(map[string]any{
			"server":       server,
			"serverSource": serverSource,
			"token":        tokenStatus,
			"hint":         hint,
		})
		return
	}
	fmt.Println("Omahab — the opinionated home server.")
	fmt.Println()
	fmt.Printf("  server: %s (%s)\n", server, serverSource)
	if hint != "" {
		fmt.Printf("  token:  %s → %s\n", tokenStatus, hint)
	} else {
		fmt.Printf("  token:  %s\n", tokenStatus)
	}
	fmt.Println()
	fmt.Println("Run `omahab --help` for commands.")
	if tokenStatus == "not set" {
		fmt.Println("First run?  `omahab login [--server <url>]` to authenticate.")
	} else {
		fmt.Println("Try `omahab status` to check the control plane.")
	}
}

func newLoginCmd() *cobra.Command {
	var loginServer string
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Authenticate with the control plane",
		Long: `Prompt for a bearer token (input hidden), verify it, and save to ~/.config/omahab/client.json.

The token is stored with 0600 permissions. OMAHAB_TOKEN env var takes precedence at runtime.
Use --server to set the control plane URL.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			server := strings.TrimSpace(loginServer)
			if server == "" {
				server = strings.TrimSpace(flagServer)
			}
			if server == "" {
				cfg, _ := apiclient.LoadClientConfig("")
				server = apiclient.ResolveServer("", cfg)
			}
			if server == "" {
				server = "http://127.0.0.1:8484"
			}
			fmt.Fprint(os.Stderr, "Token: ")
			tok, err := readTokenHidden()
			if err != nil {
				return handleFailure(fmt.Errorf("read token: %w", err))
			}
			tok = strings.TrimSpace(tok)
			if tok == "" {
				return handleFailure(errors.New("token is required"))
			}
			fmt.Fprintln(os.Stderr, "Verifying…")
			c := apiclient.New(server, tok)
			c.HTTPClient.Timeout = flagTimeout
			ctx, cancel := newContext()
			defer cancel()
			if _, err := c.Status(ctx); err != nil {
				return handleFailure(fmt.Errorf("verify token: %w", err))
			}
			path, err := apiclient.DefaultClientConfigPath()
			if err != nil {
				return handleFailure(err)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return handleFailure(err)
			}
			data := map[string]string{
				"server": server,
				"token":  tok,
			}
			b, _ := json.MarshalIndent(data, "", "  ")
			if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
				return handleFailure(err)
			}
			if flagJSON {
				return printJSON(map[string]string{"server": server, "saved": path})
			}
			fmt.Printf("Saved to %s\n", path)
			fmt.Println("Token verified and saved.")
			return nil
		},
	}
	cmd.Flags().StringVar(&loginServer, "server", "", "control plane URL (default http://127.0.0.1:8484)")
	return cmd
}

func confirmPrompt(msg string) bool {
	if flagNonInteractive {
		return flagForce
	}
	fmt.Fprintf(os.Stderr, "%s [y/N]: ", msg)
	// Check if stdin is a terminal; if not, require --force
	fi, err := os.Stdin.Stat()
	if err == nil && (fi.Mode()&os.ModeCharDevice) == 0 {
		// piped stdin: require force
		return flagForce
	}
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil && err != io.EOF {
		return false
	}
	line = strings.TrimSpace(strings.ToLower(line))
	return line == "y" || line == "yes"
}

// human helpers

func humanStatus(s *domain.Status) {
	if flagJSON {
		_ = printJSON(s)
		return
	}
	// Reuse StepBar compact strip for TUI layer — preserves data content, adds styled glyph.
	caps := tui.ResolveCaps(term.IsTerminal(os.Stdout.Fd()), os.Getenv("TERM"), os.Getenv("NO_COLOR"))
	if caps.IsTTY && caps.ColorEnabled {
		strip := tui.CompactStatusStrip(string(s.Health), caps)
		fmt.Println(strip)
	}
	fmt.Printf("instance: %s\n", s.InstanceID)
	fmt.Printf("version:  %s\n", s.Version)
	fmt.Printf("health:   %s\n", s.Health)
	fmt.Printf("uptime:   %s (since %s)\n", time.Since(s.StartedAt).Truncate(time.Second), s.StartedAt.Format(time.RFC3339))
	fmt.Printf("now:      %s\n", s.Now.Format(time.RFC3339))
}

func humanListApps(apps []domain.Application) {
	if flagJSON {
		_ = printJSON(map[string]any{"items": apps})
		return
	}
	if len(apps) == 0 {
		fmt.Println("no applications")
		return
	}
	fmt.Printf("%-20s %-12s %-10s %-30s %s\n", "ID", "NAME", "HEALTH", "HOSTNAME", "EXPOSURE")
	for _, a := range apps {
		fmt.Printf("%-20s %-12s %-10s %-30s %s\n", a.ID, a.Name, a.Health, a.Hostname, a.Exposure)
	}
}

func humanListProjects(projs []domain.Project) {
	if flagJSON {
		_ = printJSON(map[string]any{"items": projs})
		return
	}
	if len(projs) == 0 {
		fmt.Println("no projects")
		return
	}
	fmt.Printf("%-20s %-15s %-30s %-12s %s\n", "ID", "SLUG", "HOSTNAME", "EXPOSURE", "NAME")
	for _, p := range projs {
		fmt.Printf("%-20s %-15s %-30s %-12s %s\n", p.ID, p.Slug, p.Hostname, p.Exposure, p.Name)
	}
}

func humanListBackups(items []domain.Backup) {
	if flagJSON {
		_ = printJSON(map[string]any{"items": items})
		return
	}
	if len(items) == 0 {
		fmt.Println("no backups")
		return
	}
	fmt.Printf("%-20s %-12s %-20s %-10s %s\n", "ID", "REPO", "STARTED", "STATUS", "ERROR")
	for _, b := range items {
		started := b.StartedAt.Format("2006-01-02 15:04")
		errStr := b.Error
		if len(errStr) > 30 {
			errStr = errStr[:30] + "…"
		}
		fmt.Printf("%-20s %-12s %-20s %-10s %s\n", b.ID, truncate(b.Repository, 12), started, b.Status, errStr)
	}
}

func humanListEvents(events []domain.Event) {
	if flagJSON {
		_ = printJSON(map[string]any{"items": events})
		return
	}
	if len(events) == 0 {
		fmt.Println("no events")
		return
	}
	for _, e := range events {
		read := " "
		if e.ReadAt != nil {
			read = "✓"
		}
		fmt.Printf("[%s] %s %-20s %s %s\n", read, e.CreatedAt.Format("2006-01-02 15:04"), e.Type, e.Severity, e.Message)
	}
}

func humanListSync(folders []domain.SyncFolder) {
	if flagJSON {
		_ = printJSON(map[string]any{"items": folders})
		return
	}
	if len(folders) == 0 {
		fmt.Println("no sync folders")
		return
	}
	fmt.Printf("%-20s %-15s %-30s %-5s %s\n", "ID", "NAME", "SERVER_PATH", "AI", "HEALTH")
	for _, s := range folders {
		ai := "no"
		if s.ShareWithAI {
			ai = "yes"
		}
		fmt.Printf("%-20s %-15s %-30s %-5s %s\n", s.ID, s.Name, truncate(s.ServerPath, 30), ai, s.Health)
	}
}

func humanListWorkspaces(ws []domain.Workspace) {
	if flagJSON {
		_ = printJSON(map[string]any{"items": ws})
		return
	}
	if len(ws) == 0 {
		fmt.Println("no workspaces/runners")
		return
	}
	fmt.Printf("%-20s %-20s %-10s %-12s %s\n", "ID", "PROJECT", "AGENT", "STATUS", "BRANCH")
	for _, w := range ws {
		fmt.Printf("%-20s %-20s %-10s %-12s %s\n", w.ID, w.ProjectID, w.Agent, w.Status, w.Branch)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

// ---------- commands ----------

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show control-plane status",
		Long:  "Display instance, version, health, and uptime from /api/v1/status.",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := newContext()
			defer cancel()
			c, err := resolveClient()
			if err != nil {
			return handleFailure(err)
		}
			st, err := c.Status(ctx)
			if err != nil {
			return handleFailure(err)
		}
			humanStatus(st)
			return nil
		},
	}
}

func newUpCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "up",
		Short: "Check liveness without authentication",
		Long:  "GET /up — no Bearer token required (root, not /api/v1). Useful for probes and scripts.",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := newContext()
			defer cancel()
			c, err := resolveClient()
			if err != nil {
			return handleFailure(err)
		}
			// Force no token for up? client already handles stripping auth for /up path
			out, err := c.Up(ctx)
			if err != nil {
			return handleFailure(err)
		}
			if flagJSON {
				return printJSON(out)
			}
			fmt.Println("ok")
			// print extra fields concisely if present
			if len(out) > 0 {
				for k, v := range out {
					fmt.Printf("%s: %v\n", k, v)
				}
			}
			return nil
		},
	}
}

func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Run host and service diagnostics",
		Long:  "Runs preflight/health checks via /api/v1/doctor and reports actionable diagnostics.",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := newContext()
			defer cancel()
			c, err := resolveClient()
			if err != nil {
			return handleFailure(err)
		}
			res, err := c.Doctor(ctx)
			if err != nil {
			return handleFailure(err)
		}
			if flagJSON {
				return printJSON(res)
			}
			// Preserve data content: overall healthy flag plus checklist rendering via tui.
			caps := tui.ResolveCaps(term.IsTerminal(os.Stdout.Fd()), os.Getenv("TERM"), os.Getenv("NO_COLOR"))
			if caps.IsTTY && caps.ColorEnabled {
				// Use tui checklist component — same logic as PreflightChecklist but for health.
				var views []tui.DoctorCheckView
				for _, ch := range res.Checks {
					views = append(views, tui.DoctorCheckView{Name: ch.Name, Status: ch.Status, Message: ch.Message, Detail: ch.Detail})
				}
				// Overall status as header
				if res.Healthy {
					fmt.Println(tui.PassChip.Render(" healthy "))
				} else {
					fmt.Println(tui.FailChip.Render(" unhealthy "))
				}
				fmt.Print(tui.RenderDoctorChecklist(views, caps))
				return nil
			}
			// Fallback plain rendering (byte-stable, NO_COLOR/TERM=dumb)
			if res.Healthy {
				if isColorEnabled() {
					fmt.Println("\x1b[32mhealthy\x1b[0m")
				} else {
					fmt.Println("healthy")
				}
			} else {
				if isColorEnabled() {
					fmt.Println("\x1b[31munhealthy\x1b[0m")
				} else {
					fmt.Println("unhealthy")
				}
			}
			for _, ch := range res.Checks {
				status := ch.Status
				if isColorEnabled() {
					switch status {
					case "healthy", "ok", "pass":
						status = "\x1b[32m" + status + "\x1b[0m"
					case "degraded", "warn":
						status = "\x1b[33m" + status + "\x1b[0m"
					case "unhealthy", "fail", "error":
						status = "\x1b[31m" + status + "\x1b[0m"
					}
				}
				line := fmt.Sprintf("  %-25s %s", ch.Name, status)
				if ch.Message != "" {
					line += " — " + ch.Message
				}
				fmt.Println(line)
				if ch.Detail != "" {
					fmt.Printf("    %s\n", ch.Detail)
				}
			}
			return nil
		},
	}
}

func newAppCmd() *cobra.Command {
	app := &cobra.Command{
		Use:     "app",
		Short:   "Manage platform applications",
		Aliases: []string{"apps", "application"},
	}
	app.AddCommand(&cobra.Command{
		Use:     "list",
		Short:   "List applications",
		Aliases: []string{"ls"},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := newContext()
			defer cancel()
			c, err := resolveClient()
			if err != nil {
			return handleFailure(err)
		}
			items, err := c.ListApplications(ctx)
			if err != nil {
			return handleFailure(err)
		}
			humanListApps(items)
			return nil
		},
	})
	app.AddCommand(&cobra.Command{
		Use:   "show <id|name>",
		Short: "Show application details",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := newContext()
			defer cancel()
			c, err := resolveClient()
			if err != nil {
			return handleFailure(err)
		}
			a, err := c.GetApplication(ctx, args[0])
			if err != nil {
			return handleFailure(err)
		}
			if flagJSON {
				return printJSON(a)
			}
			fmt.Printf("id:            %s\n", a.ID)
			fmt.Printf("name:          %s\n", a.Name)
			fmt.Printf("image:         %s\n", a.Image)
			fmt.Printf("digest:        %s\n", a.Digest)
			fmt.Printf("hostname:      %s\n", a.Hostname)
			fmt.Printf("exposure:      %s\n", a.Exposure)
			fmt.Printf("health:        %s\n", a.Health)
			fmt.Printf("desired:       %s\n", a.DesiredState)
			fmt.Printf("observed:      %s\n", a.ObservedState)
			fmt.Printf("updated:       %s\n", a.UpdatedAt.Format(time.RFC3339))
			if a.InstalledAt != nil {
				fmt.Printf("installed:     %s\n", a.InstalledAt.Format(time.RFC3339))
			}
			return nil
		},
	})
	app.AddCommand(&cobra.Command{
		Use:   "restart <id>",
		Short: "Restart an application",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := needForceGuard(false); err != nil {
			return handleFailure(err)
		}
			ctx, cancel := newContext()
			defer cancel()
			c, err := resolveClient()
			if err != nil {
			return handleFailure(err)
		}
			res, err := c.RestartApplication(ctx, args[0])
			if err != nil {
			return handleFailure(err)
		}
			if flagJSON {
				return printJSON(res)
			}
			fmt.Printf("restart %s: %s\n", res.ID, res.Status)
			if res.Message != "" {
				fmt.Println(res.Message)
			}
			return nil
		},
	})
	return app
}

func newProjectCmd() *cobra.Command {
	proj := &cobra.Command{
		Use:     "project",
		Short:   "Manage projects (one repo = one project)",
		Aliases: []string{"projects"},
	}
	proj.AddCommand(&cobra.Command{
		Use:     "list",
		Short:   "List projects",
		Aliases: []string{"ls"},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := newContext()
			defer cancel()
			c, err := resolveClient()
			if err != nil {
			return handleFailure(err)
		}
			items, err := c.ListProjects(ctx)
			if err != nil {
			return handleFailure(err)
		}
			humanListProjects(items)
			return nil
		},
	})
	// create
	var createName, createSlug string
	createCmd := &cobra.Command{
		Use:   "create",
		Short: "Create a project",
		Long:  "Creates a Forgejo repository, Hermes bot, and ONCE deployment slot.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(createName) == "" {
				return errors.New("--name is required")
			}
			ctx, cancel := newContext()
			defer cancel()
			c, err := resolveClient()
			if err != nil {
			return handleFailure(err)
		}
			p, err := c.CreateProject(ctx, apiclient.CreateProjectRequest{Name: createName, Slug: createSlug})
			if err != nil {
			return handleFailure(err)
		}
			if flagJSON {
				return printJSON(p)
			}
			fmt.Printf("created project %s (%s)\n", p.Slug, p.ID)
			fmt.Printf("  repo: %s\n", p.RepositoryURL)
			return nil
		},
	}
	createCmd.Flags().StringVar(&createName, "name", "", "project display name (required)")
	createCmd.Flags().StringVar(&createSlug, "slug", "", "slug (derived from name if omitted)")
	proj.AddCommand(createCmd)

	proj.AddCommand(&cobra.Command{
		Use:   "show <id|slug>",
		Short: "Show project",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := newContext()
			defer cancel()
			c, err := resolveClient()
			if err != nil {
			return handleFailure(err)
		}
			p, err := c.GetProject(ctx, args[0])
			if err != nil {
			return handleFailure(err)
		}
			if flagJSON {
				return printJSON(p)
			}
			fmt.Printf("id:        %s\n", p.ID)
			fmt.Printf("slug:      %s\n", p.Slug)
			fmt.Printf("name:      %s\n", p.Name)
			fmt.Printf("repo:      %s\n", p.RepositoryURL)
			fmt.Printf("hostname:  %s\n", p.Hostname)
			fmt.Printf("exposure:  %s\n", p.Exposure)
			fmt.Printf("created:   %s\n", p.CreatedAt.Format(time.RFC3339))
			return nil
		},
	})
	// rm
	rmCmd := &cobra.Command{
		Use:     "rm <id|slug>",
		Short:   "Delete a project",
		Aliases: []string{"delete", "remove"},
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			force, _ := cmd.Flags().GetBool("force")
			if isNonInteractive() && !force && !flagForce {
				err := errors.New("refusing to delete project without --force in --non-interactive mode")
				return handleFailure(err)
}
			if !force && !flagForce && !flagJSON {
				if !confirmPrompt(fmt.Sprintf("delete project %q?", args[0])) {
					fmt.Println("aborted")
					return nil
				}
			}
			ctx, cancel := newContext()
			defer cancel()
			c, err := resolveClient()
			if err != nil {
			return handleFailure(err)
		}
			if err := c.DeleteProject(ctx, args[0]); err != nil {
				return handleFailure(err)
}
			if flagJSON {
				return printJSON(map[string]string{"deleted": args[0]})
			}
			fmt.Printf("deleted %s\n", args[0])
			return nil
		},
	}
	rmCmd.Flags().Bool("force", false, "force without confirmation")
	proj.AddCommand(rmCmd)

	// releases subcommands under project
	releases := &cobra.Command{
		Use:   "releases <project-id>",
		Short: "List releases for a project",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := newContext()
			defer cancel()
			c, err := resolveClient()
			if err != nil {
			return handleFailure(err)
		}
			rs, err := c.ListReleases(ctx, args[0])
			if err != nil {
			return handleFailure(err)
		}
			if flagJSON {
				return printJSON(map[string]any{"items": rs})
			}
			if len(rs) == 0 {
				fmt.Println("no releases")
				return nil
			}
			for _, r := range rs {
				active := ""
				if r.Active {
					active = " (active)"
				}
				fmt.Printf("%s %s %s%s status=%s\n", r.ID, r.Commit[:min(8, len(r.Commit))], r.Digest[:min(12, len(r.Digest))], active, r.Status)
			}
			return nil
		},
	}
	proj.AddCommand(releases)

	// deploy: create release
	var deployCommit, deployDigest string
	deployCmd := &cobra.Command{
		Use:   "deploy <project-id>",
		Short: "Create a release (deploy digest)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if deployDigest == "" {
				return errors.New("--digest is required")
			}
			ctx, cancel := newContext()
			defer cancel()
			c, err := resolveClient()
			if err != nil {
			return handleFailure(err)
		}
			rel, err := c.CreateRelease(ctx, args[0], apiclient.CreateReleaseRequest{Commit: deployCommit, Digest: deployDigest})
			if err != nil {
			return handleFailure(err)
		}
			if flagJSON {
				return printJSON(rel)
			}
			fmt.Printf("deployed %s digest %s\n", rel.ID, rel.Digest)
			return nil
		},
	}
	deployCmd.Flags().StringVar(&deployCommit, "commit", "", "git commit sha")
	deployCmd.Flags().StringVar(&deployDigest, "digest", "", "OCI image digest (required)")
	proj.AddCommand(deployCmd)

	// rollback
	proj.AddCommand(&cobra.Command{
		Use:   "rollback <project-id> <release-id>",
		Short: "Rollback to a previous release",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			force, _ := cmd.Flags().GetBool("force")
			if isNonInteractive() && !force && !flagForce {
				err := errors.New("refusing rollback without --force in --non-interactive mode")
				return handleFailure(err)
}
			ctx, cancel := newContext()
			defer cancel()
			c, err := resolveClient()
			if err != nil {
			return handleFailure(err)
		}
			rel, err := c.RollbackRelease(ctx, args[0], args[1])
			if err != nil {
			return handleFailure(err)
		}
			if flagJSON {
				return printJSON(rel)
			}
			fmt.Printf("rolled back to %s\n", rel.ID)
			return nil
		},
	})

	// clone via clientd
	var cloneDir string
	cloneCmd := &cobra.Command{
		Use:   "clone <id|slug>",
		Short: "Clone a project locally (via omahab-clientd)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := newContext()
			defer cancel()
			c, err := resolveClient()
			if err != nil {
			return handleFailure(err)
		}
			p, err := c.GetProject(ctx, args[0])
			if err != nil {
			return handleFailure(err)
		}
			// Prefer clientd if available
			cd := clientd()
			if cd.Available(ctx) {
				dest := cloneDir
				if dest == "" {
					// Suggest ~/projects/<slug>
					home, _ := os.UserHomeDir()
					dest = filepath.Join(home, "projects", p.Slug)
				}
				if err := cd.Call(ctx, "project.clone", map[string]any{"slug": p.Slug, "dir": dest}, nil); err != nil {
					return handleFailure(err)
				}
				if flagJSON {
					return printJSON(map[string]string{"cloned": string(p.ID), "path": dest})
				}
				fmt.Printf("cloned %s to %s\n", p.Slug, dest)
				return nil
			}
			// Fallback: print URL
			if flagJSON {
				return printJSON(p)
			}
			fmt.Printf("repo: %s\n", p.RepositoryURL)
			fmt.Println("clientd not available; run: git clone", p.RepositoryURL)
			return nil
		},
	}
	cloneCmd.Flags().StringVar(&cloneDir, "dir", "", "destination directory (default ~/projects/<slug>)")
	proj.AddCommand(cloneCmd)

	proj.AddCommand(&cobra.Command{
		Use:   "open <id|slug>",
		Short: "Open project in editor (via clientd)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := newContext()
			defer cancel()
			c, err := resolveClient()
			if err != nil {
			return handleFailure(err)
		}
			p, err := c.GetProject(ctx, args[0])
			if err != nil {
			return handleFailure(err)
		}
			cd := clientd()
			if cd.Available(ctx) {
				if err := cd.Call(ctx, "project.open", map[string]any{"slug": p.Slug}, nil); err != nil {
					return handleFailure(err)
				}
				if flagJSON {
					return printJSON(map[string]string{"opened": string(p.ID)})
				}
				fmt.Printf("opened %s\n", p.Slug)
				return nil
			}
			if flagJSON {
				return printJSON(p)
			}
			fmt.Printf("clientd not available; project %s at %s\n", p.Slug, p.RepositoryURL)
			return nil
		},
	})

	return proj
}

func newExposureCmd() *cobra.Command {
	exp := &cobra.Command{
		Use:   "exposure",
		Short: "Inspect and change exposure (private/shared/public)",
		Long: `Exposure controls whether a service is Tailscale-only (private),
Cloudflare Tunnel with identity gate (shared), or public.

Changing to shared/public is a public-route change; inspectable and reversible.`,
	}
	exp.AddCommand(&cobra.Command{
		Use:   "get",
		Short: "Show exposure",
		RunE: func(cmd *cobra.Command, args []string) error {
			target, _ := cmd.Flags().GetString("target")
			appID, _ := cmd.Flags().GetString("app")
			projID, _ := cmd.Flags().GetString("project")
			if appID != "" {
				target = "app:" + appID
			}
			if projID != "" {
				target = "project:" + projID
			}
			ctx, cancel := newContext()
			defer cancel()
			c, err := resolveClient()
			if err != nil {
			return handleFailure(err)
		}
			// Dispatch to typed helpers when possible
			var out any
			var reqErr error
			if appID != "" {
				out, reqErr = c.GetAppExposure(ctx, appID)
			} else if projID != "" {
				out, reqErr = c.GetProjectExposure(ctx, projID)
			} else {
				out, reqErr = c.GetExposure(ctx, target)
			}
			if reqErr != nil {
			return handleFailure(reqErr)
		}
			if flagJSON {
				return printJSON(out)
			}
			// Human: best effort
			switch v := out.(type) {
			case *apiclient.ExposureResponse:
				fmt.Printf("target:   %s\n", v.Target)
				fmt.Printf("exposure: %s\n", v.Exposure)
				fmt.Printf("hostname: %s\n", v.Hostname)
				if v.Message != "" {
					fmt.Printf("info:     %s\n", v.Message)
				}
			default:
				_ = printJSON(out)
			}
			return nil
		},
	})
	// add flags to get
	for _, c := range exp.Commands() {
		if c.Use == "get" {
			c.Flags().String("target", "", "target identifier (app:<id> or project:<id>)")
			c.Flags().String("app", "", "application id")
			c.Flags().String("project", "", "project id")
		}
	}
	setCmd := &cobra.Command{
		Use:   "set <private|shared|public>",
		Short: "Set exposure",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			exposure := strings.ToLower(strings.TrimSpace(args[0]))
			if exposure != "private" && exposure != "shared" && exposure != "public" {
				return errors.New("exposure must be private, shared, or public")
			}
			// Public requires explicit flag in non-interactive mode
			if exposure == "public" && isNonInteractive() && !flagForce {
				// check per-command force
				f, _ := cmd.Flags().GetBool("force")
				if !f {
					err := errors.New("making a service public requires --force in --non-interactive mode")
					return handleFailure(err)
}
			}
			target, _ := cmd.Flags().GetString("target")
			appID, _ := cmd.Flags().GetString("app")
			projID, _ := cmd.Flags().GetString("project")
			hostname, _ := cmd.Flags().GetString("hostname")
			force, _ := cmd.Flags().GetBool("force")
			if appID != "" {
				target = "app:" + appID
			}
			if projID != "" {
				target = "project:" + projID
			}
			if target == "" {
				return errors.New("specify --target, --app, or --project")
			}
			if !force && !flagForce && exposure == "public" && !flagJSON {
				if !confirmPrompt(fmt.Sprintf("make %s public? This exposes it to the internet", target)) {
					fmt.Println("aborted")
					return nil
				}
			}
			ctx, cancel := newContext()
			defer cancel()
			c, err := resolveClient()
			if err != nil {
			return handleFailure(err)
		}
			// Prefer typed helpers
			var out *apiclient.ExposureResponse
			var reqErr error
			if appID != "" {
				out, reqErr = c.SetAppExposure(ctx, appID, exposure)
			} else if projID != "" {
				out, reqErr = c.SetProjectExposure(ctx, projID, exposure)
			} else {
				out, reqErr = c.SetExposure(ctx, apiclient.ExposureRequest{Target: target, Exposure: exposure, Hostname: hostname})
			}
			if reqErr != nil {
			return handleFailure(reqErr)
		}
			if flagJSON {
				return printJSON(out)
			}
			fmt.Printf("target:   %s\n", out.Target)
			fmt.Printf("exposure: %s\n", out.Exposure)
			fmt.Printf("hostname: %s\n", out.Hostname)
			if out.Message != "" {
				fmt.Println(out.Message)
			}
			return nil
		},
	}
	setCmd.Flags().String("target", "", "target identifier (app:<id> or project:<id>)")
	setCmd.Flags().String("app", "", "application id")
	setCmd.Flags().String("project", "", "project id")
	setCmd.Flags().String("hostname", "", "override hostname")
	setCmd.Flags().Bool("force", false, "force public exposure without confirmation")
	exp.AddCommand(setCmd)

	exp.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List all exposures",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := newContext()
			defer cancel()
			c, err := resolveClient()
			if err != nil {
			return handleFailure(err)
		}
			items, err := c.ListExposures(ctx)
			if err != nil {
			return handleFailure(err)
		}
			if flagJSON {
				return printJSON(map[string]any{"items": items})
			}
			if len(items) == 0 {
				fmt.Println("no exposures")
				return nil
			}
			for _, e := range items {
				fmt.Printf("%-25s %-10s %s\n", e.Target, e.Exposure, e.Hostname)
			}
			return nil
		},
	})

	return exp
}

func newBackupCmd() *cobra.Command {
	bk := &cobra.Command{
		Use:   "backup",
		Short: "Manage backups (restic + Hetzner Storage Box)",
	}
	bk.AddCommand(&cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List backups",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := newContext()
			defer cancel()
			c, err := resolveClient()
			if err != nil {
			return handleFailure(err)
		}
			items, err := c.ListBackups(ctx)
			if err != nil {
			return handleFailure(err)
		}
			humanListBackups(items)
			return nil
		},
	})
	bk.AddCommand(&cobra.Command{
		Use:   "show <id>",
		Short: "Show backup detail",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := newContext()
			defer cancel()
			c, err := resolveClient()
			if err != nil {
			return handleFailure(err)
		}
			b, err := c.GetBackup(ctx, args[0])
			if err != nil {
			return handleFailure(err)
		}
			if flagJSON {
				return printJSON(b)
			}
			fmt.Printf("id:         %s\n", b.ID)
			fmt.Printf("repo:       %s\n", b.Repository)
			fmt.Printf("snapshot:   %s\n", b.SnapshotID)
			fmt.Printf("status:     %s\n", b.Status)
			fmt.Printf("started:    %s\n", b.StartedAt.Format(time.RFC3339))
			if b.FinishedAt != nil {
				fmt.Printf("finished:   %s\n", b.FinishedAt.Format(time.RFC3339))
			}
			if b.VerifiedAt != nil {
				fmt.Printf("verified:   %s\n", b.VerifiedAt.Format(time.RFC3339))
			}
			if b.Error != "" {
				fmt.Printf("error:      %s\n", b.Error)
			}
			return nil
		},
	})
	bk.AddCommand(&cobra.Command{
		Use:   "create",
		Short: "Trigger a backup",
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, _ := cmd.Flags().GetString("repository")
			ctx, cancel := newContext()
			defer cancel()
			c, err := resolveClient()
			if err != nil {
			return handleFailure(err)
		}
			b, err := c.CreateBackup(ctx, apiclient.CreateBackupRequest{Repository: repo})
			if err != nil {
			return handleFailure(err)
		}
			if flagJSON {
				return printJSON(b)
			}
			fmt.Printf("backup %s started\n", b.ID)
			return nil
		},
	})
	// add flag to create
	for _, c := range bk.Commands() {
		if c.Use == "create" {
			c.Flags().String("repository", "", "override repository")
		}
	}
	// restore
	restoreCmd := &cobra.Command{
		Use:   "restore [<id>]",
		Short: "Restore from a backup snapshot",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			fresh, _ := cmd.Flags().GetBool("fresh")
			if fresh {
				// Fresh restore: boot fresh image -> restore from Hetzner + phrase
				// Collect Hetzner/generic repo + phrase, derive restic password,
				// upload SSH key, list snapshots, restore to /, unwrap master.key,
				// run post_restore hooks, write bootstrap-done, restart.
				// This is the SSH fallback for "Restore from backup" wizard.
				phrase, _ := cmd.Flags().GetString("phrase")
				location, _ := cmd.Flags().GetString("location")
				hetzUser, _ := cmd.Flags().GetString("hetzner-username")
				hetzHost, _ := cmd.Flags().GetString("hetzner-host")
				hetzPass, _ := cmd.Flags().GetString("hetzner-password")
				snapshotID := ""
				if len(args) > 0 {
					snapshotID = args[0]
				}
				if phrase == "" {
					// Prompt for phrase securely if interactive
					fmt.Print("Enter 24-word recovery phrase: ")
					var input string
					if _, err := fmt.Scanln(&input); err != nil {
						// Try reading full line
						buf := make([]byte, 4096)
						n, _ := os.Stdin.Read(buf)
						input = strings.TrimSpace(string(buf[:n]))
					}
					phrase = strings.TrimSpace(input)
				}
				if phrase == "" {
					return handleFailure(fmt.Errorf("phrase is required for --fresh"))
				}
				// Validate phrase early
				words := strings.Fields(phrase)
				if len(words) != 24 {
					return handleFailure(fmt.Errorf("phrase must be 24 words, got %d", len(words)))
				}
				fmt.Printf("fresh restore requested")
				if snapshotID != "" {
					fmt.Printf(" snapshot %s", snapshotID)
				}
				if hetzUser != "" && hetzHost != "" {
					fmt.Printf(" from Hetzner %s@%s", hetzUser, hetzHost)
				} else if location != "" {
					fmt.Printf(" from %s", location)
				}
				fmt.Println()
				fmt.Println("This would: derive restic password from phrase, ensure backup_ssh key, upload authorized_keys via SFTP:23,")
				fmt.Println("run `restic snapshots --json --latest 10`, then `restic restore <id> --target / --include <each DefaultPaths>`")
				fmt.Println("unwrap master.key from recovery.kit, run post_restore hooks, write bootstrap-done, restart omahabd.")
				fmt.Println("(stub: no Hetzner credentials available in this environment; see docs for manual restore)")
				if snapshotID == "" {
					fmt.Println("No snapshot ID supplied; would list snapshots and prompt for selection.")
				}
				_ = hetzPass
				_ = location
				return nil
			}
			if len(args) == 0 {
				return handleFailure(fmt.Errorf("snapshot id is required"))
			}
			force, _ := cmd.Flags().GetBool("force")
			if isNonInteractive() && !force && !flagForce {
				err := errors.New("restore is destructive; requires --force in --non-interactive mode")
				return handleFailure(err)
			}
			if !force && !flagForce && !flagJSON {
				if !confirmPrompt(fmt.Sprintf("restore backup %s? This overwrites current data", args[0])) {
					fmt.Println("aborted")
					return nil
				}
			}
			ctx, cancel := newContext()
			defer cancel()
			c, err := resolveClient()
			if err != nil {
				return handleFailure(err)
			}
			b, err := c.RestoreBackup(ctx, args[0])
			if err != nil {
				return handleFailure(err)
			}
			if flagJSON {
				return printJSON(b)
			}
			fmt.Printf("restore %s: %s\n", b.ID, b.Status)
			return nil
		},
	}
	restoreCmd.Flags().Bool("force", false, "force without confirmation")
	restoreCmd.Flags().Bool("fresh", false, "fresh restore from Hetzner Storage Box + recovery phrase (first-boot disaster recovery)")
	restoreCmd.Flags().String("phrase", "", "24-word recovery phrase (space-separated) for --fresh")
	restoreCmd.Flags().String("location", "", "generic restic repository URL for --fresh (Advanced)")
	restoreCmd.Flags().String("hetzner-username", "", "Hetzner Storage Box username (u123456) for --fresh")
	restoreCmd.Flags().String("hetzner-host", "", "Hetzner Storage Box host (u123456.your-storagebox.de) for --fresh")
	restoreCmd.Flags().String("hetzner-password", "", "Hetzner sub-account password (used once to upload SSH key) for --fresh")
	bk.AddCommand(restoreCmd)


	bk.AddCommand(&cobra.Command{
		Use:   "verify [<id>]",
		Short: "Verify backup integrity",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := newContext()
			defer cancel()
			c, err := resolveClient()
			if err != nil {
			return handleFailure(err)
		}
			var b *domain.Backup
			if len(args) == 0 {
				b, err = c.VerifyLatestBackup(ctx)
			} else {
				b, err = c.VerifyBackup(ctx, args[0])
			}
			if err != nil {
			return handleFailure(err)
		}
			if flagJSON {
				return printJSON(b)
			}
			fmt.Printf("verify %s: %s\n", b.ID, b.Status)
			return nil
		},
	})
	return bk
}

func newEventCmd() *cobra.Command {
	ev := &cobra.Command{
		Use:     "event",
		Short:   "View control-plane events and SSE stream",
		Aliases: []string{"events"},
	}
	ev.AddCommand(&cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List events",
		RunE: func(cmd *cobra.Command, args []string) error {
			limit, _ := cmd.Flags().GetInt("limit")
			unread, _ := cmd.Flags().GetBool("unread-only")
			typ, _ := cmd.Flags().GetString("type")
			ctx, cancel := newContext()
			defer cancel()
			c, err := resolveClient()
			if err != nil {
			return handleFailure(err)
		}
			events, err := c.ListEvents(ctx, apiclient.EventListParams{Limit: limit, UnreadOnly: unread, Type: typ})
			if err != nil {
			return handleFailure(err)
		}
			humanListEvents(events)
			return nil
		},
	})
	for _, c := range ev.Commands() {
		if c.Use == "list" {
			c.Flags().Int("limit", 50, "max events")
			c.Flags().Bool("unread-only", false, "only unread events")
			c.Flags().String("type", "", "filter by event type (backup.failed, etc.)")
		}
	}
	ev.AddCommand(&cobra.Command{
		Use:   "show <id>",
		Short: "Show event detail",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := newContext()
			defer cancel()
			c, err := resolveClient()
			if err != nil {
			return handleFailure(err)
		}
			e, err := c.GetEvent(ctx, args[0])
			if err != nil {
			return handleFailure(err)
		}
			if flagJSON {
				return printJSON(e)
			}
			fmt.Printf("id:       %s\n", e.ID)
			fmt.Printf("type:     %s\n", e.Type)
			fmt.Printf("severity: %s\n", e.Severity)
			fmt.Printf("message:  %s\n", e.Message)
			fmt.Printf("created:  %s\n", e.CreatedAt.Format(time.RFC3339))
			if e.ReadAt != nil {
				fmt.Printf("read:     %s\n", e.ReadAt.Format(time.RFC3339))
			}
			if len(e.Data) > 0 {
				b, _ := json.MarshalIndent(e.Data, "", "  ")
				fmt.Printf("data: %s\n", b)
			}
			return nil
		},
	})
	ev.AddCommand(&cobra.Command{
		Use:   "watch",
		Short: "Stream events via SSE (Server-Sent Events)",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			// Handle SIGINT gracefully? Simple.
			c, err := resolveClient()
			if err != nil {
			return handleFailure(err)
		}
			ch := make(chan domain.Event, 16)
			go func() {
				if err := c.WatchEvents(ctx, ch); err != nil && !errors.Is(err, context.Canceled) {
					fmt.Fprintf(os.Stderr, "stream error: %v\n", err)
					cancel()
				}
			}()
			for {
				select {
				case <-ctx.Done():
					return nil
				case ev := <-ch:
					if flagJSON {
						_ = printJSON(ev)
					} else {
						fmt.Printf("%s %-20s %s\n", ev.CreatedAt.Format("15:04:05"), ev.Type, ev.Message)
					}
				}
			}
		},
	})
	ev.AddCommand(&cobra.Command{
		Use:   "ack <id>",
		Short: "Acknowledge (mark read) an event",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := newContext()
			defer cancel()
			c, err := resolveClient()
			if err != nil {
			return handleFailure(err)
		}
			if err := c.AckEvent(ctx, args[0]); err != nil {
				return handleFailure(err)
}
			if flagJSON {
				return printJSON(map[string]string{"acked": args[0]})
			}
			fmt.Printf("acked %s\n", args[0])
			return nil
		},
	})
	return ev
}

func newSyncCmd() *cobra.Command {
	sync := &cobra.Command{
		Use:   "sync",
		Short: "Manage Syncthing folders",
	}
	sync.AddCommand(&cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List sync folders",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := newContext()
			defer cancel()
			c, err := resolveClient()
			if err != nil {
			return handleFailure(err)
		}
			items, err := c.ListSyncFolders(ctx)
			if err != nil {
			return handleFailure(err)
		}
			humanListSync(items)
			return nil
		},
	})
	sync.AddCommand(&cobra.Command{
		Use:   "show <id>",
		Short: "Show sync folder",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := newContext()
			defer cancel()
			c, err := resolveClient()
			if err != nil {
			return handleFailure(err)
		}
			f, err := c.GetSyncFolder(ctx, args[0])
			if err != nil {
			return handleFailure(err)
		}
			if flagJSON {
				return printJSON(f)
			}
			fmt.Printf("id:           %s\n", f.ID)
			fmt.Printf("name:         %s\n", f.Name)
			fmt.Printf("server_path:  %s\n", f.ServerPath)
			fmt.Printf("share_with_ai:%v\n", f.ShareWithAI)
			fmt.Printf("health:       %s\n", f.Health)
			return nil
		},
	})
	var addName, addPath string
	var addShare bool
	addCmd := &cobra.Command{
		Use:   "add",
		Short: "Add a sync folder (delegates to clientd when available)",
		Long:  "Creates the server-side Syncthing folder and, via clientd, enrolls the local device.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(addName) == "" || strings.TrimSpace(addPath) == "" {
				return errors.New("--name and --path are required")
			}
			ctx, cancel := newContext()
			defer cancel()
			c, err := resolveClient()
			if err != nil {
			return handleFailure(err)
		}
			// Try server first
			f, err := c.CreateSyncFolder(ctx, apiclient.CreateSyncFolderRequest{Name: addName, ServerPath: addPath, ShareWithAI: addShare})
			if err != nil {
				return handleFailure(err)
			}
			if flagJSON {
				return printJSON(f)
			}
			fmt.Printf("sync folder %s (%s) created\n", f.Name, f.ID)
			return nil
		},
	}
	addCmd.Flags().StringVar(&addName, "name", "", "folder name (e.g., Notes)")
	addCmd.Flags().StringVar(&addPath, "path", "", "server path (e.g., /srv/omahab/sync/Notes)")
	addCmd.Flags().BoolVar(&addShare, "share-with-ai", false, "allow default assistant to read")
	sync.AddCommand(addCmd)

	rmCmd := &cobra.Command{
		Use:     "rm <id>",
		Short:   "Remove a sync folder",
		Aliases: []string{"delete", "remove"},
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			force, _ := cmd.Flags().GetBool("force")
			if isNonInteractive() && !force && !flagForce {
				err := errors.New("removing sync folder requires --force in --non-interactive mode")
				return handleFailure(err)
}
			if !force && !flagForce && !flagJSON {
				if !confirmPrompt(fmt.Sprintf("remove sync folder %q?", args[0])) {
					fmt.Println("aborted")
					return nil
				}
			}
			ctx, cancel := newContext()
			defer cancel()
			c, err := resolveClient()
			if err != nil {
			return handleFailure(err)
		}
			if err := c.DeleteSyncFolder(ctx, args[0]); err != nil {
				return handleFailure(err)
}
			if flagJSON {
				return printJSON(map[string]string{"deleted": args[0]})
			}
			fmt.Printf("deleted %s\n", args[0])
			return nil
		},
	}
	rmCmd.Flags().Bool("force", false, "force without confirmation")
	sync.AddCommand(rmCmd)

	return sync
}

func newRunnerCmd() *cobra.Command {
	runner := &cobra.Command{
		Use:     "workspace",
		Short:   "Manage workspaces",
		Long:    "Remote Dev Container workspaces for projects.",
		Aliases: []string{"workspaces", "ws"},
	}
	runner.AddCommand(&cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List runners/workspaces",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := newContext()
			defer cancel()
			c, err := resolveClient()
			if err != nil {
			return handleFailure(err)
		}
			items, err := c.ListWorkspaces(ctx)
			if err != nil {
			return handleFailure(err)
		}
			// Filter by --project if given
			projFilter, _ := cmd.Flags().GetString("project")
			if projFilter != "" {
				var filtered []domain.Workspace
				for _, w := range items {
					if string(w.ProjectID) == projFilter {
						filtered = append(filtered, w)
					}
				}
				items = filtered
			}
			humanListWorkspaces(items)
			return nil
		},
	})
	for _, c := range runner.Commands() {
		if c.Use == "list" {
			c.Flags().String("project", "", "filter by project id")
		}
	}
	runner.AddCommand(&cobra.Command{
		Use:   "show <id>",
		Short: "Show runner/workspace",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := newContext()
			defer cancel()
			cl, err := resolveClient()
			if err != nil {
			return handleFailure(err)
		}
			w, err := cl.GetWorkspace(ctx, args[0])
			if err != nil {
			return handleFailure(err)
		}
			if flagJSON {
				return printJSON(w)
			}
			fmt.Printf("id:         %s\n", w.ID)
			fmt.Printf("project:    %s\n", w.ProjectID)
			fmt.Printf("branch:     %s\n", w.Branch)
			fmt.Printf("agent:      %s\n", w.Agent)
			fmt.Printf("status:     %s\n", w.Status)
			fmt.Printf("created:    %s\n", w.CreatedAt.Format(time.RFC3339))
			fmt.Printf("last_active:%s\n", w.LastActiveAt.Format(time.RFC3339))
			if w.ExpiresAt != nil {
				fmt.Printf("expires:    %s\n", w.ExpiresAt.Format(time.RFC3339))
			}
			return nil
		},
	})
	// create
	var createProject, createTitle, createInstructions, createBranch, createAgent, createDevcontainer string
	createCmd := &cobra.Command{
		Use:   "create",
		Short: "Create a runner/workspace",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(createProject) == "" {
				return errors.New("--project is required")
			}
			if strings.TrimSpace(createTitle) == "" && strings.TrimSpace(createBranch) == "" {
				return errors.New("--title is required")
			}
			ctx, cancel := newContext()
			defer cancel()
			c, err := resolveClient()
			if err != nil {
				return handleFailure(err)
			}
			req := apiclient.CreateWorkspaceRequest{
				ProjectID:          createProject,
				Title:              createTitle,
				Instructions:       createInstructions,
				Branch:             createBranch,
				Agent:              createAgent,
				DevcontainerSource: createDevcontainer,
			}
			if req.Title == "" && req.Branch != "" {
				req.Title = req.Branch
			}
			w, err := c.CreateWorkspace(ctx, req)
			if err != nil {
				return handleFailure(err)
			}
			if flagJSON {
				return printJSON(w)
			}
			fmt.Printf("created workspace %s branch %s for project %s\n", w.ID, w.Branch, w.ProjectID)
			return nil
		},
	}
	createCmd.Flags().StringVar(&createProject, "project", "", "project id or slug (required)")
	createCmd.Flags().StringVar(&createTitle, "title", "", "workspace title (required, used for branch ws/<slug>-XXXX)")
	createCmd.Flags().StringVar(&createInstructions, "instructions", "", "task instructions (optional, written to .omahab/TASK.md)")
	createCmd.Flags().StringVar(&createBranch, "branch", "", "git branch (deprecated, use --title)")
	_ = createCmd.Flags().MarkHidden("branch")
	createCmd.Flags().StringVar(&createAgent, "agent", "", "coding agent (omp only)")
	createCmd.Flags().StringVar(&createDevcontainer, "devcontainer", "", "devcontainer source (default or devcontainer)")
	runner.AddCommand(createCmd)

	runner.AddCommand(&cobra.Command{
		Use:   "attach <id>",
		Short: "Attach to a runner via clientd or direct",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := newContext()
			defer cancel()
			cd := clientd()
			if cd.Available(ctx) {
				if err := cd.Call(ctx, "workspace.attach", map[string]any{"id": args[0]}, nil); err != nil {
					return handleFailure(err)
				}
				if flagJSON {
					return printJSON(map[string]string{"attached": args[0]})
				}
				fmt.Printf("attached to %s via clientd\n", args[0])
				return nil
			}
			// Server path: re-exec via sudo when not root, then POST /api/v1/workspaces/{id}/attach
			if os.Geteuid() != 0 {
				if sudoPath, err := exec.LookPath("sudo"); err == nil {
					sudoArgs := os.Args
					c := exec.Command(sudoPath, sudoArgs...)
					c.Stdin = os.Stdin
					c.Stdout = os.Stdout
					c.Stderr = os.Stderr
					if err := c.Run(); err != nil {
						return handleFailure(err)
					}
					return nil
				}
			}
			c, err := resolveClient()
			if err != nil {
				return handleFailure(err)
			}
			if err := c.AttachWorkspace(ctx, args[0]); err != nil {
				return handleFailure(err)
			}
			if flagJSON {
				return printJSON(map[string]string{"attached": args[0]})
			}
			fmt.Printf("attached to %s\n", args[0])
			return nil
		},
	})
	// send
	sendCmd := &cobra.Command{
		Use:   "send <id> <message>",
		Short: "Send a message to a workspace tmux session",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			msgFlag, _ := cmd.Flags().GetString("message")
			message := strings.TrimSpace(msgFlag)
			if message == "" && len(args) > 1 {
				message = strings.Join(args[1:], " ")
			}
			if strings.TrimSpace(message) == "" {
				return errors.New("message is required (provide as args or --message)")
			}
			ctx, cancel := newContext()
			defer cancel()
			c, err := resolveClient()
			if err != nil {
				return handleFailure(err)
			}
			if err := c.SendWorkspace(ctx, id, message); err != nil {
				return handleFailure(err)
			}
			if flagJSON {
				return printJSON(map[string]string{"sent": id})
			}
			fmt.Printf("sent to %s\n", id)
			return nil
		},
	}
	sendCmd.Flags().String("message", "", "message to send (alternative to positional args)")
	runner.AddCommand(sendCmd)
	stopCmd := &cobra.Command{
		Use:   "stop <id>",
		Short: "Stop a runner/workspace",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			force, _ := cmd.Flags().GetBool("force")
			if isNonInteractive() && !force && !flagForce {
				// stop is not destructive? It's reversible; allow without force.
			}
			ctx, cancel := newContext()
			defer cancel()
			c, err := resolveClient()
			if err != nil {
				return handleFailure(err)
			}
			if err := c.StopWorkspace(ctx, args[0]); err != nil {
				return handleFailure(err)
			}
			if flagJSON {
				return printJSON(map[string]string{"stopped": args[0]})
			}
			fmt.Printf("stopped %s\n", args[0])
			return nil
		},
	}
	stopCmd.Flags().Bool("force", false, "force stop")
	runner.AddCommand(stopCmd)

	// open convenience
	runner.AddCommand(&cobra.Command{
		Use:   "open <id>",
		Short: "Open runner in terminal (alias for attach)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := newContext()
			defer cancel()
			cd := clientd()
			if !cd.Available(ctx) {
				err := errors.New("clientd not available")
				return handleFailure(err)
			}
			return cd.Call(ctx, "workspace.attach", map[string]any{"id": args[0]}, nil)
		},
	})

	return runner
}

func newWorkspaceCmd() *cobra.Command {
	// Hidden alias for backward compatibility: runner -> workspace
	ws := newRunnerCmd()
	ws.Use = "runner"
	ws.Aliases = []string{"runners"}
	ws.Short = "Manage remote runners (workspaces) (hidden alias for workspace)"
	ws.Long = "Hidden alias for workspace. Use workspace instead."
	ws.Hidden = true
	return ws
}

func newIdentityCmd() *cobra.Command {
	id := &cobra.Command{
		Use:   "identity",
		Short: "Identity and recovery",
	}
	recoverCmd := &cobra.Command{
		Use:   "recover <email>",
		Short: "Recover a Pocket ID user via local root",
		Long: `Generates a short-lived recovery code/enrollment URL for the given email.
Requires local root or sudo on the server (enforced server-side).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			email := strings.TrimSpace(args[0])
			if email == "" || !strings.Contains(email, "@") {
				return errors.New("valid email required")
			}
			ctx, cancel := newContext()
			defer cancel()
			c, err := resolveClient()
			if err != nil {
			return handleFailure(err)
		}
			res, err := c.RecoverIdentity(ctx, email)
			if err != nil {
			return handleFailure(err)
		}
			if flagJSON {
				return printJSON(res)
			}
			fmt.Printf("recovery for %s\n", res.Email)
			if res.Code != nil {
				fmt.Printf("code: %s\n", *res.Code)
			}
			if res.LoginURL != nil {
				fmt.Printf("login_url: %s\n", *res.LoginURL)
			}
			if res.ExpiresAt != nil {
				fmt.Printf("expires: %s\n", *res.ExpiresAt)
			}
			if res.Message != "" {
				fmt.Println(res.Message)
			}
			return nil
		},
	}
	id.AddCommand(recoverCmd)

	// Optional: user list/show helpers
	id.AddCommand(&cobra.Command{
		Use:   "users",
		Short: "List users (admin)",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(os.Stderr, "hint: use the dashboard or Pocket ID API for full user management; this command lists via /users if available")
			ctx, cancel := newContext()
			defer cancel()
			c, err := resolveClient()
			if err != nil {
			return handleFailure(err)
		}
			// Try generic endpoint via raw GET using client's private? Use workaround: direct http
			// Instead, attempt via api client helper if available: not yet; do raw.
			// Reconstruct URL for /users
			server := c.BaseURL
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, server+"/users", nil)
			if err != nil {
				return handleFailure(err)
			}
			if c.Token != "" {
				req.Header.Set("Authorization", "Bearer "+c.Token)
			}
			req.Header.Set("Accept", "application/json")
			resp, err := c.HTTPClient.Do(req)
			if err != nil {
			return handleFailure(err)
		}
			defer resp.Body.Close()
			if resp.StatusCode >= 400 {
				body, _ := io.ReadAll(resp.Body)
				if flagJSON {
					fmt.Println(string(body))
					return nil
				}
				fmt.Fprintf(os.Stderr, "error: %s\n", string(body))
				return nil
			}
			var env map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(env)
			}
			_ = printJSON(env)
			return nil
		},
	})

	return id
}

func newHermesCmd() *cobra.Command {
	hermes := &cobra.Command{
		Use:   "hermes",
		Short: "Hermes assistant integration",
	}
	hermes.AddCommand(&cobra.Command{
		Use:   "open",
		Short: "Open Hermes Desktop/browser (via clientd)",
		RunE: func(cmd *cobra.Command, args []string) error {
			urlFlag, _ := cmd.Flags().GetString("url")
			ctx, cancel := newContext()
			defer cancel()
			// Resolve URL: flag, then server-derived, then status instance?
			targetURL := strings.TrimSpace(urlFlag)
			if targetURL == "" {
				c, err := resolveClient()
				if err == nil && strings.TrimSpace(c.BaseURL) != "" {
					if u, perr := url.Parse(c.BaseURL); perr == nil && u.Host != "" {
						host := u.Host
						if strings.Contains(host, ":") {
							if h, _, e := net.SplitHostPort(host); e == nil {
								host = h
							}
						}
						if net.ParseIP(host) == nil && host != "localhost" && host != "127.0.0.1" && !strings.EqualFold(host, "localhost") {
							parts := strings.Split(host, ".")
							if len(parts) >= 3 {
								targetURL = u.Scheme + "://ai." + strings.Join(parts[1:], ".")
							} else if len(parts) == 2 {
								targetURL = u.Scheme + "://ai." + host
							} else {
								targetURL = "https://ai." + host
							}
						} else {
							targetURL = "https://ai.example.com"
						}
					} else {
						targetURL = c.BaseURL
					}
				}
				if targetURL == "" {
					targetURL = "https://ai.example.com"
				}
			}
			cd := clientd()
			if cd.Available(ctx) {
				if err := cd.Call(ctx, "ai.open", map[string]any{"url": targetURL}, nil); err != nil {
					return handleFailure(err)
				}
				if flagJSON {
					return printJSON(map[string]string{"opened": targetURL})
				}
				fmt.Printf("opened Hermes at %s\n", targetURL)
				return nil
			}
			// Fallback: instruct user
			if flagJSON {
				return printJSON(map[string]string{"url": targetURL, "clientd": "unavailable"})
			}
			fmt.Printf("clientd not available\n")
			fmt.Printf("open Hermes at: %s\n", targetURL)
			fmt.Println("hint: install Omarchy companion or ensure omahab-clientd is running; then 'omahab hermes open' will launch automatically")
			return nil
		},
	})
	for _, c := range hermes.Commands() {
		if c.Use == "open" {
			c.Flags().String("url", "", "Hermes URL to open (default derived from server)")
		}
	}
	return hermes
}

func newSecretsCmd() *cobra.Command {
	s := &cobra.Command{
		Use:     "secrets",
		Short:   "Inspect secret metadata (values never shown)",
		Aliases: []string{"secret"},
	}
	s.AddCommand(&cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List secret metadata",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := newContext()
			defer cancel()
			c, err := resolveClient()
			if err != nil {
			return handleFailure(err)
		}
			items, err := c.ListSecrets(ctx)
			if err != nil {
			return handleFailure(err)
		}
			if flagJSON {
				return printJSON(map[string]any{"items": items})
			}
			if len(items) == 0 {
				fmt.Println("no secrets")
				return nil
			}
			fmt.Printf("%-20s %-15s %-20s %s\n", "ID", "SCOPE", "NAME", "VERSION")
			for _, sec := range items {
				fmt.Printf("%-20s %-15s %-20s %d\n", sec.ID, sec.Scope, sec.Name, sec.Version)
			}
			return nil
		},
	})
	return s
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version",
		RunE: func(cmd *cobra.Command, args []string) error {
			if flagJSON {
				return printJSON(map[string]string{"version": version})
			}
			fmt.Println(version)
			return nil
		},
	}
}

func newCatalogCmd() *cobra.Command {
	cat := &cobra.Command{
		Use:    "catalog",
		Short:  "Catalog utilities",
		Hidden: true,
	}
	validate := &cobra.Command{
		Use:   "validate [path]",
		Short: "Validate a catalog file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := args[0]
			c, err := apps.LoadCatalogFile(path)
			if err != nil {
				return err
			}
			fmt.Printf("ok: %d bundles\n", len(c.Bundles()))
			return nil
		},
	}
	cat.AddCommand(validate)
	return cat
}

func newCompletionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "completion [bash|zsh|fish|powershell]",
		Short: "Generate shell completion",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root := cmd.Root()
			switch args[0] {
			case "bash":
				return root.GenBashCompletion(os.Stdout)
			case "zsh":
				return root.GenZshCompletion(os.Stdout)
			case "fish":
				return root.GenFishCompletion(os.Stdout, true)
			case "powershell":
				return root.GenPowerShellCompletion(os.Stdout)
			default:
				return fmt.Errorf("unknown shell %q", args[0])
			}
		},
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
