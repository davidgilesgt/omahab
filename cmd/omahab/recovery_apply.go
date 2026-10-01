package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/omahab/omahab/internal/backups"
	"github.com/omahab/omahab/internal/controlplane"
	"github.com/omahab/omahab/internal/secrets"
)

// `omahab recovery apply` — the offline disaster-recovery entry
// (DESIGN §5.4, §19, §24.6). Runs on a fresh machine as root with omahabd
// stopped (or stopped mid-command): prompts for the 24-word recovery
// phrase (never argv), derives the restic password via the same HKDF as
// setup, lists server-config snapshots, stages and validates the newest
// (or --snapshot), then runs the ONE shared offline executor
// (controlplane.RunOfflineRecoveryRestore) — the same code path the
// daemon's omahab-restore unit runs: swap control-plane state and the
// inseparable identity checkpoint, restart omahabd, restore the requested
// app payloads and databases, activate snapshot-carried custom bundles,
// and attach the exact source repository. Requested scopes cover app
// payloads AND server-owned data scopes (server-sync, server-projects,
// machine-backups) when include-apps is set or they are listed
// explicitly; excluded scopes are never touched.
//
// Listing and staging never touch live state, so --list and --stage are
// safe while the daemon runs; only the apply stops and restarts omahabd.
// Interruptions are resumable: re-running the same command resumes the
// persisted operation from its journalled stage (FindOfflineRecoveryOperation),
// keeping the ORIGINAL source snapshot, scope selection and rollback tree —
// never re-extracting a newer snapshot on top of it. The interactive
// shell resolves the installed native catalog/layout through the stable
// /etc/omahab links (the service environment is omahabd-only), and the
// CLI guarantees omahabd comes back whether the restore succeeds or fails
// (including config-only restores) instead of leaning on the unit's
// ExecStopPost.
func newRecoveryApplyCmd() *cobra.Command {
	var (
		location    string
		passwordArg string
		snapshotID  string
		kitPath     string
		listOnly    bool
		stageOnly   bool
		applyStaged bool
		scopes      []string
		includeApps bool
	)
	c := &cobra.Command{
		Use:   "apply",
		Short: "Restore this server from a backup repository (offline, root)",
		Long: `Disaster recovery on a fresh machine.

Prompts for the 24-word recovery phrase (never pass it as an argument).
The repository password is derived from the phrase exactly as during
setup (HKDF-SHA256, salt "omahab-recovery-v1", info "restic-password"),
or read from --password-file for generic repositories.

Flow: list snapshots -> stage + validate the latest server-config
snapshot (or --snapshot) -> stop omahabd -> run the shared offline
executor: swap control.db, master.key, recovery.kit and configuration
atomically (rollback copy kept) plus the PocketID identity checkpoint,
restart omahabd, restore requested app payloads and databases, activate
snapshot-carried custom bundles, and attach the source repository so
backups continue into the same instance path -> omahabd is back.

--scopes app:<bundle-id>,... restores a subset of app data;
--include-apps restores everything. Server configuration always
restores.

The apply requires root. --list and --stage are safe while omahabd runs.
An interactive apply stops omahabd and always starts it again afterwards,
restore success or failure alike. --apply-staged consumes a restore
staged through the panel (phrase read from the trigger handoff, never
argv); it is what the omahab-restore systemd unit runs. An interrupted
apply is resumable: re-running the same command continues the ORIGINAL
operation from its persisted journal stage, keeping the staged snapshot,
scope selection and rollback tree.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := newContext()
			defer cancel()
			if applyStaged {
				return runApplyStaged(ctx)
			}
			location = strings.TrimSpace(location)
			if location == "" {
				return fmt.Errorf("--repository is required (sftp://… URL or local path)")
			}
			phrase, err := promptPhrase()
			if err != nil {
				return err
			}
			words := strings.Fields(phrase)
			if len(words) != 24 {
				return fmt.Errorf("phrase must be 24 words, got %d", len(words))
			}
			seed, err := secrets.PhraseToSeed(words)
			if err != nil {
				return fmt.Errorf("invalid phrase: %w", err)
			}
			// Derive the repository password from the phrase BEFORE any
			// source validation touches the repository.
			repoPassword, err := readRepoPassword(passwordArg, seed)
			if err != nil {
				return err
			}
			src := backups.RecoverySource{Location: location, Password: repoPassword}
			stateDir := defaultStateDir()
			runner := recoveryCLIRunner(stateDir)
			fp := phraseFingerprintCLI(seed)
			// An interactive root shell carries none of omahabd's
			// service-only environment: resolve the installed native
			// catalog and layout through the stable /etc/omahab links
			// when the env vars are unset.
			catalog, layout := installedRecoveryCatalogLayout()

			// Resume check FIRST: an earlier run of this same command
			// (same repository, phrase, snapshot, selection) may have
			// been interrupted — continue that operation's journalled
			// stage with its original staging tree and rollback copy.
			resume, selectedSnapshot, err := controlplane.FindOfflineRecoveryOperation(stateDir, location, snapshotID, fp, scopes, includeApps)
			if err != nil {
				return err
			}
			if resume.ID == "" || selectedSnapshot == "" || listOnly {
				snaps, err := backups.ListRecoverySnapshots(ctx, runner, src, stateDir, 10)
				if err != nil {
					return fmt.Errorf("list snapshots: %w", err)
				}
				if listOnly {
					return printRecoverySnapshots(snaps)
				}
				pick, err := backups.PickRecoverySnapshot(snaps, snapshotID)
				if err != nil {
					return err
				}
				selectedSnapshot = pick.ID
				if !flagJSON {
					fmt.Printf("restoring snapshot %s (%s)\n", pick.ID, pick.Time)
				}
			}
			op := resume
			if op.ID != "" && !flagJSON {
				fmt.Printf("resuming unfinished restore (operation %s, stage %s)\n", op.ID, op.Stage)
			}
			if op.ID == "" || op.Stage == "listing" || op.Stage == "staging" {
				staging := filepath.Join(recoveryCLIStagingRoot(stateDir), "apply-"+strings.ReplaceAll(selectedSnapshot, "/", "-"))
				op, err = controlplane.StageOfflineRecoverySnapshot(ctx,
					stateDir, defaultDataDir(), catalog, layout,
					selectedSnapshot, staging, fp, location,
					scopes, includeApps, src, op.ID)
				if err != nil {
					return err
				}
			}
			if stageOnly {
				if flagJSON {
					return printJSON(map[string]string{"operation": op.ID, "stage": op.Stage, "snapshot": selectedSnapshot, "status": op.Status})
				}
				fmt.Printf("operation %s at stage %s; no apply requested\n", op.ID, op.Stage)
				return nil
			}
			// The apply replaces the running daemon's database: require
			// root and a stopped (or stopping) omahabd.
			if os.Geteuid() != 0 {
				return fmt.Errorf("applying a server-config restore requires root (re-run with sudo)")
			}
			daemonRan := isUnitActive("omahabd.service")
			if daemonRan {
				if !flagForce && !flagYes {
					fmt.Println("omahabd is running; the restore must stop and restart it.")
					if !confirmPrompt(fmt.Sprintf("stop omahabd and restore snapshot %s?", selectedSnapshot)) {
						fmt.Println("aborted")
						return nil
					}
				}
				if out, err := exec.Command("systemctl", "stop", "omahabd.service").CombinedOutput(); err != nil {
					return fmt.Errorf("stop omahabd: %v: %s", err, strings.TrimSpace(string(out)))
				}
			}
			// One shared executor: same code path as the daemon-side
			// omahab-restore unit run. The CLI itself guarantees the
			// daemon comes back afterwards — success OR failure,
			// config-only restores included: the interactive path runs
			// outside the omahab-restore unit, so its ExecStopPost
			// never fires here.
			op, err = controlplane.RunOfflineRecoveryRestore(ctx, controlplane.OfflineRecoveryOptions{
				StateDir:           stateDir,
				DataDir:            defaultDataDir(),
				Catalog:            catalog,
				LayoutPath:         layout,
				Phrase:             phrase,
				RepositoryPassword: repoPassword,
				OpID:               op.ID,
			})
			if daemonRan {
				if out, serr := exec.Command("systemctl", "start", "omahabd.service").CombinedOutput(); serr != nil {
					return errors.Join(err, fmt.Errorf("restart omahabd: %w: %s", serr, strings.TrimSpace(string(out))))
				}
			}
			if err != nil {
				return err
			}
			if flagJSON {
				return printJSON(map[string]any{"operation": op.ID, "snapshot": selectedSnapshot, "status": op.Status, "stage": op.Stage})
			}
			fmt.Printf("restore %s (operation %s); omahabd is back — open the panel to finish setup\n", op.Status, op.ID)
			return nil
		},
	}
	c.Flags().StringVar(&location, "repository", "", "restic repository location (sftp://… or local path)")
	c.Flags().StringVar(&passwordArg, "password-file", "", "file holding the restic repository password (default: derive from the phrase)")
	c.Flags().StringVar(&snapshotID, "snapshot", "", "specific snapshot id (default: newest server-config snapshot)")
	c.Flags().StringSliceVar(&scopes, "scopes", nil, "scopes to restore (app:<bundle-id>, server-sync, server-projects, machine-backups); server-config always restores")
	c.Flags().BoolVar(&includeApps, "include-apps", false, "restore every captured scope (apps and server-owned data)")
	c.Flags().StringVar(&kitPath, "kit", "", "path to recovery.kit (default: staged copy from the snapshot)")
	c.Flags().BoolVar(&listOnly, "list", false, "list candidate snapshots and exit")
	c.Flags().BoolVar(&stageOnly, "stage", false, "stage and validate only; do not swap")
	c.Flags().BoolVar(&applyStaged, "apply-staged", false, "apply a restore staged through the panel (phrase from the trigger handoff)")
	return c
}

// runApplyStaged applies a restore staged through the panel: the phrase
// arrives via the tmpfs handoff written at trigger time, the journal and
// staging tree live under the state dir. This is what omahab-restore.service
// runs; omahabd is expected to be stopped by the unit's ExecStartPre.
// It consumes the same shared executor as the interactive path.
func runApplyStaged(ctx context.Context) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("--apply-staged requires root (the omahab-restore unit runs it)")
	}
	stateDir := defaultStateDir()
	// The unit environment carries OMAHAB_CATALOG/OMAHAB_NATIVE_LAYOUT,
	// but resolve the stable links as well for standalone runs.
	catalog, layout := installedRecoveryCatalogLayout()
	phrase, opID, repoPassword, err := readPhraseHandoffCLI()
	if err != nil {
		return err
	}
	// The shared executor validates the phrase against the journal's
	// recorded fingerprint and refuses a changed phrase.
	op, err := controlplane.RunOfflineRecoveryRestore(ctx, controlplane.OfflineRecoveryOptions{
		StateDir:           stateDir,
		DataDir:            defaultDataDir(),
		Catalog:            catalog,
		LayoutPath:         layout,
		Phrase:             phrase,
		RepositoryPassword: repoPassword,
		OpID:               opID,
	})
	if err != nil {
		return err
	}
	if flagJSON {
		return printJSON(map[string]string{"operation": op.ID, "status": op.Status, "stage": op.Stage})
	}
	fmt.Printf("restore %s (operation %s); omahabd restarts via the unit — open the panel to finish setup\n", op.Status, op.ID)
	return nil
}

// readPhraseHandoffCLI consumes (and removes) the tmpfs phrase handoff:
// {operation_id, phrase, password?}. The optional password is the generic
// repository password for non-derived repositories; it exists only in the
// root-only tmpfs file and this process's memory, never argv or journals.
// A malformed handoff is deleted so a one-time trigger never loops.
func readPhraseHandoffCLI() (phrase, opID, repoPassword string, err error) {
	path := "/run/omahab/recovery-phrase"
	if v := strings.TrimSpace(os.Getenv("OMAHAB_RUNTIME_DIR")); v != "" {
		path = filepath.Join(v, "recovery-phrase")
	}
	raw, rerr := os.ReadFile(path)
	_ = os.Remove(path)
	if rerr != nil {
		return "", "", "", fmt.Errorf("recovery phrase handoff missing (expected %s from the panel trigger): %w", path, rerr)
	}
	var h struct {
		OperationID string `json:"operation_id"`
		Phrase      string `json:"phrase"`
		Password    string `json:"password"`
	}
	if err := json.Unmarshal(raw, &h); err != nil {
		return "", "", "", fmt.Errorf("parse phrase handoff: %w", err)
	}
	return h.Phrase, h.OperationID, h.Password, nil
}

// installedRecoveryCatalogLayout resolves the native catalog and layout
// the offline restore should plan with. OMAHAB_CATALOG and
// OMAHAB_NATIVE_LAYOUT (the omahabd/omahab-restore unit environments) win
// when set; an interactive recovery shell has neither, so fall back to
// the stable /etc/omahab declarative links nix/module.nix points at the
// exact store paths the services use. Empty when nothing resolves (dev
// runs): the shared executor then degrades exactly as before.
func installedRecoveryCatalogLayout() (catalog, layout string) {
	catalog = strings.TrimSpace(os.Getenv("OMAHAB_CATALOG"))
	if catalog == "" {
		if _, err := os.Stat(stableCatalogPath); err == nil {
			catalog = stableCatalogPath
		}
	}
	layout = strings.TrimSpace(os.Getenv("OMAHAB_NATIVE_LAYOUT"))
	if layout == "" {
		if _, err := os.Stat(stableNativeLayoutPath); err == nil {
			layout = stableNativeLayoutPath
		}
	}
	return catalog, layout
}

// readRepoPassword resolves the repository password: --password-file
// wins, else derive from the phrase seed (the setup-time HKDF).
func readRepoPassword(passwordFile string, seed [32]byte) (string, error) {
	if strings.TrimSpace(passwordFile) != "" {
		raw, err := os.ReadFile(strings.TrimSpace(passwordFile))
		if err != nil {
			return "", fmt.Errorf("read --password-file: %w", err)
		}
		pw := strings.TrimSpace(string(raw))
		if pw == "" {
			return "", fmt.Errorf("--password-file is empty")
		}
		return pw, nil
	}
	return secrets.DeriveResticPassword(seed), nil
}

// defaultStateDir mirrors the daemon's state directory resolution.
func defaultStateDir() string {
	if v := os.Getenv("OMAHAB_STATE_DIR"); v != "" {
		return v
	}
	return "/var/lib/omahab"
}

// defaultDataDir mirrors the daemon's data directory resolution.
func defaultDataDir() string {
	if v := os.Getenv("OMAHAB_DATA_DIR"); v != "" {
		return v
	}
	return "/srv/omahab"
}

func recoveryCLIRunner(stateDir string) *backups.CommandRunner {
	cache := filepath.Join(stateDir, "restic-cache")
	_ = os.MkdirAll(cache, 0o700)
	return &backups.CommandRunner{CacheDir: cache}
}

func recoveryCLIStagingRoot(stateDir string) string {
	return filepath.Join(stateDir, "recovery-restore-staging")
}

func phraseFingerprintCLI(seed [32]byte) string {
	sum := sha256.Sum256(seed[:])
	return hex.EncodeToString(sum[:8])
}

func isUnitActive(unit string) bool {
	return exec.Command("systemctl", "is-active", "--quiet", unit).Run() == nil
}

func printRecoverySnapshots(snaps []backups.RecoverySnapshot) error {
	if flagJSON {
		return printJSON(snaps)
	}
	for _, s := range snaps {
		mark := ""
		if s.ServerCfg {
			mark = " [server-config]"
		}
		fmt.Printf("%s  %s  %s%s\n", s.ID, s.Time, s.Hostname, mark)
	}
	return nil
}
