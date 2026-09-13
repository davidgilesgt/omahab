package main

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/omahab/omahab/internal/drops"
)

func newDropsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "drops",
		Short: "Route files from the drops inbox to app consume/staging dirs",
		Long:  "Move files out of the drops inbox by extension: PDFs and office documents to the Paperless consume directory, camera photos and phone video to the photos staging directory, library audio/video to the media staging directory. Unknown files stay put. Run by the omahab-drops-route timer; safe to run by hand.",
	}
	cmd.AddCommand(newDropsRouteCmd())
	return cmd
}

func newDropsRouteCmd() *cobra.Command {
	var dataDir string
	c := &cobra.Command{
		Use:   "route",
		Short: "Route inbox files once",
		RunE: func(cmd *cobra.Command, args []string) error {
			dataDir = strings.TrimSpace(dataDir)
			if dataDir == "" {
				dataDir = "/srv/omahab"
			}
			res, err := drops.RouteInbox(
				filepath.Join(dataDir, "sync", "drops", "inbox"),
				drops.Destinations{
					Paperless: filepath.Join(dataDir, "apps", "paperless", "consume"),
					Photos:    filepath.Join(dataDir, "sync", "drops", "photos"),
					Media:     filepath.Join(dataDir, "sync", "drops", "media"),
				},
			)
			if err != nil {
				return err
			}
			if res == nil {
				res = []drops.Result{}
			}
			if flagJSON {
				return printJSON(map[string]any{"routed": res})
			}
			if len(res) == 0 {
				fmt.Println("drops inbox: nothing to route")
				return nil
			}
			for _, r := range res {
				fmt.Printf("routed %s -> %s (%s)\n", r.File, r.To, r.Target)
			}
			return nil
		},
	}
	c.Flags().StringVar(&dataDir, "data-dir", "", "data root (default /srv/omahab)")
	return c
}
