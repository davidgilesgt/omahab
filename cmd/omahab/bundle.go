package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/omahab/omahab/internal/apiclient"
	"github.com/omahab/omahab/internal/apps"
	"github.com/omahab/omahab/internal/bundles"
)

// bundleRepoPath is overridable in tests.
var bundleRepoPath = bundles.RepoPath

// bundleCatalogPath is the assembled curated catalog (OMAHAB_CATALOG on
// the installed system); overridable in tests.
var bundleCatalogPath = os.Getenv("OMAHAB_CATALOG")

// stableCatalogPath and stableNativeLayoutPath are the declarative links
// nix/module.nix writes into /etc/omahab: an interactive root shell (the
// recovery console) has none of omahabd's service-only environment, so
// `omahab recovery apply` resolves the installed native catalog and
// layout through them when the env vars are unset. nix/module.nix keeps
// them pointed at the exact store paths the daemon services use.
const (
	stableCatalogPath      = "/etc/omahab/catalog/catalog.json"
	stableNativeLayoutPath = "/etc/omahab/native-layout.json"
)

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

// mergeBundlesForCLI is the merged-view loader shared by commands that
// need the committed merged catalog.
func mergeBundlesForCLI(ctx context.Context) (*bundles.Merged, error) {
	return bundles.LoadMerged(ctx, bundleCatalogPath, bundleRepoPath)
}

// mergeCustomDocsCLI is the apply-time merge function: it loads the
// curated catalog fresh and merges the given custom docs through the
// full collision and custom-contract validation.
func mergeCustomDocsCLI(ctx context.Context, docs []bundles.CustomDoc) (*bundles.Merged, error) {
	curated, err := loadCuratedCatalogCLI()
	if err != nil {
		return nil, err
	}
	return bundles.Merge(curated, docs)
}

func loadCuratedCatalogCLI() (*apps.Catalog, error) {
	if bundleCatalogPath == "" {
		return &apps.Catalog{}, nil
	}
	c, err := apps.LoadCatalogFile(bundleCatalogPath)
	if err != nil {
		if os.IsNotExist(err) {
			return &apps.Catalog{}, nil
		}
		return nil, err
	}
	return c, nil
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

// validateIntendedCLI runs the full custom + collision and module
// validation over the intended dirty worktree so an invalid bundle fails
// before it can reach the committed catalog (and from there a build).
func validateIntendedCLI(ctx context.Context, svc *bundles.Service) error {
	return svc.ValidateIntended(ctx, mergeCustomDocsCLI)
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
			// The full custom contract (collisions against the merged
			// catalog, port range, module presence) must hold before the
			// add is committed: an invalid definition must never reach
			// the committed catalog.
			if err := validateIntendedCLI(cmd.Context(), svc); err != nil {
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
			res, err := runBundleApply(cmd.Context(), flake, nil)
			if err != nil {
				return handleFailure(err)
			}
			if flagJSON {
				return printJSON(res)
			}
			fmt.Println("Bundles applied; system healthy.")
			return nil
		},
	}
	cmd.Flags().StringVar(&flake, "flake", "", "flake ref (default /etc/omahab/flake#omahab-installed)")
	cmd.Flags().BoolVar(&applyStaged, "apply-staged", false, "apply the operation staged by the panel (run by omahab-bundle-apply.service)")
	return cmd
}

// osGeteuid is overridable in tests (the staged path requires root).
var osGeteuid = os.Geteuid

// runBundleApply is the single apply flow shared by the interactive CLI
// and the staged (unit-run) path. skipHealth lists bundle IDs whose
// post-switch health probes are skipped; only the trusted staged
// handoff (restore-paused scopes) ever supplies it.
func runBundleApply(ctx context.Context, flake string, skipHealth []string) (*bundles.ApplyResult, error) {
	svc, err := newBundleSvc()
	if err != nil {
		return nil, err
	}
	if err := svc.EnsureRepo(ctx); err != nil {
		return nil, err
	}
	probe := probeCustomBundleHealth
	if len(skipHealth) > 0 {
		skip := make(map[string]bool, len(skipHealth))
		for _, id := range skipHealth {
			skip[strings.TrimSpace(id)] = true
		}
		probe = func(ctx context.Context, id, revision string) error {
			if skip[id] {
				return nil
			}
			return probeCustomBundleHealth(ctx, id, revision)
		}
	}
	return svc.Apply(ctx, flake, bundles.ApplyHooks{
		Build: func(ctx context.Context, flakeArg, repo, rev string) ([]byte, error) {
			full := append([]string{"build", "--flake", flakeArg}, bundles.OverrideInputArgs(repo, rev)...)
			return exec.Command("nixos-rebuild", full...).CombinedOutput()
		},
		Switch: func(ctx context.Context, flakeArg, repo, rev string) ([]byte, error) {
			full := append([]string{"switch", "--flake", flakeArg}, bundles.OverrideInputArgs(repo, rev)...)
			return exec.Command("nixos-rebuild", full...).CombinedOutput()
		},
		HealthGate:        waitPanelHealthy,
		Rollback:          func() error { _, err := runNixosRebuildRollbackFunc(); return err },
		ProbeBundleHealth: probe,
	}, mergeCustomDocsCLI)
}

