package main

import (
	"bufio"
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
)

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
		return queryDaemon("open-ai", nil)
	},
}

func init() {
	rootCmd.PersistentFlags().StringVar(&cfgPath, "config", "", "config file path (default $XDG_CONFIG_HOME/omahab/client.json)")
	rootCmd.PersistentFlags().StringVar(&socketPath, "socket", "", "unix socket path (default $XDG_RUNTIME_DIR/omahab-clientd.sock)")

	rootCmd.AddCommand(runCmd)
	rootCmd.AddCommand(statusCmd)
	rootCmd.AddCommand(diagnoseCmd)
	rootCmd.AddCommand(openAICmd)

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

	creds := client.NewMemoryCredentialStore()
	// In production, the daemon would use the desktop keyring. For now,
	// Memory store is safe and keeps credentials out of config/logs.
	// An OS keyring implementation can be injected here when available.

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

func queryDaemon(action string, params map[string]any) error {
	cfg, _, err := loadConfig()
	if err != nil {
		return err
	}
	sock := cfg.EffectiveSocketPath()
	if socketPath != "" {
		sock = socketPath
	}
	// Also check default if config missing.
	if sock == "" {
		sock = client.DefaultSocketPath()
	}
	conn, err := net.DialTimeout("unix", sock, 3*time.Second)
	if err != nil {
		return fmt.Errorf("connect %s: %w (is omahab-clientd running?)", sock, err)
	}
	defer conn.Close()

	req := client.Request{Action: action, Params: params}
	data, _ := json.Marshal(req)
	// Newline-delimited JSON (NDJSON) — one object per line.
	if _, err := conn.Write(append(data, '\n')); err != nil {
		return err
	}
	// Read response (single JSON object, newline or EOF).
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
	var resp client.Response
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		// Try to show raw for debugging (no credentials in response).
		return fmt.Errorf("invalid response: %w: %s", err, raw)
	}
	if !resp.OK {
		return fmt.Errorf("%s failed: %s", action, resp.Error)
	}
	// Pretty-print data.
	if resp.Data != nil {
		enc := json.NewEncoder(os.Stdout)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		_ = enc.Encode(resp.Data)
	} else {
		fmt.Fprintln(os.Stdout, raw)
	}
	return nil
}
