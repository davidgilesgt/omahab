package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/omahab/omahab/internal/backups"
	"github.com/omahab/omahab/internal/secrets"
)

// `omahab recovery apply` — the offline disaster-recovery executor
// (DESIGN §5.4, §19, §24.6). Runs on a fresh machine as root with omahabd
// stopped (or stopped mid-command): prompts for the 24-word recovery
// phrase (never argv), derives the restic password via the same HKDF as
// setup, lists server-config snapshots, stages and validates the newest
// (or --snapshot), then swaps control-plane state into place with a
// rollback copy and restarts omahabd so setup reconverges apps.
//
// Listing and staging never touch live state, so --list and --stage are
// safe while the daemon runs; only the swap stops and restarts omahabd.

func newRecoveryApplyCmd() *cobra.Command {
	var (
		location    string
		passwordArg string
		snapshotID  string
		kitPath     string
		listOnly    bool
		stageOnly   bool
		applyStaged bool
	)
	c := &cobra.Command{
		Use:   "apply",
		Short: "Restore control-plane state from a backup repository (offline, root)",
		Long: `Disaster recovery on a fresh machine.

Prompts for the 24-word recovery phrase (never pass it as an argument).
The repository password is derived from the phrase exactly as during
setup (HKDF-SHA256, salt "omahab-recovery-v1", info "restic-password"),
or read from --password-file for generic repositories.

Flow: list snapshots -> stage + validate the latest server-config
snapshot (or --snapshot) -> stop omahabd -> swap control.db, master.key,
recovery.kit and configuration atomically (rollback copy kept) -> restart
omahabd. App payloads restore afterwards through the normal app-scoped
restore once the dashboard is back.

The swap requires root. --list and --stage are safe while omahabd runs.
--apply-staged consumes a restore staged through the panel (phrase read
from the trigger handoff, never argv); it is what the omahab-restore
systemd unit runs.`,
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
			repoPassword, err := readRepoPassword(passwordArg, seed)
			if err != nil {
				return err
			}
			src := backups.RecoverySource{Location: location, Password: repoPassword}
			stateDir := defaultStateDir()
			runner := recoveryCLIRunner(stateDir)

			// Listing and staging never touch live state.
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
			if !flagJSON {
				fmt.Printf("restoring snapshot %s (%s)\n", pick.ID, pick.Time)
			}
			staging, err := os.MkdirTemp(recoveryCLIStagingRoot(stateDir), "apply-")
			if err != nil {
				return err
			}
			if err := backups.StageRecoverySnapshot(ctx, runner, src, pick.ID, staging); err != nil {
				return fmt.Errorf("extract snapshot: %w", err)
			}
			rep, err := backups.ValidateStaging(ctx, staging)
			if err != nil {
				return fmt.Errorf("staging validation: %w", err)
			}
			manifest, err := backups.ServerConfigManifest(staging)
			if err != nil {
				return err
			}
			fp := phraseFingerprintCLI(seed)
			if err := backups.CheckRecoveryFingerprint(manifest, fp); err != nil {
				return err
			}
			plan, err := backups.PlanServerConfigSwap(staging, stateDir, manifest)
			if err != nil {
				return fmt.Errorf("swap plan: %w", err)
			}
			master, err := unwrapStagedKit(staging, stateDir, seed, kitPath)
			if err != nil {
				return err
			}
			defer zeroSlice(master[:])
			if stageOnly {
				if flagJSON {
					return printJSON(map[string]string{"staging": staging, "snapshot": pick.ID, "status": "staged"})
				}
				fmt.Printf("staged and validated at %s (not applied; re-run without --stage to swap)\n", staging)
				return nil
			}
			// The swap replaces the running daemon's database: require
			// root and a stopped (or stopping) omahabd.
			if os.Geteuid() != 0 {
				return fmt.Errorf("applying a server-config restore requires root (re-run with sudo)")
			}
			daemonRan := isUnitActive("omahabd.service")
			if daemonRan {
				if !flagForce && !flagYes {
					fmt.Println("omahabd is running; the restore must stop and restart it.")
					if !confirmPrompt(fmt.Sprintf("stop omahabd and restore snapshot %s?", pick.ID)) {
						fmt.Println("aborted")
						return nil
					}
				}
				if out, err := exec.Command("systemctl", "stop", "omahabd.service").CombinedOutput(); err != nil {
					return fmt.Errorf("stop omahabd: %v: %s", err, strings.TrimSpace(string(out)))
				}
			}
			defer restartOmahabd()
			rollback, cleanup, err := backups.ApplyServerConfigSwap(ctx, plan, master[:])
			if err != nil {
				if rbErr := rollback(); rbErr != nil {
					return fmt.Errorf("swap failed (%v) and rollback failed (%v); inspect %s and %s", err, rbErr, staging, stateDir)
				}
				return fmt.Errorf("swap failed and rolled back: %w", err)
			}
			defer cleanup()
			if flagJSON {
				return printJSON(map[string]any{"snapshot": pick.ID, "status": "completed", "files": rep.Files, "sqlite_ok": rep.SQLiteOK, "sqlite_checked": rep.SQLiteChecked})
			}
			fmt.Printf("control-plane state restored from %s; validated files: %d, sqlite: %d/%d, pg: %d/%d\n",
				pick.ID, rep.Files, rep.SQLiteOK, rep.SQLiteChecked, rep.PGOK, rep.PGChecked)
			fmt.Println("omahabd is restarting; open the panel to finish setup (apps reconverge automatically).")
			return nil
		},
	}
	c.Flags().StringVar(&location, "repository", "", "restic repository location (sftp://… or local path)")
	c.Flags().StringVar(&passwordArg, "password-file", "", "file holding the restic repository password (default: derive from the phrase)")
	c.Flags().StringVar(&snapshotID, "snapshot", "", "specific snapshot id (default: newest server-config snapshot)")
	c.Flags().StringVar(&kitPath, "kit", "", "path to recovery.kit (default: staged copy from the snapshot)")
	c.Flags().BoolVar(&listOnly, "list", false, "list candidate snapshots and exit")
	c.Flags().BoolVar(&stageOnly, "stage", false, "stage and validate only; do not swap")
	c.Flags().BoolVar(&applyStaged, "apply-staged", false, "apply a restore staged through the panel (phrase from the trigger handoff)")
	return c
}

