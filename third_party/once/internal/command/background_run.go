package command

import (
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/basecamp/once/internal/background"
)

type backgroundRunCommand struct {
	cmd *cobra.Command
}

func newBackgroundRunCommand() *backgroundRunCommand {
	b := &backgroundRunCommand{}
	b.cmd = &cobra.Command{
		Use:    "run",
		Short:  "Run background tasks (automatic backups and updates)",
		Args:   cobra.NoArgs,
		Hidden: true,
		RunE:   b.run,
	}
	return b
}

// Private

func (b *backgroundRunCommand) run(cmd *cobra.Command, args []string) error {
	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	runner := background.NewRunner(namespaceFlag(cmd))

	return runner.Run(ctx)
}
