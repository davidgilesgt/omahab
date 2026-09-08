package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/omahab/omahab/internal/diskinstall"
	"github.com/omahab/omahab/internal/sshkeys"
	"github.com/omahab/omahab/internal/tui"
)

// newInstallCmd builds `omahab install` guided installer.
func newInstallCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install Omahab to disk (guided)",
		Long: `Guided installer for Omahab.

Booting the ISO shows this command. It walks disk selection, administrator account,
and SSH access without requiring NixOS knowledge. Nothing is erased until you confirm.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("omahab install takes no arguments")
			}
			return runInstallWizard(cmd)
		},
	}
	cmd.AddCommand(newProvisionKeysCmd())
	return cmd
}

// newProvisionKeysCmd is hidden `omahab install provision-keys --root /mnt --username NAME --keys-file PATH`
func newProvisionKeysCmd() *cobra.Command {
	var root, username, keysFile string
	c := &cobra.Command{
		Use:    "provision-keys",
		Short:  "Provision SSH keys into target (hidden, called by installer backend)",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runProvisionKeys(root, username, keysFile)
		},
	}
	c.Flags().StringVar(&root, "root", "", "target root (must be /mnt)")
	c.Flags().StringVar(&username, "username", "", "administrator username")
	c.Flags().StringVar(&keysFile, "keys-file", "", "path to authorized keys file")
	_ = c.MarkFlagRequired("root")
	_ = c.MarkFlagRequired("username")
	_ = c.MarkFlagRequired("keys-file")
	return c
}

// runProvisionKeys implements hidden wiring.
func runProvisionKeys(root, username, keysFile string) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("provision-keys must run as root")
	}
	// Validate username
	if err := diskinstall.ValidateUsername(username); err != nil {
		return fmt.Errorf("invalid username: %w", err)
	}
	// Validate root
	if strings.TrimSpace(root) == "" {
		return fmt.Errorf("root required")
	}
	cleanRoot := filepath.Clean(root)
	if cleanRoot != "/mnt" {
		return fmt.Errorf("root must be /mnt (got %q)", root)
	}
	fi, err := os.Stat(cleanRoot)
	if err != nil {
		return fmt.Errorf("root %q not accessible: %w", cleanRoot, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("root %q is not a directory", cleanRoot)
	}
	// Verify target is mounted session root: check /mnt/etc/passwd exists and is not host's
	if _, err := os.Stat(filepath.Join(cleanRoot, "etc", "passwd")); err != nil {
		return fmt.Errorf("target passwd not found at %q: %w", filepath.Join(cleanRoot, "etc", "passwd"), err)
	}
	// Avoid trivial symlink escape: ensure root is not symlink? Check Lstat
	if isLink, err := isSymlink(cleanRoot); err != nil {
		return fmt.Errorf("stat root: %w", err)
	} else if isLink {
		return fmt.Errorf("root is symlink")
	}
	// Read target passwd
	passwdPath := filepath.Join(cleanRoot, "etc", "passwd")
	data, err := os.ReadFile(passwdPath)
	if err != nil {
		return fmt.Errorf("read target passwd: %w", err)
	}
	uid, gid, home, err := parsePasswdEntry(string(data), username)
	if err != nil {
		return fmt.Errorf("lookup user %q in target: %w", username, err)
	}
	// Resolve home strictly beneath root
	if !filepath.IsAbs(home) {
		return fmt.Errorf("home %q is not absolute", home)
	}
	// Join with root and clean
	targetHome := filepath.Join(cleanRoot, strings.TrimPrefix(filepath.Clean(home), "/"))
	// Ensure targetHome is beneath cleanRoot
	rel, err := filepath.Rel(cleanRoot, targetHome)
	if err != nil {
		return fmt.Errorf("home path error: %w", err)
	}
	if strings.HasPrefix(rel, "..") || rel == "." && home != "/" {
		// rel == "." would mean home == "/", not allowed for admin
		return fmt.Errorf("home %q not strictly beneath %q", home, cleanRoot)
	}
	// Also ensure home string starts with /home/ (expected for admin)
	if !strings.HasPrefix(filepath.Clean(home), "/home/") {
		return fmt.Errorf("home %q must be beneath /home", home)
	}
	// Validate keys-file
	if strings.TrimSpace(keysFile) == "" {
		return fmt.Errorf("keys-file required")
	}
	if _, err := os.Stat(keysFile); err != nil {
		return fmt.Errorf("keys-file not readable: %w", err)
	}
	// Reject symlink for keys-file?
	if isLink, err := isSymlink(keysFile); err == nil && isLink {
		return fmt.Errorf("keys-file is symlink")
	}
	// Read and parse keys
	raw, err := os.ReadFile(keysFile)
	if err != nil {
		return fmt.Errorf("read keys-file: %w", err)
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return fmt.Errorf("keys-file empty")
	}
	keys, err := sshkeys.ParsePastedKeys(string(raw))
	if err != nil {
		return fmt.Errorf("parse keys: %w", err)
	}
	// EnsureAuthorizedKeysAt with target UID/GID
	added, _, err := sshkeys.EnsureAuthorizedKeysAt(targetHome, uid, gid, keys)
	if err != nil {
		return fmt.Errorf("ensure authorized_keys: %w", err)
	}
	// Success — do not log keys
	_ = added
	return nil
}

func parsePasswdEntry(passwd, username string) (int, int, string, error) {
	lines := strings.Split(passwd, "\n")
	for _, line := range lines {
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, ":")
		if len(parts) < 7 {
			continue
		}
		if parts[0] != username {
			continue
		}
		uidStr := parts[2]
		gidStr := parts[3]
		home := parts[5]
		var uid, gid int
		_, err1 := fmt.Sscanf(uidStr, "%d", &uid)
		_, err2 := fmt.Sscanf(gidStr, "%d", &gid)
		if err1 != nil || err2 != nil {
			return 0, 0, "", fmt.Errorf("invalid uid/gid for %q", username)
		}
		if home == "" {
			return 0, 0, "", fmt.Errorf("user %q has no home", username)
		}
		return uid, gid, home, nil
	}
	return 0, 0, "", fmt.Errorf("user %q not found in target passwd", username)
}

func isSymlink(path string) (bool, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	return fi.Mode()&os.ModeSymlink != 0, nil
}

// ---------- wizard ----------

type wizardState struct {
	caps tui.Caps
	in   *bufio.Reader
	out  io.Writer
	errW io.Writer

	sessionDir string
	logPath    string
	logFile    *os.File

	hostname    string
	username    string
	password    string // transient only for hashing, never written
	sshChoice   diskinstall.SSHChoice
	disksResp   *diskinstall.ListDisksResponse
	externalOpt map[string]bool
	systemDisk  string // path

	networkFile string // path to staged network file (root-owned 0600)
	networkMode string // cached mode
}

func runInstallWizard(cmd *cobra.Command) error {
	isTTY := term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
	caps := tui.ResolveCaps(isTTY, os.Getenv("TERM"), os.Getenv("NO_COLOR"))
	out := os.Stdout
	errW := os.Stderr
	in := bufio.NewReader(os.Stdin)

	// Root check
	if os.Geteuid() != 0 {
		fmt.Fprintln(errW, "error: omahab install must run as root (live ISO session is already root)")
		return fmt.Errorf("must run as root")
	}

	// Require interactive TTY unless --help already handled
	if !isTTY {
		// Still allow wizard to run over pipe for tests? Check if stdin is pipe but we allow.
		// If non-interactive without terminal, proceed but warn.
	}

	state := &wizardState{
		caps:        caps,
		in:          in,
		out:         out,
		errW:        errW,
		externalOpt: make(map[string]bool),
	}
	// Setup session dir and log
	sessionDir := "/run/omahab-installer"
	if err := os.MkdirAll(sessionDir, 0700); err != nil {
		return fmt.Errorf("create session dir: %w", err)
	}
	_ = os.Chmod(sessionDir, 0700)
	state.sessionDir = sessionDir
	logPath := filepath.Join(sessionDir, "install.log")
	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return fmt.Errorf("open log: %w", err)
	}
	_ = os.Chmod(logPath, 0600)
	state.logPath = logPath
	state.logFile = lf
	defer func() {
		if lf != nil {
			lf.Close()
		}
	}()
	// Cleanup secrets on exit
	defer state.cleanupSecrets()

	steps := []string{"Welcome", "Connection", "Disks", "Administrator", "SSH access", "Review", "Install", "Ready"}
	_ = steps // for progress rendering

	// Step index for back navigation
	stepIdx := 0

	// Keep loop until done or quit
	for stepIdx < len(steps) {
		step := steps[stepIdx]
		printProgress(state.out, caps, stepIdx+1, len(steps), step)

		var back bool
		var quit bool
		var err error

		switch step {
		case "Welcome":
			err, back, quit = state.stepWelcome()
		case "Connection":
			err, back, quit = state.stepConnection()
		case "Disks":
			err, back, quit = state.stepDisks()
		case "Administrator":
			err, back, quit = state.stepAdministrator()
		case "SSH access":
			err, back, quit = state.stepSSH()
		case "Review":
			err, back, quit = state.stepReview()
		case "Install":
			err, back, quit = state.stepInstall()
		case "Ready":
			err, back, quit = state.stepReady()
		default:
			err = fmt.Errorf("unknown step %q", step)
		}

		if quit {
			fmt.Fprintln(state.out, "\nQuitting installer. No changes have been made before confirmation.")
			return nil
		}
		if back {
			if stepIdx > 0 {
				stepIdx--
			}
			continue
		}
		if err != nil {
			// For non-fatal validation, step functions already loop internally; here is unexpected error
			fmt.Fprintf(state.errW, "error: %v\n", err)
			// For install step failures, do not advance to Ready; stay or offer quit
			if step == "Install" {
				fmt.Fprintln(state.out, "\nInstallation did not complete. The system was not fully installed.")
				fmt.Fprintf(state.out, "See log: %s\n", state.logPath)
				fmt.Fprintln(state.out, "Press Enter to return to Review, or type 'quit' to exit.")
				line, _ := state.readLineWithBack("Choice [Enter/quit]: ", true)
				if strings.EqualFold(strings.TrimSpace(line), "quit") {
					return nil
				}
				// Return to Review for retry/edits
				stepIdx = 5 // Review
				continue
			}
			return err
		}
		// Success: advance
		stepIdx++
		// If we just completed Install, next is Ready which will be shown
		if step == "Install" {
			// Install step already handled success; continue to Ready
		}
	}

	return nil
}

func printProgress(w io.Writer, caps tui.Caps, current, total int, name string) {
	// Compact numbered progress
	line := fmt.Sprintf("Step %d/%d: %s", current, total, name)
	if caps.ColorEnabled {
		style := lipgloss.NewStyle().Bold(true).Foreground(tui.AccentAdaptive)
		// Add bullet
		bullet := lipgloss.NewStyle().Foreground(tui.AccentAdaptive).Render("▶")
		fmt.Fprintf(w, "\n%s %s\n", bullet, style.Render(line))
	} else {
		fmt.Fprintf(w, "\n[%d/%d] %s\n", current, total, name)
		fmt.Fprintln(w, strings.Repeat("-", 40))
	}
}

func printHeading(w io.Writer, caps tui.Caps) {
	title := "Install Omahab"
	if caps.ColorEnabled {
		style := lipgloss.NewStyle().Bold(true).Foreground(tui.AccentAdaptive).Padding(0, 1)
		// Use lipgloss for heading
		fmt.Fprintln(w, style.Render(title))
	} else {
		fmt.Fprintln(w, title)
		fmt.Fprintln(w, strings.Repeat("=", len(title)))
	}
}

// readLineWithBack reads a line, handling back/quit.
// If allowBack is false (secret prompts), back is not recognized.
func (s *wizardState) readLineWithBack(prompt string, allowBack bool) (string, bool) {
	// Print prompt without newline
	fmt.Fprint(s.out, prompt)
	line, err := s.in.ReadString('\n')
	if err != nil && err != io.EOF {
		// treat as empty?
	}
	line = strings.TrimRight(line, "\r\n")
	trimmed := strings.TrimSpace(line)
	lower := strings.ToLower(trimmed)
	if allowBack {
		if lower == "back" {
			return "", true
		}
		if lower == "quit" || lower == "exit" {
			// Signal quit via special return? We handle via separate bool from caller, but here we need to propagate
			// Use convention: return line and let caller check? Instead we set global quit detection via reading again?
			// For simplicity, return line and caller checks for quit strings.
			return trimmed, false
		}
	}
	// Check quit even when allowBack false? We still allow quit? Spec says back/quit at nonsecret prompts, so secrets shouldn't have back/quit.
	// But we handle quit detection in caller.
	return line, false
}

func (s *wizardState) readLine(prompt string) (string, error) {
	fmt.Fprint(s.out, prompt)
	line, err := s.in.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func (s *wizardState) readPassword(prompt string) (string, error) {
	// If stdin is not a terminal, fallback to line read (hidden not possible)
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		fmt.Fprint(s.out, prompt)
		line, err := s.in.ReadString('\n')
		if err != nil && err != io.EOF {
			return "", err
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
	fmt.Fprint(s.out, prompt)
	// Ensure terminal state restored on interrupt
	// term.ReadPassword handles raw mode; we need to ensure newline on success
	b, err := term.ReadPassword(fd)
	fmt.Fprintln(s.out)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (s *wizardState) isQuitInput(line string) bool {
	t := strings.ToLower(strings.TrimSpace(line))
	return t == "quit" || t == "exit"
}

// cleanupSecrets removes staged secret files on all exits.
func (s *wizardState) cleanupSecrets() {
	// Delete password hash file, keys file, network staged keyfile copy, etc.
	// Session dir may contain multiple files; we remove specific known patterns.
	if s.sessionDir == "" {
		return
	}
	// List files in session dir, remove sensitive ones
	entries, err := os.ReadDir(s.sessionDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		// Remove password hash, keys, staged network keyfiles
		if name == "passwd.hash" || name == "authorized_keys" || strings.HasPrefix(name, "nm-") || strings.HasPrefix(name, "network-") || name == "selection.json" || name == "network.json" {
			_ = os.Remove(filepath.Join(s.sessionDir, name))
		}
	}
	// Also networkFile if outside sessionDir? But we keep networkFile within sessionDir per spec
	if s.networkFile != "" && !strings.HasPrefix(s.networkFile, s.sessionDir) {
		// Should not happen, but try to delete staged keyfile reference inside network file if profile
		if nf, err := diskinstall.ReadNetworkFile(s.networkFile); err == nil && nf.Mode == "profile" && nf.Keyfile != "" {
			_ = os.Remove(nf.Keyfile)
		}
	}
}

// ---------- steps ----------

func (s *wizardState) stepWelcome() (error, bool, bool) {
	printHeading(s.out, s.caps)
	fmt.Fprintln(s.out, "")
	fmt.Fprintln(s.out, "This installer will erase the selected disks and install Omahab.")
	fmt.Fprintln(s.out, "All existing partitions and files on those disks will be removed.")
	fmt.Fprintln(s.out, "This is not a secure data-sanitization erase.")
	fmt.Fprintln(s.out, "")
	fmt.Fprintln(s.out, "No encryption is configured. Apps and accounts beyond the")
	fmt.Fprintln(s.out, "Linux administrator are configured later in the WebUI.")
	fmt.Fprintln(s.out, "")
	fmt.Fprintln(s.out, "Nothing is erased until you confirm with ERASE <N> DISKS at Review.")
	fmt.Fprintln(s.out, "")

	for {
		line, _ := s.readLineWithBack("Press Enter to continue, or type 'quit' to exit: ", true)
		if s.isQuitInput(line) {
			return nil, false, true
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			return nil, false, false
		}
		if strings.EqualFold(trimmed, "back") {
			// At welcome, back is quit
			return nil, false, true
		}
		fmt.Fprintln(s.out, "Press Enter to continue.")
	}
}

func (s *wizardState) stepConnection() (error, bool, bool) {
	fmt.Fprintln(s.out, "")
	fmt.Fprintln(s.out, "Checking network connectivity (wired DHCP)...")
	// Try connectivity
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	err := diskinstall.CheckConnectivity(ctx)
	if err == nil {
		fmt.Fprintln(s.out, "Network is connected.")
		// Create network file as wired-dhcp
		nf := diskinstall.NetworkFile{Mode: "wired-dhcp", ConnectionUUID: "", Interface: "", Keyfile: ""}
		path := filepath.Join(s.sessionDir, "network.json")
		if err := diskinstall.WriteNetworkFile(path, nf); err != nil {
			fmt.Fprintf(s.out, "Failed to stage network file: %v\n", err)
			// Allow retry
		} else {
			_ = os.Chmod(path, 0600)
			s.networkFile = path
			s.networkMode = "wired-dhcp"
			fmt.Fprintln(s.out, "Will use wired DHCP on the installed system.")
		}
		// Validate before erasure (wired-dhcp always valid)
		return nil, false, false
	}
	fmt.Fprintf(s.out, "Network check failed: %v\n", err)
	fmt.Fprintln(s.out, "")
	fmt.Fprintln(s.out, "Choose an option:")
	fmt.Fprintln(s.out, "  1) Retry")
	fmt.Fprintln(s.out, "  2) Network setup (nmtui)")
	fmt.Fprintln(s.out, "  3) Quit")

	for {
		line, _ := s.readLineWithBack("Choice [1/2/3, back, quit]: ", true)
		if s.isQuitInput(line) {
			return nil, false, true
		}
		if strings.TrimSpace(line) == "back" {
			return nil, true, false
		}
		switch strings.TrimSpace(line) {
		case "1", "retry", "Retry", "":
			// Retry
			ctx2, cancel2 := context.WithTimeout(context.Background(), 12*time.Second)
			err2 := diskinstall.CheckConnectivity(ctx2)
			cancel2()
			if err2 == nil {
				fmt.Fprintln(s.out, "Network is now connected.")
				nf := diskinstall.NetworkFile{Mode: "wired-dhcp", ConnectionUUID: "", Interface: "", Keyfile: ""}
				path := filepath.Join(s.sessionDir, "network.json")
				_ = diskinstall.WriteNetworkFile(path, nf)
				_ = os.Chmod(path, 0600)
				s.networkFile = path
				s.networkMode = "wired-dhcp"
				return nil, false, false
			}
			fmt.Fprintf(s.out, "Still not connected: %v\n", err2)
			fmt.Fprintln(s.out, "Choose 1) Retry  2) Network setup (nmtui)  3) Quit")
		case "2", "nmtui":
			// Run nmtui foreground
			fmt.Fprintln(s.out, "Launching nmtui (NetworkManager). Use it to connect, then exit to return.")
			nmtuiPath, err := exec.LookPath("nmtui")
			if err != nil {
				// Try common paths
				for _, p := range []string{"/run/current-system/sw/bin/nmtui", "/usr/bin/nmtui"} {
					if _, e := os.Stat(p); e == nil {
						nmtuiPath = p
						err = nil
						break
					}
				}
			}
			if err != nil {
				fmt.Fprintln(s.out, "nmtui not found on this system.")
				fmt.Fprintln(s.out, "Choose 1) Retry  2) Network setup (nmtui)  3) Quit")
				continue
			}
			cmd := exec.Command(nmtuiPath)
			cmd.Stdin = os.Stdin
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			_ = cmd.Run()
			fmt.Fprintln(s.out, "Returned from nmtui.")
			// After nmtui, check connectivity again; if active profile exists, try to stage it
			// Try to detect active NM profile and stage keyfile if needed
			// Simplified: re-check connectivity and stage wired-dhcp if ok, else try profile detection
			ctx3, cancel3 := context.WithTimeout(context.Background(), 12*time.Second)
			err3 := diskinstall.CheckConnectivity(ctx3)
			cancel3()
			if err3 == nil {
				// Try to detect profile for persistence: look for active connection
				// Attempt to run nmcli to get active UUID/interface
				nf, staged := s.tryStageNetworkProfile()
				if staged != "" {
					s.networkFile = staged
					s.networkMode = nf.Mode
					fmt.Fprintln(s.out, "Network configured and verified.")
					return nil, false, false
				}
				// Fallback to wired-dhcp
				nf2 := diskinstall.NetworkFile{Mode: "wired-dhcp", ConnectionUUID: "", Interface: "", Keyfile: ""}
				path := filepath.Join(s.sessionDir, "network.json")
				_ = diskinstall.WriteNetworkFile(path, nf2)
				_ = os.Chmod(path, 0600)
				s.networkFile = path
				s.networkMode = "wired-dhcp"
				fmt.Fprintln(s.out, "Using wired DHCP.")
				return nil, false, false
			}
			fmt.Fprintf(s.out, "Still not connected after nmtui: %v\n", err3)
			fmt.Fprintln(s.out, "Choose 1) Retry  2) Network setup (nmtui)  3) Quit")
		case "3", "quit":
			return nil, false, true
		default:
			fmt.Fprintln(s.out, "Please enter 1, 2, 3, back, or quit.")
		}
	}
}

// tryStageNetworkProfile attempts to detect active NM connection and stage its keyfile.
// Returns NetworkFile and staged network.json path if successful, else not staged.
func (s *wizardState) tryStageNetworkProfile() (diskinstall.NetworkFile, string) {
	// Try nmcli
	nmcli, err := exec.LookPath("nmcli")
	if err != nil {
		for _, p := range []string{"/run/current-system/sw/bin/nmcli", "/usr/bin/nmcli"} {
			if _, e := os.Stat(p); e == nil {
				nmcli = p
				err = nil
				break
			}
		}
		if err != nil {
			return diskinstall.NetworkFile{}, ""
		}
	}
	// Get active connections: nmcli -t -f UUID,DEVICE,STATE,TYPE c show --active
	cmd := exec.Command(nmcli, "-t", "-f", "UUID,DEVICE,STATE,TYPE", "connection", "show", "--active")
	out, err := cmd.Output()
	if err != nil {
		return diskinstall.NetworkFile{}, ""
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var chosenUUID, chosenIFace string
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.Split(line, ":")
		if len(parts) < 4 {
			continue
		}
		uuid := strings.TrimSpace(parts[0])
		dev := strings.TrimSpace(parts[1])
		state := strings.TrimSpace(parts[2])
		// typ := parts[3]
		if state != "activated" {
			continue
		}
		if dev == "" || uuid == "" {
			continue
		}
		// Prefer first activated
		chosenUUID = uuid
		chosenIFace = dev
		break
	}
	if chosenUUID == "" {
		return diskinstall.NetworkFile{}, ""
	}
	// Find keyfile for this UUID under /etc/NetworkManager/system-connections/
	// Look for file containing id or uuid
	connDir := "/etc/NetworkManager/system-connections"
	entries, err := os.ReadDir(connDir)
	if err != nil {
		// Try alternate
		connDir = "/run/NetworkManager/system-connections"
		entries, err = os.ReadDir(connDir)
		if err != nil {
			return diskinstall.NetworkFile{}, ""
		}
	}
	var srcPath string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		p := filepath.Join(connDir, e.Name())
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if strings.Contains(string(data), chosenUUID) {
			srcPath = p
			break
		}
	}
	if srcPath == "" {
		// No persistent file found - likely not a persistent profile (e.g., DHCP without saved)
		// For wired DHCP this is fine; we fallback to wired-dhcp elsewhere.
		return diskinstall.NetworkFile{}, ""
	}
	// Verify persistent credentials: read file, check non-empty and not missing psk for wifi
	data, err := os.ReadFile(srcPath)
	if err != nil || len(data) == 0 {
		return diskinstall.NetworkFile{}, ""
	}
	lower := strings.ToLower(string(data))
	if strings.Contains(lower, "802-11-wireless") || strings.Contains(lower, "wifi") {
		if !strings.Contains(lower, "psk=") && !strings.Contains(lower, "password=") {
			// Wi-Fi lacking persistent creds before erasure should be rejected
			fmt.Fprintln(s.out, "Wi-Fi profile lacks persistent credentials; please ensure Wi-Fi password is saved.")
			return diskinstall.NetworkFile{}, ""
		}
	}
	// Stage keyfile copy 0600 in session dir
	stagedKeyfile := filepath.Join(s.sessionDir, fmt.Sprintf("nm-%s.keyfile", chosenUUID[:8]))
	// Ensure session dir exists
	_ = os.MkdirAll(s.sessionDir, 0700)
	if err := os.WriteFile(stagedKeyfile, data, 0600); err != nil {
		return diskinstall.NetworkFile{}, ""
	}
	_ = os.Chmod(stagedKeyfile, 0600)
	// Create network.json
	nf := diskinstall.NetworkFile{
		Mode:           "profile",
		ConnectionUUID: chosenUUID,
		Interface:      chosenIFace,
		Keyfile:        stagedKeyfile,
	}
	path := filepath.Join(s.sessionDir, "network.json")
	if err := diskinstall.WriteNetworkFile(path, nf); err != nil {
		_ = os.Remove(stagedKeyfile)
		return diskinstall.NetworkFile{}, ""
	}
	_ = os.Chmod(path, 0600)
	// Validate before proceeding
	if err := diskinstall.ValidateNetworkFileForInstall(nf); err != nil {
		fmt.Fprintf(s.out, "Network profile validation failed: %v\n", err)
		_ = os.Remove(stagedKeyfile)
		_ = os.Remove(path)
		return diskinstall.NetworkFile{}, ""
	}
	return nf, path
}

func (s *wizardState) stepDisks() (error, bool, bool) {
	fmt.Fprintln(s.out, "")
	fmt.Fprintln(s.out, "Scanning disks...")
	backend, err := findBackend()
	if err != nil {
		fmt.Fprintf(s.out, "Backend not found: %v\n", err)
		fmt.Fprintln(s.out, "Cannot scan disks without backend.")
		fmt.Fprintln(s.out, "Choose: [1] Retry  [2] Quit  [back]")
		for {
			line, _ := s.readLineWithBack("Choice [1/2/back/quit]: ", true)
			if s.isQuitInput(line) {
				return nil, false, true
			}
			if strings.TrimSpace(line) == "back" {
				return nil, true, false
			}
			switch strings.TrimSpace(line) {
			case "1":
				return s.stepDisks()
			case "2":
				return nil, false, true
			default:
				fmt.Fprintln(s.out, "Enter 1, 2, back, or quit.")
			}
		}
	}
	// Call --list-disks
	cmd := exec.Command(backend, "--list-disks")
	var outBuf bytes.Buffer
	var errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(s.out, "Failed to list disks: %v\n%s\n", err, errBuf.String())
		fmt.Fprintln(s.out, "Choose: [1] Rescan  [2] Quit  [back]")
		for {
			line, _ := s.readLineWithBack("Choice: ", true)
			if s.isQuitInput(line) {
				return nil, false, true
			}
			if strings.TrimSpace(line) == "back" {
				return nil, true, false
			}
			if strings.TrimSpace(line) == "1" {
				return s.stepDisks()
			}
			if strings.TrimSpace(line) == "2" {
				return nil, false, true
			}
		}
	}
	resp, err := diskinstall.ParseListDisks(outBuf.Bytes())
	if err != nil {
		fmt.Fprintf(s.out, "Failed to parse disk list: %v\n", err)
		return s.stepDisks()
	}
	s.disksResp = resp

	if len(resp.Disks) == 0 {
		fmt.Fprintln(s.out, "No eligible disks found.")
		fmt.Fprintln(s.out, "Ensure disks are connected and not in use.")
		fmt.Fprintln(s.out, "Choose: [1] Rescan  [2] Quit")
		for {
			line, _ := s.readLineWithBack("Choice: ", true)
			if s.isQuitInput(line) {
				return nil, false, true
			}
			if strings.TrimSpace(line) == "1" {
				return s.stepDisks()
			}
			if strings.TrimSpace(line) == "2" {
				return nil, false, true
			}
		}
	}

	// Display disks
	fmt.Fprintln(s.out, "")
	fmt.Fprintln(s.out, "Found disks:")
	for i, d := range resp.Disks {
		elig := ""
		if d.SystemEligible && d.DataEligible {
			elig = "system+data eligible"
		} else if d.SystemEligible {
			elig = "system eligible"
		} else if d.DataEligible {
			elig = "data eligible"
		} else {
			// Show reasons
			reasons := []string{}
			if d.SystemReason != "" {
				reasons = append(reasons, "system: "+d.SystemReason)
			}
			if d.DataReason != "" {
				reasons = append(reasons, "data: "+d.DataReason)
			}
			if len(reasons) == 0 {
				reasons = append(reasons, "not eligible")
			}
			elig = strings.Join(reasons, ", ")
		}
		externalMark := ""
		if d.External {
			externalMark = " [external]"
		}
		fmt.Fprintf(s.out, "  %d) %s  %s  %s%s  — %s\n", i+1, d.Path, diskinstall.FormatSize(d.SizeBytes), strings.TrimSpace(d.Model), externalMark, elig)
		if d.Serial != "" {
			fmt.Fprintf(s.out, "      serial: %s  transport: %s\n", d.Serial, d.Transport)
		}
	}

	// Separate internal vs external
	var internalDisks, externalDisks []diskinstall.Disk
	for _, d := range resp.Disks {
		if d.External {
			externalDisks = append(externalDisks, d)
		} else {
			internalDisks = append(internalDisks, d)
		}
	}

	// Determine system-eligible candidates
	// Recommend smallest internal system-eligible, or smallest opted-in external if none
	recommended := diskinstall.RecommendSystemDisk(resp.Disks, s.externalOpt)
	// If recommended is nil and there are external system-eligible not opted in, we could offer opt-in first
	if recommended == nil {
		// Check if there are external system-eligible that could be candidate if opted in
		hasExternalCandidate := false
		for _, d := range externalDisks {
			if d.SystemEligible {
				hasExternalCandidate = true
				break
			}
		}
		if hasExternalCandidate {
			fmt.Fprintln(s.out, "")
			fmt.Fprintln(s.out, "No system-eligible internal disk found.")
			fmt.Fprintln(s.out, "External disks require explicit opt-in.")
			// Offer opt-in for external system-eligible
			for _, d := range externalDisks {
				if !d.SystemEligible {
					continue
				}
				for {
					prompt := fmt.Sprintf("Include external disk %s (%s) as system candidate? [y/N, back, quit]: ", d.Path, diskinstall.FormatSize(d.SizeBytes))
					line, _ := s.readLineWithBack(prompt, true)
					if s.isQuitInput(line) {
						return nil, false, true
					}
					if strings.TrimSpace(line) == "back" {
						return nil, true, false
					}
					switch strings.ToLower(strings.TrimSpace(line)) {
					case "y", "yes":
						s.externalOpt[d.Path] = true
						hasExternalCandidate = true
					case "n", "no", "":
						s.externalOpt[d.Path] = false
					default:
						fmt.Fprintln(s.out, "Please enter y or n.")
						continue
					}
					break
				}
			}
			recommended = diskinstall.RecommendSystemDisk(resp.Disks, s.externalOpt)
		}
	}

	if recommended == nil {
		fmt.Fprintln(s.out, "")
		fmt.Fprintln(s.out, "No system-eligible disk available.")
		fmt.Fprintln(s.out, "Choose: [1] Rescan  [2] Quit  [back]")
		for {
			line, _ := s.readLineWithBack("Choice: ", true)
			if s.isQuitInput(line) {
				return nil, false, true
			}
			if strings.TrimSpace(line) == "back" {
				return nil, true, false
			}
			switch strings.TrimSpace(line) {
			case "1":
				return s.stepDisks()
			case "2":
				return nil, false, true
			default:
				fmt.Fprintln(s.out, "Enter 1, 2, back, or quit.")
			}
		}
	}

	// Show recommendation
	fmt.Fprintln(s.out, "")
	fmt.Fprintf(s.out, "Recommended system disk: %s (%s, %s)\n", recommended.Path, recommended.Model, diskinstall.FormatSize(recommended.SizeBytes))
	fmt.Fprintln(s.out, "All other selected disks will become separate data volumes at /srv/omahab/data1, data2, ...")
	// List data disks that would be included automatically
	dataDisks := diskinstall.DataDisks(resp.Disks, recommended.Path, s.externalOpt)
	if len(dataDisks) > 0 {
		fmt.Fprintln(s.out, "Data disks (automatically included):")
		for _, d := range dataDisks {
			// Only internal automatically; external those opted in already marked
			if d.External && !s.externalOpt[d.Path] {
				continue
			}
			if d.External {
				fmt.Fprintf(s.out, "  - %s (%s) [external, opted-in]\n", d.Path, diskinstall.FormatSize(d.SizeBytes))
			} else {
				fmt.Fprintf(s.out, "  - %s (%s)\n", d.Path, diskinstall.FormatSize(d.SizeBytes))
			}
		}
	} else {
		fmt.Fprintln(s.out, "No additional data disks will be used (only system disk).")
	}

	// Offer external opt-in for remaining external data-eligible not yet opted
	remainingExternal := []diskinstall.Disk{}
	for _, d := range externalDisks {
		if d.Path == recommended.Path {
			continue
		}
		if !d.DataEligible {
			continue
		}
		if s.externalOpt[d.Path] {
			continue // already opted
		}
		remainingExternal = append(remainingExternal, d)
	}
	if len(remainingExternal) > 0 {
		fmt.Fprintln(s.out, "")
		fmt.Fprintln(s.out, "External disks require opt-in:")
		for _, d := range remainingExternal {
			for {
				prompt := fmt.Sprintf("Include external disk %s (%s, %s)? [y/N, back, quit]: ", d.Path, d.Model, diskinstall.FormatSize(d.SizeBytes))
				line, _ := s.readLineWithBack(prompt, true)
				if s.isQuitInput(line) {
					return nil, false, true
				}
				if strings.TrimSpace(line) == "back" {
					return nil, true, false
				}
				switch strings.ToLower(strings.TrimSpace(line)) {
				case "y", "yes":
					s.externalOpt[d.Path] = true
					fmt.Fprintf(s.out, "Will include %s.\n", d.Path)
				case "n", "no", "":
					s.externalOpt[d.Path] = false
					fmt.Fprintf(s.out, "Skipping %s.\n", d.Path)
				default:
					fmt.Fprintln(s.out, "Please enter y or n.")
					continue
				}
				break
			}
		}
		// Recompute recommended after opt-ins? System already chosen.
	}

	// Confirm system disk acceptance
	fmt.Fprintln(s.out, "")
	for {
		prompt := fmt.Sprintf("Accept system disk %s? [Y/n, or enter number to choose different, back, quit]: ", recommended.Path)
		line, _ := s.readLineWithBack(prompt, true)
		if s.isQuitInput(line) {
			return nil, false, true
		}
		if strings.TrimSpace(line) == "back" {
			return nil, true, false
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.EqualFold(trimmed, "y") || strings.EqualFold(trimmed, "yes") {
			s.systemDisk = recommended.Path
			break
		}
		if strings.EqualFold(trimmed, "n") || strings.EqualFold(trimmed, "no") {
			fmt.Fprintln(s.out, "Choose system disk by number:")
			for i, d := range resp.Disks {
				if !d.SystemEligible {
					continue
				}
				if d.External && !s.externalOpt[d.Path] {
					continue
				}
				mark := ""
				if d.Path == recommended.Path {
					mark = " (recommended)"
				}
				fmt.Fprintf(s.out, "  %d) %s %s%s\n", i+1, d.Path, diskinstall.FormatSize(d.SizeBytes), mark)
			}
			continue
		}
		// Try parse as number
		var idx int
		_, err := fmt.Sscanf(trimmed, "%d", &idx)
		if err == nil && idx >= 1 && idx <= len(resp.Disks) {
			chosen := resp.Disks[idx-1]
			if !chosen.SystemEligible {
				fmt.Fprintf(s.out, "Disk %s is not system-eligible: %s\n", chosen.Path, chosen.SystemReason)
				continue
			}
			if chosen.External && !s.externalOpt[chosen.Path] {
				fmt.Fprintf(s.out, "External disk %s requires opt-in first.\n", chosen.Path)
				continue
			}
			s.systemDisk = chosen.Path
			fmt.Fprintf(s.out, "Selected system disk: %s\n", chosen.Path)
			break
		}
		fmt.Fprintln(s.out, "Please enter Y, n, a number, back, or quit.")
	}

	// Final summary of selected disks
	selected := diskinstall.SelectedDisks(resp.Disks, s.systemDisk, s.externalOpt)
	fmt.Fprintln(s.out, "")
	fmt.Fprintln(s.out, "Selected disks for installation:")
	for i, d := range selected {
		role := "data"
		if i == 0 {
			role = "system"
		}
		fmt.Fprintf(s.out, "  %s: %s (%s)\n", role, d.Path, diskinstall.FormatSize(d.SizeBytes))
	}
	fmt.Fprintln(s.out, "")
	// Ask to proceed to next step (administrator)
	for {
		line, _ := s.readLineWithBack("Press Enter to continue to Administrator setup, or type 'back' to revise disks: ", true)
		if s.isQuitInput(line) {
			return nil, false, true
		}
		if strings.TrimSpace(line) == "back" {
			return nil, true, false
		}
		if strings.TrimSpace(line) == "" {
			return nil, false, false
		}
		fmt.Fprintln(s.out, "Press Enter to continue or type 'back'.")
	}
}

func (s *wizardState) stepAdministrator() (error, bool, bool) {
	fmt.Fprintln(s.out, "")
	fmt.Fprintln(s.out, "Create the administrator account.")

	// Hostname
	for {
		prompt := "Hostname [omahab]: "
		line, _ := s.readLineWithBack(prompt, true)
		if s.isQuitInput(line) {
			return nil, false, true
		}
		if strings.TrimSpace(line) == "back" {
			return nil, true, false
		}
		hostname := strings.TrimSpace(line)
		if hostname == "" {
			hostname = "omahab"
		}
		if err := diskinstall.ValidateHostname(hostname); err != nil {
			fmt.Fprintf(s.out, "Invalid hostname: %v\n", err)
			continue
		}
		s.hostname = hostname
		fmt.Fprintf(s.out, "Hostname: %s\n", s.hostname)
		break
	}

	// Username
	for {
		prompt := "Username (no default): "
		line, _ := s.readLineWithBack(prompt, true)
		if s.isQuitInput(line) {
			return nil, false, true
		}
		if strings.TrimSpace(line) == "back" {
			// Go back to hostname? Instead return to previous step? But spec says back navigation at nonsecret prompts.
			// We treat back as returning to previous wizard step (disks). To allow editing hostname, user can stay.
			// Here back means go to previous step.
			return nil, true, false
		}
		username := strings.TrimSpace(line)
		if err := diskinstall.ValidateUsername(username); err != nil {
			fmt.Fprintf(s.out, "Invalid username: %v\n", err)
			continue
		}
		// Additional check: username must not conflict with target system users (reserved list already covers many)
		// Also check against live /etc/passwd? If username exists, reject?
		// Use user.Lookup to see if live system has that user (except root is already rejected)
		// But on ISO, only root exists, so fine.
		s.username = username
		fmt.Fprintf(s.out, "Username: %s\n", s.username)
		break
	}

	// Password twice hidden
	for {
		pw1, err := s.readPassword("Password (min 8 characters): ")
		if err != nil {
			fmt.Fprintf(s.out, "Failed to read password: %v\n", err)
			continue
		}
		// Allow back? Password reads are secret prompts, should restore tty on interrupt but not allow back per spec
		// Spec says back/quit at nonsecret prompts, so secret prompts not having back is fine.
		// But we handle empty and length
		if err := diskinstall.ValidatePassword(pw1); err != nil {
			fmt.Fprintf(s.out, "Invalid password: %v\n", err)
			continue
		}
		pw2, err := s.readPassword("Confirm password: ")
		if err != nil {
			fmt.Fprintf(s.out, "Failed to read password: %v\n", err)
			continue
		}
		if err := diskinstall.ValidatePasswordConfirmation(pw1, pw2); err != nil {
			fmt.Fprintf(s.out, "Passwords do not match or invalid: %v\n", err)
			fmt.Fprintln(s.out, "Please try again.")
			continue
		}
		s.password = pw1
		fmt.Fprintln(s.out, "Password confirmed.")
		break
	}

	fmt.Fprintln(s.out, "")
	// Show summary without password
	fmt.Fprintf(s.out, "Administrator: %s @ %s\n", s.username, s.hostname)
	fmt.Fprintln(s.out, "Press Enter to continue to SSH setup, or type 'back' to revise.")
	for {
		line, _ := s.readLineWithBack("Choice [Enter/back/quit]: ", true)
		if s.isQuitInput(line) {
			return nil, false, true
		}
		if strings.TrimSpace(line) == "back" {
			// Clear password from memory? Keep but will be overwritten
			return nil, true, false
		}
		if strings.TrimSpace(line) == "" {
			return nil, false, false
		}
	}
}

func (s *wizardState) stepSSH() (error, bool, bool) {
	fmt.Fprintln(s.out, "")
	fmt.Fprintln(s.out, "SSH access (key-only, password SSH remains disabled).")
	fmt.Fprintln(s.out, "Choose how to set up remote SSH for the administrator:")
	fmt.Fprintln(s.out, "  1) Import from GitHub")
	fmt.Fprintln(s.out, "  2) Paste public keys")
	fmt.Fprintln(s.out, "  3) Set up later in WebUI")

	for {
		line, _ := s.readLineWithBack("Choice [1/2/3, back, quit]: ", true)
		if s.isQuitInput(line) {
			return nil, false, true
		}
		if strings.TrimSpace(line) == "back" {
			return nil, true, false
		}
		choice := strings.TrimSpace(line)
		if choice == "" {
			fmt.Fprintln(s.out, "Please choose 1, 2, or 3.")
			continue
		}
		switch choice {
		case "1":
			if s.handleGitHubImport() {
				return nil, false, false
			}
			// handleGitHubImport returns false if user chose back to menu
			continue
		case "2":
			if s.handlePasteImport() {
				return nil, false, false
			}
			continue
		case "3":
			fmt.Fprintln(s.out, "Remote SSH will stay unavailable until you add a key.")
			fmt.Fprintln(s.out, "Your local username and password will still work for console login and sudo.")
			s.sshChoice = diskinstall.SSHChoice{Mode: "defer"}
			return nil, false, false
		default:
			fmt.Fprintln(s.out, "Please enter 1, 2, or 3, back, or quit.")
		}
	}
}

func (s *wizardState) handleGitHubImport() bool {
	for {
		ghLine, _ := s.readLineWithBack("GitHub username: ", true)
		if s.isQuitInput(ghLine) {
			// quit handled by caller via return? For now treat as signal to quit whole wizard
			// We can't directly return quit from helper; instead set a flag via panic? Simpler: handle quit here by exiting.
			fmt.Fprintln(s.out, "Quitting...")
			os.Exit(0)
		}
		if strings.TrimSpace(ghLine) == "back" {
			return false
		}
		ghUser := strings.TrimSpace(ghLine)
		if ghUser == "" {
			fmt.Fprintln(s.out, "GitHub username required.")
			continue
		}
		fmt.Fprintf(s.out, "Fetching keys for %q...\n", ghUser)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		keys, err := diskinstall.FetchGitHubKeys(ctx, ghUser)
		cancel()
		if err != nil {
			fmt.Fprintf(s.out, "Failed to fetch GitHub keys: %v\n", err)
			fmt.Fprintln(s.out, "Options: [1] Retry  [2] Back to SSH menu  [quit]")
			for {
				retryLine, _ := s.readLineWithBack("Choice [1/2/quit]: ", true)
				if s.isQuitInput(retryLine) {
					fmt.Fprintln(s.out, "Quitting...")
					os.Exit(0)
				}
				switch strings.TrimSpace(retryLine) {
				case "1":
					// retry same user without re-prompting username? Re-fetch immediately
					fmt.Fprintf(s.out, "Retrying fetch for %q...\n", ghUser)
					ctx2, cancel2 := context.WithTimeout(context.Background(), 15*time.Second)
					keys2, err2 := diskinstall.FetchGitHubKeys(ctx2, ghUser)
					cancel2()
					if err2 != nil {
						fmt.Fprintf(s.out, "Still failed: %v\n", err2)
						continue
					}
					fmt.Fprintln(s.out, "Fetched keys:")
					for _, k := range keys2 {
						fmt.Fprintf(s.out, "  %s  %s  %s\n", k.Type, k.Fingerprint, k.Comment)
					}
					fmt.Fprintln(s.out, "Use these keys? [Y/n, back]")
					if s.confirmGitHubKeys(ghUser, keys2) {
						return true
					}
					break
				case "2", "back":
					// back to SSH menu
					return false
				default:
					fmt.Fprintln(s.out, "Enter 1, 2, or quit.")
					continue
				}
				break
			}
			continue
		}
		fmt.Fprintln(s.out, "Fetched keys:")
		for _, k := range keys {
			fmt.Fprintf(s.out, "  %s  %s  %s\n", k.Type, k.Fingerprint, k.Comment)
		}
		fmt.Fprintln(s.out, "Use these keys? [Y/n, back]")
		if s.confirmGitHubKeys(ghUser, keys) {
			return true
		}
		// not confirmed, loop for new username
	}
}

func (s *wizardState) confirmGitHubKeys(ghUser string, keys []sshkeys.SSHKey) bool {
	for {
		confirm, _ := s.readLineWithBack("Confirm [Y/n/back]: ", true)
		if s.isQuitInput(confirm) {
			fmt.Fprintln(s.out, "Quitting...")
			os.Exit(0)
		}
		if strings.TrimSpace(confirm) == "back" {
			return false
		}
		if strings.TrimSpace(confirm) == "" || strings.EqualFold(strings.TrimSpace(confirm), "y") || strings.EqualFold(strings.TrimSpace(confirm), "yes") {
			s.sshChoice = diskinstall.SSHChoice{Mode: "github", GithubUser: ghUser, Keys: keys}
			fmt.Fprintln(s.out, "GitHub keys will be installed.")
			return true
		}
		if strings.EqualFold(strings.TrimSpace(confirm), "n") || strings.EqualFold(strings.TrimSpace(confirm), "no") {
			fmt.Fprintln(s.out, "Not using fetched keys. Choose again.")
			return false
		}
		fmt.Fprintln(s.out, "Please enter Y, n, or back.")
	}
}

func (s *wizardState) handlePasteImport() bool {
	fmt.Fprintln(s.out, "Paste public keys, one per line. End with a blank line.")
	for {
		var lines []string
		for {
			peek, _ := s.readLine("")
			if strings.TrimSpace(peek) == "" {
				break
			}
			if len(lines) == 0 && strings.EqualFold(strings.TrimSpace(peek), "back") {
				return false
			}
			lines = append(lines, peek)
		}
		if len(lines) == 0 {
			fmt.Fprintln(s.out, "No keys pasted. Choose again: 1) Import from GitHub  2) Paste  3) Defer  [back]")
			return false
		}
		raw := strings.Join(lines, "\n")
		keys, err := diskinstall.ParsePastedKeys(raw)
		if err != nil {
			fmt.Fprintf(s.out, "Invalid pasted keys: %v\n", err)
			fmt.Fprintln(s.out, "Choose: [1] Retry paste  [2] Back to SSH menu  [quit]")
			for {
				retryLine, _ := s.readLineWithBack("Choice [1/2/quit]: ", true)
				if s.isQuitInput(retryLine) {
					fmt.Fprintln(s.out, "Quitting...")
					os.Exit(0)
				}
				switch strings.TrimSpace(retryLine) {
				case "1":
					fmt.Fprintln(s.out, "Paste public keys, one per line. End with a blank line.")
					break
				case "2", "back":
					return false
				default:
					fmt.Fprintln(s.out, "Enter 1, 2, or quit.")
					continue
				}
				break
			}
			// retry paste: continue outer paste loop
			fmt.Fprintln(s.out, "Paste public keys, one per line. End with a blank line.")
			continue
		}
		fmt.Fprintln(s.out, "Pasted keys:")
		for _, k := range keys {
			fmt.Fprintf(s.out, "  %s  %s  %s\n", k.Type, k.Fingerprint, k.Comment)
		}
		fmt.Fprintln(s.out, "Use these keys? [Y/n, back]")
		for {
			confirm, _ := s.readLineWithBack("Confirm [Y/n/back]: ", true)
			if s.isQuitInput(confirm) {
				fmt.Fprintln(s.out, "Quitting...")
				os.Exit(0)
			}
			if strings.TrimSpace(confirm) == "back" {
				break
			}
			if strings.TrimSpace(confirm) == "" || strings.EqualFold(strings.TrimSpace(confirm), "y") {
				s.sshChoice = diskinstall.SSHChoice{Mode: "paste", Keys: keys}
				fmt.Fprintln(s.out, "Pasted keys will be installed.")
				return true
			}
			if strings.EqualFold(strings.TrimSpace(confirm), "n") {
				fmt.Fprintln(s.out, "Not using pasted keys.")
				break
			}
		}
		// not confirmed, offer to re-paste? loop again
		fmt.Fprintln(s.out, "Paste again or type 'back' to return to SSH menu.")
		// continue outer loop to re-paste
	}
}

func (s *wizardState) stepReview() (error, bool, bool) {
	fmt.Fprintln(s.out, "")
	fmt.Fprintln(s.out, "Review installation plan:")
	fmt.Fprintln(s.out, strings.Repeat("-", 40))
	fmt.Fprintf(s.out, "Hostname: %s\n", s.hostname)
	fmt.Fprintf(s.out, "Administrator: %s\n", s.username)
	// Connection
	if s.networkMode == "profile" {
		fmt.Fprintf(s.out, "Network: profile (%s on %s)\n", s.networkFile, s.networkMode)
	} else {
		fmt.Fprintln(s.out, "Network: wired DHCP")
	}
	// SSH
	switch s.sshChoice.Mode {
	case "defer":
		fmt.Fprintln(s.out, "SSH: Set up later in WebUI (remote SSH unavailable until key added)")
	case "github":
		fmt.Fprintf(s.out, "SSH: GitHub import (%s) — %d keys\n", s.sshChoice.GithubUser, len(s.sshChoice.Keys))
		for _, k := range s.sshChoice.Keys {
			fmt.Fprintf(s.out, "  %s  %s  %s\n", k.Type, k.Fingerprint, k.Comment)
		}
	case "paste":
		fmt.Fprintf(s.out, "SSH: Pasted keys — %d keys\n", len(s.sshChoice.Keys))
		for _, k := range s.sshChoice.Keys {
			fmt.Fprintf(s.out, "  %s  %s  %s\n", k.Type, k.Fingerprint, k.Comment)
		}
	}
	// Disks
	if s.disksResp != nil && s.systemDisk != "" {
		selected := diskinstall.SelectedDisks(s.disksResp.Disks, s.systemDisk, s.externalOpt)
		fmt.Fprintln(s.out, "Disks:")
		for i, d := range selected {
			role := "data"
			if i == 0 {
				role = "system"
			}
			fmt.Fprintf(s.out, "  %s: %s (%s) %s\n", role, d.Path, diskinstall.FormatSize(d.SizeBytes), d.Model)
		}
		fmt.Fprintln(s.out, "")
		fmt.Fprintln(s.out, "All existing partitions and files on these disks will be removed. This is not a secure data-sanitization erase.")
	} else {
		fmt.Fprintln(s.out, "Disks: (not selected)")
	}
	fmt.Fprintln(s.out, strings.Repeat("-", 40))
	fmt.Fprintln(s.out, "You can edit before confirming:")
	fmt.Fprintln(s.out, "  type 'back' to revise, 'quit' to exit, or edit specific step:")
	fmt.Fprintln(s.out, "  'edit disks' / 'edit admin' / 'edit ssh' / 'edit network' / 'edit hostname'")
	fmt.Fprintln(s.out, "")

	selected := []diskinstall.Disk{}
	if s.disksResp != nil && s.systemDisk != "" {
		selected = diskinstall.SelectedDisks(s.disksResp.Disks, s.systemDisk, s.externalOpt)
	}
	erasePhrase := diskinstall.ErasePhrase(len(selected))
	fmt.Fprintf(s.out, "To confirm, type exactly: %s\n", erasePhrase)
	fmt.Fprintln(s.out, "(No default — you must type the phrase.)")

	for {
		line, _ := s.readLineWithBack("Confirmation: ", true)
		if s.isQuitInput(line) {
			return nil, false, true
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "back" {
			return nil, true, false
		}
		// Allow edit commands
		lower := strings.ToLower(trimmed)
		switch lower {
		case "edit disks":
			// Go back to disks step
			// We need to jump multiple steps back; easiest is to signal back multiple times?
			// For simplicity, return back and let main loop handle stepping back? But we need to go back 3 steps.
			// Instead we can directly invoke stepDisks and then loop?
			// We'll implement by returning a special error to indicate jump? Simpler: handle edits by directly calling step functions.
			fmt.Fprintln(s.out, "Re-entering disk selection...")
			// Save current state, call stepDisks; if it returns back, stay?
			if _, back, quit := s.stepDisks(); quit {
				return nil, false, true
			} else if back {
				return nil, true, false
			}
			// Re-show review
			return s.stepReview()
		case "edit admin":
			fmt.Fprintln(s.out, "Re-entering administrator setup...")
			if _, back, quit := s.stepAdministrator(); quit {
				return nil, false, true
			} else if back {
				// back from admin goes to disks; we just re-show review
			}
			return s.stepReview()
		case "edit ssh":
			fmt.Fprintln(s.out, "Re-entering SSH setup...")
			if _, back, quit := s.stepSSH(); quit {
				return nil, false, true
			} else if back {
			}
			return s.stepReview()
		case "edit network", "edit connection":
			fmt.Fprintln(s.out, "Re-entering network setup...")
			if _, back, quit := s.stepConnection(); quit {
				return nil, false, true
			} else if back {
			}
			return s.stepReview()
		case "edit hostname":
			fmt.Fprintln(s.out, "Re-entering hostname...")
			// Re-use admin step but only hostname part? For simplicity call admin again
			if _, back, quit := s.stepAdministrator(); quit {
				return nil, false, true
			} else if back {
			}
			return s.stepReview()
		}

		if trimmed == erasePhrase {
			// Correct confirmation
			fmt.Fprintln(s.out, "Confirmed. Proceeding to installation...")
			return nil, false, false
		}
		if trimmed == "" {
			fmt.Fprintf(s.out, "Please type exactly %q to confirm, or 'back' to revise.\n", erasePhrase)
			continue
		}
		fmt.Fprintf(s.out, "Incorrect phrase. Type exactly %q (or 'back' to revise).\n", erasePhrase)
	}
}

// drainProgressEvents collects remaining progress events until the channel
// is closed. It is nil-safe: the install loop nils the progress channel on
// close, and ranging a nil channel would block forever (deadlock).
func drainProgressEvents(ch chan diskinstall.ProgressEvent) []diskinstall.ProgressEvent {
	if ch == nil {
		return nil
	}
	var out []diskinstall.ProgressEvent
	for ev := range ch {
		out = append(out, ev)
	}
	return out
}

func (s *wizardState) stepInstall() (error, bool, bool) {
	fmt.Fprintln(s.out, "")
	fmt.Fprintln(s.out, "Starting installation...")

	// Validate before erasure
	if s.hostname == "" || s.username == "" || s.password == "" {
		fmt.Fprintln(s.out, "Missing administrator information; returning to Review.")
		return fmt.Errorf("missing admin info"), true, false
	}
	if s.systemDisk == "" || s.disksResp == nil {
		fmt.Fprintln(s.out, "Missing disk selection; returning to Review.")
		return fmt.Errorf("missing disks"), true, false
	}
	// Validate hostname/username before erasure (already validated but re-check)
	if err := diskinstall.ValidateHostname(s.hostname); err != nil {
		fmt.Fprintf(s.out, "Hostname invalid: %v\n", err)
		return err, true, false
	}
	if err := diskinstall.ValidateUsername(s.username); err != nil {
		fmt.Fprintf(s.out, "Username invalid: %v\n", err)
		return err, true, false
	}
	// Validate network file before erasure
	if s.networkFile != "" {
		nf, err := diskinstall.ReadNetworkFile(s.networkFile)
		if err != nil {
			fmt.Fprintf(s.out, "Network file unreadable: %v\n", err)
			return err, true, false
		}
		if err := diskinstall.ValidateNetworkFileForInstall(*nf); err != nil {
			fmt.Fprintf(s.out, "Network validation failed before erasure: %v\n", err)
			return err, true, false
		}
	}

	// Stage password hash
	hashFile := filepath.Join(s.sessionDir, "passwd.hash")
	if err := stagePasswordHash(s.password, hashFile); err != nil {
		fmt.Fprintf(s.out, "Failed to hash password: %v\n", err)
		return err, false, false
	}
	_ = os.Chmod(hashFile, 0600)
	// Ensure removal on exit via cleanup, but also keep track
	// Clear password from memory after hashing? Keep for now but zero after?
	// Stage authorized_keys file if needed
	var keysFile string
	if len(s.sshChoice.Keys) > 0 {
		keysFile = filepath.Join(s.sessionDir, "authorized_keys")
		var buf bytes.Buffer
		for _, k := range s.sshChoice.Keys {
			buf.WriteString(k.Raw)
			buf.WriteString("\n")
		}
		if err := os.WriteFile(keysFile, buf.Bytes(), 0600); err != nil {
			_ = os.Remove(hashFile)
			return fmt.Errorf("stage keys: %w", err), false, false
		}
		_ = os.Chmod(keysFile, 0600)
	}
	// Stage selection file
	selectionFile := filepath.Join(s.sessionDir, "selection.json")
	selected := diskinstall.SelectedDisks(s.disksResp.Disks, s.systemDisk, s.externalOpt)
	selData := diskinstall.SelectionFileData{Disks: selected}
	jsonData, err := json.MarshalIndent(selData, "", "  ")
	if err != nil {
		_ = os.Remove(hashFile)
		if keysFile != "" {
			_ = os.Remove(keysFile)
		}
		return err, false, false
	}
	if err := os.WriteFile(selectionFile, append(jsonData, '\n'), 0600); err != nil {
		_ = os.Remove(hashFile)
		if keysFile != "" {
			_ = os.Remove(keysFile)
		}
		return err, false, false
	}
	_ = os.Chmod(selectionFile, 0600)

	// Prepare network file path already staged; if empty and wired-dhcp? Ensure file exists
	if s.networkFile == "" {
		// Create wired-dhcp fallback
		nf := diskinstall.NetworkFile{Mode: "wired-dhcp", ConnectionUUID: "", Interface: "", Keyfile: ""}
		path := filepath.Join(s.sessionDir, "network.json")
		_ = diskinstall.WriteNetworkFile(path, nf)
		_ = os.Chmod(path, 0600)
		s.networkFile = path
	}

	// Find backend
	backend, err := findBackend()
	if err != nil {
		fmt.Fprintf(s.out, "Backend not found: %v\n", err)
		_ = os.Remove(hashFile)
		if keysFile != "" {
			_ = os.Remove(keysFile)
		}
		_ = os.Remove(selectionFile)
		return err, false, false
	}

	// Prepare progress pipe
	pr, pw, err := os.Pipe()
	if err != nil {
		return err, false, false
	}
	defer pr.Close()
	// Ensure pw closed after backend finishes

	// Prepare log file already opened; we will also tee backend output to log
	// Create command
	args := []string{
		"--hostname", s.hostname,
		"--username", s.username,
		"--password-hash-file", hashFile,
		"--selection-file", selectionFile,
		"--network-file", s.networkFile,
		"--yes",
	}
	if keysFile != "" {
		args = append(args, "--authorized-keys-file", keysFile)
	}
	// Check if backend supports --progress-fd by probing help? Assume yes; if fails fallback without
	// We will try to pass --progress-fd; if backend doesn't support, it will error and we fallback.

	// Determine fd number for ExtraFiles
	// ExtraFiles[0] becomes fd 3 in child
	progressFdNum := 3
	args = append(args, "--progress-fd", fmt.Sprintf("%d", progressFdNum))

	cmd := exec.Command(backend, args...)
	// Set ExtraFiles to pass pipe write end as fd 3
	cmd.ExtraFiles = []*os.File{pw}
	// Redirect stdout/stderr to log file (and also to consume for progress)
	// We need to multiplex: backend stdout/stderr to log file, progress to pipe
	cmd.Stdout = s.logFile
	cmd.Stderr = s.logFile
	// Also ensure secrets not in argv: they are file paths, not values, ok

	fmt.Fprintln(s.out, "Invoking installer backend... (log follows)")
	fmt.Fprintf(s.out, "Log: %s\n", s.logPath)
	fmt.Fprintln(s.out, "Stages: preflight, partition, format, mount, configure, install, account, unmount, done")
	fmt.Fprintln(s.out, "This may take several minutes. Elapsed time will be shown.")

	// Start backend
	start := time.Now()
	if err := cmd.Start(); err != nil {
		pw.Close()
		_ = os.Remove(hashFile)
		if keysFile != "" {
			_ = os.Remove(keysFile)
		}
		_ = os.Remove(selectionFile)
		return fmt.Errorf("start backend: %w", err), false, false
	}
	// Close write end in parent after start (child has copy)
	// But we keep pw open for reading? Actually parent should close write end after start, but we passed pw as ExtraFiles, closing parent's copy still leaves child's fd open.
	// We still have pw in parent; we close it after command finishes? For now close parent's duplicate that is not needed? We need to keep pw open in child only; parent should close its copy of write end so that read end gets EOF when child closes.
	// The pw we passed is the write end; after Start, we can close it in parent after command finishes? But we need to close the parent's fd that is ExtraFiles? Actually os.Pipe returns pr (read) and pw (write). We set ExtraFiles = []*os.File{pw}, which duplicates pw as fd 3. After Start, we should close pw in parent (the original fd) because child has its own copy; but reading from pr will still block until child closes its fd 3.
	pw.Close()

	// Read progress events in goroutine
	progressDone := make(chan error, 1)
	progressEvents := make(chan diskinstall.ProgressEvent, 10)
	go func() {
		scanner := bufio.NewScanner(pr)
		// Increase buffer for long messages
		buf := make([]byte, 0, 64*1024)
		scanner.Buffer(buf, 1024*1024)
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			var ev diskinstall.ProgressEvent
			if err := json.Unmarshal(line, &ev); err != nil {
				// Ignore malformed, but log to stderr
				continue
			}
			progressEvents <- ev
		}
		close(progressEvents)
		progressDone <- scanner.Err()
	}()

	// Display progress as it arrives
	// Also monitor elapsed
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	cmdDone := make(chan error, 1)
	go func() {
		cmdDone <- cmd.Wait()
	}()

	elapsed := func() string {
		d := time.Since(start).Truncate(time.Second)
		return d.String()
	}

	lastStage := ""
	loop:
	for {
		select {
		case ev, ok := <-progressEvents:
			if !ok {
				// Progress channel closed
				progressEvents = nil
				continue
			}
			// Render stage
			switch ev.Status {
			case "running":
				if ev.Stage != lastStage {
					fmt.Fprintf(s.out, "[%s] %s: running — %s\n", elapsed(), ev.Stage, ev.Message)
					lastStage = ev.Stage
				} else {
					fmt.Fprintf(s.out, "  [%s] %s\n", elapsed(), ev.Message)
				}
			case "complete":
				fmt.Fprintf(s.out, "[%s] %s: complete — %s\n", elapsed(), ev.Stage, ev.Message)
			case "failed":
				fmt.Fprintf(s.out, "[%s] %s: failed — %s\n", elapsed(), ev.Stage, ev.Message)
			default:
				fmt.Fprintf(s.out, "[%s] %s %s: %s\n", elapsed(), ev.Stage, ev.Status, ev.Message)
			}
			// Also write to log file already via backend stdout? progress is separate
		case err := <-cmdDone:
			// Backend finished. Drain remaining progress events (nil-safe:
			// the channel is nilled on close above, and ranging nil blocks
			// forever).
			for _, ev := range drainProgressEvents(progressEvents) {
				fmt.Fprintf(s.out, "[%s] %s: %s — %s\n", elapsed(), ev.Stage, ev.Status, ev.Message)
			}
			progressEvents = nil
			<-progressDone // wait for scanner
			// Check result
			if err != nil {
				fmt.Fprintf(s.out, "\nInstallation failed: %v\n", err)
				fmt.Fprintf(s.out, "Stage: %s\n", lastStage)
				fmt.Fprintf(s.out, "Log: %s\n", s.logPath)
				fmt.Fprintln(s.out, "Installation is incomplete. Mounted target preserved for diagnosis until you exit.")
				fmt.Fprintln(s.out, "You may view the log, fix the issue, and retry (Retry uses --resume-install for install/account failures).")
				// Offer log view and retry
				for {
					fmt.Fprintln(s.out, "Options: [v]iew log  [r]etry (if install/account failure)  [q]uit")
					line, _ := s.readLineWithBack("Choice [v/r/q]: ", true)
					if s.isQuitInput(line) {
						// Cleanup secrets already via defer, but preserve mounts? Backend preserves mounts.
						// Erase password from memory
						s.password = ""
						_ = os.Remove(hashFile)
						if keysFile != "" {
							_ = os.Remove(keysFile)
						}
						_ = os.Remove(selectionFile)
						return fmt.Errorf("installation failed"), false, false // Stay in install failure state, but main loop will handle
					}
					switch strings.TrimSpace(strings.ToLower(line)) {
					case "v", "view":
						// Show last lines of log
						data, err := os.ReadFile(s.logPath)
						if err != nil {
							fmt.Fprintf(s.out, "Cannot read log: %v\n", err)
							continue
						}
						lines := strings.Split(string(data), "\n")
						startIdx := 0
						if len(lines) > 100 {
							startIdx = len(lines) - 100
						}
						fmt.Fprintln(s.out, "--- install.log (last 100 lines) ---")
						for _, l := range lines[startIdx:] {
							fmt.Fprintln(s.out, l)
						}
						fmt.Fprintln(s.out, "--- end log ---")
					case "r", "retry":
						// Try resume-install if failure was in install/account stages
						// Check if lastStage is install/account/unmount
						if lastStage != "install" && lastStage != "account" && lastStage != "configure" {
							fmt.Fprintf(s.out, "Retry with --resume-install is only for install/account failures (current failure stage: %s). A fresh wizard is required for partition/format failures.\n", lastStage)
							continue
						}
						fmt.Fprintln(s.out, "Retrying with --resume-install...")
						// Build resume command
						resumeCmd := exec.Command(backend, "--resume-install", "--progress-fd", fmt.Sprintf("%d", progressFdNum))
						// Need new pipe
						pr2, pw2, _ := os.Pipe()
						resumeCmd.ExtraFiles = []*os.File{pw2}
						resumeCmd.Stdout = s.logFile
						resumeCmd.Stderr = s.logFile
						start2 := time.Now()
						if err := resumeCmd.Start(); err != nil {
							fmt.Fprintf(s.out, "Failed to start resume: %v\n", err)
							pw2.Close()
							pr2.Close()
							continue
						}
						pw2.Close()
						// Read progress similarly (reuse logic minimally)
						progressDone2 := make(chan error, 1)
						progressEvents2 := make(chan diskinstall.ProgressEvent, 10)
						go func() {
							scanner := bufio.NewScanner(pr2)
							buf := make([]byte, 0, 64*1024)
							scanner.Buffer(buf, 1024*1024)
							for scanner.Scan() {
								var ev diskinstall.ProgressEvent
								if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
									continue
								}
								progressEvents2 <- ev
							}
							close(progressEvents2)
							progressDone2 <- scanner.Err()
						}()
						cmdDone2 := make(chan error, 1)
						go func() { cmdDone2 <- resumeCmd.Wait() }()
						loop2:
						for {
							select {
							case ev, ok := <-progressEvents2:
								if !ok {
									progressEvents2 = nil
									continue
								}
								fmt.Fprintf(s.out, "[%s] %s: %s — %s\n", time.Since(start2).Truncate(time.Second), ev.Stage, ev.Status, ev.Message)
							case err2 := <-cmdDone2:
								for _, ev := range drainProgressEvents(progressEvents2) {
									fmt.Fprintf(s.out, "[%s] %s: %s — %s\n", time.Since(start2).Truncate(time.Second), ev.Stage, ev.Status, ev.Message)
								}
								progressEvents2 = nil
								<-progressDone2
								pr2.Close()
								if err2 != nil {
									fmt.Fprintf(s.out, "Retry failed: %v\n", err2)
									fmt.Fprintln(s.out, "Options: [v]iew log  [r]etry  [q]uit")
									break loop2
								}
								fmt.Fprintln(s.out, "Retry succeeded.")
								// Success: clean up and proceed to Ready
								_ = os.Remove(hashFile)
								if keysFile != "" {
									_ = os.Remove(keysFile)
								}
								_ = os.Remove(selectionFile)
								s.password = "" // erase
								// Sync disks
								_ = exec.Command("sync").Run()
								return nil, false, false
							case <-ticker.C:
								// keep elapsed updated? already per event
							}
						}
					case "q", "quit":
						s.password = ""
						_ = os.Remove(hashFile)
						if keysFile != "" {
							_ = os.Remove(keysFile)
						}
						_ = os.Remove(selectionFile)
						return fmt.Errorf("installation failed"), false, false
					default:
						fmt.Fprintln(s.out, "Enter v, r, or q.")
					}
				}
			}
			// Success path
			fmt.Fprintln(s.out, "\nInstallation stages complete.")
			// Sync
			_ = exec.Command("sync").Run()
			// Verify unmount? Backend does unmount; we just ensure secrets cleaned
			_ = os.Remove(hashFile)
			if keysFile != "" {
				_ = os.Remove(keysFile)
			}
			_ = os.Remove(selectionFile)
			s.password = "" // erase plaintext
			// Success: proceed to Ready
			break loop
		case <-ticker.C:
			// Periodic elapsed update could be printed, but we show per event
		}
		if progressEvents == nil {
			// No more progress events, but cmd still running
			continue
		}
	}

	return nil, false, false
}

func (s *wizardState) stepReady() (error, bool, bool) {
	fmt.Fprintln(s.out, "")
	fmt.Fprintln(s.out, "Installation complete!")
	fmt.Fprintln(s.out, strings.Repeat("=", 30))
	fmt.Fprintln(s.out, "The OS, administrator account, and bootloader are ready.")
	if s.username != "" && s.hostname != "" {
		if len(s.sshChoice.Keys) > 0 {
			fmt.Fprintf(s.out, "Remote SSH: ssh %s@%s.local\n", s.username, s.hostname)
		} else {
			fmt.Fprintln(s.out, "Remote SSH: not configured (add a key later in WebUI). Local login with username and password still works.")
		}
	}
	// List data paths
	if s.disksResp != nil && s.systemDisk != "" {
		selected := diskinstall.SelectedDisks(s.disksResp.Disks, s.systemDisk, s.externalOpt)
		if len(selected) > 1 {
			fmt.Fprintln(s.out, "Data volumes:")
			for i, d := range selected {
				if i == 0 {
					continue
				}
				fmt.Fprintf(s.out, "  /srv/omahab/data%d on %s (%s)\n", i, d.Path, diskinstall.FormatSize(d.SizeBytes))
			}
		}
	}
	fmt.Fprintln(s.out, "")
	fmt.Fprintln(s.out, "Next boot will start the WebUI for final setup (domain, apps, backups).")
	fmt.Fprintln(s.out, "Remove the ISO/USB installer media before rebooting.")
	fmt.Fprintln(s.out, "The system will boot from the installed disk by default (verified firmware entries).")
	fmt.Fprintln(s.out, "")
	fmt.Fprintln(s.out, "Choose:")
	fmt.Fprintln(s.out, "  1) Reboot now")
	fmt.Fprintln(s.out, "  2) Stay here (default)")
	for {
		line, _ := s.readLineWithBack("Choice [1/2, quit]: ", true)
		if s.isQuitInput(line) {
			return nil, false, true
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || trimmed == "2" {
			fmt.Fprintln(s.out, "Staying here. You can reboot later with 'reboot'.")
			return nil, false, false
		}
		if trimmed == "1" {
			fmt.Fprintln(s.out, "Rebooting...")
			// Try reboot commands
			for _, c := range [][]string{{"reboot"}, {"systemctl", "reboot"}, {"/run/current-system/sw/bin/reboot"}} {
				cmd := exec.Command(c[0], c[1:]...)
				cmd.Stdout = s.out
				cmd.Stderr = s.errW
				if err := cmd.Run(); err == nil {
					return nil, false, false
				}
			}
			fmt.Fprintln(s.out, "Reboot command not found; please reboot manually.")
			return nil, false, false
		}
		if trimmed == "back" {
			// At Ready, back goes to Review? But installation already done, don't go back.
			fmt.Fprintln(s.out, "Installation already complete; cannot go back.")
			continue
		}
		fmt.Fprintln(s.out, "Please enter 1 or 2.")
	}
}

// ---------- helpers ----------

func findBackend() (string, error) {
	if p, err := exec.LookPath("omahab-install-disk"); err == nil {
		return p, nil
	}
	candidates := []string{
		"/run/current-system/sw/bin/omahab-install-disk",
		"/etc/omahab-installer/omahab-install-disk",
		"./scripts/install-disk.sh",
		"scripts/install-disk.sh",
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("omahab-install-disk not found in PATH")
}

func stagePasswordHash(password, dest string) error {
	// Find mkpasswd
	mkpasswd, err := exec.LookPath("mkpasswd")
	if err != nil {
		for _, p := range []string{"/run/current-system/sw/bin/mkpasswd", "/usr/bin/mkpasswd", "/bin/mkpasswd"} {
			if _, e := os.Stat(p); e == nil {
				mkpasswd = p
				err = nil
				break
			}
		}
	}
	if err != nil {
		return fmt.Errorf("mkpasswd not found: %w", err)
	}
	cmd := exec.Command(mkpasswd, "--method=yescrypt", "--stdin")
	cmd.Stdin = strings.NewReader(password)
	var out bytes.Buffer
	var errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("mkpasswd failed: %v: %s", err, errBuf.String())
	}
	hash := strings.TrimSpace(out.String())
	if hash == "" {
		return fmt.Errorf("mkpasswd produced empty hash")
	}
	// Basic validation: yescrypt hash starts with $y$ or $gy$ etc? Just check contains $
	if !strings.Contains(hash, "$") {
		return fmt.Errorf("invalid hash format")
	}
	// Write to dest 0600
	dir := filepath.Dir(dest)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if err := os.WriteFile(dest, []byte(hash+"\n"), 0600); err != nil {
		return err
	}
	_ = os.Chmod(dest, 0600)
	// Also ensure file is owned by root (already root) and not world readable
	return nil
}