// runBundleApplyStaged applies the bundle operation staged by the panel:
// the daemon wrote an operation journal plus a tmpfs handoff and started
// omahab-bundle-apply.service, which runs this. It runs the SAME
// runBundleApply flow, then records the outcome next to the journal so the
// daemon (restarted by the switch) converges the operation on poll.
func runBundleApplyStaged(ctx context.Context) error {
	if osGeteuid() != 0 {
		return fmt.Errorf("--apply-staged requires root (the omahab-bundle-apply unit runs it)")
	}
	opID, skipHealth, err := readBundleApplyHandoffCLI()
	if err != nil {
		return err
	}
	stateDir := defaultStateDir()
	jPath := filepath.Join(stateDir, "bundle-apply-operations", opID, "journal.json")
	res, applyErr := runBundleApply(ctx, filepath.Join(canonicalFlakeDir, "#omahab-installed"), skipHealth)
	status, detail := "completed", ""
	if res != nil && res.Detail != "" {
		detail = res.Detail
	}
	switch {
	case applyErr == nil:
		// status stays completed
	case res != nil && res.RolledBack:
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
		if applyErr != nil {
			return handleFailure(applyErr)
		}
		return printJSON(map[string]string{"operation": opID, "status": status})
	}
	fmt.Printf("bundle apply %s (operation %s)\n", status, opID)
	if applyErr != nil {
		return handleFailure(applyErr)
	}
	return nil
}

// readBundleApplyHandoffCLI consumes (and removes) the tmpfs handoff
// holding the staged operation id and the optional trusted skip_health
// bundle IDs (restore-paused scopes only).
func readBundleApplyHandoffCLI() (string, []string, error) {
	path := "/run/omahab/bundle-apply"
	if v := strings.TrimSpace(os.Getenv("OMAHAB_RUNTIME_DIR")); v != "" {
		path = filepath.Join(v, "bundle-apply")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", nil, fmt.Errorf("read bundle apply handoff %s: %w (stage an apply from the panel first)", path, err)
	}
	_ = os.Remove(path)
	var h struct {
		OperationID string   `json:"operation_id"`
		SkipHealth  []string `json:"skip_health,omitempty"`
	}
	if err := json.Unmarshal(raw, &h); err != nil {
		return "", nil, fmt.Errorf("parse bundle apply handoff: %w", err)
	}
	if strings.TrimSpace(h.OperationID) == "" {
		return "", nil, fmt.Errorf("bundle apply handoff has no operation_id")
	}
	return strings.TrimSpace(h.OperationID), h.SkipHealth, nil
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

// panelAPIBase and panelTokenPath locate the loopback panel API; overridable
// in tests. The apply unit runs as root and reads the root-only token.
var (
	panelAPIBase   = "http://127.0.0.1:8484"
	panelTokenPath = apiTokenPath
)

// probeCustomBundleHealth runs one bundle health probe through the
// omahabd API surface on the loopback panel: the daemon probes the
// currently configured service for the bundle (no application database
// row, no stored health) and answers 204 only when the fresh probe
// succeeds. Any non-2xx fails the probe so the apply health-gate rolls
// the generation back.
func probeCustomBundleHealth(ctx context.Context, id, revision string) error {
	raw, err := os.ReadFile(panelTokenPath)
	if err != nil {
		return fmt.Errorf("probe bundle %q: read panel token: %w", id, err)
	}
	c := apiclient.New(panelAPIBase, strings.TrimSpace(string(raw)))
	c.HTTPClient.Timeout = 10 * time.Second
	if err := c.ProbeBundleHealth(ctx, id, revision); err != nil {
		return fmt.Errorf("probe bundle %q: %w", id, err)
	}
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
