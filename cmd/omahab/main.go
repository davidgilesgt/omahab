package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/omahab/omahab/internal/apiclient"
	"github.com/omahab/omahab/internal/domain"
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
		// Cobra already prints error; ensure exit code
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "omahab",
		Short: "The opinionated home server.",
		Long: `Omahab CLI — control plane for the Omahab home server.

Every operation has a JSON equivalent via --json.
Credentials are never passed as arguments; set OMAHAB_TOKEN or use the credential store.
Use --server to target a different control plane (env OMAHAB_SERVER, then ~/.config/omahab/client.json).`,
		SilenceUsage:  true,
		SilenceErrors: false,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			// Validate mutating content-type requirement is handled client side
			// Ensure timeout is sane
			if flagTimeout < time.Second || flagTimeout > 5*time.Minute {
				return fmt.Errorf("--timeout must be between 1s and 5m")
			}
			return nil
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
	root.AddCommand(newStatusCmd())
	root.AddCommand(newUpCmd())
	root.AddCommand(newDoctorCmd())
	root.AddCommand(newAppCmd())
	root.AddCommand(newProjectCmd())
	root.AddCommand(newExposureCmd())
	root.AddCommand(newBackupCmd())
	root.AddCommand(newEventCmd())
	root.AddCommand(newSyncCmd())
	root.AddCommand(newRunnerCmd())
	root.AddCommand(newWorkspaceCmd()) // alias
	root.AddCommand(newIdentityCmd())
	root.AddCommand(newHermesCmd())
	root.AddCommand(newSecretsCmd())
	root.AddCommand(newVersionCmd())
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
	// TERM=dumb or non-TTY also considered non-interactive for destructive guard?
	if os.Getenv("TERM") == "dumb" {
		return true
	}
	// If stdin is not a tty, treat as non-interactive for safety
	if fi, err := os.Stdin.Stat(); err == nil {
		if (fi.Mode() & os.ModeCharDevice) == 0 {
			// stdin is piped; but not necessarily non-interactive for all cmds.
			// We only enforce for destructive operations.
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

func resolveClient() (*apiclient.Client, error) {
	cfg, err := apiclient.LoadClientConfig("")
	if err != nil {
		return nil, fmt.Errorf("load client config: %w", err)
	}
	server := apiclient.ResolveServer(flagServer, cfg)
	// Token via CredentialStore or OMAHAB_TOKEN, never args
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
	// Allow insecure? No. Respect timeout
	return c, nil
}

func clientd() *apiclient.ClientdClient {
	return apiclient.NewClientdClient("")
}

func isColorEnabled() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	// Could also check TERM
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

func handleError(err error) error {
	if err == nil {
		return nil
	}
	if flagJSON {
		printErrorJSON(err)
		// Return nil to avoid double printing by cobra?
		// But we want non-zero exit; cobra will print error again if we return err.
		// So we print JSON envelope and return a sentinel that suppresses cobra's error?
		// Instead, return err but also ensure SilenceErrors maybe? Root has SilenceErrors false.
		// So we output JSON and then return err for exit code, but avoid duplicate.
		// We'll have caller return err after printing; cobra will print again. To avoid, we
		// print JSON and return nil with os.Exit? Simpler: print and return custom that cobra treats silent?
		// We set SilenceErrors true per command and manually handle.
		return nil
	}
	// Human concise error
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
	if apiErr, ok := err.(*apiclient.APIError); ok && apiErr.Raw != "" && flagJSON == false {
		// Don't leak raw if not JSON? Keep concise.
		_ = apiErr
	}
	return nil
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
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				return err
			}
			st, err := c.Status(ctx)
			if err != nil {
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				return nil
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
		Long:  "GET /api/v1/up — no Bearer token required. Useful for probes and scripts.",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := newContext()
			defer cancel()
			c, err := resolveClient()
			if err != nil {
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				return err
			}
			// Force no token for up? client already handles stripping auth for /up path
			out, err := c.Up(ctx)
			if err != nil {
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				return nil
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
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				return err
			}
			res, err := c.Doctor(ctx)
			if err != nil {
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				return nil
			}
			if flagJSON {
				return printJSON(res)
			}
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
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				return err
			}
			items, err := c.ListApplications(ctx)
			if err != nil {
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				return nil
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
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				return err
			}
			a, err := c.GetApplication(ctx, args[0])
			if err != nil {
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				return nil
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
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				return err
			}
			ctx, cancel := newContext()
			defer cancel()
			c, err := resolveClient()
			if err != nil {
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				return err
			}
			res, err := c.RestartApplication(ctx, args[0])
			if err != nil {
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				return nil
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
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				return err
			}
			items, err := c.ListProjects(ctx)
			if err != nil {
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				return nil
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
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				return err
			}
			p, err := c.CreateProject(ctx, apiclient.CreateProjectRequest{Name: createName, Slug: createSlug})
			if err != nil {
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				return nil
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
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				return err
			}
			p, err := c.GetProject(ctx, args[0])
			if err != nil {
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				return nil
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
			fmt.Printf("bot:       %s\n", p.BotProfileID)
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
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				return err
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
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				return err
			}
			if err := c.DeleteProject(ctx, args[0]); err != nil {
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				return nil
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
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				return err
			}
			rs, err := c.ListReleases(ctx, args[0])
			if err != nil {
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				return nil
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
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				return err
			}
			rel, err := c.CreateRelease(ctx, args[0], apiclient.CreateReleaseRequest{Commit: deployCommit, Digest: deployDigest})
			if err != nil {
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				return nil
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
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				return err
			}
			ctx, cancel := newContext()
			defer cancel()
			c, err := resolveClient()
			if err != nil {
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				return err
			}
			rel, err := c.RollbackRelease(ctx, args[0], args[1])
			if err != nil {
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				return nil
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
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				return err
			}
			p, err := c.GetProject(ctx, args[0])
			if err != nil {
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				return nil
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
				if err := cd.ProjectClone(ctx, string(p.ID), dest); err != nil {
					if flagJSON {
						printErrorJSON(err)
						return nil
					}
					fmt.Fprintf(os.Stderr, "clientd clone failed: %v\n", err)
					fmt.Printf("repo: %s\n", p.RepositoryURL)
					fmt.Printf("hint: git clone %s %s\n", p.RepositoryURL, dest)
					return nil
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
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				return err
			}
			p, err := c.GetProject(ctx, args[0])
			if err != nil {
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				return nil
			}
			cd := clientd()
			if cd.Available(ctx) {
				if err := cd.ProjectOpen(ctx, string(p.ID)); err != nil {
					if flagJSON {
						printErrorJSON(err)
						return nil
					}
					fmt.Fprintf(os.Stderr, "error: %v\n", err)
					return nil
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
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				return err
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
				if flagJSON {
					printErrorJSON(reqErr)
					return nil
				}
				fmt.Fprintf(os.Stderr, "error: %v\n", reqErr)
				return nil
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
					if flagJSON {
						printErrorJSON(err)
						return nil
					}
					return err
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
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				return err
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
				if flagJSON {
					printErrorJSON(reqErr)
					return nil
				}
				fmt.Fprintf(os.Stderr, "error: %v\n", reqErr)
				return nil
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
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				return err
			}
			items, err := c.ListExposures(ctx)
			if err != nil {
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				return nil
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
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				return err
			}
			items, err := c.ListBackups(ctx)
			if err != nil {
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				return nil
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
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				return err
			}
			b, err := c.GetBackup(ctx, args[0])
			if err != nil {
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				return nil
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
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				return err
			}
			b, err := c.CreateBackup(ctx, apiclient.CreateBackupRequest{Repository: repo})
			if err != nil {
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				return nil
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
		Use:   "restore <id>",
		Short: "Restore from a backup snapshot",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			force, _ := cmd.Flags().GetBool("force")
			if isNonInteractive() && !force && !flagForce {
				err := errors.New("restore is destructive; requires --force in --non-interactive mode")
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				return err
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
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				return err
			}
			b, err := c.RestoreBackup(ctx, args[0])
			if err != nil {
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				return nil
			}
			if flagJSON {
				return printJSON(b)
			}
			fmt.Printf("restore %s: %s\n", b.ID, b.Status)
			return nil
		},
	}
	restoreCmd.Flags().Bool("force", false, "force without confirmation")
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
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				return err
			}
			var b *domain.Backup
			if len(args) == 0 {
				b, err = c.VerifyLatestBackup(ctx)
			} else {
				b, err = c.VerifyBackup(ctx, args[0])
			}
			if err != nil {
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				return nil
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
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				return err
			}
			events, err := c.ListEvents(ctx, apiclient.EventListParams{Limit: limit, UnreadOnly: unread, Type: typ})
			if err != nil {
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				return nil
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
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				return err
			}
			e, err := c.GetEvent(ctx, args[0])
			if err != nil {
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				return nil
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
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				return err
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
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				return err
			}
			if err := c.AckEvent(ctx, args[0]); err != nil {
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				return nil
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
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				return err
			}
			items, err := c.ListSyncFolders(ctx)
			if err != nil {
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				return nil
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
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				return err
			}
			f, err := c.GetSyncFolder(ctx, args[0])
			if err != nil {
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				return nil
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
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				return err
			}
			// Try server first
			f, err := c.CreateSyncFolder(ctx, apiclient.CreateSyncFolderRequest{Name: addName, ServerPath: addPath, ShareWithAI: addShare})
			if err != nil {
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				return nil
			}
			// Best-effort clientd enrollment
			cd := clientd()
			if cd.Available(ctx) {
				_ = cd.SyncAdd(ctx, addName, addPath, addShare)
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
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				return err
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
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				return err
			}
			if err := c.DeleteSyncFolder(ctx, args[0]); err != nil {
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				return nil
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
		Use:     "runner",
		Short:   "Manage remote runners (workspaces)",
		Long:    "Remote Dev Container workspaces for projects. Alias: workspace.",
		Aliases: []string{"runners"},
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
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				return err
			}
			items, err := c.ListWorkspaces(ctx)
			if err != nil {
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				return nil
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
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				return err
			}
			w, err := cl.GetWorkspace(ctx, args[0])
			if err != nil {
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				return nil
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
	var createProject, createBranch, createAgent string
	createCmd := &cobra.Command{
		Use:   "create",
		Short: "Create a runner/workspace",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(createProject) == "" {
				return errors.New("--project is required")
			}
			ctx, cancel := newContext()
			defer cancel()
			c, err := resolveClient()
			if err != nil {
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				return err
			}
			w, err := c.CreateWorkspace(ctx, apiclient.CreateWorkspaceRequest{ProjectID: createProject, Branch: createBranch, Agent: createAgent})
			if err != nil {
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				return nil
			}
			if flagJSON {
				return printJSON(w)
			}
			fmt.Printf("created workspace %s for project %s\n", w.ID, w.ProjectID)
			return nil
		},
	}
	createCmd.Flags().StringVar(&createProject, "project", "", "project id (required)")
	createCmd.Flags().StringVar(&createBranch, "branch", "", "git branch (default main)")
	createCmd.Flags().StringVar(&createAgent, "agent", "", "coding agent (omp, codex, etc.)")
	runner.AddCommand(createCmd)

	runner.AddCommand(&cobra.Command{
		Use:   "attach <id>",
		Short: "Attach to a runner via clientd terminal",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := newContext()
			defer cancel()
			cd := clientd()
			if !cd.Available(ctx) {
				err := errors.New("omahab-clientd not available; ensure it is running and OMAHAB_CLIENTD_SOCKET is correct")
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				fmt.Println("hint: runner attach requires the Omarchy companion daemon (omahab-clientd) on this machine")
				return nil
			}
			if err := cd.RunnerAttach(ctx, args[0]); err != nil {
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				return nil
			}
			if flagJSON {
				return printJSON(map[string]string{"attached": args[0]})
			}
			fmt.Printf("attached to %s\n", args[0])
			return nil
		},
	})
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
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				return err
			}
			if err := c.StopWorkspace(ctx, args[0]); err != nil {
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				return nil
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
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				return err
			}
			return cd.RunnerAttach(ctx, args[0])
		},
	})

	return runner
}

