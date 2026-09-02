package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/omahab/omahab/internal/client"
)

// newEnvCmd returns `omahab-clientd env` parent with status/sync/clear.
func newEnvCmd() *cobra.Command {
	env := &cobra.Command{
		Use:   "env",
		Short: "Tool environment sync (agent-tools)",
		Long:  "Manage the synchronized tool environment (agent-tools) — values never shown in logs or process args.",
	}
	env.AddCommand(newEnvStatusCmd())
	env.AddCommand(newEnvSyncCmd())
	env.AddCommand(newEnvClearCmd())
	return env
}

func newEnvStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show tool environment sync status (redacted)",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Try daemon status first.
			cfg, _, err := loadConfig()
			if err == nil {
				sock := cfg.EffectiveSocketPath()
				if socketPath != "" {
					sock = socketPath
				}
				if st, err := queryDaemonStatus(sock); err == nil {
					// st is DaemonStatus JSON map
					rev := intValue(st, "environment_revision")
					cnt := intValue(st, "environment_variable_count")
					syncedAt := stringValue(st, "environment_synced_at")
					envErr := stringValue(st, "environment_error")
					fmt.Printf("revision: %d\n", rev)
					fmt.Printf("variable_count: %d\n", cnt)
					if syncedAt != "" {
						fmt.Printf("synced_at: %s\n", syncedAt)
					} else {
						fmt.Printf("synced_at: <never>\n")
					}
					if envErr != "" {
						fmt.Printf("error: %s\n", envErr)
					}
					fmt.Printf("file: %s\n", client.EnvironmentFilePath())
					return nil
				}
			}
			// Fallback: file-based status when daemon not running.
			path := client.EnvironmentFilePath()
			keys := readEnvFileKeys(path)
			fmt.Printf("revision: %d\n", 0)
			fmt.Printf("variable_count: %d\n", len(keys))
			if fi, err := os.Stat(path); err == nil {
				fmt.Printf("synced_at: %s (file mtime)\n", fi.ModTime().UTC().Format(time.RFC3339))
			} else {
				fmt.Printf("synced_at: <never>\n")
			}
			fmt.Printf("error: daemon not running\n")
			fmt.Printf("file: %s\n", path)
			if len(keys) > 0 {
				fmt.Printf("managed_keys: %s\n", strings.Join(keys, ", "))
			}
			return nil
		},
	}
}

func newEnvSyncCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "sync",
		Short: "Sync tool environment from server (fetch with ETag, atomic 0600, D-Bus live apply)",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Try daemon first.
			if err := queryDaemon("environment.sync", nil); err == nil {
				fmt.Println("environment synced")
				fmt.Println("Applied to new apps; restart existing apps")
				return nil
			} else {
				// If daemon not available, try direct sync.
				if isDaemonNotRunning(err) {
					fmt.Fprintln(os.Stderr, "daemon not running, trying direct sync...")
					if err2 := directEnvSync(); err2 != nil {
						return fmt.Errorf("direct sync failed: %w", err2)
					}
					fmt.Println("environment synced (direct)")
					fmt.Println("Applied to new apps; restart existing apps")
					return nil
				}
				return err
			}
		},
	}
}

func newEnvClearCmd() *cobra.Command {
	var confirm bool
	c := &cobra.Command{
		Use:   "clear",
		Short: "Remove managed tool environment file and unset via D-Bus (requires --confirm)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if !confirm {
				if !isTerminal() {
					return fmt.Errorf("clear requires --confirm (non-interactive)")
				}
				fmt.Print("Remove managed environment file and unset from systemd user manager? [y/N]: ")
				reader := bufio.NewReader(os.Stdin)
				line, _ := reader.ReadString('\n')
				line = strings.TrimSpace(strings.ToLower(line))
				if line != "y" && line != "yes" {
					fmt.Println("aborted")
					return nil
				}
			}
			// Try daemon first.
			if err := queryDaemon("environment.clear", nil); err == nil {
				fmt.Println("environment cleared")
				fmt.Println("Removed managed file and unset from systemd user manager; does not revoke server grants")
				return nil
			} else {
				if isDaemonNotRunning(err) {
					fmt.Fprintln(os.Stderr, "daemon not running, trying direct clear...")
					if err2 := directEnvClear(); err2 != nil {
						return fmt.Errorf("direct clear failed: %w", err2)
					}
					fmt.Println("environment cleared (direct)")
					return nil
				}
				return err
			}
		},
	}
	c.Flags().BoolVar(&confirm, "confirm", false, "confirm removal without prompt")
	return c
}

