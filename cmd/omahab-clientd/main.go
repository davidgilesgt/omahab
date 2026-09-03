package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/omahab/omahab/internal/client"
)

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

var (
	cfgPath    string
	socketPath string
	version    = "dev"
)

func init() {
	// Wire compiled-in version into internal/client for self-update comparison.
	client.Version = version
}

var rootCmd = &cobra.Command{
	Use:   "omahab-clientd",
	Short: "Omahab user daemon (Omarchy companion)",
	Long:  "omahab-clientd is a user-level daemon that syncs status/events, manages project fetch state, and exposes a Unix-socket JSON API for the Omarchy shell plugin.",
}

var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Run the daemon (foreground)",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runDaemon()
	},
}

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Query daemon status via Unix socket",
	RunE: func(cmd *cobra.Command, args []string) error {
		return queryDaemon("status", nil)
	},
}

var diagnoseCmd = &cobra.Command{
	Use:   "diagnose",
	Short: "Run connection diagnostics via daemon",
	RunE: func(cmd *cobra.Command, args []string) error {
		return queryDaemon("diagnose", nil)
	},
}

var openAICmd = &cobra.Command{
	Use:   "open-ai",
	Short: "Open AI (Hermes) via daemon",
	RunE: func(cmd *cobra.Command, args []string) error {
		return queryDaemon("ai.open", nil)
	},
}


var enrollCmd = &cobra.Command{
	Use:   "enroll",
	Short: "Enroll this device with a single-use enrollment code",
	Long:  "Enroll this device as a companion. Prompts securely for a single-use code (never as argument) and stores the device token (oma_dev_...) in the desktop keyring (service \"omahab\", account \"device-token\"). Never falls back to plaintext.",
	Args: func(cmd *cobra.Command, args []string) error {
		if len(args) != 0 {
			return fmt.Errorf("code must not be passed as argument; enroll will prompt securely")
		}
		return nil
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		return runEnroll()
	},
}

func init() {
	rootCmd.PersistentFlags().StringVar(&cfgPath, "config", "", "config file path (default $XDG_CONFIG_HOME/omahab/client.json)")
	rootCmd.PersistentFlags().StringVar(&socketPath, "socket", "", "unix socket path (default $XDG_RUNTIME_DIR/omahab-clientd.sock)")

	rootCmd.AddCommand(runCmd)
	rootCmd.AddCommand(statusCmd)
	rootCmd.AddCommand(diagnoseCmd)
	rootCmd.AddCommand(openAICmd)
	rootCmd.AddCommand(enrollCmd)

	// Default action is run when no subcommand given.
	rootCmd.RunE = func(cmd *cobra.Command, args []string) error {
		return runDaemon()
	}
}

func loadConfig() (*client.Config, string, error) {
	path := cfgPath
	if path == "" {
		path = client.DefaultConfigPath()
	}
	cfg, err := client.LoadConfig(path)
	if err != nil {
		return nil, path, err
	}
	if socketPath != "" {
		cfg.SocketPath = socketPath
	}
	return cfg, path, nil
}

func runDaemon() error {
	cfg, cfgFile, err := loadConfig()
	if err != nil {
		return err
	}
	// Ensure config dir exists but never log credentials (config has none).
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if cfg.ServerURL == "" {
		logger.Warn("no server_url configured", "config", cfgFile, "hint", "set server_url and pinned_instance_id via enrollment")
	}

	creds := client.NewKeyringStore()
	// Verify keyring availability early: fail with diagnostic if Secret Service missing, never fallback to plaintext.
	if _, err := creds.Get(client.CredentialService, client.CredentialDeviceAccount); err != nil && !isCredentialNotFound(err) {
		// If error is secret/dbus related, fail fast with diagnostic.
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "secret") || strings.Contains(msg, "dbus") {
			return fmt.Errorf("keyring unavailable: %v — ensure Secret Service (gnome-keyring/kwallet) is running for user %q", err, os.Getenv("USER"))
		}
	}

	daemon, err := client.NewDaemon(client.DaemonOpts{
		Config:          cfg,
		CredentialStore: creds,
		Launcher:        &client.ExecLauncher{},
		Logger:          logger,
	})
	if err != nil {
		return err
	}

	if err := daemon.Start(); err != nil {
		return err
	}
	defer func() {
		_ = daemon.Stop()
	}()

	// Handle signals for clean stop.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	logger.Info("shutting down on signal")
	return nil
}

func isCredentialNotFound(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "credential not found") || strings.Contains(err.Error(), "not found")
}

