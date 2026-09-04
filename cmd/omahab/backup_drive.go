package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/omahab/omahab/internal/client"
)

func newBackupDriveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backup-drive",
		Short: "Machine backups to the Omahab server (restic REST, append-only)",
		Long:  "Manage per-machine backups via the Omahab restic REST server. Data is stored under /srv/omahab/machine-backups on the server (append-only) and included in the server's Hetzner backups. Credentials are per-device (dev-<id>) via the desktop keyring and never touch files.",
	}
	cmd.AddCommand(newBackupDriveEnableCmd())
	cmd.AddCommand(newBackupDriveRunCmd())
	cmd.AddCommand(newBackupDriveStatusCmd())
	return cmd
}

func newBackupDriveEnableCmd() *cobra.Command {
	var pathsStr string
	c := &cobra.Command{
		Use:   "enable",
		Short: "Enable nightly machine backups",
		Long:  "Enable nightly machine backups. Writes ~/.config/omahab/backup-paths and installs a systemd user timer (daily, Persistent) that runs 'omahab backup-drive run'. Default paths: $HOME excluding ~/.cache, ~/.local/share/Trash, **/node_modules, **/.git.",
		RunE: func(cmd *cobra.Command, args []string) error {
			var paths []string
			if strings.TrimSpace(pathsStr) != "" {
				// Split on comma.
				for _, p := range strings.Split(pathsStr, ",") {
					trim := strings.TrimSpace(p)
					if trim != "" {
						paths = append(paths, trim)
					}
				}
			}
			// Also allow positional args as paths.
			for _, a := range args {
				trim := strings.TrimSpace(a)
				if trim != "" {
					paths = append(paths, trim)
				}
			}
			if err := client.EnableBackupDrive(paths); err != nil {
				return handleFailure(err)
			}
			if flagJSON {
				return printJSON(map[string]string{"result": "enabled", "paths_file": client.BackupPathsFile()})
			}
			fmt.Println("Machine backup enabled. Paths written to", client.BackupPathsFile())
			fmt.Println("Timer: omahab-machine-backup.timer (daily, Persistent)")
			return nil
		},
	}
	c.Flags().StringVar(&pathsStr, "paths", "", "comma-separated backup paths (default: $HOME)")
	return c
}

func newBackupDriveRunCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "run",
		Short: "Run a machine backup now (restic backup + forget)",
		Long:  "Run restic backup for the configured paths (from ~/.config/omahab/backup-paths) using credentials from the desktop keyring (backup-repo/password/rest-user/rest-password), then restic forget --keep-daily 14 --keep-weekly 8 --keep-monthly 12 (no --prune, server is append-only).",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := newContext()
			defer cancel()
			// Use keyring store (desktop Secret Service). Fail if unavailable.
			ks := client.NewKeyringStore()
			// Test if restic is available, but let RunBackupDrive surface error.
			if err := client.RunBackupDrive(ctx, ks); err != nil {
				return handleFailure(err)
			}
			if flagJSON {
				return printJSON(map[string]string{"result": "completed"})
			}
			fmt.Println("Machine backup completed (restic backup + forget)")
			return nil
		},
	}
	return c
}

func newBackupDriveStatusCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "status",
		Short: "Show last machine backup snapshot",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			ks := client.NewKeyringStore()
			// Allow override for tests via env.
			if os.Getenv("OMAHAB_BACKUP_STATUS_MOCK") != "" {
				if flagJSON {
					return printJSON(map[string]string{"result": "mock"})
				}
				fmt.Println("mock status")
				return nil
			}
			st, err := client.StatusBackupDrive(ctx, ks)
			if err != nil {
				if flagJSON {
					return printJSON(map[string]any{"error": err.Error()})
				}
				return handleFailure(err)
			}
			if flagJSON {
				return printJSON(st)
			}
			if st.LastSnapshotTime == nil {
				fmt.Println("No machine backup snapshots yet")
				if st.Error != "" {
					fmt.Println("error:", st.Error)
				}
				return nil
			}
			age := time.Since(*st.LastSnapshotTime).Truncate(time.Second)
			fmt.Printf("Last snapshot: %s (%s ago) id %s\n", st.LastSnapshotTime.Format(time.RFC3339), age, st.SnapshotID)
			return nil
		},
	}
	return c
}