func init() {
	// Register env commands.
	rootCmd.AddCommand(newEnvCmd())
}

func isDaemonNotRunning(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "is omahab-clientd running") || strings.Contains(msg, "connect") && strings.Contains(msg, "no such file") || strings.Contains(msg, "connection refused")
}

func isTerminal() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

func intValue(m map[string]any, key string) int {
	if v, ok := m[key]; ok {
		switch x := v.(type) {
		case float64:
			return int(x)
		case int:
			return x
		case json.Number:
			i, _ := x.Int64()
			return int(i)
		}
	}
	return 0
}

func stringValue(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func queryDaemonStatus(sock string) (map[string]any, error) {
	if sock == "" {
		cfg, _, err := loadConfig()
		if err != nil {
			return nil, err
		}
		sock = cfg.EffectiveSocketPath()
		if socketPath != "" {
			sock = socketPath
		}
	}
	conn, err := dialDaemon(sock)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	// Use SocketRequest for status to get full DaemonStatus including env fields.
	req := client.SocketRequest{ID: "1", Method: "status", Params: map[string]any{}}
	data, _ := json.Marshal(req)
	if _, err := conn.Write(append(data, '\n')); err != nil {
		return nil, err
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
		return nil, fmt.Errorf("empty response")
	}
	var resp client.SocketResponse
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("%s", resp.Error.Message)
	}
	if m, ok := resp.Result.(map[string]any); ok {
		return m, nil
	}
	// Try to remarshal.
	b, _ := json.Marshal(resp.Result)
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func dialDaemon(sock string) (netConn, error) {
	// netConn abstracts net.Conn for testability.
	conn, err := netDial(sock)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// netConn and netDial are thin wrappers to allow import cycle handling.
type netConn = interface {
	Read([]byte) (int, error)
	Write([]byte) (int, error)
	Close() error
	SetReadDeadline(time.Time) error
}

func netDial(sock string) (netConn, error) {
	return net.DialTimeout("unix", sock, 3*time.Second)
}

func readEnvFileKeys(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var keys []string
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.Index(line, "=")
		if idx <= 0 {
			continue
		}
		k := strings.TrimSpace(line[:idx])
		if k != "" {
			keys = append(keys, k)
		}
	}
	return keys
}

func directEnvSync() error {
	cfg, _, err := loadConfig()
	if err != nil {
		return err
	}
	if strings.TrimSpace(cfg.ServerURL) == "" {
		return fmt.Errorf("no server_url configured")
	}
	creds := client.NewMemoryCredentialStore()
	// If daemon's creds were keyring, direct sync via memory will fail; hint.
	remote, err := client.NewRemoteClient(client.RemoteClientConfig{
		ServerURL:        cfg.ServerURL,
		PinnedInstanceID: cfg.PinnedInstanceID,
		CredentialStore:  creds,
	})
	if err != nil {
		return err
	}
	mgr := client.NewEnvironmentManager(client.EnvironmentManagerOpts{
		Remote: remote,
		Creds:  creds,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return mgr.Sync(ctx)
}

func directEnvClear() error {
	cfg, _, err := loadConfig()
	if err != nil {
		return err
	}
	// For clear we don't need remote; just file + D-Bus.
	creds := client.NewMemoryCredentialStore()
	var remote *client.RemoteClient
	if strings.TrimSpace(cfg.ServerURL) != "" {
		remote, _ = client.NewRemoteClient(client.RemoteClientConfig{
			ServerURL:        cfg.ServerURL,
			PinnedInstanceID: cfg.PinnedInstanceID,
			CredentialStore:  creds,
		})
	}
	mgr := client.NewEnvironmentManager(client.EnvironmentManagerOpts{
		Remote: remote,
		Creds:  creds,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return mgr.Clear(ctx)
}