func runEnroll() error {
	cfg, _, err := loadConfig()
	if err != nil {
		return err
	}
	if strings.TrimSpace(cfg.ServerURL) == "" {
		return fmt.Errorf("server_url not configured; set server_url in %s", client.DefaultConfigPath())
	}
	// Hidden prompt for code (never as argument, never echo, never in logs).
	fmt.Fprint(os.Stderr, "Enter enrollment code: ")
	var code string
	if term.IsTerminal(int(os.Stdin.Fd())) {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr, "")
		if err != nil {
			return fmt.Errorf("read enrollment code: %w", err)
		}
		code = strings.TrimSpace(string(b))
	} else {
		reader := bufio.NewReader(os.Stdin)
		line, err := reader.ReadString('\n')
		if err != nil && err.Error() != "EOF" {
			return fmt.Errorf("read enrollment code: %w", err)
		}
		code = strings.TrimSpace(line)
	}
	if code == "" {
		return fmt.Errorf("enrollment code is required")
	}
	if strings.Contains(code, "\x00") {
		return fmt.Errorf("code contains NUL")
	}
	// Use RemoteClient with no prior credentials (enroll is unauthenticated).
	rc, err := client.NewRemoteClient(client.RemoteClientConfig{
		ServerURL: cfg.ServerURL,
		// No CredentialStore needed for unauthenticated enroll; use nil to avoid bearer.
		HTTPClient: nil,
	})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	res, err := rc.EnrollCompanion(ctx, code)
	if err != nil {
		return fmt.Errorf("enroll failed: %w", err)
	}
	token := strings.TrimSpace(res.Token)
	if !strings.HasPrefix(token, "oma_dev_") {
		return fmt.Errorf("invalid device token received")
	}
	// Store only via keyring, service omahab account device-token, no plaintext fallback.
	ks := client.NewKeyringStore()
	if err := ks.Set(client.CredentialService, client.CredentialDeviceAccount, token); err != nil {
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "secret") || strings.Contains(msg, "dbus") {
			return fmt.Errorf("keyring unavailable (Secret Service not found): %v — cannot store device token without Secret Service; ensure org.freedesktop.secrets is running", err)
		}
		return fmt.Errorf("store device token: %w", err)
	}
	// Store per-device machine backup credentials when server provided them (Step 10).
	if strings.TrimSpace(res.ResticRepo) != "" {
		_ = ks.Set(client.CredentialService, client.CredentialBackupRepo, strings.TrimSpace(res.ResticRepo))
		_ = ks.Set(client.CredentialService, client.CredentialBackupPassword, strings.TrimSpace(res.ResticPassword))
		_ = ks.Set(client.CredentialService, client.CredentialBackupRestUser, strings.TrimSpace(res.RestUser))
		_ = ks.Set(client.CredentialService, client.CredentialBackupRestPassword, strings.TrimSpace(res.RestPassword))
		fmt.Fprintln(os.Stderr, "Machine backup credentials stored (backup-repo, backup-password, backup-rest-*).")
	}
	// Store per-device Forgejo token for git clone/push (C4) — same path as workspace ws-<id> tokens.
	if strings.TrimSpace(res.ForgejoToken) != "" {
		_ = ks.Set(client.CredentialService, client.CredentialForgejoToken, strings.TrimSpace(res.ForgejoToken))
		if h := strings.TrimSpace(res.ForgejoHost); h != "" {
			_ = ks.Set(client.CredentialService, client.CredentialForgejoHost, h)
		}
		fmt.Fprintln(os.Stderr, "Forgejo git token stored (forgejo-token).")
	}
	fmt.Fprintln(os.Stderr, "Enrolled successfully. Device token stored in keyring (service \"omahab\", account \"device-token\").")
	for i := range []byte(code) {
		_ = i
	}
	return nil
}

func queryDaemon(method string, params map[string]any) error {
	cfg, _, err := loadConfig()
	if err != nil {
		return err
	}
	sock := cfg.EffectiveSocketPath()
	if socketPath != "" {
		sock = socketPath
	}
	if sock == "" {
		sock = client.DefaultSocketPath()
	}
	conn, err := net.DialTimeout("unix", sock, 3*time.Second)
	if err != nil {
		return fmt.Errorf("connect %s: %w (is omahab-clientd running?)", sock, err)
	}
	defer conn.Close()

	req := client.SocketRequest{ID: "1", Method: method, Params: params}
	data, _ := json.Marshal(req)
	if _, err := conn.Write(append(data, '\n')); err != nil {
		return err
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(conn)
	var respBuf strings.Builder
	tmp := make([]byte, 4096)
	for {
		n, rerr := br.Read(tmp)
		if n > 0 {
			respBuf.Write(tmp[:n])
			trimmed := strings.TrimSpace(respBuf.String())
			if trimmed != "" && json.Valid([]byte(trimmed)) && br.Buffered() == 0 {
				break
			}
		}
		if rerr != nil {
			break
		}
	}
	raw := strings.TrimSpace(respBuf.String())
	if raw == "" {
		return fmt.Errorf("empty response from daemon")
	}
	var resp client.SocketResponse
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		return fmt.Errorf("invalid response: %w: %s", err, raw)
	}
	if resp.Error != nil {
		return fmt.Errorf("%s failed: %s (%s)", method, resp.Error.Message, resp.Error.Code)
	}
	if resp.Result != nil {
		enc := json.NewEncoder(os.Stdout)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		_ = enc.Encode(resp.Result)
	} else {
		fmt.Fprintln(os.Stdout, raw)
	}
	return nil
}
