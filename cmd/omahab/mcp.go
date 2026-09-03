package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/omahab/omahab/internal/apiclient"
	"github.com/omahab/omahab/internal/client"
)

func newMCPBridgeCmd() *cobra.Command {
	var flagMCPServer string
	var flagMCPToken string
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "MCP stdio ↔ HTTP bridge (for agents that only speak stdio)",
		Long: `Bridge MCP JSON-RPC between stdio and the Omahab MCP HTTP endpoint.

Reads newline-delimited JSON-RPC requests from stdin, POSTs each to the
MCP HTTP endpoint, and writes the JSON response to stdout (one per line).
Stderr is for diagnostics only and never contains secrets.

Token resolution (first match wins):
  --token flag, OMAHAB_MCP_TOKEN env, device token from keyring (service "omahab" account "device-token"), OMAHAB_TOKEN env, client.json token.
If the token starts with "oma_dev_" the bridge targets /api/v1/companion/mcp (per-device, non-destructive, workspace_* scoped to your creations);
otherwise it targets /mcp (Hermes MCP token, SHA-256 verified).

Server resolution: --server flag, OMAHAB_SERVER env, ~/.config/omahab/client.json server, fallback http://127.0.0.1:8484.
Use --server to point at a different control plane in tests.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Resolve server URL.
			serverURL := strings.TrimSpace(flagMCPServer)
			if serverURL == "" {
				serverURL = strings.TrimSpace(flagServer)
			}
			if serverURL == "" {
				serverURL = strings.TrimSpace(os.Getenv("OMAHAB_SERVER"))
			}
			if serverURL == "" {
				if cfg, err := apiclient.LoadClientConfig(""); err == nil {
					serverURL = apiclient.ResolveServer("", cfg)
				}
			}
			// Also try daemon config as fallback for device context.
			if serverURL == "" {
				if cfg, err := client.LoadConfig(""); err == nil && cfg != nil && strings.TrimSpace(cfg.ServerURL) != "" {
					serverURL = strings.TrimSpace(cfg.ServerURL)
				}
			}
			if serverURL == "" {
				serverURL = "http://127.0.0.1:8484"
			}
			serverURL = strings.TrimRight(serverURL, "/")

			// Resolve token.
			token := strings.TrimSpace(flagMCPToken)
			if token == "" {
				token = strings.TrimSpace(os.Getenv("OMAHAB_MCP_TOKEN"))
			}
			if token == "" {
				// Try device token from keyring (primary for companion MCP).
				if ks := client.NewKeyringStore(); ks != nil {
					if t, err := ks.Get(client.CredentialService, client.CredentialDeviceAccount); err == nil && strings.TrimSpace(t) != "" {
						token = strings.TrimSpace(t)
					}
				}
			}
			if token == "" {
				token = strings.TrimSpace(os.Getenv("OMAHAB_TOKEN"))
			}
			if token == "" {
				// Fallback to file credential store (admin token or device token via file for tests).
				if cfg, err := apiclient.LoadClientConfig(""); err == nil {
					_ = cfg // server already resolved
				}
				// Try apiclient composite store
				store := apiclient.CompositeCredentialStore{
					Stores: []apiclient.CredentialStore{
						apiclient.EnvCredentialStore{},
						apiclient.FileCredentialStore{},
					},
				}
				if t, err := apiclient.ResolveToken(store); err == nil && strings.TrimSpace(t) != "" {
					token = strings.TrimSpace(t)
				}
			}
			if token == "" {
				// Last try: OMAHAB_MCP_URL may contain token via header? No, require token.
				return fmt.Errorf("no token found: set --token, OMAHAB_MCP_TOKEN, device keyring (omahab/device-token), OMAHAB_TOKEN, or login via omahab login")
			}

			// Choose endpoint.
			mcpURL := serverURL + "/mcp"
			if strings.HasPrefix(token, "oma_dev_") {
				mcpURL = serverURL + "/api/v1/companion/mcp"
			}
			// Allow override via OMAHAB_MCP_URL env (explicit).
			if envURL := strings.TrimSpace(os.Getenv("OMAHAB_MCP_URL")); envURL != "" {
				mcpURL = strings.TrimSpace(envURL)
			}

			// Prepare HTTP client.
			httpClient := &http.Client{
				Timeout: flagTimeout,
			}

			// Bridge loop: stdin -> HTTP POST -> stdout
			reader := bufio.NewReader(os.Stdin)
			writer := bufio.NewWriter(os.Stdout)
			defer writer.Flush()

			// Use a scanner with large buffer for big tool responses.
			scanner := bufio.NewScanner(reader)
			const maxCapacity = 10 * 1024 * 1024 // 10 MiB
			buf := make([]byte, 64*1024)
			scanner.Buffer(buf, maxCapacity)
			scanner.Split(bufio.ScanLines)

			// Also handle raw JSON without newline via fallback to reading all?
			// Prefer scanner; if input is not newline-delimited but JSON stream, decoder will handle.
			// We use scanner for line-delimited; if EOF without newline, scanner still returns last line.

			for scanner.Scan() {
				line := scanner.Bytes()
				trimmed := bytes.TrimSpace(line)
				if len(trimmed) == 0 {
					continue
				}
				// Forward to HTTP.
				ctx, cancel := context.WithTimeout(context.Background(), flagTimeout)
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, mcpURL, bytes.NewReader(trimmed))
				if err != nil {
					cancel()
					fmt.Fprintf(os.Stderr, "mcp bridge: new request: %v\n", err)
					continue
				}
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Accept", "application/json, text/event-stream")
				req.Header.Set("Authorization", "Bearer "+token)
				resp, err := httpClient.Do(req)
				cancel()
				if err != nil {
					fmt.Fprintf(os.Stderr, "mcp bridge: do %s: %v\n", mcpURL, err)
					// Write JSON-RPC error to stdout so caller sees failure.
					errPayload := fmt.Sprintf(`{"jsonrpc":"2.0","error":{"code":-32603,"message":%q}}`, err.Error())
					_, _ = writer.WriteString(errPayload + "\n")
					_ = writer.Flush()
					continue
				}
				body, err := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if err != nil {
					fmt.Fprintf(os.Stderr, "mcp bridge: read response: %v\n", err)
					continue
				}
				// MCP may return SSE event-stream (data: {...}) for some responses.
				// For stdio bridge, unwrap SSE data line to raw JSON if needed.
				out := bytes.TrimSpace(body)
				if bytes.HasPrefix(out, []byte("data:")) || bytes.Contains(out, []byte("\ndata:")) {
					// Extract first data: line.
					var jsonLine []byte
					for _, l := range bytes.Split(out, []byte("\n")) {
						l = bytes.TrimSpace(l)
						if bytes.HasPrefix(l, []byte("data:")) {
							j := bytes.TrimSpace(bytes.TrimPrefix(l, []byte("data:")))
							if len(j) > 0 && j[0] == '{' {
								jsonLine = j
								break
							}
						}
					}
					if len(jsonLine) > 0 {
						out = jsonLine
					}
				}
				if len(out) == 0 {
					out = []byte(`{}`)
				}
				if _, err := writer.Write(out); err != nil {
					fmt.Fprintf(os.Stderr, "mcp bridge: write stdout: %v\n", err)
					return err
				}
				if _, err := writer.WriteString("\n"); err != nil {
					return err
				}
				_ = writer.Flush()
			}
			if err := scanner.Err(); err != nil && err != io.EOF {
				fmt.Fprintf(os.Stderr, "mcp bridge: read stdin: %v\n", err)
				return err
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&flagMCPServer, "server", "", "MCP server base URL (overrides --server, OMAHAB_SERVER, client.json)")
	cmd.Flags().StringVar(&flagMCPToken, "token", "", "bearer token (device oma_dev_... or Hermes MCP token; env OMAHAB_MCP_TOKEN, keyring, OMAHAB_TOKEN)")
	return cmd
}
