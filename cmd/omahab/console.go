package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/skip2/go-qrcode"
	"github.com/spf13/cobra"
)

const bootstrapCodePath = "/run/omahab/bootstrap-code"
const bootstrapDonePath = "/var/lib/omahab/bootstrap-done"

// newConsoleCmd builds `omahab console`: the tty1 first-boot display.
// Clears the screen, shows the LAN bootstrap URL + one-time code + QR,
// refreshing every 5s; after bootstrap completes, shows the Tailscale
// dashboard URL instead.
func newConsoleCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "console",
		Short: "First-boot console display (tty1)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConsole()
		},
	}
}

func runConsole() error {
	w := os.Stdout
	for {
		fmt.Fprint(w, "\033[2J\033[H") // clear + home
		fmt.Fprintln(w, "")
		fmt.Fprintln(w, "  ┌─────────────────────────────────────────────┐")
		fmt.Fprintln(w, "  │            OMAHAB  ·  first boot            │")
		fmt.Fprintln(w, "  └─────────────────────────────────────────────┘")
		fmt.Fprintln(w, "")

		if _, err := os.Stat(bootstrapDonePath); err == nil {
			// Bootstrap complete: show the Tailscale dashboard pointer.
			ip := tailscaleIPv4()
			fmt.Fprintln(w, "  Setup complete. The dashboard is served over Tailscale:")
			fmt.Fprintln(w, "")
			if ip != "" {
				fmt.Fprintf(w, "      http://%s:8484\n", ip)
			} else {
				fmt.Fprintln(w, "      http://<tailscale-ip>:8484")
				fmt.Fprintln(w, "")
				fmt.Fprintln(w, "      (Tailscale IP not yet assigned — run `tailscale ip -4`)")
			}
			fmt.Fprintln(w, "")
			fmt.Fprintln(w, "  Press Ctrl-C to exit.")
			select {}
		}

		// Bootstrap pending: LAN wizard URL + one-time code.
		ip := lanIPv4()
		code := readBootstrapCode()
		fmt.Fprintln(w, "  Complete setup from any device on this network:")
		fmt.Fprintln(w, "")
		if ip != "" {
			fmt.Fprintf(w, "      http://%s:8485\n", ip)
			if code != "" {
				fmt.Fprintln(w, "")
				fmt.Fprintln(w, "  One-time code:")
				fmt.Fprintf(w, "      %s\n", code)
				if qr, err := qrcode.New("http://"+ip+":8485/#code="+code, qrcode.Medium); err == nil {
					fmt.Fprintln(w, "")
					fmt.Fprint(w, qr.ToSmallString(false))
				}
			} else {
				fmt.Fprintln(w, "")
				fmt.Fprintln(w, "  (waiting for the one-time code — omahabd is starting)")
			}
		} else {
			fmt.Fprintln(w, "      (waiting for a network address)")
		}
		fmt.Fprintln(w, "")
		fmt.Fprintf(w, "  Refreshing every 5s — %s\n", time.Now().Format("15:04:05"))
		time.Sleep(5 * time.Second)
	}
}

// lanIPv4 returns the first non-loopback, non-tailscale IPv4 address.
func lanIPv4() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 || !strings.HasPrefix(iface.Name, "e") {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipnet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipnet.IP.To4()
			if ip4 == nil || ip4.IsLoopback() || ip4[0] == 100 {
				continue
			}
			return ip4.String()
		}
	}
	return ""
}

// tailscaleIPv4 returns the tailscale IPv4 via `tailscale ip -4`.
func tailscaleIPv4() string {
	out, err := exec.Command("tailscale", "ip", "-4").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// readBootstrapCode reads the one-time code from tmpfs.
func readBootstrapCode() string {
	data, err := os.ReadFile(bootstrapCodePath)
	if err != nil {
		return ""
	}
	code := strings.TrimSpace(string(data))
	if len(code) > 10 {
		code = code[:10]
	}
	return code
}
