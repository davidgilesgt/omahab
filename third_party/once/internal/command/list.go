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

func (l *listCommand) run(_ context.Context, ns *docker.Namespace, cmd *cobra.Command, args []string) error {
	if jsonOutput, _ := cmd.Flags().GetBool("json"); jsonOutput {
		type out struct {
			Name    string `json:"name"`
			Host    string `json:"host"`
			Running bool   `json:"running"`
			Status  string `json:"status"`
		}
		var list []out
		for _, app := range ns.Applications() {
			status := "stopped"
			if app.Running {
				status = "running"
			}
			list = append(list, out{Name: app.Settings.Name, Host: app.Settings.Host, Running: app.Running, Status: status})
		}
		if list == nil {
			list = []out{}
		}
		importJSON, _ := json.Marshal(list)
		fmt.Println(string(importJSON))
		return nil
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
