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

	"github.com/omahab/omahab/internal/apiclient"
	"github.com/omahab/omahab/internal/client"
)

func newEnvCmd() *cobra.Command {
	env := &cobra.Command{
		Use:   "env",
		Short: "Tool environment (agent-tools) — sync to Omarchy client",
		Long:  "Manage the synchronized tool environment (agent-tools) via the Omarchy companion. Values are never shown in logs or process args.",
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
			// Try clientd daemon status first.
			if st, err := fetchClientdStatus(); err == nil {
				rev := intFromMap(st, "environment_revision")
				cnt := intFromMap(st, "environment_variable_count")
				syncedAt := strFromMap(st, "environment_synced_at")
				envErr := strFromMap(st, "environment_error")
				if flagJSON {
					return printJSON(map[string]any{
						"revision":       rev,
						"variable_count": cnt,
						"synced_at":      syncedAt,
						"error":          envErr,
						"file":           client.EnvironmentFilePath(),
					})
				}
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
				// Also show file presence.
				if fi, err := os.Stat(client.EnvironmentFilePath()); err == nil {
					fmt.Printf("file_mode: %o\n", fi.Mode().Perm())
				}
				return nil
			}
			// Fallback: file-based when daemon not running.
			path := client.EnvironmentFilePath()
			keys := envFileKeys(path)
			if flagJSON {
				var syncedAt string
				if fi, err := os.Stat(path); err == nil {
					syncedAt = fi.ModTime().UTC().Format(time.RFC3339)
				}
				return printJSON(map[string]any{
					"revision":       0,
					"variable_count": len(keys),
					"synced_at":      syncedAt,
					"error":          "clientd not running",
					"file":           path,
				})
			}
			fmt.Printf("revision: 0\n")
			fmt.Printf("variable_count: %d\n", len(keys))
			if fi, err := os.Stat(path); err == nil {
				fmt.Printf("synced_at: %s (file mtime)\n", fi.ModTime().UTC().Format(time.RFC3339))
			} else {
				fmt.Printf("synced_at: <never>\n")
			}
			fmt.Printf("error: clientd not running\n")
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
		Short: "Sync tool environment now (via clientd, ETag/304, atomic 0600, D-Bus)",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := newContext()
			defer cancel()
			// Prefer clientd socket.
			cd := clientd()
			if cd.Available(ctx) {
				// Try raw socket sync for reliable status; clientd HTTP may not have env endpoint.
				if err := envViaClientdSocket("environment.sync"); err == nil {
					if flagJSON {
						return printJSON(map[string]string{"result": "environment synced", "detail": "Applied to new apps; restart existing apps"})
					}
					fmt.Println("environment synced")
					fmt.Println("Applied to new apps; restart existing apps")
					return nil
				}
				// Fallback to HTTP clientd if raw fails? Try via apiclient status fallback.
			}
			// Fallback: if clientd not available, try direct sync via EnvironmentManager if we have a device token.
			// For omahab host CLI, direct device sync is not expected; inform user.
			if cd.Available(ctx) {
				return fmt.Errorf("clientd sync failed (is device enrolled? check 'omahab-clientd enroll')")
			}
			// Try direct via client config if possible (best-effort).
			if err := directOmahabEnvSync(ctx); err != nil {
				if flagJSON {
					return printJSON(map[string]string{"error": err.Error(), "hint": "ensure omahab-clientd is running and device is enrolled"})
				}
				return handleFailure(fmt.Errorf("sync failed: %w (hint: ensure omahab-clientd is running and device is enrolled via 'omahab-clientd enroll')", err))
			}
			if flagJSON {
				return printJSON(map[string]string{"result": "environment synced (direct)", "detail": "Applied to new apps; restart existing apps"})
			}
			fmt.Println("environment synced (direct)")
			fmt.Println("Applied to new apps; restart existing apps")
			return nil
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
				if isNonInteractive() {
					return fmt.Errorf("clear requires --confirm in non-interactive mode")
				}
				if !confirmPrompt("Remove managed environment file and unset from systemd user manager?") {
					fmt.Println("aborted")
					return nil
				}
			}
			ctx, cancel := newContext()
			defer cancel()
			cd := clientd()
			if cd.Available(ctx) {
				if err := envViaClientdSocket("environment.clear"); err == nil {
					if flagJSON {
						return printJSON(map[string]string{"result": "environment cleared"})
					}
					fmt.Println("environment cleared")
					fmt.Println("Removed managed file and unset from systemd user manager; does not revoke server grants")
					return nil
				}
			}
			if cd.Available(ctx) {
				return fmt.Errorf("clientd clear failed")
			}
			if err := directOmahabEnvClear(ctx); err != nil {
				if flagJSON {
					return printJSON(map[string]string{"error": err.Error()})
				}
				return handleFailure(err)
			}
			if flagJSON {
				return printJSON(map[string]string{"result": "environment cleared (direct)"})
			}
			fmt.Println("environment cleared (direct)")
			return nil
		},
	}
	c.Flags().BoolVar(&confirm, "confirm", false, "confirm removal without prompt")
	// Also support --yes alias via persistent flag, but add explicit.
	c.Flags().BoolVarP(&confirm, "yes", "y", false, "alias for --confirm")
	return c
}

