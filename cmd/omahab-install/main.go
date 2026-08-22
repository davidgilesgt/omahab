package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	_ "modernc.org/sqlite"

	"github.com/omahab/omahab/internal/installer"
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
				out.printf("The rollback timer restores the previous sshd config in 10 minutes if not confirmed.\n")
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
	}
	return nil
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
