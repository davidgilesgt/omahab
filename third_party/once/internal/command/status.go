package command

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/basecamp/once/internal/docker"
)

type statusCommand struct {
	cmd       *cobra.Command
	app       string
	hostname  string
	proxyBind string
	jsonOutput bool
}

func newStatusCommand() *statusCommand {
	s := &statusCommand{}
	s.cmd = &cobra.Command{
		Use:   "status",
		Short: "Show application status",
		RunE:  WithNamespace(s.run),
	}
	s.cmd.Flags().StringVar(&s.app, "app", "", "application name")
	s.cmd.Flags().StringVar(&s.hostname, "hostname", "", "hostname (alternative to --app)")
	s.cmd.Flags().StringVar(&s.proxyBind, "proxy-bind", "", "proxy bind address (ignored, for API compat)")
	s.cmd.Flags().BoolVar(&s.jsonOutput, "json", false, "output JSON")
	return s
}

func (s *statusCommand) run(ctx context.Context, ns *docker.Namespace, cmd *cobra.Command, args []string) error {
	appName := s.app
	if appName == "" && len(args) > 0 {
		appName = args[0]
	}
	if appName == "" {
		appName = s.hostname
	}
	var app *docker.Application
	if appName != "" {
		// Try by Name first, then by Host
		app = ns.Application(appName)
		if app == nil {
			app = ns.ApplicationByHost(appName)
		}
		if app == nil {
			// Try hostname flag
			if s.hostname != "" {
				app = ns.ApplicationByHost(s.hostname)
				if app == nil {
					app = ns.Application(s.hostname)
				}
			}
		}
	} else {
		// No app specified, try to find single app or return not found
		apps := ns.Applications()
		if len(apps) == 1 {
			app = apps[0]
		}
	}

	healthy := false
	detail := ""
	status := "ok"
	if app == nil {
		healthy = false
		detail = "application not found"
		status = "not_found"
	} else {
		// Simple health: running means healthy
		healthy = app.Running
		if !healthy {
			detail = "application not running"
		}
	}

	if s.jsonOutput {
		out := map[string]interface{}{
			"healthy": healthy,
			"detail":  detail,
			"status":  status,
		}
		if app != nil {
			out["app"] = app.Settings.Name
			out["host"] = app.Settings.Host
		}
		b, _ := json.Marshal(out)
		fmt.Println(string(b))
		return nil
	}

	if app == nil {
		fmt.Println("not found")
		return nil
	}
	if healthy {
		fmt.Printf("%s is healthy\n", app.Settings.Name)
	} else {
		fmt.Printf("%s is not healthy: %s\n", app.Settings.Name, detail)
	}
	return nil
}