func newWorkspaceCmd() *cobra.Command {
	// Alias to runner with Use workspace
	ws := newRunnerCmd()
	ws.Use = "workspace"
	ws.Aliases = []string{"workspaces", "ws"}
	ws.Short = "Manage workspaces (alias for runner)"
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
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				return err
			}
			res, err := c.RecoverIdentity(ctx, email)
			if err != nil {
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				if apiErr, ok := err.(*apiclient.APIError); ok && apiErr.StatusCode == http.StatusForbidden {
					fmt.Fprintln(os.Stderr, "hint: identity recover requires local root/sudo on the server")
				}
				return nil
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
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				return err
			}
			// Try generic endpoint via raw GET using client's private? Use workaround: direct http
			// Instead, attempt via api client helper if available: not yet; do raw.
			// Reconstruct URL for /users
			server := c.BaseURL
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, server+"/users", nil)
			if err != nil {
				return err
			}
			if c.Token != "" {
				req.Header.Set("Authorization", "Bearer "+c.Token)
			}
			req.Header.Set("Accept", "application/json")
			resp, err := c.HTTPClient.Do(req)
			if err != nil {
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				return nil
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
				// Try to derive from status hostname? Use server as fallback
				c, err := resolveClient()
				if err == nil {
					st, _ := c.Status(ctx)
					if st != nil {
						// Not ideal but use server host as placeholder
						targetURL = c.BaseURL
						_ = st
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
				if err := cd.HermesOpen(ctx, targetURL); err != nil {
					if flagJSON {
						printErrorJSON(err)
						return nil
					}
					fmt.Fprintf(os.Stderr, "error: %v\n", err)
					return nil
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
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				return err
			}
			items, err := c.ListSecrets(ctx)
			if err != nil {
				if flagJSON {
					printErrorJSON(err)
					return nil
				}
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				return nil
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