// fetchClientdStatus dials clientd socket and returns status map (including env fields).
func fetchClientdStatus() (map[string]any, error) {
	sock := apiclient.DefaultClientdSocketPath()
	// Also try client config override.
	if cfg, _, err := loadClientConfigForEnv(); err == nil && cfg != nil {
		if p := cfg.EffectiveSocketPath(); p != "" {
			sock = p
		}
	}
	conn, err := net.DialTimeout("unix", sock, 3*time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	req := client.SocketRequest{ID: "1", Method: "status", Params: map[string]any{}}
	data, _ := json.Marshal(req)
	if _, err := conn.Write(append(data, '\n')); err != nil {
		return nil, err
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(conn)
	var buf strings.Builder
	tmp := make([]byte, 4096)
	for {
		n, rerr := br.Read(tmp)
		if n > 0 {
			buf.Write(tmp[:n])
			trimmed := strings.TrimSpace(buf.String())
			if trimmed != "" && json.Valid([]byte(trimmed)) && br.Buffered() == 0 {
				break
			}
		}
		if rerr != nil {
			break
		}
	}
	raw := strings.TrimSpace(buf.String())
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
	b, _ := json.Marshal(resp.Result)
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func envViaClientdSocket(method string) error {
	sock := apiclient.DefaultClientdSocketPath()
	if cfg, _, err := loadClientConfigForEnv(); err == nil && cfg != nil {
		if p := cfg.EffectiveSocketPath(); p != "" {
			sock = p
		}
	}
	conn, err := net.DialTimeout("unix", sock, 3*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	req := client.SocketRequest{ID: "1", Method: method, Params: map[string]any{}}
	data, _ := json.Marshal(req)
	if _, err := conn.Write(append(data, '\n')); err != nil {
		return err
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(conn)
	var buf strings.Builder
	tmp := make([]byte, 4096)
	for {
		n, rerr := br.Read(tmp)
		if n > 0 {
			buf.Write(tmp[:n])
			trimmed := strings.TrimSpace(buf.String())
			if trimmed != "" && json.Valid([]byte(trimmed)) && br.Buffered() == 0 {
				break
			}
		}
		if rerr != nil {
			break
		}
	}
	raw := strings.TrimSpace(buf.String())
	if raw == "" {
		return fmt.Errorf("empty response")
	}
	// Try SocketResponse first.
	var sresp client.SocketResponse
	if err := json.Unmarshal([]byte(raw), &sresp); err == nil {
		if sresp.Error != nil {
			return fmt.Errorf("%s", sresp.Error.Message)
		}
		return nil
	}
	// Fallback to legacy Response.
	var resp client.Response
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("%s", resp.Error)
	}
	return nil
}

func loadClientConfigForEnv() (*client.Config, string, error) {
	path := client.DefaultConfigPath()
	cfg, err := client.LoadConfig(path)
	if err != nil {
		return nil, path, err
	}
	return cfg, path, nil
}

func intFromMap(m map[string]any, key string) int {
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

func strFromMap(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func envFileKeys(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var keys []string
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
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

func directOmahabEnvSync(ctx context.Context) error {
	cfg, _, err := loadClientConfigForEnv()
	if err != nil {
		return err
	}
	if strings.TrimSpace(cfg.ServerURL) == "" {
		return fmt.Errorf("no server_url configured")
	}
	creds := client.NewMemoryCredentialStore()
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
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return mgr.Sync(cctx)
}

func directOmahabEnvClear(ctx context.Context) error {
	cfg, _, err := loadClientConfigForEnv()
	if err != nil {
		return err
	}
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
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return mgr.Clear(cctx)
}
