package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// newHassCmd returns `omahab hass`: optional Home Assistant integration
// (DESIGN §17). Connect validates a read via hass-cli, stores the token for
// the default assistant scope only, and installs the hass-cli Hermes skill.
// Hermes invokes hass-cli directly; AI tools never receive the token.
func newHassCmd() *cobra.Command {
	hass := &cobra.Command{
		Use:   "hass",
		Short: "Home Assistant integration (optional)",
	}
	hass.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "Show Home Assistant connection state",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := newContext()
			defer cancel()
			c, err := resolveClient()
			if err != nil {
				return handleFailure(err)
			}
			ha, err := c.GetHomeAssistant(ctx)
			if err != nil {
				return handleFailure(err)
			}
			if flagJSON {
				return printJSON(ha)
			}
			if ha.Status == "not_connected" {
				fmt.Println("home assistant: not connected (omahab hass connect --server <url>)")
				return nil
			}
			fmt.Printf("home assistant: %s (%s)\n", ha.Status, ha.ServerURL)
			return nil
		},
	})
	var server, tokenFile, tokenEnv string
	connect := &cobra.Command{
		Use:   "connect",
		Short: "Connect Home Assistant (validates a read, installs skill)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(server) == "" {
				return handleFailure(fmt.Errorf("--server is required (http(s)://host:port)"))
			}
			token, err := readSecretKey(tokenFile, tokenEnv, "token", "Home Assistant long-lived token: ")
			if err != nil {
				return handleFailure(err)
			}
			ctx, cancel := newContext()
			defer cancel()
			c, err := resolveClient()
			if err != nil {
				return handleFailure(err)
			}
			ha, err := c.ConfigureHomeAssistant(ctx, server, token)
			if err != nil {
				return handleFailure(err)
			}
			if flagJSON {
				return printJSON(ha)
			}
			fmt.Printf("home assistant connected: %s\n", ha.ServerURL)
			return nil
		},
	}
	connect.Flags().StringVar(&server, "server", "", "Home Assistant URL (http or https)")
	connect.Flags().StringVar(&tokenFile, "token-file", "", "read the long-lived token from a file")
	connect.Flags().StringVar(&tokenEnv, "token-env", "", "read the long-lived token from an environment variable")
	hass.AddCommand(connect)
	hass.AddCommand(&cobra.Command{
		Use:   "remove",
		Short: "Disconnect Home Assistant and drop credentials",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := needForceGuard(false); err != nil {
				return handleFailure(err)
			}
			ctx, cancel := newContext()
			defer cancel()
			c, err := resolveClient()
			if err != nil {
				return handleFailure(err)
			}
			if err := c.RemoveHomeAssistant(ctx); err != nil {
				return handleFailure(err)
			}
			if flagJSON {
				return printJSON(map[string]any{"removed": true})
			}
			fmt.Println("home assistant disconnected")
			return nil
		},
	})
	return hass
}
