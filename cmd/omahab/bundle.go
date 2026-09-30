package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/omahab/omahab/internal/apps"
	"github.com/omahab/omahab/internal/bundles"
)

// bundleRepoPath is overridable in tests.
var bundleRepoPath = bundles.RepoPath

// bundleCatalogPath is the assembled curated catalog (OMAHAB_CATALOG on
// the installed system); overridable in tests.
var bundleCatalogPath = os.Getenv("OMAHAB_CATALOG")

// newBundleCmd builds `omahab bundle`.
func newBundleCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bundle",
		Short: "Manage platform bundles (curated + custom)",
	}
	cmd.AddCommand(newBundleListCmd())
	cmd.AddCommand(newBundleAddCmd())
	cmd.AddCommand(newBundleRemoveCmd())
	cmd.AddCommand(newBundleApplyCmd())
	cmd.AddCommand(newBundleStatusCmd())
	cmd.AddCommand(&cobra.Command{
		Use:   "validate-bundle <path>",
		Short: "Validate one bundle.json file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := os.ReadFile(args[0])
			if err != nil {
				return handleFailure(err)
			}
			b, err := apps.ParseBundleBytes(raw)
			if err != nil {
				return handleFailure(err)
			}
			if _, err := b.Validate(); err != nil {
				return handleFailure(err)
			}
			if !flagJSON {
				fmt.Printf("ok: bundle %q (%d units)\n", b.ID, len(b.Units))
			}
			return nil
		},
	})
	return cmd
}

func newBundleSvc() (*bundles.Service, error) {
	return bundles.NewService(bundleRepoPath, nil, nil)
}

func newBundleListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List bundles in the merged catalog (curated + custom)",
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := bundles.LoadMerged(cmd.Context(), bundleCatalogPath, bundleRepoPath)
			if err != nil {
				return handleFailure(err)
			}
			if flagJSON {
				type row struct {
					ID       string `json:"id"`
					Name     string `json:"name"`
					Source   string `json:"source"`
					Port     int    `json:"port,omitempty"`
					Units    int    `json:"units"`
					Disabled bool   `json:"disabled,omitempty"`
				}
				rows := make([]row, 0, len(m.IDs()))
				for _, b := range m.Bundles() {
					rows = append(rows, row{ID: b.ID, Name: b.Name, Source: string(b.Source), Port: b.Port, Units: len(b.Units)})
				}
				return printJSON(rows)
			}
			for _, b := range m.Bundles() {
				fmt.Printf("%-18s %-24s %s\n", b.ID, b.Name, b.Source)
			}
			return nil
		},
	}
}

func newBundleAddCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "add <dir>",
		Short: "Copy a bundle directory (bundle.json + module.nix) into the custom bundles repo",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := newBundleSvc()
			if err != nil {
				return err
			}
			if err := svc.EnsureRepo(cmd.Context()); err != nil {
				return handleFailure(err)
			}
			id, err := svc.Add(cmd.Context(), args[0])
			if err != nil {
				return handleFailure(err)
			}
			if commit, err := svc.Commit(cmd.Context(), "omahab bundle add "+id); err != nil && commit == "" {
				return handleFailure(fmt.Errorf("bundle %q added but commit failed: %w", id, err))
			}
			if flagJSON {
				return printJSON(map[string]string{"id": id})
			}
			fmt.Printf("Added custom bundle %q. Run `omahab bundle apply` to build and activate it.\n", id)
			return nil
		},
	}
}

func newBundleRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <id>",
		Short: "Remove a custom bundle from the repo",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := newBundleSvc()
			if err != nil {
				return err
			}
			if err := svc.Remove(cmd.Context(), args[0]); err != nil {
				return handleFailure(err)
			}
			if commit, err := svc.Commit(cmd.Context(), "omahab bundle remove "+args[0]); err != nil && commit == "" {
				return handleFailure(fmt.Errorf("bundle %q removed but commit failed: %w", args[0], err))
			}
			if !flagJSON {
				fmt.Printf("Removed custom bundle %q. Run `omahab bundle apply` to activate the removal.\n", args[0])
			}
			return nil
		},
	}
}

func newBundleApplyCmd() *cobra.Command {
	var flake string
	var applyStaged bool
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Validate, commit, build with the custom-bundles override, switch, and health-gate",
		RunE: func(cmd *cobra.Command, args []string) error {
			if applyStaged {
				return runBundleApplyStaged(cmd.Context())
			}
			if flake == "" {
				flake = filepath.Join(canonicalFlakeDir, "#omahab-installed")
			}
			res, err := runBundleApply(cmd.Context(), flake)
			if flagJSON {
				return printJSON(res)
			}
			if err != nil {
				return handleFailure(err)
			}
			fmt.Println("Bundles applied; system healthy.")
			return nil
		},
	}
	cmd.Flags().StringVar(&flake, "flake", "", "flake ref (default /etc/omahab/flake#omahab-installed)")
	cmd.Flags().BoolVar(&applyStaged, "apply-staged", false, "apply the operation staged by the panel (run by omahab-bundle-apply.service)")
	return cmd
}

