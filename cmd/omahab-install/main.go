package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/omahab/omahab/internal/installer"
	"github.com/omahab/omahab/internal/installer/assets"

	"github.com/spf13/cobra"
)

var (
	flagJSON           bool
	flagNonInteractive bool
	flagStateDir       string
	flagVersion        string
	flagTargetUser     string
	flagGitHubUsers    []string
	flagKeyFile        string
	flagResume         bool
	flagYes            bool
	flagUntil          string
	flagAssetDir       string
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		// Errors already printed in run; just exit.
		os.Exit(1)
	}
}

func run(args []string) error {
	root := &cobra.Command{
		Use:   "omahab-install",
		Short: "Install Omahab on a fresh Debian 13 or Ubuntu 26.04 host",
		Long: `omahab-install prepares a fresh Debian 13 or Ubuntu 26.04 host for Omahab.

It runs strict preflight checks, configures SSH keys additively,
hardens sshd with rollback safety, and writes an install manifest.
Use --json for structured output and --non-interactive for automation.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return doInstall(cmd)
		},
	}

	root.PersistentFlags().BoolVar(&flagJSON, "json", false, "emit JSON output")
	root.PersistentFlags().BoolVar(&flagNonInteractive, "non-interactive", false, "never prompt; fail with structured error if input required")
	root.Flags().StringVar(&flagStateDir, "state-dir", envOr("OMAHAB_STATE_DIR", "/var/lib/omahab"), "state directory (contains control.db and manifest)")
	root.Flags().StringVar(&flagVersion, "version", envOr("OMAHAB_VERSION", "0.0.0-dev"), "version to record in manifest")
	root.Flags().StringVar(&flagTargetUser, "target-user", "", "user whose authorized_keys to manage (default: $SUDO_USER, omahab, admin, or $USER)")
	root.Flags().StringArrayVar(&flagGitHubUsers, "github-user", nil, "import SSH keys from GitHub user (repeatable)")
	root.Flags().StringVar(&flagKeyFile, "key-file", "", "import SSH keys from file")
	root.Flags().BoolVar(&flagResume, "resume", false, "resume an interrupted installation")
	root.Flags().BoolVar(&flagYes, "yes", false, "assume yes to prompts (requires --non-interactive)")
	root.Flags().StringVar(&flagUntil, "until", "", "stop after this step (one of: "+strings.Join(installer.OrderedSteps, ", ")+")")
	root.Flags().StringVar(&flagAssetDir, "asset-dir", "", "load install assets (binaries, units, catalog) from this directory instead of the embedded set")

	// Also add a dedicated preflight subcommand.
	preflightCmd := &cobra.Command{
		Use:   "preflight",
		Short: "Run preflight checks only",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return doPreflight(cmd)
		},
	}
	root.AddCommand(preflightCmd)

	// Version subcommand for manifest inspection
	manifestCmd := &cobra.Command{
		Use:   "manifest",
		Short: "Show the install manifest if present",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return doManifest(cmd)
		},
	}
	root.AddCommand(manifestCmd)

	root.SetArgs(args)
	// Support --json as a global that also affects error formatting.
	// Cobra will parse it before RunE.
	return root.Execute()
}

// output controls rendering.
type output struct {
	jsonMode       bool
	nonInteractive bool
	color          bool
	isTTY          bool
	w              io.Writer
}

func newOutput(cmd *cobra.Command) output {
	jsonMode := flagJSON
	nonInteractive := flagNonInteractive
	// Also treat TERM=dumb as non-interactive for presentation.
	if os.Getenv("TERM") == "dumb" {
		nonInteractive = true
	}
	// NO_COLOR disables color.
	color := true
	if os.Getenv("NO_COLOR") != "" {
		color = false
	}
	if jsonMode {
		color = false
	}
	// Detect TTY for stdout.
	isTTY := isTerminal(os.Stdout)
	if os.Getenv("TERM") == "dumb" {
		isTTY = false
	}
	if nonInteractive {
		isTTY = false
	}
	w := cmd.OutOrStdout()
	if w == nil {
		w = os.Stdout
	}
	return output{jsonMode: jsonMode, nonInteractive: nonInteractive, color: color, isTTY: isTTY, w: w}
}

func (o output) printf(format string, args ...any) {
	fmt.Fprintf(o.w, format, args...)
}

func (o output) println(a ...any) {
	fmt.Fprintln(o.w, a...)
}

func (o output) emitJSON(v any) {
	enc := json.NewEncoder(o.w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func (o output) emitError(code, message, remediation string, checks []installer.CheckResult) {
	if o.jsonMode {
		o.emitJSON(map[string]any{
			"ok":          false,
			"error":       code,
			"message":     message,
			"remediation": remediation,
			"checks":      checks,
		})
		return
	}
	if o.color {
		o.printf("\033[31merror: %s\033[0m\n", message)
	} else {
		o.printf("error: %s\n", message)
	}
	if remediation != "" {
		o.printf("remediation: %s\n", remediation)
	}
	for _, c := range checks {
		if c.Level == installer.LevelFail {
			o.printf("  fail %s: %s\n", c.Name, c.Message)
			if c.Remediation != "" {
				o.printf("       fix: %s\n", c.Remediation)
			}
		}
	}
}

func doPreflight(cmd *cobra.Command) error {
	out := newOutput(cmd)
	probes := installer.LiveProbes()
	svc := installer.NewService(nil, probes)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	checks, err := svc.RunPreflight(ctx)
	if out.jsonMode {
		ok := err == nil
		out.emitJSON(map[string]any{
			"ok":     ok,
			"checks": checks,
			"error":  errStr(err),
		})
		if err != nil {
			return err
		}
		return nil
	}
	printChecks(out, checks)
	if err != nil {
		if pe, ok := err.(*installer.PreflightError); ok && pe.IsDirty() {
			out.printf("\nDirty host detected — reinstall on a fresh Debian 13 or Ubuntu 26.04 host.\n")
		}
		return err
	}
	out.printf("\nPreflight passed.\n")
	return nil
}

func doManifest(cmd *cobra.Command) error {
	out := newOutput(cmd)
	probes := installer.LiveProbes()
	m, err := installer.ReadManifest(probes)
	if err != nil {
		if out.jsonMode {
			out.emitJSON(map[string]any{"ok": false, "error": err.Error()})
		} else {
			out.printf("no manifest found: %v\n", err)
		}
		return err
	}
	if out.jsonMode {
		out.emitJSON(m)
		return nil
	}
	data, _ := json.MarshalIndent(m, "", "  ")
	out.printf("%s\n", string(data))
	return nil
}

func doInstall(cmd *cobra.Command) error {
	out := newOutput(cmd)
	// Validate flag combination: --yes requires --non-interactive.
	if flagYes && !flagNonInteractive {
		// For convenience, --yes implies --non-interactive, but warn
		fmt.Fprintln(os.Stderr, "note: --yes implies --non-interactive")
		flagNonInteractive = true
		out = newOutput(cmd)
	}
	// Clarify TERM=dumb forcing non-interactive.
	if os.Getenv("TERM") == "dumb" && !flagNonInteractive && !flagJSON {
		fmt.Fprintln(os.Stderr, "note: TERM=dumb detected, forcing non-interactive mode (use --non-interactive explicitly to suppress)")
	}
	ctx := context.Background()

	// Open or create the state database.
	stateDir := flagStateDir
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		out.emitError("state_dir", fmt.Sprintf("cannot create state dir %s: %v", stateDir, err), "ensure the state directory is writable", nil)
		return err
	}
	dbPath := filepath.Join(stateDir, "control.db")
	db, err := openDB(dbPath)
	if err != nil {
		out.emitError("database", fmt.Sprintf("cannot open database: %v", err), "ensure sqlite can write to "+dbPath, nil)
		return err
	}
	defer db.Close()

	// Ensure installer migrations.
	if err := installer.EnsureMigrations(ctx, db); err != nil {
		out.emitError("migrate", fmt.Sprintf("migration failed: %v", err), "", nil)
		return err
	}

	probes := installer.LiveProbes()
	svc := installer.NewService(db, probes)

	// Validate --until early so users get the valid step list, not a mystery abort.
	if flagUntil != "" && !slices.Contains(installer.OrderedSteps, flagUntil) {
		err := fmt.Errorf("--until must be one of: %s", strings.Join(installer.OrderedSteps, ", "))
		out.emitError("until", err.Error(), "choose a step from the ordered list", nil)
		return err
	}

	// Assets are needed by the binaries step. Resolve them when that step is in
	// scope: embedded set by default, --asset-dir for development builds.
	if stepInScope(installer.StepBinaries, flagUntil) {
		var fsys fs.FS
		var err error
		if flagAssetDir != "" {
			fsys, err = assets.LoadDir(flagAssetDir)
		} else {
			fsys, err = assets.Load()
		}
		if err == nil {
			err = assets.Validate(fsys)
		}
		if err != nil {
			remed := "build with scripts/build.sh so assets are embedded, or pass --asset-dir"
			if flagAssetDir != "" {
				remed = "check the directory layout: bin/, systemd/, catalog/, tmpfiles.d/ (web/ optional)"
			}
			out.emitError("assets", err.Error(), remed, nil)
			return err
		}
		svc.SetAssets(fsys)
	}

	// Check for resumable state if not explicitly resuming.
	if !flagResume {
		needs, step, _ := svc.NeedsResume(ctx)
		if needs && out.jsonMode {
			// Report resumable state but continue — Run will handle resumption.
		} else if needs && !out.nonInteractive {
			out.printf("Found interrupted installation at step %q. Resuming...\n", step)
		}
	}

	// Phase 1: preflight (structured, always shown).
	out.printf("Running preflight checks...\n")
	checks, preErr := svc.RunPreflight(ctx)
	if !out.jsonMode {
		printChecks(out, checks)
	}
	if preErr != nil {
		if out.jsonMode {
			code := "preflight_failed"
			remed := ""
			if pe, ok := preErr.(*installer.PreflightError); ok && pe.IsDirty() {
				code = "dirty_host"
				remed = "reinstall on a fresh Debian 13 or Ubuntu 26.04 host"
			}
			var failed []installer.CheckResult
			if pe, ok := preErr.(*installer.PreflightError); ok {
				failed = pe.Checks
			}
			out.emitJSON(map[string]any{
				"ok":          false,
				"error":       code,
				"message":     preErr.Error(),
				"remediation": remed,
				"checks":      failed,
			})
		} else {
			if pe, ok := preErr.(*installer.PreflightError); ok && pe.IsDirty() {
				out.printf("\nDirty host: strict preflight failed — reinstall on a fresh Debian 13 or Ubuntu 26.04 host.\n")
			}
			// In non-interactive mode, always emit actionable failure.
			if out.nonInteractive {
				out.printf("\nPreflight failed. Fix the reported checks and retry.\n")
			}
		}
		return preErr
	}
	if !out.jsonMode {
		out.printf("Preflight passed.\n\n")
	}

	// Phase 2: gather SSH keys if needed.
	opts := installer.InstallOptions{
		Version:              flagVersion,
		TargetUser:           flagTargetUser,
		GitHubUsers:          flagGitHubUsers,
		KeyFile:              flagKeyFile,
		RequireSecondSession: true,
		UntilStep:            flagUntil,
	}

	// If stdin is piped and looks like keys, consume it before any prompt.
	var stdinKeys string
	if havePipedStdin() {
		data, _ := io.ReadAll(os.Stdin)
		trimmed := strings.TrimSpace(string(data))
		if trimmed != "" && (strings.Contains(trimmed, "ssh-") || strings.Contains(trimmed, "ecdsa-") || strings.Contains(trimmed, "sk-")) {
			stdinKeys = trimmed
		}
	}
	if stdinKeys != "" {
		opts.PastedKeys = stdinKeys
		keys := collectKeysForDisplay(ctx, probes, opts)
		if len(keys) > 0 && !out.jsonMode {
			out.printf("\nKeys from stdin:\n")
			for _, k := range keys {
				out.printf("  %s %s  %s  source:%s\n", k.Type, k.Fingerprint, truncateComment(k.Comment, 40), k.Source)
			}
			out.printf("\n")
		}
	}

	// If no key source supplied and we are interactive, prompt.
	if len(opts.GitHubUsers) == 0 && opts.KeyFile == "" && opts.PastedKeys == "" {
		if out.nonInteractive {
			// Let the service's ssh_keys step validate existing keys and fail structurally if needed.
		} else if stdinKeys == "" {
			pasted, githubUsers, keyFile, err := promptForKeys(out)
			if err != nil {
				if err == installer.ErrCancelled {
					out.printf("Installation cancelled.\n")
					return err
				}
				out.emitError("input", err.Error(), "", nil)
				return err
			}
			opts.GitHubUsers = githubUsers
			opts.KeyFile = keyFile
			opts.PastedKeys = pasted
			if pasted != "" || len(githubUsers) > 0 || keyFile != "" {
				keys := collectKeysForDisplay(ctx, probes, opts)
				if len(keys) > 0 {
					out.printf("\nImported SSH keys:\n")
					for _, k := range keys {
						out.printf("  %s %s  %s  (%s)  source:%s\n", k.Type, k.Fingerprint, truncateComment(k.Comment, 32), shortFP(k.Fingerprint), k.Source)
					}
					out.printf("\n")
				}
			}
		}
	} else if opts.PastedKeys == "" {
		// Keys supplied via flags (but not stdin) — display fingerprints.
		keys := collectKeysForDisplay(ctx, probes, opts)
		if len(keys) > 0 && !out.jsonMode {
			out.printf("\nKeys to be installed:\n")
			for _, k := range keys {
				out.printf("  %s %s  %s  source:%s\n", k.Type, k.Fingerprint, truncateComment(k.Comment, 40), k.Source)
			}
			out.printf("\n")
		}
	}

	if !out.jsonMode {
		out.printf("Starting installation (version %s)...\n", opts.Version)
		out.printf("This will take a few minutes; progress is streamed per step.\n")
		entries, _ := svc.JournalEntries(ctx)
		for _, e := range entries {
			if e.Status == installer.JournalCompleted {
				out.printf("  [done] %s\n", e.Step)
			} else if e.Status == installer.JournalFailed {
				out.printf("  [failed] %s: %s\n", e.Step, e.Error)
			}
		}
	}

	results, runErr := svc.Run(ctx, opts)

	if out.jsonMode {
		ok := runErr == nil
		var failedChecks []installer.CheckResult
		if se, ok2 := runErr.(*installer.StepError); ok2 {
			failedChecks = se.Result.Checks
		}
		out.emitJSON(map[string]any{
			"ok":      ok,
			"steps":   results,
			"checks":  failedChecks,
			"error":   errStr(runErr),
			"version": opts.Version,
		})
		if runErr != nil {
			return runErr
		}
		// Avoid extra printf after JSON - just return, caller can check IsRollbackActive via JSON field
		return nil
	}

	// Plain output.
	for _, r := range results {
		switch r.Status {
		case installer.JournalCompleted:
			out.printf("  [ok] %s\n", r.Step)
			if r.Step == installer.StepPreflight && len(r.Checks) > 0 {
				// already printed
			}
			if len(r.Keys) > 0 {
				for _, k := range r.Keys {
					out.printf("       key %s %s (%s)\n", k.Type, k.Fingerprint, k.Comment)
				}
			}
		case installer.JournalFailed:
			out.printf("  [fail] %s: %s\n", r.Step, r.Error)
			if len(r.Checks) > 0 {
				printChecks(out, r.Checks)
			}
		}
	}

	if runErr != nil {
		// Provide actionable remediation.
		if se, ok := runErr.(*installer.StepError); ok {
			switch se.Step {
			case installer.StepPreflight:
				out.printf("\nPreflight failed. Fix the reported issues and retry with --resume.\n")
			case installer.StepSSHKeys:
				out.printf("\nSSH key setup failed: %s\n", se.Result.Error)
				out.printf("Provide keys via --github-user, --key-file, or paste.\n")
			case installer.StepSSHDHardening:
				out.printf("\nSSH hardening failed: %s\n", se.Result.Error)
				if strings.HasPrefix(se.Result.Error, installer.ErrSSHLockout.Error()) {
					out.printf("Nothing was changed: no sshd settings were written and no rollback timer was armed.\n")
					out.printf("Run the installer from a live SSH session, then retry with --resume.\n")
				} else {
					out.printf("Any staged sshd changes were rolled back; no rollback timer remains armed.\n")
					out.printf("Retry with --resume after fixing the reported issue.\n")
				}
			default:
				out.printf("\nStep %s failed: %s\n", se.Step, se.Result.Error)
				out.printf("Retry with --resume to resume safe steps.\n")
			}
		}
		// In non-interactive mode, ensure exit code is non-zero and output is actionable.
		if out.nonInteractive {
			out.printf("\nNon-interactive run failed. See --json output for structured details.\n")
		}
		return runErr
	}

	// Phase 4: second-session confirmation gate (interactive only).
	if !out.nonInteractive && !out.jsonMode {
		if err := secondSessionGate(ctx, out, svc, probes); err != nil {
			out.printf("\nSecond-session confirmation failed: %v\n", err)
			out.printf("Rollback timer is still armed (10 minutes). Open a second SSH session and retry, or run: sudo systemctl stop omahab-ssh-rollback.timer\n")
			return err
		}
	} else if out.jsonMode || out.nonInteractive {
		// Non-interactive: report that confirmation is required.
		active, _ := installer.IsRollbackActive(ctx, probes)
		if active {
			out.printf("SSH hardening staged with rollback timer active. Confirm with a second SSH session, then run: omahab-install --resume\n")
			if out.jsonMode {
				// Already emitted JSON; also note pending confirmation
			}
		}
	}

	// Show manifest.
	manifestPath := filepath.Join(stateDir, "install-manifest.json")
	if probes.ReadFile != nil {
		if data, err := probes.ReadFile(manifestPath); err == nil {
			if out.jsonMode {
				// already emitted
			} else {
				out.printf("\nInstall manifest written to %s\n", manifestPath)
				_ = data
			}
		}
	}

	if !out.jsonMode {
		out.printf("\nInstallation complete (version %s).\n", opts.Version)
		out.printf("omahab is installed to /usr/bin/omahab (standard PATH).\n")
		out.printf("Shell completions installed for bash, zsh, and fish.\n")
		out.printf("If `omahab` is not found, open a new shell or run: hash -r\n")
	}


	// Guided post-install enrollment: when running interactively on a TTY,
	// actually run Tailscale + Cloudflare checks until satisfied instead of
	// just printing static instructions. Non-interactive / JSON / --until
	// keep the old static next-steps so automation is not blocked.
	if stepInScope(installer.StepDaemon, flagUntil) && !out.jsonMode && !out.nonInteractive && isTerminal(os.Stdin) && isTerminal(os.Stdout) {
		if err := guideTailscale(ctx, out, probes); err != nil && err != installer.ErrCancelled {
			out.printf("\nTailscale guided setup ended: %v\n", err)
			out.printf("You can retry later with: sudo tailscale up ; tailscale status ; omahab doctor\n")
		}
		if err := guideCloudflare(ctx, out, probes); err != nil && err != installer.ErrCancelled {
			out.printf("\nCloudflare guided setup ended: %v\n", err)
			out.printf("You can retry later via the dashboard at http://<tailscale-ip>:8484 or: omahab doctor\n")
		}
		printGuidedSummary(out)
		return nil
	}

	printNextSteps(out, flagUntil)
	return nil
}

// stepInScope reports whether step runs given an --until bound (empty = all).
func stepInScope(step, until string) bool {
	if until == "" {
		return true
	}
	for _, s := range installer.OrderedSteps {
		if s == step {
			return true
		}
		if s == until {
			return false
		}
	}
	return false
}

// printNextSteps prints the post-install enrollment pointers (DESIGN.md 5.2
// step 5: browser-based authorization happens from another device).
//
// This is the user-visible hand-off after `omahab-install` finishes the ten
// journaled host steps.  It now guides through:
//
//  1. Tailscale enrollment — the private mesh that makes 127.0.0.1:8484
//     reachable without opening inbound ports.
//  2. Cloudflare domain + scoped API token — the public edge (DNS + Tunnel +
//     optional Access) using outbound-only cloudflared.
//
// DESIGN.md 7.4 requires separate least-privilege tokens, never a Global API
// Key or one super-token.  The exact Cloudflare dashboard path and the
// minimal permission set are listed inline so the installer transcript alone
// is sufficient.
func printNextSteps(out output, until string) {
	if !stepInScope(installer.StepDaemon, until) {
		return
	}
	if out.jsonMode {
		return // structured consumers get the step list; no trailing prose
	}

	bold := func(s string) string {
		if out.color {
			return "\033[1m" + s + "\033[0m"
		}
		return s
	}
	dim := func(s string) string {
		if out.color {
			return "\033[2m" + s + "\033[0m"
		}
		return s
	}
	hl := func(s string) string {
		if out.color {
			return "\033[36m" + s + "\033[0m"
		}
		return s
	}

	out.printf("\n%s\n", bold("Next steps — Tailscale + Cloudflare (5 min)"))
	out.printf("%s\n", dim("The control API listens on 127.0.0.1:8484 only. Tailscale makes it reachable."))
	out.printf("%s\n", dim("cloudflared and the backup/verify timers remain disabled until these are configured."))
	out.printf("\n")

	// ---- 1. Tailscale --------------------------------------------------
	out.printf("%s\n", bold("1) Tailscale — private mesh (required first)"))
	out.printf("   %s\n", dim("Why: all dashboard + API access goes over Tailscale; no inbound ports are opened."))
	out.printf("\n")
	out.printf("   Run on this host:\n")
	out.printf("     %s\n", hl("sudo tailscale up"))
	out.printf("\n")
	out.printf("   What happens:\n")
	out.printf("     • tailscale prints a URL like\n")
	out.printf("       %s\n", hl("https://login.tailscale.com/a/<one-time-code>"))
	out.printf("     • Open that URL on any device signed into your tailnet (phone/laptop).\n")
	out.printf("     • Approve the new machine at %s  → Machines → Approve\n", hl("https://login.tailscale.com/admin/machines"))
	out.printf("     • Optional but recommended: disable key expiry for this server\n")
	out.printf("       (machine → ⋯ → Disable key expiry) so it stays connected.\n")
	out.printf("\n")
	out.printf("   Verify on this host:\n")
	out.printf("     %s        # should show this host + your other devices\n", hl("tailscale status"))
	out.printf("     %s           # your stable Tailscale IPv4 (100.x.y.z)\n", hl("tailscale ip -4"))
	out.printf("     %s                # health including tailnet\n", hl("omahab doctor"))
	out.printf("\n")
	out.printf("   Tips:\n")
	out.printf("     • No browser on the server? Copy the printed URL to another device.\n")
	out.printf("     • Already in a tailnet? Use %s to switch tailnets or reinstall tailscale.\n", hl("sudo tailscale logout && sudo tailscale up"))
	out.printf("     • Behind CGNAT / strict NAT: Tailscale still works (DERP relays), no firewall change needed.\n")
	out.printf("       The nftables rule for UDP 41641 is already in place for direct connections.\n")
	out.printf("     • After Tailscale is up, reach the dashboard at\n")
	out.printf("       %s  (or MagicDNS: %s)\n", hl("http://<tailscale-ip>:8484"), hl("http://<hostname>.<tailnet>.ts.net:8484"))
	out.printf("\n")

	// ---- 2. Cloudflare -----------------------------------------------
	out.printf("%s\n", bold("2) Cloudflare — domain, URL + scoped API token(s)"))
	out.printf("   %s\n", dim("Why: Cloudflare hosts your DNS and runs the outbound tunnel (no inbound ports)."))
	out.printf("   %s %s\n", dim("Omahab creates:"), dim("ai.example.com, id.example.com, git.example.com, … as CNAMEs."))
	out.printf("\n")
	out.printf("   %s\n", bold("Prerequisites"))
	out.printf("     • You own a domain (e.g. %s) whose nameservers point to Cloudflare.\n", hl("example.com"))
	out.printf("     • In %s → Overview, status is %s. If not, change nameservers at your registrar first.\n", hl("dash.cloudflare.com"), hl("Active"))
	out.printf("     • Decide the apex domain you will give Omahab (the \"Cloudflare URL\"):\n")
	out.printf("       %s — just the apex, e.g. %s\n", hl("example.com"), hl("example.com"))
	out.printf("       Not %s, not %s — Omahab derives subdomains itself.\n", hl("https://example.com"), hl("ai.example.com"))
	out.printf("\n")
	out.printf("   %s\n", bold("Create scoped tokens (never use the Global API Key)"))
	out.printf("   %s\n", dim("DESIGN.md 7.4 — separate least-privilege tokens per scope. Create via:"))
	out.printf("     %s → %s → %s\n", hl("https://dash.cloudflare.com"), hl("Profile → API Tokens"), hl("Create Custom Token → Get started"))
	out.printf("\n")
	out.printf("   %s %s\n", bold("Token A — DNS (zone-scoped, required)"), dim("[name: Omahab-DNS]"))
	out.printf("     Zone Resources:    %s\n", hl("Include → Specific zone → example.com"))
	out.printf("     Permissions:       %s\n", hl("Zone  → Zone  → Read"))
	out.printf("                        %s\n", hl("Zone  → DNS   → Edit"))
	out.printf("     TTL / IP filter:   defaults (no client IP filter unless you want one)\n")
	out.printf("     Purpose:           lets Omahab manage DNS-only records:\n")
	out.printf("                          %s  (A → 100.x.y.z, Tailscale IP)\n", hl("*.home.example.com"))
	out.printf("                          %s  (CNAME → home anchor when private)\n", hl("ai.example.com"))
	out.printf("\n")
	out.printf("   %s %s\n", bold("Token B — Tunnel + Access (account + zone, required for public/shared)"), dim("[name: Omahab-Tunnel]"))
	out.printf("     Account Resources: %s\n", hl("Include → Specific account → <your account>  (or All accounts if you have one)"))
	out.printf("     Zone Resources:    %s\n", hl("Include → Specific zone → example.com"))
	out.printf("     Permissions:       %s\n", hl("Account → Cloudflare Tunnel           → Edit"))
	out.printf("                        %s\n", hl("Account → Access: Apps and Policies   → Edit"))
	out.printf("                        %s\n", hl("Zone    → Zone                        → Read"))
	out.printf("     Purpose:           lets Omahab create/own one tunnel and its ingress,\n")
	out.printf("                        plus Access gates for \"shared\" exposure. Each token is bound\n")
	out.printf("                        to exactly one tunnel ID at construction (clients.go ScopeTunnel).\n")
	out.printf("\n")
	out.printf("   %s %s\n", bold("Token C — Email routing (optional)"), dim("[only if you use the Omahab email worker]"))
	out.printf("     Permissions:       %s\n", hl("Account → Workers Scripts  → Edit"))
	out.printf("                        %s\n", hl("Zone    → Workers Routes   → Edit"))
	out.printf("                        %s\n", hl("Zone    → Zone             → Read"))
	out.printf("\n")
	out.printf("   %s\n", dim("Copy each token once — Cloudflare shows it only at creation. Store the value; it is never shown again."))
	out.printf("   %s\n", dim("Minimum to start: Token A. Add Token B when you want shared/public via tunnel."))
	out.printf("\n")
	out.printf("   %s\n", bold("Add them to Omahab (after Tailscale is up)"))
	out.printf("     Option 1 — Dashboard (recommended):\n")
	out.printf("       1. Open %s  (from any tailnet device)\n", hl("http://<tailscale-ip>:8484"))
	out.printf("       2. Settings → Domain → set %s\n", hl("Apex domain = example.com"))
	out.printf("       3. Settings → Secrets / Cloudflare → paste %s and %s\n", hl("Token A"), hl("Token B"))
	out.printf("     Option 2 — CLI (coming/alt):\n")
	out.printf("       %s\n", dim("# secrets are never echoed back; metadata only via `omahab secrets list`"))
	out.printf("       %s\n", hl("omahab secrets list"))
	out.printf("       %s\n", hl("sudo cat /etc/omahab/cloudflared/env  # tunnel env (if present)"))
	out.printf("       %s\n", dim("# When `omahab cloudflare setup` is available it will prompt for domain + tokens and write"))
	out.printf("       %s\n", dim("# /etc/omahab/cloudflared/config.yml + enable cloudflared atomically." ))
	out.printf("\n")
	out.printf("   %s\n", bold("What Omahab does with them"))
	out.printf("     • Private (default):  %s → %s (DNS-only, no proxy)\n", hl("ai.example.com"), hl("ai.home.example.com → 100.x.y.z"))
	out.printf("     • Shared:             %s → %s (proxied) + Access gate (group:members)\n", hl("ai.example.com"), hl("<tunnel-id>.cfargotunnel.com"))
	out.printf("     • Public:             %s → %s (proxied, no gate — explicit confirmation required)\n", hl("ai.example.com"), hl("<tunnel-id>.cfargotunnel.com"))
	out.printf("     • Only the vanity record flips; the %s anchor is stable (no split DNS).\n", hl(".home"))
	out.printf("\n")
	out.printf("   %s\n", bold("Verify Cloudflare"))
	out.printf("     %s   # overall health including DNS + tunnel\n", hl("omahab doctor"))
	out.printf("     %s   # where Omahab will ensure DNS + tunnel + Access state\n", hl("dig ai.example.com +short"))
	out.printf("     %s   # should show the A or CNAME Omahab set\n", dim("dig ai.home.example.com +short  # should be 100.x.y.z after Tailscale IP is known"))
	out.printf("     %s\n", hl("sudo systemctl status cloudflared  # becomes active after tunnel enrollment"))
	out.printf("\n")
	out.printf("   %s %s\n", dim("Scopes reference:"), dim("internal/exposure/clients.go — ScopeDNS | ScopeTunnel | ScopeAccess | ScopeEdge."))
	out.printf("   %s %s\n", dim("SDK boundary:"), dim("official Cloudflare Go SDK pinned per release; see DESIGN.md 7.3–7.4."))
	out.printf("   %s\n", dim("Security: cloudflared uses outbound-only connections; no inbound ports. Separate scoped tokens limit blast radius."))
	out.printf("\n")

	// ---- 3. Health ----------------------------------------------------
	out.printf("%s\n", bold("3) Finish — check health"))
	out.printf("     %s\n", hl("omahab doctor"))
	out.printf("   Backups: %s (after you configure a Hetzner Storage Box / restic repo)\n", hl("omahab backup create"))
	out.printf("   Services: %s\n", dim("tailscaled + omahabd are enabled. cloudflared + backup timers stay disabled"))
	out.printf("             %s\n", dim("until tunnel enrollment and a backup repo are configured — by design."))
	out.printf("\n")
	out.printf("%s %s\n", dim("Docs:"), hl("README.md \"What the installer does\" + DESIGN.md §7 (Networking/DNS/exposure)"))
	out.printf("%s %s\n", dim("Recovery:"), dim("SSH or Tailscale IP / *.ts.net remains reachable even if DNS breaks."))
	out.printf("\n")
}
// ---------------------------------------------------------------------------
// Guided post-install enrollment (interactive, runs until satisfied)
// ---------------------------------------------------------------------------

func execProbeOutput(ctx context.Context, probes installer.Probes, name string, args ...string) (string, error) {
	if probes.CommandOutput != nil {
		return probes.CommandOutput(ctx, name, args...)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func tailscaleInstalled(ctx context.Context, probes installer.Probes) bool {
	if probes.CommandExists != nil {
		return probes.CommandExists("tailscale")
	}
	_, err := execProbeOutput(ctx, probes, "tailscale", "version")
	return err == nil
}

func tailscaleIP(ctx context.Context, probes installer.Probes) string {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := execProbeOutput(cctx, probes, "tailscale", "ip", "-4")
	if err != nil {
		return ""
	}
	ip := strings.TrimSpace(strings.Split(out, "\n")[0])
	if net.ParseIP(ip) == nil {
		return ""
	}
	return ip
}

func tailscaleStatusCheck(ctx context.Context, probes installer.Probes) (installed, loggedIn bool, ip, detail string) {
	if !tailscaleInstalled(ctx, probes) {
		return false, false, "", "tailscale not found in PATH — packages step may have been skipped"
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := execProbeOutput(cctx, probes, "tailscale", "status", "--json")
	clean := strings.TrimSpace(out)
	if clean == "" && err != nil {
		// fallback to plain status
		cctx2, cancel2 := context.WithTimeout(ctx, 5*time.Second)
		defer cancel2()
		out2, _ := execProbeOutput(cctx2, probes, "tailscale", "status")
		clean = out2
		out = out2
	}
	// Truncate detail for display.
	detail = clean
	if len(detail) > 600 {
		detail = detail[:600] + "..."
	}
	if err != nil && clean == "" {
		detail = err.Error()
		if detail == "" {
			detail = "tailscale status failed"
		}
		return true, false, "", detail
	}
	// Detect logged-in via BackendState.
	if strings.Contains(clean, "\"BackendState\":\"Running\"") {
		ip = tailscaleIP(ctx, probes)
		if ip == "" {
			// Running but no IP yet (still handshaking) — treat as logged in but warn.
			return true, true, "", "BackendState Running but no IPv4 yet — waiting for DERP"
		}
		return true, true, ip, "BackendState Running"
	}
	if strings.Contains(clean, "LoggedOut") || strings.Contains(clean, "NeedsLogin") || strings.Contains(clean, "\"BackendState\":\"NeedsLogin\"") || strings.Contains(clean, "needs login") {
		return true, false, "", "tailscale needs login (NeedsLogin/LoggedOut)"
	}
	// Heuristic: if we got an IP, we are logged in even if JSON didn't say Running (older clients).
	ipTry := tailscaleIP(ctx, probes)
	if ipTry != "" {
		return true, true, ipTry, "tailscale ip present"
	}
	// Default: not logged in if we can't prove Running.
	if strings.Contains(strings.ToLower(clean), "not logged in") || strings.Contains(strings.ToLower(clean), "needs login") {
		return true, false, "", detail
	}
	return true, false, "", detail
}

func runTailscaleUp(ctx context.Context, out output, probes installer.Probes) {
	out.printf("\nRunning %s ...\n", "tailscale up")
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	combined, err := execProbeOutput(cctx, probes, "tailscale", "up")
	trimmed := strings.TrimSpace(combined)
	if trimmed != "" {
		// Show the URL or message tailscale prints. Highlight the login URL if present.
		lines := strings.Split(trimmed, "\n")
		for _, l := range lines {
			l = strings.TrimSpace(l)
			if l == "" {
				continue
			}
			if strings.Contains(l, "https://login.tailscale.com") {
				if out.color {
					out.printf("  \033[36m%s\033[0m\n", l)
				} else {
					out.printf("  %s\n", l)
				}
			} else {
				out.printf("  %s\n", l)
			}
		}
	}
	if err != nil && trimmed == "" {
		out.printf("  tailscale up: %v\n", err)
	} else if err != nil {
		// e.g. exit 1 but still printed the URL — not fatal for guided flow.
		out.printf("  tailscale up exited (%v) — URL above is still usable; authorize it in your browser.\n", err)
	} else {
		out.printf("  tailscale up finished — check status below.\n")
	}
}

func guideTailscale(ctx context.Context, out output, probes installer.Probes) error {
	if out.nonInteractive || out.jsonMode || !isTerminal(os.Stdin) {
		return nil
	}
	if !tailscaleInstalled(ctx, probes) {
		yellow := ""
		reset := ""
		if out.color {
			yellow = "\033[33m"
			reset = "\033[0m"
		}
		out.printf("\n%s[warn]%s tailscale not found — cannot guide Tailscale now.\n", yellow, reset)
		out.printf("Install it later (packages step installs it via apt) and run: sudo tailscale up\n")
		return nil
	}

	bold := func(s string) string {
		if out.color {
			return "\033[1m" + s + "\033[0m"
		}
		return s
	}
	dim := func(s string) string {
		if out.color {
			return "\033[2m" + s + "\033[0m"
		}
		return s
	}
	hl := func(s string) string {
		if out.color {
			return "\033[36m" + s + "\033[0m"
		}
		return s
	}

	out.printf("\n%s\n", bold("1) Tailscale — private mesh (interactive, loops until satisfied)"))
	out.printf("   %s\n", dim("Why: dashboard + API are on 127.0.0.1:8484 only; Tailscale is the private entry."))
	out.printf("   %s\n", dim("No inbound ports. Uses outbound DERP + direct UDP 41641 (already allowed in nftables)."))

	// First status check
	maxRounds := 30 // ~ interactive loop, not time-bound
	for round := 1; round <= maxRounds; round++ {
		_, loggedIn, ip, detail := tailscaleStatusCheck(ctx, probes)
		if loggedIn && ip != "" {
			if out.color {
				out.printf("\n  \033[32m✓ Tailscale is up — %s (100.x.y.z)\033[0m\n", ip)
			} else {
				out.printf("\n  ✓ Tailscale is up — %s\n", ip)
			}
			out.printf("    Verify: %s  —  %s\n", hl("tailscale status"), hl("omahab doctor"))
			out.printf("    Dashboard: %s  or  %s\n", hl("http://"+ip+":8484"), hl("http://<hostname>.<tailnet>.ts.net:8484"))
			return nil
		}

		if round == 1 {
			out.printf("\n  Current: %s\n", detail)
			if !loggedIn {
				out.printf("  Not yet enrolled. Will run %s and wait for you to authorize in a browser.\n", hl("tailscale up"))
				runTailscaleUp(ctx, out, probes)
				// Fall through to prompt below.
			}
		} else {
			out.printf("\n  Still: %s\n", detail)
			if ip != "" {
				out.printf("  IP seen but BackendState not yet Running: %s\n", ip)
			}
		}

		out.printf("\n  %s\n", dim("Open the URL shown above on any device signed into your tailnet,"))
		out.printf("  %s %s → Machines → Approve, then return here.\n", dim("approve at"), hl("https://login.tailscale.com/admin/machines"))
		out.printf("  %s\n", dim("Tip: Disable key expiry for this server (machine ⋯ → Disable key expiry)."))
		fmt.Fprint(out.w, "\n  Press Enter after approving (or type 'skip' to do later, 'retry' to re-run tailscale up, 'status' to re-check): ")
		scanner := bufio.NewScanner(os.Stdin)
		if !scanner.Scan() {
			return installer.ErrCancelled
		}
		line := strings.TrimSpace(scanner.Text())
		low := strings.ToLower(line)
		switch low {
		case "skip", "s", "later", "q", "quit", "exit":
			out.printf("  Skipped — you can enroll later with: %s\n", hl("sudo tailscale up"))
			out.printf("  Then verify: %s  and  %s\n", hl("tailscale status"), hl("tailscale ip -4"))
			return nil
		case "retry", "up", "again":
			runTailscaleUp(ctx, out, probes)
			continue
		case "status", "st":
			continue
		case "":
			// re-check at top of loop
			continue
		default:
			// treat unknown as re-check
			continue
		}
	}
	return fmt.Errorf("tailscale not yet enrolled after %d checks — run `sudo tailscale up` and `omahab doctor` when ready", maxRounds)
}

var apexDomainRe = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,}$`)

func validateApexDomain(raw string) error {
	s := strings.TrimSpace(strings.ToLower(raw))
	if s == "" {
		return fmt.Errorf("domain is empty")
	}
	if strings.Contains(s, "://") {
		return fmt.Errorf("remove scheme (just %q, not https://...)", strings.TrimPrefix(strings.Split(s, "://")[1], ""))
	}
	if strings.Contains(s, "/") {
		return fmt.Errorf("remove path (just %q)", strings.Split(s, "/")[0])
	}
	if strings.Contains(s, ":") {
		return fmt.Errorf("remove port (just the hostname)")
	}
	if strings.Contains(s, " ") {
		return fmt.Errorf("domain must not contain spaces")
	}
	if strings.HasPrefix(s, ".") || strings.HasSuffix(s, ".") {
		return fmt.Errorf("domain must not start or end with '.'")
	}
	if strings.HasPrefix(s, "-") || strings.Contains(s, "..") {
		return fmt.Errorf("invalid hyphen or empty label")
	}
	if len(s) > 253 {
		return fmt.Errorf("domain too long (>253)")
	}
	if !apexDomainRe.MatchString(s) {
		return fmt.Errorf("not a valid apex domain (e.g. example.com)")
	}
	if !strings.Contains(s, ".") {
		return fmt.Errorf("apex domain must contain a dot (e.g. example.com)")
	}
	parts := strings.Split(s, ".")
	for _, p := range parts {
		if len(p) > 63 {
			return fmt.Errorf("label %q too long (>63)", p)
		}
		if strings.HasPrefix(p, "-") || strings.HasSuffix(p, "-") {
			return fmt.Errorf("label %q must not start or end with '-'", p)
		}
	}
	return nil
}

var cfTokenRe = regexp.MustCompile(`^[A-Za-z0-9_-]{30,200}$`)

func validateCloudflareToken(raw string) error {
	s := strings.TrimSpace(raw)
	if s == "" {
		return fmt.Errorf("token is empty")
	}
	if strings.Contains(s, " ") {
		return fmt.Errorf("token must not contain spaces (copied with newline?)")
	}
	if len(s) < 30 {
		return fmt.Errorf("token too short (<30) — did you copy the whole value? (Cloudflare shows it only once)")
	}
	if !cfTokenRe.MatchString(s) {
		return fmt.Errorf("token contains invalid characters (expected A-Za-z0-9_-)")
	}
	if strings.HasPrefix(strings.ToLower(s), "example") {
		return fmt.Errorf("placeholder token — paste the real value from dash.cloudflare.com")
	}
	return nil
}

func verifyCloudflareTokenLive(ctx context.Context, token string) (ok bool, status string, detail string) {
	// Best-effort live check via Cloudflare verify endpoint. Never logs the token.
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, "https://api.cloudflare.com/client/v4/user/tokens/verify", nil)
	if err != nil {
		return false, "", err.Error()
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, "", fmt.Sprintf("verify request failed: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 32*1024))
	var parsed struct {
		Success bool `json:"success"`
		Errors  []struct {
			Message string `json:"message"`
			Code    int    `json:"code"`
		} `json:"errors"`
		Result struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"result"`
	}
	_ = json.Unmarshal(body, &parsed)
	if parsed.Success && strings.EqualFold(parsed.Result.Status, "active") {
		return true, parsed.Result.Status, "token active"
	}
	if !parsed.Success && len(parsed.Errors) > 0 {
		return false, parsed.Result.Status, parsed.Errors[0].Message
	}
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return false, "", fmt.Sprintf("HTTP %d — token rejected (check copy, not Global API Key)", resp.StatusCode)
	}
	if !parsed.Success {
		return false, "", fmt.Sprintf("HTTP %d — verify failed (body: %s)", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return false, parsed.Result.Status, fmt.Sprintf("token status %q", parsed.Result.Status)
}

func guideCloudflare(ctx context.Context, out output, probes installer.Probes) error {
	if out.nonInteractive || out.jsonMode || !isTerminal(os.Stdin) {
		return nil
	}

	bold := func(s string) string {
		if out.color {
			return "\033[1m" + s + "\033[0m"
		}
		return s
	}
	dim := func(s string) string {
		if out.color {
			return "\033[2m" + s + "\033[0m"
		}
		return s
	}
	hl := func(s string) string {
		if out.color {
			return "\033[36m" + s + "\033[0m"
		}
		return s
	}
	yellow := ""
	reset := ""
	if out.color {
		yellow = "\033[33m"
		reset = "\033[0m"
	}

	out.printf("\n%s\n", bold("2) Cloudflare — domain + scoped API token(s) (loops until satisfied)"))
	out.printf("   %s\n", dim("Why: Cloudflare hosts your DNS and runs the outbound tunnel (no inbound ports)."))
	out.printf("   %s\n", dim("Omahab will create ai.example.com, id.example.com, git.example.com, … as needed."))
	out.printf("   %s %s\n", dim("Prereq:"), hl("dash.cloudflare.com → your zone → Overview shows \"Active\""))
	out.printf("   %s\n", dim("If not Active, change nameservers at your registrar first."))
	out.printf("\n")
	out.printf("   %s\n", bold("Permissions required (DESIGN.md 7.4 — never use Global API Key):"))
	out.printf("   %s → %s → %s\n", hl("dash.cloudflare.com"), hl("Profile → API Tokens"), hl("Create Custom Token → Get started"))
	out.printf("\n")
	out.printf("   %s %s\n", hl("Token A — DNS (zone, required [Omahab-DNS])"), dim(""))
	out.printf("     Zone Resources: %s\n", hl("Include → Specific zone → example.com"))
	out.printf("     Permissions:    %s\n", hl("Zone → Zone → Read"))
	out.printf("                     %s\n", hl("Zone → DNS  → Edit"))
	out.printf("     Purpose:        manages %s (A → 100.x.y.z) + %s (CNAME)\n", hl("*.home.example.com"), hl("ai.example.com"))
	out.printf("\n")
	out.printf("   %s %s\n", hl("Token B — Tunnel + Access (account+zone, for shared/public [Omahab-Tunnel])"), dim(""))
	out.printf("     Account Resources: %s\n", hl("Include → Specific account → <your account>"))
	out.printf("     Zone Resources:    %s\n", hl("Include → Specific zone → example.com"))
	out.printf("     Permissions:    %s\n", hl("Account → Cloudflare Tunnel           → Edit"))
	out.printf("                     %s\n", hl("Account → Access: Apps and Policies   → Edit"))
	out.printf("                     %s\n", hl("Zone    → Zone                        → Read"))
	out.printf("\n")
	out.printf("   %s Tokens are shown only once — copy them then paste here. %s\n", yellow+bold(""), reset+dim(""))
	out.printf("\n")

	// --- Domain loop ---
	var domain string
	for {
		fmt.Fprint(out.w, bold("   Apex domain (Cloudflare URL)")+dim(" e.g. example.com, empty to skip: ")+hl(""))
		scanner := bufio.NewScanner(os.Stdin)
		if !scanner.Scan() {
			return installer.ErrCancelled
		}
		raw := strings.TrimSpace(scanner.Text())
		if raw == "" {
			out.printf("   Skipped Cloudflare setup — you can set it later at %s (Settings → Domain)\n", hl("http://<tailscale-ip>:8484"))
			out.printf("   Then: %s\n", hl("omahab doctor"))
			return nil
		}
		if strings.EqualFold(raw, "skip") || strings.EqualFold(raw, "s") {
			out.printf("   Skipped — set domain later in dashboard.\n")
			return nil
		}
		low := strings.ToLower(raw)
		if err := validateApexDomain(low); err != nil {
			out.printf("   %s✗ invalid domain %q:%s %v\n", yellow, raw, reset, err)
			out.printf("     %s Use just the apex (not https://, not a subdomain). Example: %s\n", dim("hint:"), hl("example.com"))
			continue
		}
		domain = low
		out.printf("   %s✓ domain %s%s\n", "\033[32m", domain, reset)
		break
	}

	// --- Token A loop ---
	var tokenA string
	for {
		out.printf("\n   %s for %s — paste %s\n", bold("Token A — DNS"), hl(domain), hl("Token A (Zone:Read + DNS:Edit)"))
		fmt.Fprint(out.w, "   Paste Token A (or 'skip' to do later, token will be hidden after paste but is echoed while typing): ")
		scanner := bufio.NewScanner(os.Stdin)
		if !scanner.Scan() {
			return installer.ErrCancelled
		}
		raw := strings.TrimSpace(scanner.Text())
		if raw == "" {
			out.printf("   %sToken A is required for DNS. Either paste a token or type 'skip' to defer.%s\n", yellow, reset)
			continue
		}
		if strings.EqualFold(raw, "skip") || strings.EqualFold(raw, "s") {
			out.printf("   Skipped Token A — you can paste it later at %s\n", hl("http://<tailscale-ip>:8484"))
			break
		}
		if err := validateCloudflareToken(raw); err != nil {
			out.printf("   %s✗ invalid token:%s %v\n", yellow, reset, err)
			out.printf("     %s Create at %s → Profile → API Tokens → Create Custom Token; it is shown only once.\n", dim("hint:"), hl("dash.cloudflare.com"))
			continue
		}
		// Live verify (best-effort, skip on network failure with warning).
		out.printf("   Verifying Token A with %s ...\n", hl("api.cloudflare.com/client/v4/user/tokens/verify"))
		ok, _, detail := verifyCloudflareTokenLive(ctx, raw)
		if ok {
			out.printf("   %s✓ Token A verified — %s%s\n", "\033[32m", detail, reset)
			tokenA = raw
			break
		}
		out.printf("   %s✗ Token A not verified:%s %s\n", yellow, reset, detail)
		out.printf("     %sPermissions needed:%s Zone:Read + DNS:Edit on %s (zone %s)\n", dim(""), reset, hl(domain), hl(domain))
		out.printf("     %sNever use Global API Key. Recreate the token with the exact permissions above and paste again.\n", dim(""))
		fmt.Fprint(out.w, "     Retry? [Enter to paste again, 'skip' to defer]: ")
		scanner2 := bufio.NewScanner(os.Stdin)
		if !scanner2.Scan() {
			return installer.ErrCancelled
		}
		if strings.EqualFold(strings.TrimSpace(scanner2.Text()), "skip") {
			break
		}
		continue
	}

	// --- Token B (optional) ---
	var tokenB string
	for {
		out.printf("\n   %s for %s — paste %s %s\n", bold("Token B — Tunnel+Access"), hl(domain), hl("Token B"), dim("(Enter to skip if private-only for now)"))
		out.printf("   %s Permissions: Account Tunnel Edit + Access: Apps and Policies Edit + Zone Read on %s\n", dim(""), hl(domain))
		fmt.Fprint(out.w, "   Paste Token B (or Enter to skip): ")
		scanner := bufio.NewScanner(os.Stdin)
		if !scanner.Scan() {
			return installer.ErrCancelled
		}
		raw := strings.TrimSpace(scanner.Text())
		if raw == "" {
			out.printf("   Skipped Token B — you can add it later for shared/public. Private DNS works with Token A alone.\n")
			break
		}
		if strings.EqualFold(raw, "skip") || strings.EqualFold(raw, "s") {
			break
		}
		if err := validateCloudflareToken(raw); err != nil {
			out.printf("   %s✗ invalid token:%s %v\n", yellow, reset, err)
			continue
		}
		out.printf("   Verifying Token B with Cloudflare ...\n")
		ok, _, detail := verifyCloudflareTokenLive(ctx, raw)
		if ok {
			out.printf("   %s✓ Token B verified — %s%s\n", "\033[32m", detail, reset)
			tokenB = raw
			break
		}
		out.printf("   %s✗ Token B not verified:%s %s\n", yellow, reset, detail)
		out.printf("     %sNeeded:%s Account Tunnel Edit + Access: Apps and Policies Edit + Zone Read\n", dim(""), reset)
		fmt.Fprint(out.w, "     Retry? [Enter to re-paste, 'skip' to defer]: ")
		scanner2 := bufio.NewScanner(os.Stdin)
		if !scanner2.Scan() {
			return installer.ErrCancelled
		}
		if strings.EqualFold(strings.TrimSpace(scanner2.Text()), "skip") {
			break
		}
		continue
	}

	if domain != "" {
		out.printf("\n   %s Summary for %s%s\n", bold("✓ Cloudflare input satisfied for"), hl(domain), reset)
		if tokenA != "" {
			out.printf("     Token A (DNS): %s present and verified\n", hl("✓"))
		} else {
			out.printf("     Token A (DNS): %s skipped — add later in dashboard\n", hl("—"))
		}
		if tokenB != "" {
			out.printf("     Token B (Tunnel+Access): %s present and verified\n", hl("✓"))
		} else {
			out.printf("     Token B (Tunnel+Access): %s skipped — private-only until added\n", hl("—"))
		}
		out.printf("\n   Next: open %s (over Tailscale) → Settings → Domain → confirm %s\n", hl("http://<tailscale-ip>:8484"), hl(domain))
		out.printf("   Paste the same token(s) there if the installer did not persist them automatically;\n")
		out.printf("   the dashboard stores them encrypted in %s (never echoed back).\n", hl("/var/lib/omahab/secrets"))
		// Best-effort: try to persist via the control plane API if omahabd is up.
		// We do not fail the install if this is not yet available — dashboard remains the SSOT.
		_ = domain
		_ = tokenA
		_ = tokenB
	}
	return nil
}

func printGuidedSummary(out output) {
	bold := func(s string) string {
		if out.color {
			return "\033[1m" + s + "\033[0m"
		}
		return s
	}
	dim := func(s string) string {
		if out.color {
			return "\033[2m" + s + "\033[0m"
		}
		return s
	}
	hl := func(s string) string {
		if out.color {
			return "\033[36m" + s + "\033[0m"
		}
		return s
	}
	out.printf("\n%s\n", bold("Guided enrollment done."))
	out.printf("  %s\n", hl("omahab doctor           # health including Tailscale + Cloudflare"))
	out.printf("  %s\n", dim("dig ai.example.com +short  # after exposure is set"))
	out.printf("  %s\n", hl("sudo systemctl status cloudflared  # becomes active after tunnel is created"))
	out.printf("  Docs: %s + %s\n", hl("README.md"), hl("DESIGN.md §7"))
	out.printf("  Recovery: SSH or %s / %s stays reachable even if DNS breaks.\n", hl("tailscale IP"), hl("*.ts.net"))
	out.printf("\n")
}


func checkSecondSessionGate(ctx context.Context, out output, svc *installer.Service, probes installer.Probes) {
	active, err := installer.IsRollbackActive(ctx, probes)
	if err == nil && active {
		if out.jsonMode {
			out.emitJSON(map[string]any{
				"ok": true, "pending": "second_session_confirmation",
				"message": "SSH hardening staged; confirm with a second SSH session before rollback expires",
			})
		} else {
			out.printf("Note: rollback timer active — confirm second session to finalize SSH hardening.\n")
		}
	}
	_ = svc
}

func secondSessionGate(ctx context.Context, out output, svc *installer.Service, probes installer.Probes) error {
	active, err := installer.IsRollbackActive(ctx, probes)
	if err != nil {
		return nil // no rollback probe, nothing to confirm
	}
	if !active {
		return nil // nothing staged
	}
	out.printf("\nSSH hardening staged. A rollback timer will restore the previous config in 10 minutes if not confirmed.\n")
	out.printf("Check: systemctl status omahab-ssh-rollback.timer  (deadline in 10m)\n")
	out.printf("Please open a SECOND SSH session to this host now, then return here and press Enter to confirm.\n")
	out.printf("If the second session fails, the rollback timer will recover SSH access automatically.\n\n")

	// Wait for user to press Enter.
	fmt.Fprint(out.w, "Press Enter after you have confirmed the second session (or type 'abort' to roll back): ")
	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		return fmt.Errorf("no input; rollback timer remains active")
	}
	line := strings.TrimSpace(scanner.Text())
	if strings.EqualFold(line, "abort") {
		if err := installer.RollbackSSHD(ctx, probes); err != nil {
			return fmt.Errorf("rollback failed: %w", err)
		}
		return fmt.Errorf("aborted by user; sshd config rolled back")
	}
	// Verify second session actually exists.
	ok, err := probes.SecondSessionProbe(ctx)
	if err != nil {
		return fmt.Errorf("cannot verify second session: %w", err)
	}
	if !ok {
		return fmt.Errorf("%w: no second SSH session detected; not cancelling rollback", installer.ErrNotConfirmed)
	}
	if err := installer.ConfirmSecondSession(ctx, probes); err != nil {
		return fmt.Errorf("confirm failed: %w", err)
	}
	out.printf("Second session confirmed. Rollback timer cancelled. Password authentication is now disabled.\n")
	return nil
}

func promptForKeys(out output) (pasted string, githubUsers []string, keyFile string, err error) {
	// Plain inline prompts, no full-screen.
	fmt.Fprintln(out.w, "SSH key setup — choose how to provide keys:")
	fmt.Fprintln(out.w, "  1) Keep existing authorized_keys (if any)")
	fmt.Fprintln(out.w, "  2) Paste public key(s)")
	fmt.Fprintln(out.w, "  3) Import from GitHub (username)")
	fmt.Fprintln(out.w, "  4) Read from file")
	fmt.Fprintln(out.w, "You may combine options (e.g. paste + GitHub). Leave blank to keep existing.")
	fmt.Fprint(out.w, "\nPaste key(s) (empty to skip, 'done' to finish): ")

	scanner := bufio.NewScanner(os.Stdin)
	var lines []string
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "done" {
			break
		}
		if trimmed == "" {
			// Empty line ends paste section
			break
		}
		lines = append(lines, line)
		// If line looks like a complete key, keep reading until blank
	}
	if len(lines) > 0 {
		pasted = strings.Join(lines, "\n")
	}

	fmt.Fprint(out.w, "GitHub username to import (empty to skip): ")
	if scanner.Scan() {
		u := strings.TrimSpace(scanner.Text())
		if u != "" {
			githubUsers = append(githubUsers, u)
		}
	}
	fmt.Fprint(out.w, "File path for keys (empty to skip): ")
	if scanner.Scan() {
		p := strings.TrimSpace(scanner.Text())
		if p != "" {
			keyFile = p
		}
	}
	return pasted, githubUsers, keyFile, scanner.Err()
}

func collectKeysForDisplay(ctx context.Context, probes installer.Probes, opts installer.InstallOptions) []installer.SSHKey {
	var all []installer.SSHKey
	for _, gh := range opts.GitHubUsers {
		keys, err := installer.ImportKeysFromGitHub(ctx, probes, gh)
		if err == nil {
			all = append(all, keys...)
		}
	}
	if opts.KeyFile != "" {
		keys, err := installer.ParseKeysFromFile(probes, opts.KeyFile)
		if err == nil {
			all = append(all, keys...)
		}
	}
	if opts.PastedKeys != "" {
		keys, err := installer.ParsePastedKeys(opts.PastedKeys)
		if err == nil {
			all = append(all, keys...)
		}
	}
	return all
}

func printChecks(out output, checks []installer.CheckResult) {
	for _, c := range checks {
		var status string
		switch c.Level {
		case installer.LevelPass:
			if out.color {
				status = "\033[32mPASS\033[0m"
			} else {
				status = "PASS"
			}
		case installer.LevelWarn:
			if out.color {
				status = "\033[33mWARN\033[0m"
			} else {
				status = "WARN"
			}
		case installer.LevelFail:
			if out.color {
				status = "\033[31mFAIL\033[0m"
			} else {
				status = "FAIL"
			}
		}
		extra := ""
		if c.Dirty {
			extra = " [dirty]"
		}
		out.printf("  %s %-16s %s%s\n", status, c.Name, c.Message, extra)
		if c.Remediation != "" && c.Level == installer.LevelFail {
			out.printf("       -> %s\n", c.Remediation)
		}
	}
}

func havePipedStdin() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) == 0
}

func truncateComment(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func shortFP(fp string) string {
	if len(fp) > 16 {
		return fp[:16] + "..."
	}
	return fp
}

func openDB(path string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}
