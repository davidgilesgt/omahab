package command

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/basecamp/once/internal/docker"
)

type removeCommand struct {
	cmd        *cobra.Command
	removeData bool
	app        string
	hostname   string
	jsonOutput bool
}

func newRemoveCommand() *removeCommand {
	r := &removeCommand{}
	r.cmd = &cobra.Command{
		Use:     "remove [<host>]",
		Aliases: []string{"rm", "undeploy"},
		Short:   "Remove an application",
		Args:    cobra.MaximumNArgs(1),
		RunE:    WithNamespace(r.run),
	}
	r.cmd.Flags().BoolVar(&r.removeData, "remove-data", false, "Also remove application data volume")
	r.cmd.Flags().StringVar(&r.app, "app", "", "application name (for undeploy)")
	r.cmd.Flags().StringVar(&r.hostname, "hostname", "", "hostname (for undeploy)")
	r.cmd.Flags().BoolVar(&r.jsonOutput, "json", false, "output JSON")
	r.cmd.Flags().String("proxy-bind", "", "proxy bind (compat, ignored)")
	return r
}

// Private

func (r *removeCommand) run(ctx context.Context, ns *docker.Namespace, cmd *cobra.Command, args []string) error {
	host := ""
	if r.app != "" {
		host = r.app
		// Try by app name
		if app := ns.Application(r.app); app != nil {
			host = app.Settings.Host
		} else if r.hostname != "" {
			host = r.hostname
		}
	} else if r.hostname != "" {
		host = r.hostname
	} else if len(args) > 0 {
		host = args[0]
	}
	if host == "" {
		return fmt.Errorf("host or --app is required")
	}

	// Try to find app by host or name
	var targetApp *docker.Application
	targetApp = ns.ApplicationByHost(host)
	if targetApp == nil {
		targetApp = ns.Application(host)
	}
	if targetApp != nil {
		host = targetApp.Settings.Host
	}

	err := withApplication(ns, host, "removing", func(app *docker.Application) error {
		return app.Remove(ctx, r.removeData)
	})
	if err != nil {
		if r.jsonOutput {
			out, _ := json.Marshal(map[string]string{"error": err.Error(), "status": "error"})
			fmt.Println(string(out))
		}
		return err
	}

	if r.jsonOutput {
		out, _ := json.Marshal(map[string]string{"status": "ok"})
		fmt.Println(string(out))
		return nil
	}
	fmt.Printf("Removed %s\n", host)
	return nil
}
