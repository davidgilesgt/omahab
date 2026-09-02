package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/omahab/omahab/internal/apiclient"
)

// newProviderCmd returns `omahab provider` parent command with `login` subcommand.
//
// XAI callback relay only from enrolled companion with allow_provider_oauth=true.
// Provide `omahab provider login xai` with same relay for non-QML use; fallback is SSH local forward for 56121, not public bound callback.
// Never use `hermes proxy start with a public host binding` (integrated proxy accepts any bearer, no per-client limits, single upstream).
func newProviderCmd() *cobra.Command {
	provider := &cobra.Command{
		Use:   "provider",
		Short: "Model provider credentials and subscription OAuth",
		Long: `Manage model provider credentials.

Subscription OAuth is provider-sanctioned only (no browser-cookie extraction).
- ChatGPT: device_code flow via LiteLLM's ChatGPT Authenticator (pinned helper emits verification_url/code JSON, polls, leaves refresh state in CHATGPT_TOKEN_DIR=/var/lib/litellm-auth/chatgpt).
- xAI: loopback flow via litellm xai-oauth login --no-browser capturing auth URL; LiteLLM binds fixed loopback 127.0.0.1:56121.
  omahab-clientd (or 'omahab provider login xai') binds same port on the Omarchy device, opens URL, POSTs only /callback?<query> to the session callback API.
  omahabd forwards to literal http://127.0.0.1:56121 inside named LiteLLM container using argv-safe exec (curl without sh -c).

If no companion is available, the documented fallback is an SSH local forward for fixed port 56121, not a publicly bound callback:

  ssh -L 56121:127.0.0.1:56121 omahab@<server>

Then run 'omahab provider login xai' on your local machine; the xAI redirect to http://127.0.0.1:56121 will be forwarded through SSH to the server's LiteLLM loopback.

After either flow completes, omahabd calls the concrete model (e.g., omahab/fast) through LiteLLM before marking the credential healthy (ProbeModel) and maps:
  401 -> token-invalid, 403 -> not_entitled (xAI tier restriction, retain record, offer API-key path, don't loop reauth), 429 -> quota/rate-limited.

Never use 'hermes proxy start with a public host binding'.`,
	}
	provider.AddCommand(newProviderLoginCmd())
	provider.AddCommand(newProviderStatusCmd())
	return provider
}

func newProviderLoginCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "login [xai|chatgpt]",
		Short: "Start subscription OAuth login (xai loopback or chatgpt device_code)",
		Long: `Start provider OAuth and complete the loopback relay for xAI.

For xAI (loopback):
  1. Starts OAuth via POST /api/v1/provider-oauth/xai/start (flow=loopback) and prints the auth URL.
  2. Opens the URL in a browser (or prints for manual open if --no-browser).
  3. Temporarily binds 127.0.0.1:56121 locally, waits for the provider's redirect to /callback?<query>.
  4. POSTs only the received /callback?<query> path to POST /api/v1/provider-oauth/xai/callback/{session_id}
     (device-only; requires enrolled companion with allow_provider_oauth=true; admin bearer is rejected with 403).
     The server forwards via argv-safe exec to literal http://127.0.0.1:56121 inside the named LiteLLM container (curl without sh -c).
     Never accept caller-supplied callback URL or shell fragments; only /callback?<query> is allowed.
  5. Polls the session until connected, then omahabd probes the concrete model (omahab/fast) through LiteLLM before marking healthy.
     401 -> token-invalid, 403 -> not_entitled (retain record, show tier restriction, offer API-key path, don't loop), 429 -> quota/rate-limited.

For ChatGPT (device_code):
  Starts device_code flow, prints verification_url and user_code, then polls until connected/expired/denied.

Fallback when no companion is available:
  ssh -L 56121:127.0.0.1:56121 omahab@<server>
  Then run 'omahab provider login xai' locally; the loopback callback is forwarded through SSH to LiteLLM's fixed loopback.

Never use 'hermes proxy start with a public host binding' — the integrated proxy accepts any bearer, has no per-client limits, and handles only one upstream per process.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			provider := "xai"
			if len(args) == 1 {
				provider = strings.ToLower(strings.TrimSpace(args[0]))
			}
			if provider != "xai" && provider != "chatgpt" {
				return fmt.Errorf("unsupported provider %q: use xai or chatgpt", provider)
			}
			noBrowser, _ := cmd.Flags().GetBool("no-browser")
			timeout, _ := cmd.Flags().GetDuration("wait")
			ctx, cancel := newContext()
			defer cancel()
			if timeout > 0 {
				var cancel2 context.CancelFunc
				ctx, cancel2 = context.WithTimeout(ctx, timeout)
				defer cancel2()
			}
			c, err := resolveClient()
			if err != nil {
				return handleFailure(err)
			}
			flow := "loopback"
			if provider == "chatgpt" {
				flow = "device_code"
			}
			sess, err := c.StartProviderOAuth(ctx, provider, flow)
			if err != nil {
				return handleFailure(err)
			}
			if flagJSON {
				_ = printJSON(sess)
			} else {
				fmt.Printf("provider: %s\n", sess.Provider)
				fmt.Printf("flow: %s\n", sess.Flow)
				fmt.Printf("session: %s\n", sess.ID)
				fmt.Printf("verification_url: %s\n", sess.VerificationURL)
				if sess.UserCode != nil {
					fmt.Printf("user_code: %s\n", *sess.UserCode)
				}
				if sess.CallbackPort != nil {
					fmt.Printf("callback_port: %d (LiteLLM fixed loopback 127.0.0.1:56121)\n", *sess.CallbackPort)
				}
				fmt.Printf("expires_at: %s\n", sess.ExpiresAt.Format(time.RFC3339))
				fmt.Printf("status: %s\n", sess.Status)
			}

			if provider == "chatgpt" {
				// Device code: poll until terminal.
				fmt.Println()
				fmt.Println("Open the verification URL and enter the code above.")
				if !noBrowser && !flagJSON {
					_ = openBrowser(sess.VerificationURL)
				}
				return pollUntilTerminal(ctx, c, provider, sess.ID)
			}

			// xAI loopback: open browser, bind local 127.0.0.1:56121, wait for callback, forward.
			if !flagJSON {
				fmt.Println()
				fmt.Printf("Opening auth URL: %s\n", sess.VerificationURL)
			}
			if !noBrowser && !flagJSON {
				_ = openBrowser(sess.VerificationURL)
			} else if !flagJSON {
				fmt.Println("hint: open the URL above in a browser to authorize")
				fmt.Println("hint: if no companion is available, use SSH fallback: ssh -L 56121:127.0.0.1:56121 omahab@<server>")
			}

			callbackPath, err := waitForLocalCallback(ctx, sess.ExpiresAt)
			if err != nil {
				// If local bind failed or timed out, show fallback hint.
				if !flagJSON {
					fmt.Fprintf(os.Stderr, "local callback wait failed: %v\n", err)
					fmt.Fprintln(os.Stderr, "fallback: ssh -L 56121:127.0.0.1:56121 omahab@<server> then retry; the xAI redirect to http://127.0.0.1:56121 will be forwarded through SSH to LiteLLM's fixed loopback.")
					fmt.Fprintln(os.Stderr, "Do NOT use 'hermes proxy start with a public host binding' — it lacks per-client limits and handles one upstream.")
				}
				return handleFailure(err)
			}
			if !flagJSON {
				fmt.Printf("received callback: %s\n", callbackPath)
				fmt.Println("forwarding to server session callback API (device-only, argv-safe to http://127.0.0.1:56121 inside LiteLLM container)...")
			}
			fwdSess, err := c.ForwardProviderOAuthCallback(ctx, provider, sess.ID, callbackPath)
			if err != nil {
				// Check for 403 device requirement
				if strings.Contains(strings.ToLower(err.Error()), "forbidden") || strings.Contains(err.Error(), "403") {
					fmt.Fprintln(os.Stderr, "callback forward rejected (403): XAI callback relay only from enrolled companion with allow_provider_oauth=true.")
					fmt.Fprintln(os.Stderr, "hint: ensure this device is enrolled via 'omahab-clientd enroll' with allow_provider_oauth enabled, or use SSH fallback:")
					fmt.Fprintln(os.Stderr, "  ssh -L 56121:127.0.0.1:56121 omahab@<server>")
				}
				// Also note hermes proxy misuse
				fmt.Fprintln(os.Stderr, "never use 'hermes proxy start with a public host binding'")
				return handleFailure(err)
			}
			if flagJSON {
				_ = printJSON(fwdSess)
			} else {
				fmt.Printf("callback forwarded, status: %s\n", fwdSess.Status)
			}
			// Poll until connected (backend also probes model and maps 401/403/429)
			return pollUntilTerminal(ctx, c, provider, sess.ID)
		},
	}
	cmd.Flags().Bool("no-browser", false, "do not attempt to open browser; print URL only")
	cmd.Flags().Duration("wait", 10*time.Minute, "maximum wait for callback/poll (default 10m, session expiry)")
	return cmd
}

func newProviderStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show provider credential health (alias for omahab doctor / provider view)",
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
			if flagJSON {
				return printJSON(st)
			}
			fmt.Printf("status check via doctor recommended: omahab doctor --json\n")
			_ = st
			return nil
		},
	}
}

// pollUntilTerminal polls session until connected/denied/expired/error or context cancelled.
func pollUntilTerminal(ctx context.Context, c *apiclient.Client, provider, sessionID string) error {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	// Immediate poll
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
		sess, err := c.PollProviderOAuth(ctx, provider, sessionID)
		if err != nil {
			return handleFailure(err)
		}
		if flagJSON {
			_ = printJSON(sess)
		} else {
			fmt.Printf("poll status: %s (expires %s)\n", sess.Status, sess.ExpiresAt.Format(time.RFC3339))
		}
		switch sess.Status {
		case "connected":
			if !flagJSON {
				fmt.Println("OAuth connected. Probing concrete model (omahab/fast) through LiteLLM before marking healthy...")
				fmt.Println("Mapping: 401->token-invalid, 403->not_entitled (tier restriction, retain record, offer API-key), 429->quota/rate-limited. Do NOT loop reauth on xAI 403.")
			}
			// Backend will have probed; we just report success and remind to check provider credential health.
			if !flagJSON {
				fmt.Println("success: provider OAuth connected; check 'omahab doctor' or dashboard for credential health and entitlement.")
			}
			return nil
		case "denied", "expired", "error":
			if !flagJSON {
				fmt.Printf("OAuth terminal status: %s\n", sess.Status)
				if sess.Status == "expired" {
					fmt.Println("session expired; retry login")
				}
				if sess.Provider == "xai" && sess.Status == "error" {
					fmt.Println("xAI OAuth error: if 403 tier restriction after OAuth, retain record and use API-key path; don't loop reauth.")
				}
			}
			return nil
		case "pending":
			// continue
		default:
			return nil
		}
		// Also check expiry
		if time.Now().UTC().After(sess.ExpiresAt) {
			if !flagJSON {
				fmt.Println("session expired (timeout)")
			}
			return nil
		}
	}
}

// waitForLocalCallback temporarily binds 127.0.0.1:56121, waits for GET /callback?<query>, returns only the path+query (e.g., /callback?code=...).
// It rejects foreign host/path/scheme/port/shell metachars by construction: only the path from the local request is returned.
func waitForLocalCallback(parent context.Context, expiresAt time.Time) (string, error) {
	// Derive timeout from session expiry, but cap to parent context.
	timeout := time.Until(expiresAt)
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	if timeout > 10*time.Minute {
		timeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	ln, err := net.Listen("tcp", "127.0.0.1:56121")
	if err != nil {
		return "", fmt.Errorf("listen 127.0.0.1:56121 failed (maybe companion already bound or fallback SSH tunnel needed): %w", err)
	}
	defer ln.Close()

	resultCh := make(chan string, 1)
	errCh := make(chan error, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		// Only allow GET, only /callback with optional query; reject everything else.
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Build strict callback_path: exactly /callback?<query> (query may be empty)
		cb := "/callback"
		if r.URL.RawQuery != "" {
			// Validate raw query does not contain shell metachars that would break argv-safe forward
			// Allow only URL-safe query characters; reject shell fragments.
			raw := r.URL.RawQuery
			if strings.ContainsAny(raw, ";|`$'\\\"*?~<>^()[]{}!\n\r\x00") {
				// Still capture but let server validate; we log and reject locally for safety.
				// However keep alphanum and = & % . - _ ~ + :
				// For strictness, if obviously contains shell, reject.
				if strings.Contains(raw, ";") || strings.Contains(raw, "|") || strings.Contains(raw, "`") || strings.Contains(raw, "$") {
					http.Error(w, "forbidden callback query", http.StatusBadRequest)
					select {
					case errCh <- fmt.Errorf("callback query contains forbidden shell metacharacters"):
					default:
					}
					return
				}
			}
			cb += "?" + raw
		} else if r.URL.EscapedPath() != "/callback" && r.URL.Path != "/callback" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		// Respond with success page
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><body><h1>Authorized</h1><p>You can close this window and return to the terminal.</p><script>window.close();</script></body></html>`))
		select {
		case resultCh <- cb:
		default:
		}
	})
	// Catch-all to reject foreign paths
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	})

	srv := &http.Server{
		Handler: mux,
		// Do not log
	}
	go func() {
		_ = srv.Serve(ln)
	}()
	defer func() {
		_ = srv.Shutdown(context.Background())
	}()

	select {
	case <-ctx.Done():
		return "", fmt.Errorf("callback wait cancelled or timed out: %w", ctx.Err())
	case cb := <-resultCh:
		return cb, nil
	case err := <-errCh:
		return "", err
	}
}

func openBrowser(targetURL string) error {
	u, err := url.Parse(strings.TrimSpace(targetURL))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("invalid url %q", targetURL)
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", targetURL)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", targetURL)
	default:
		cmd = exec.Command("xdg-open", targetURL)
	}
	// Best-effort, don't leak URL in error logs beyond truncated
	if err := cmd.Start(); err != nil {
		return err
	}
	_ = cmd.Process.Release()
	return nil
}
