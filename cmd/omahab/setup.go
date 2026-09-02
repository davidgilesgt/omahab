package main

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/omahab/omahab/internal/setupguide"
	"github.com/omahab/omahab/internal/tailnet"
)

// newSetupCmd builds `omahab setup`: the SSH fallback for first-boot
// enrollment when no browser is available on the LAN. Walks Tailscale
// then Cloudflare, then hands off to the authenticated dashboard.
func newSetupCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "setup",
		Short: "Interactive first-run enrollment over SSH",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSetup()
		},
	}
}

func runSetup() error {
	reader := bufio.NewReader(os.Stdin)
	fmt.Println("Omahab first-run setup (SSH fallback)")
	fmt.Println()

	// --- Tailscale ---
	fmt.Println("1) Tailscale — private mesh")
	st, _ := tailnet.Status(context.Background())
	if st.Running && tailnet.IsTailscaleIPv4(st.IP) {
		fmt.Printf("   ✓ already running — %s\n", st.IP)
	} else {
		fmt.Println("   Starting tailscale up (approve the URL in your tailnet)…")
		url, err := tailnet.Up(context.Background())
		if err != nil {
			fmt.Printf("   tailscale up: %v\n", err)
		}
		if url != "" {
			fmt.Printf("   Auth URL: %s\n", url)
		}
		// Poll until Running + 100.x.
		deadline := time.Now().Add(5 * time.Minute)
		for time.Now().Before(deadline) {
			time.Sleep(5 * time.Second)
			st, _ = tailnet.Status(context.Background())
			if st.Running && tailnet.IsTailscaleIPv4(st.IP) {
				break
			}
			fmt.Print(".")
		}
		fmt.Println()
		if !st.Running || !tailnet.IsTailscaleIPv4(st.IP) {
			fmt.Println("   ! Tailscale not yet running; continue with `sudo tailscale up` and rerun `omahab setup`.")
		} else {
			fmt.Printf("   ✓ running — %s\n", st.IP)
		}
	}

	// --- Cloudflare ---
	fmt.Println()
	fmt.Println("2) Cloudflare — domain + tokens")
	fmt.Print("   Apex domain (e.g. example.com, blank to skip): ")
	domain, _ := reader.ReadString('\n')
	domain = strings.TrimSpace(domain)
	if domain != "" {
		if err := setupguide.ValidateApexDomain(domain); err != nil {
			fmt.Printf("   ! %v — continuing anyway\n", err)
		}
		fmt.Print("   Cloudflare DNS token (Token A): ")
		token, _ := reader.ReadString('\n')
		token = strings.TrimSpace(token)
		if token != "" {
			if err := setupguide.ValidateCloudflareToken(token); err == nil {
				ok, status, detail := setupguide.VerifyCloudflareTokenLive(context.Background(), token)
				if ok {
					fmt.Printf("   ✓ token active (%s)\n", status)
				} else {
					fmt.Printf("   ! token check: %s (%s)\n", detail, status)
				}
			} else {
				fmt.Printf("   ! %v\n", err)
			}
		}
	}

	fmt.Println()
	fmt.Println("Next: open the dashboard to finish enrollment")
	fmt.Println("  (domain + tokens, recovery key, storage, AI providers, backups)")
	ip, _ := tailnet.Status(context.Background())
	if ip.IP != "" {
		fmt.Printf("  http://%s:8484\n", ip.IP)
	}
	return nil
}

// ensure http is referenced (kept for future API calls from setup)
var _ = http.DefaultClient