// runBundleApply is the single apply flow shared by the interactive CLI
// and the staged (unit-run) path.
func runBundleApply(ctx context.Context, flake string) (*bundles.ApplyResult, error) {
	svc, err := newBundleSvc()
	if err != nil {
		return nil, err
	}
	if err := svc.EnsureRepo(ctx); err != nil {
		return nil, err
	}
	m, err := bundles.LoadMerged(ctx, bundleCatalogPath, bundleRepoPath)
	if err != nil {
		return nil, err
	}
	return svc.Apply(ctx, flake, bundles.ApplyHooks{
		Build: func(ctx context.Context, flakeArg, repo string) ([]byte, error) {
			full := append([]string{"build", "--flake", flakeArg}, bundles.OverrideInputArg()...)
			return exec.Command("nixos-rebuild", full...).CombinedOutput()
		},
		Switch: func(ctx context.Context, flakeArg, repo string) ([]byte, error) {
			full := append([]string{"switch", "--flake", flakeArg}, bundles.OverrideInputArg()...)
			return exec.Command("nixos-rebuild", full...).CombinedOutput()
		},
		HealthGate:        waitPanelHealthy,
		Rollback:          func() error { _, err := runNixosRebuildRollbackFunc(); return err },
		ProbeBundleHealth: probeCustomBundleHealth,
	}, m)
}

// runBundleApplyStaged applies the bundle operation staged by the panel:
// the daemon wrote an operation journal plus a tmpfs handoff and started
// omahab-bundle-apply.service, which runs this. It runs the SAME
// runBundleApply flow, then records the outcome next to the journal so the
// daemon (restarted by the switch) converges the operation on poll.
func runBundleApplyStaged(ctx context.Context) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("--apply-staged requires root (the omahab-bundle-apply unit runs it)")
	}
	opID, err := readBundleApplyHandoffCLI()
	if err != nil {
		return err
	}
	stateDir := defaultStateDir()
	jPath := filepath.Join(stateDir, "bundle-apply-operations", opID, "journal.json")
	res, applyErr := runBundleApply(ctx, filepath.Join(canonicalFlakeDir, "#omahab-installed"))
	status, detail := "completed", ""
	if res != nil && res.Detail != "" {
		detail = res.Detail
	}
	switch {
	case applyErr == nil:
		// status stays completed
	case strings.Contains(applyErr.Error(), "rollback"):
		status = "rolled_back"
		detail = applyErr.Error()
	default:
		status = "failed"
		detail = applyErr.Error()
	}
	if err := writeBundleApplyOutcome(stateDir, opID, status, detail); err != nil {
		return fmt.Errorf("apply finished (%s) but recording the outcome failed: %w", status, err)
	}
	if raw, rerr := os.ReadFile(jPath); rerr == nil {
		var op struct {
			ID        string `json:"id"`
			Kind      string `json:"kind"`
			Status    string `json:"status"`
			Stage     string `json:"stage"`
			Error     string `json:"error,omitempty"`
			CreatedAt string `json:"created_at"`
		}
		if json.Unmarshal(raw, &op) == nil {
			op.Status = status
			op.Stage = "applied"
			op.Error = detail
			if out, merr := json.MarshalIndent(op, "", "  "); merr == nil {
				_ = os.WriteFile(jPath, append(out, '\n'), 0o600)
			}
		}
	}
	if flagJSON {
		return printJSON(map[string]string{"operation": opID, "status": status})
	}
	fmt.Printf("bundle apply %s (operation %s)\n", status, opID)
	if applyErr != nil {
		return handleFailure(applyErr)
	}
	return nil
}

// readBundleApplyHandoffCLI consumes (and removes) the tmpfs handoff
// holding the staged operation id.
func readBundleApplyHandoffCLI() (string, error) {
	path := "/run/omahab/bundle-apply"
	if v := strings.TrimSpace(os.Getenv("OMAHAB_RUNTIME_DIR")); v != "" {
		path = filepath.Join(v, "bundle-apply")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read bundle apply handoff %s: %w (stage an apply from the panel first)", path, err)
	}
	_ = os.Remove(path)
	var h struct {
		OperationID string `json:"operation_id"`
	}
	if err := json.Unmarshal(raw, &h); err != nil {
		return "", fmt.Errorf("parse bundle apply handoff: %w", err)
	}
	if strings.TrimSpace(h.OperationID) == "" {
		return "", fmt.Errorf("bundle apply handoff has no operation_id")
	}
	return strings.TrimSpace(h.OperationID), nil
}

// writeBundleApplyOutcome records the CLI outcome marker the daemon
// converges the operation journal from.
func writeBundleApplyOutcome(stateDir, opID, status, detail string) error {
	dir := filepath.Join(stateDir, "bundle-apply-operations", opID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(map[string]string{"status": status, "detail": detail}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "outcome.json"), append(raw, '\n'), 0o600)
}

// probeCustomBundleHealth runs one bundle health check via the omahabd
// API surface on the loopback panel (single probe, no grace: the unit
// already started during switch).
func probeCustomBundleHealth(ctx context.Context, id string) error {
	out, err := exec.Command("curl", "-sf", "-m", "10",
		fmt.Sprintf("http://127.0.0.1:8484/api/v1/applications?bundle=%s", id)).Output()
	if err != nil {
		return fmt.Errorf("probe bundle %q: %w", id, err)
	}
	var probe struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(out, &probe)
	return nil
}

func newBundleStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Custom bundles repo status (committed bundles, disabled markers, dirty state)",
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := newBundleSvc()
			if err != nil {
				return err
			}
			st, err := svc.Snapshot(cmd.Context())
			if err != nil {
				return handleFailure(err)
			}
			if flagJSON {
				return printJSON(st)
			}
			fmt.Printf("repo %s (commit %s, dirty=%v)\n", st.Repo, st.Commit, st.Dirty)
			for _, b := range st.Bundles {
				state := "enabled"
				if b.Disabled {
					state = "disabled"
				}
				fmt.Printf("  %-18s %s\n", b.ID, state)
			}
			return nil
		},
	}
}

// mergeBundlesForCLI is a helper for other commands needing the merged view.
func mergeBundlesForCLI(ctx context.Context) (*bundles.Merged, error) {
	return bundles.LoadMerged(ctx, bundleCatalogPath, bundleRepoPath)
}

var _ = apps.Bundle{} // keep import while the command set grows
