package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// newEmailCmd returns `omahab email`: receiving-only AI email enrollment
// (DESIGN §18). Mail flows Cloudflare Email Routing -> Send to Worker ->
// omahab-email-worker -> HMAC ingest; omahabd verifies DKIM, exact sender,
// signed From, and alignment before routing to the inbox, Paperless, or
// Karakeep. There is no outbound AI email.
func newEmailCmd() *cobra.Command {
	email := &cobra.Command{
		Use:   "email",
		Short: "Receiving-only AI email enrollment",
		Long: `Enroll the one allowed personal sender address and activate the AI address route.

1. omahab email senders enroll <you@example.com>   (stays pending)
2. omahab email route ensure [ai@domain]           (Send-to-Worker rule)
3. send a test email from the enrolled address
4. first DKIM-passing ingest promotes the sender to verified

Only enrolled senders are accepted; everything else is quarantined.`,
	}
	email.AddCommand(newEmailSendersCmd())
	email.AddCommand(newEmailRouteCmd())
	return email
}

func newEmailSendersCmd() *cobra.Command {
	senders := &cobra.Command{
		Use:   "senders",
		Short: "Manage enrolled sender addresses",
	}
	senders.AddCommand(&cobra.Command{
		Use:   "enroll <email>",
		Short: "Enroll the allowed personal sender address",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			email := strings.TrimSpace(args[0])
			if email == "" || !strings.Contains(email, "@") {
				return handleFailure(fmt.Errorf("valid email required"))
			}
			ctx, cancel := newContext()
			defer cancel()
			c, err := resolveClient()
			if err != nil {
				return handleFailure(err)
			}
			sender, err := c.EnrollEmailSender(ctx, email)
			if err != nil {
				return handleFailure(err)
			}
			if flagJSON {
				return printJSON(sender)
			}
			fmt.Printf("enrolled %s (status %s; send a test email to verify)\n", sender.Email, sender.Status)
			return nil
		},
	})
	senders.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List enrolled senders with verification state",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := newContext()
			defer cancel()
			c, err := resolveClient()
			if err != nil {
				return handleFailure(err)
			}
			list, err := c.ListEmailSenders(ctx)
			if err != nil {
				return handleFailure(err)
			}
			if flagJSON {
				return printJSON(list)
			}
			if len(list) == 0 {
				fmt.Println("no senders enrolled")
				return nil
			}
			for _, s := range list {
				fmt.Printf("%s\t%s\n", s.Email, s.Status)
			}
			return nil
		},
	})
	senders.AddCommand(&cobra.Command{
		Use:   "remove <email>",
		Short: "Remove an enrolled sender address",
		Args:  cobra.ExactArgs(1),
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
			if err := c.RemoveEmailSender(ctx, strings.TrimSpace(args[0])); err != nil {
				return handleFailure(err)
			}
			if flagJSON {
				return printJSON(map[string]any{"removed": true})
			}
			fmt.Println("sender removed")
			return nil
		},
	})
	return senders
}

func newEmailRouteCmd() *cobra.Command {
	route := &cobra.Command{
		Use:   "route",
		Short: "Manage the AI address routing rule",
	}
	route.AddCommand(&cobra.Command{
		Use:   "ensure [recipient]",
		Short: "Create the Send-to-Worker rule (requires an enrolled sender)",
		RunE: func(cmd *cobra.Command, args []string) error {
			recipient := ""
			if len(args) > 0 {
				recipient = strings.TrimSpace(args[0])
			}
			ctx, cancel := newContext()
			defer cancel()
			c, err := resolveClient()
			if err != nil {
				return handleFailure(err)
			}
			if err := c.EnsureEmailRoute(ctx, recipient); err != nil {
				return handleFailure(err)
			}
			if flagJSON {
				return printJSON(map[string]any{"ok": true})
			}
			fmt.Println("email route ensured (Send to Worker)")
			return nil
		},
	})
	return route
}