// runApplyStaged applies a restore staged through the panel: the phrase
// arrives via the tmpfs handoff written at trigger time, the journal and
// staging tree live under the state dir. This is what omahab-restore.service
// runs; omahabd is expected to be stopped by the unit.
func runApplyStaged(ctx context.Context) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("--apply-staged requires root (the omahab-restore unit runs it)")
	}
	stateDir := defaultStateDir()
	phrase, opID, err := readPhraseHandoffCLI()
	if err != nil {
		return err
	}
	words := strings.Fields(phrase)
	if len(words) != 24 {
		return fmt.Errorf("phrase handoff must hold 24 words, got %d", len(words))
	}
	seed, err := secrets.PhraseToSeed(words)
	if err != nil {
		return fmt.Errorf("invalid phrase in handoff: %w", err)
	}
	jPath := filepath.Join(stateDir, "recovery-restore-operations", opID, "journal.json")
	raw, err := os.ReadFile(jPath)
	if err != nil {
		return fmt.Errorf("read staged restore journal %s: %w", jPath, err)
	}
	var j struct {
		Operation struct {
			Status string `json:"status"`
			Stage  string `json:"stage"`
		} `json:"operation"`
		Source struct {
			SnapshotID string `json:"snapshot_id"`
			PhraseFP   string `json:"phrase_fingerprint"`
		} `json:"source"`
		StagingDir string `json:"staging_dir"`
	}
	if err := json.Unmarshal(raw, &j); err != nil {
		return fmt.Errorf("parse staged restore journal: %w", err)
	}
	if j.Operation.Stage != "staged" || j.Operation.Status != "completed" {
		return fmt.Errorf("staged restore %s is not ready (stage %q, status %q)", opID, j.Operation.Stage, j.Operation.Status)
	}
	fp := phraseFingerprintCLI(seed)
	if !strings.EqualFold(fp, strings.TrimSpace(j.Source.PhraseFP)) {
		return fmt.Errorf("phrase fingerprint does not match the staged restore; re-stage with the original phrase")
	}
	manifest, err := backups.ServerConfigManifest(j.StagingDir)
	if err != nil {
		return err
	}
	if err := backups.CheckRecoveryFingerprint(manifest, fp); err != nil {
		return err
	}
	master, err := unwrapStagedKit(j.StagingDir, stateDir, seed, "")
	if err != nil {
		return err
	}
	defer zeroSlice(master[:])
	plan, err := backups.PlanServerConfigSwap(j.StagingDir, stateDir, manifest)
	if err != nil {
		return fmt.Errorf("swap plan: %w", err)
	}
	rollback, cleanup, err := backups.ApplyServerConfigSwap(ctx, plan, master[:])
	if err != nil {
		if rbErr := rollback(); rbErr != nil {
			return fmt.Errorf("swap failed (%v) and rollback failed (%v); inspect %s", err, rbErr, j.StagingDir)
		}
		return fmt.Errorf("swap failed and rolled back: %w", err)
	}
	defer cleanup()
	// Mark the journal completed so the panel shows the outcome.
	j.Operation.Stage = "completed"
	j.Operation.Status = "completed"
	if raw2, merr := json.MarshalIndent(map[string]any{
		"operation":   j.Operation,
		"source":      j.Source,
		"staging_dir": j.StagingDir,
	}, "", "  "); merr == nil {
		_ = os.WriteFile(jPath, append(raw2, '\n'), 0o600)
	}
	if flagJSON {
		return printJSON(map[string]string{"operation": opID, "snapshot": j.Source.SnapshotID, "status": "completed"})
	}
	fmt.Printf("control-plane state restored from %s (operation %s)\n", j.Source.SnapshotID, opID)
	fmt.Println("omahabd restarts via the unit; open the panel to finish setup.")
	return nil
}

