package command

import (
	"context"
	"encoding/json"
	"fmt"

	"charm.land/lipgloss/v2"
	"github.com/spf13/cobra"

	"github.com/basecamp/once/internal/docker"
)

var hostStyle = lipgloss.NewStyle().Foreground(lipgloss.BrightBlue)

type listCommand struct {
	cmd *cobra.Command
}

func newListCommand() *listCommand {
	l := &listCommand{}
	l.cmd = &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List installed applications",
		RunE:    WithNamespace(l.run),
	}
	l.cmd.Flags().Bool("json", false, "output JSON")
	return l
}

// Private

// listEntry is one row of the list --json array. Empty lists encode as [].
type listEntry struct {
	Name    string `json:"name"`
	Host    string `json:"host"`
	Running bool   `json:"running"`
	Status  string `json:"status"`
}

func (l *listCommand) run(_ context.Context, ns *docker.Namespace, cmd *cobra.Command, args []string) error {
	if jsonOutput, _ := cmd.Flags().GetBool("json"); jsonOutput {
		list := []listEntry{}
		for _, app := range ns.Applications() {
			status := "stopped"
			if app.Running {
				status = "running"
			}
			list = append(list, listEntry{Name: app.Settings.Name, Host: app.Settings.Host, Running: app.Running, Status: status})
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(list)
	}
	for _, app := range ns.Applications() {
		status := "stopped"
		if app.Running {
			status = "running"
		}

		host := hostStyle.Hyperlink(app.URL()).Render(app.Settings.Host)

		fmt.Printf("%s (%s)\n", host, status)
	}

	return nil
}
