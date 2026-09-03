package command

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/basecamp/once/internal/version"
)

type versionCommand struct {
	cmd *cobra.Command
}

func newVersionCommand() *versionCommand {
	v := &versionCommand{}
	v.cmd = &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			if jsonOutput, _ := cmd.Flags().GetBool("json"); jsonOutput {
				fmt.Printf("{\"version\":\"%s\",\"status\":\"ok\"}\n", version.Version)
				return
			}
			fmt.Println(version.Version)
		},
	}
	v.cmd.Flags().Bool("json", false, "output JSON")
	return v
}