// readPhraseHandoffCLI consumes (and removes) the tmpfs phrase handoff.
func readPhraseHandoffCLI() (phrase, opID string, err error) {
	path := "/run/omahab/recovery-phrase"
	if v := strings.TrimSpace(os.Getenv("OMAHAB_RUNTIME_DIR")); v != "" {
		path = filepath.Join(v, "recovery-phrase")
	}
	raw, rerr := os.ReadFile(path)
	_ = os.Remove(path)
	if rerr != nil {
		return "", "", fmt.Errorf("recovery phrase handoff missing (expected %s from the panel trigger): %w", path, rerr)
	}
	var h struct {
		OperationID string `json:"operation_id"`
		Phrase      string `json:"phrase"`
	}
	if err := json.Unmarshal(raw, &h); err != nil {
		return "", "", fmt.Errorf("parse phrase handoff: %w", err)
	}
	return h.Phrase, h.OperationID, nil
}

// readRepoPassword resolves the repository password: --password-file
// wins, else derive from the phrase seed (the setup-time HKDF).
func readRepoPassword(passwordFile string, seed [32]byte) (string, error) {
	passwordFile = strings.TrimSpace(passwordFile)
	if passwordFile == "" {
		return secrets.DeriveResticPassword(seed), nil
	}
	raw, err := os.ReadFile(passwordFile)
	if err != nil {
		return "", fmt.Errorf("read password file: %w", err)
	}
	pw := strings.TrimSpace(string(raw))
	if pw == "" {
		return "", fmt.Errorf("password file %s is empty", passwordFile)
	}
	return pw, nil
}

// defaultStateDir mirrors the daemon's state directory resolution.
func defaultStateDir() string {
	if v := os.Getenv("OMAHAB_STATE_DIR"); v != "" {
		return v
	}
	return "/var/lib/omahab"
}

func recoveryCLIRunner(stateDir string) *backups.CommandRunner {
	cache := stateDir + "/restic-cache"
	_ = os.MkdirAll(cache, 0o700)
	return &backups.CommandRunner{CacheDir: cache}
}

func recoveryCLIStagingRoot(stateDir string) string {
	root := stateDir + "/recovery-restore-staging"
	_ = os.MkdirAll(root, 0o700)
	return root
}

func phraseFingerprintCLI(seed [32]byte) string {
	sum := sha256.Sum256(seed[:])
	return hex.EncodeToString(sum[:4])
}

func printRecoverySnapshots(snaps []backups.RecoverySnapshot) error {
	if flagJSON {
		return printJSON(snaps)
	}
	if len(snaps) == 0 {
		fmt.Println("no snapshots found")
		return nil
	}
	for _, s := range snaps {
		mark := ""
		if !s.ServerCfg {
			mark = "  (no server-config)"
		}
		fmt.Printf("%s  %s  %s%s\n", s.ID, s.Time, s.Hostname, mark)
	}
	return nil
}

// unwrapStagedKit unwraps the master key from the snapshot's staged
// recovery.kit. An explicit --kit path overrides the staged copy (fresh
// machine where the kit was copied by hand).
func unwrapStagedKit(staging, stateDir string, seed [32]byte, kitOverride string) ([32]byte, error) {
	kitPath := strings.TrimSpace(kitOverride)
	if kitPath == "" {
		p, err := backups.StagedRecoveryKitPath(staging, stateDir)
		if err != nil {
			return [32]byte{}, err
		}
		kitPath = p
	}
	raw, err := os.ReadFile(kitPath)
	if err != nil {
		return [32]byte{}, fmt.Errorf("read recovery.kit: %w", err)
	}
	var kit struct {
		Version       int    `json:"version"`
		Fingerprint   string `json:"fingerprint"`
		MasterWrapped string `json:"master_wrapped"`
		CreatedAt     string `json:"created_at"`
	}
	if err := json.Unmarshal(raw, &kit); err != nil {
		return [32]byte{}, fmt.Errorf("parse recovery.kit: %w", err)
	}
	blob, err := base64.StdEncoding.DecodeString(kit.MasterWrapped)
	if err != nil {
		return [32]byte{}, fmt.Errorf("decode master_wrapped: %w", err)
	}
	recoveryKey := secrets.DeriveRecoveryKey(seed)
	master, err := secrets.UnwrapMasterKey(blob, recoveryKey)
	if err != nil {
		return [32]byte{}, fmt.Errorf("unwrap failed: the phrase does not match this recovery kit")
	}
	fp := phraseFingerprintCLI(seed)
	if !strings.EqualFold(strings.TrimSpace(kit.Fingerprint), fp) {
		return [32]byte{}, fmt.Errorf("phrase fingerprint %s does not match kit fingerprint %s", fp, kit.Fingerprint)
	}
	return master, nil
}

func restartOmahabd() {
	_, _ = exec.Command("systemctl", "start", "omahabd.service").CombinedOutput()
}

func isUnitActive(unit string) bool {
	return exec.Command("systemctl", "is-active", "--quiet", unit).Run() == nil
}

func zeroSlice(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
