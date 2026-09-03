package command

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/basecamp/once/internal/docker"
)

type keysCommand struct {
	cmd *cobra.Command
}

func newKeysCommand() *keysCommand {
	k := &keysCommand{}
	k.cmd = &cobra.Command{
		Use:   "keys",
		Short: "Manage application secret keys",
	}

	k.cmd.AddCommand(newKeysResetCommand().cmd)
	k.cmd.AddCommand(newKeysSetCommand().cmd)

	return k
}

// Helpers

func changeKeys(ctx context.Context, ns *docker.Namespace, host string, label string, change func(*docker.Keys) error) error {
	app, err := findApplication(ns, host)
	if err != nil {
		return err
	}

	if !app.Running {
		return docker.ErrApplicationNotRunning
	}

	if err := ns.Setup(ctx); err != nil {
		return fmt.Errorf("%w: %w", docker.ErrSetupFailed, err)
	}

	settings := app.Settings
	if err := change(&settings.Keys); err != nil {
		return err
	}

	return runWithProgress(label+" "+host, func(progress docker.DeployProgressCallback) error {
		if err := app.UpdateSettings(ctx, settings, progress); err != nil {
			return fmt.Errorf("%w: %w", docker.ErrDeployFailed, err)
		}
		return nil
	})
}
