package command

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/basecamp/once/internal/docker"
)

type keysResetCommand struct {
	cmd           *cobra.Command
	secretKeyBase bool
	vapid         bool
}

func newKeysResetCommand() *keysResetCommand {
	k := &keysResetCommand{}
	k.cmd = &cobra.Command{
		Use:   "reset <host>",
		Short: "Regenerate secret keys and redeploy the application",
		Args:  cobra.ExactArgs(1),
		RunE:  WithNamespace(k.run),
	}

	k.cmd.Flags().BoolVar(&k.secretKeyBase, "secret-key-base", false, "regenerate the secret key base")
	k.cmd.Flags().BoolVar(&k.vapid, "vapid", false, "regenerate the VAPID key pair")
	k.cmd.MarkFlagsOneRequired("secret-key-base", "vapid")

	return k
}

// Private

func (k *keysResetCommand) run(ctx context.Context, ns *docker.Namespace, cmd *cobra.Command, args []string) error {
	return changeKeys(ctx, ns, args[0], "Resetting keys for", func(keys *docker.Keys) error {
		if err := keys.Regenerate(k.secretKeyBase, k.vapid); err != nil {
			return fmt.Errorf("regenerating keys: %w", err)
		}
		return nil
	})
}
