package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/omahab/omahab/internal/controlplane"
)

// Storage changes use the panel/API; this entry point runs their staged unit.
func newStorageMigrateCmd() *cobra.Command {
	var applyStaged bool
	var operationID string
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Run a staged storage migration (unit-run)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !applyStaged {
				return fmt.Errorf("migrations run staged from the panel/API; --apply-staged is for the storage migration unit")
			}
			if err := controlplane.RunStorageMigrationStaged(operationID); err != nil {
				return handleFailure(err)
			}
			fmt.Println("storage migration staged run finished")
			return nil
		},
	}
	cmd.Flags().BoolVar(&applyStaged, "apply-staged", false, "apply the operation staged by the panel")
	cmd.Flags().StringVar(&operationID, "operation", "", "staged operation identifier (unit-run)")
	return cmd
}
