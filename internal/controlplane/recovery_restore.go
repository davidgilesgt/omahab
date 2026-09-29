package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/omahab/omahab/internal/apitypes"
	"github.com/omahab/omahab/internal/backups"
	"github.com/omahab/omahab/internal/secrets"
	"github.com/omahab/omahab/internal/store"
)

// Offline recovery executor (DESIGN §5.4, §19, §24.6): the server-config
// restore path `omahab recovery apply` and the setup-UI "Restore from
// backup" flow both land here. It runs while omahabd is stopped or in
// maintenance (the omahab-restore oneshot unit), because it replaces
// control.db, master.key, recovery.kit and rendered configuration
// atomically. The daemon-side stage (PrepareRecoveryRestore) only lists
// and extracts; the swap itself never runs inside a live daemon.

// recoveryRestoreJournalKind names the journal family for offline recovery
// restores so the shared operation polling surface can observe progress.
const recoveryRestoreJournalKind = "recovery-restore"

// RecoveryRestoreStatus is the observable progress of one offline restore.
type RecoveryRestoreStatus = apitypes.RecoveryRestoreStatus

// recoveryStagingDir is where the offline executor extracts snapshots.
func recoveryStagingDir(stateDir string) string {
	return filepath.Join(stateDir, "recovery-restore-staging")
}

// deriveRecoverySource maps a request to a restic source: Hetzner derives
// the repository password from the phrase (HKDF restic-password), generic
// uses the supplied location and password.
func deriveRecoverySource(req apitypes.RecoveryRestoreRequest, seed [32]byte) (backups.RecoverySource, error) {
	loc := strings.TrimSpace(req.Location)
	if loc == "" {
		return backups.RecoverySource{}, fmt.Errorf("%w: repository location is required", store.ErrValidation)
	}
	if strings.HasPrefix(strings.ToLower(loc), "sftp://") && strings.TrimSpace(req.Password) == "" {
		return backups.RecoverySource{}, fmt.Errorf("%w: sftp repositories need their password (or use the Hetzner form with the recovery phrase)", store.ErrValidation)
	}
	return backups.RecoverySource{
		Location: loc,
		Password: strings.TrimSpace(req.Password),
	}, nil
}

// recoveryRunner builds the restic runner for offline restores. The cache
// lives under the state dir like the daemon's own invocations.
func recoveryRunner(stateDir string) *backups.CommandRunner {
	cache := filepath.Join(stateDir, "restic-cache")
	_ = os.MkdirAll(cache, 0o700)
	return &backups.CommandRunner{CacheDir: cache}
}

// prepareBackupSSHKey generates (when absent) and uploads the backup SSH
// key for Hetzner SFTP repositories on a fresh machine, exactly like the
// setup wizard. The sub-account password is used once and discarded.
func (b *Backend) prepareBackupSSHKey(ctx context.Context, host, username, subPass string) error {
	username = strings.TrimSpace(username)
	host = strings.TrimSpace(host)
	if username == "" || host == "" || strings.TrimSpace(subPass) == "" {
		return nil
	}
	if !validBackupUsername(username) {
		return translateError(fmt.Errorf("%w: invalid Hetzner username", store.ErrValidation))
	}
	if !validBackupHost(host) {
		return translateError(fmt.Errorf("%w: invalid Hetzner host", store.ErrValidation))
	}
	_, pubLine, err := ensureBackupSSHKey(b.cfg.StateDir)
	if err != nil {
		return fmt.Errorf("generate backup ssh key: %w", err)
	}
	if err := uploadHetznerKey(ctx, b.cfg.StateDir, host, username, subPass, pubLine); err != nil {
		return fmt.Errorf("upload backup ssh key: %w", err)
	}
	return nil
}

// ListRecoverySnapshots implements the offline listing step: derive the
// restic password from the phrase, list the repository's newest snapshots,
// and mark which carry server-config. The phrase is required — it is the
// only credential this path trusts.
func (b *Backend) ListRecoverySnapshots(ctx context.Context, req apitypes.RecoveryListRequest) (apitypes.RecoverySnapshotList, error) {
	seed, err := phraseSeedFromRequest(req.Phrase)
	if err != nil {
		return apitypes.RecoverySnapshotList{}, err
	}
	// A fresh machine may have no backup SSH key yet: Hetzner
	// credentials upload the generated key once (the same setup flow)
	// before any restic sftp call.
	if req.Username != "" && req.Host != "" && req.SubAccountPassword != "" {
		if err := b.prepareBackupSSHKey(ctx, req.Host, req.Username, req.SubAccountPassword); err != nil {
			return apitypes.RecoverySnapshotList{}, err
		}
	}
	src, err := deriveRecoverySource(apitypes.RecoveryRestoreRequest{
		Location: req.Location, Password: req.Password,
	}, seed)
	if err != nil {
		return apitypes.RecoverySnapshotList{}, translateError(err)
	}
	// Hetzner form: password derived from the phrase unless supplied.
	if strings.TrimSpace(req.Password) == "" {
		src.Password = secrets.DeriveResticPassword(seed)
	}
	latest := req.Latest
	if latest <= 0 || latest > 50 {
		latest = 10
	}
	snaps, err := backups.ListRecoverySnapshots(ctx, recoveryRunner(b.cfg.StateDir), src, b.cfg.StateDir, latest)
	if err != nil {
		return apitypes.RecoverySnapshotList{}, translateError(err)
	}
	out := make([]apitypes.RecoverySnapshot, 0, len(snaps))
	for _, s := range snaps {
		out = append(out, apitypes.RecoverySnapshot{
			ID: s.ID, Time: s.Time, Hostname: s.Hostname, ServerConfig: s.ServerCfg,
		})
	}
	return apitypes.RecoverySnapshotList{Snapshots: out}, nil
}

// PrepareRecoveryRestore performs the safe, daemon-side half of the
// offline restore: phrase validation, snapshot selection, extraction to
// staging, and full validation (manifest, fingerprint, scope). It writes
// an operation journal and never swaps anything: the caller (CLI as root,
// or the omahab-restore oneshot unit) applies the swap with omahabd
// stopped. App scopes may be included; they restore afterwards through
// the existing app-scoped staging/live restore once the daemon reconverges.
func (b *Backend) PrepareRecoveryRestore(ctx context.Context, req apitypes.RecoveryRestoreRequest) (RecoveryRestoreStatus, error) {
	seed, err := phraseSeedFromRequest(req.Phrase)
	if err != nil {
		return RecoveryRestoreStatus{}, err
	}
	fp := recoverySeedFingerprint(seed)
	if req.Username != "" && req.Host != "" && req.SubAccountPassword != "" {
		if err := b.prepareBackupSSHKey(ctx, req.Host, req.Username, req.SubAccountPassword); err != nil {
			return RecoveryRestoreStatus{}, err
		}
	}
	src, err := deriveRecoverySource(req, seed)
	if err != nil {
		return RecoveryRestoreStatus{}, translateError(err)
	}
	if strings.TrimSpace(req.Password) == "" {
		src.Password = secrets.DeriveResticPassword(seed)
	}
	op := newOperation(recoveryRestoreJournalKind, nil)
	op.Status = OpRunning
	op.Stage = "listing"
	staging := filepath.Join(recoveryStagingDir(b.cfg.StateDir), op.ID)
	j := recoveryRestoreJournal{
		Operation: op,
		Source: recoveryJournalSource{
			Location:    src.Location,
			SnapshotID:  strings.TrimSpace(req.SnapshotID),
			PhraseFP:    fp,
			Scopes:      dedupeStrings(req.Scopes),
			IncludeApps: req.IncludeApps,
		},
		StagingDir: staging,
	}
	if err := writeRecoveryRestoreJournal(b.cfg.StateDir, j); err != nil {
		return RecoveryRestoreStatus{}, err
	}
	snaps, err := backups.ListRecoverySnapshots(ctx, recoveryRunner(b.cfg.StateDir), src, b.cfg.StateDir, 10)
	if err != nil {
		return b.failRecoveryRestore(&j, "list snapshots: "+err.Error())
	}
	snap, err := backups.PickRecoverySnapshot(snaps, req.SnapshotID)
	if err != nil {
		return b.failRecoveryRestore(&j, "select snapshot: "+err.Error())
	}
	j.Source.SnapshotID = snap.ID
	j.Operation.Stage = "staging"
	_ = writeRecoveryRestoreJournal(b.cfg.StateDir, j)
	if err := backups.StageRecoverySnapshot(ctx, recoveryRunner(b.cfg.StateDir), src, snap.ID, staging); err != nil {
		return b.failRecoveryRestore(&j, "extract snapshot: "+err.Error())
	}
	rep, err := backups.ValidateStaging(ctx, staging)
	if err != nil {
		return b.failRecoveryRestore(&j, "staging validation: "+err.Error())
	}
	manifest, err := backups.ServerConfigManifest(staging)
	if err != nil {
		return b.failRecoveryRestore(&j, err.Error())
	}
	if err := backups.CheckRecoveryFingerprint(manifest, fp); err != nil {
		return b.failRecoveryRestore(&j, err.Error())
	}
	scopes := j.Source.Scopes
	if len(scopes) == 0 {
		scopes = manifest.Scopes
	}
	if err := checkRestoreScopes(rep, scopes); err != nil {
		return b.failRecoveryRestore(&j, err.Error())
	}
	if _, err := backups.PlanServerConfigSwap(staging, b.cfg.StateDir, manifest); err != nil {
		return b.failRecoveryRestore(&j, "swap plan: "+err.Error())
	}
	j.Operation.Stage = "staged"
	j.Operation.Status = OpCompleted
	j.Operation.Progress = 100
	j.Manifest = &manifest
	_ = writeRecoveryRestoreJournal(b.cfg.StateDir, j)
	return j.Operation, nil
}

// ApplyRecoveryRestore performs the offline swap half. It must run with
// omahabd stopped (the CLI refuses otherwise; the omahab-restore unit
// stops it). The phrase is re-validated against the staged snapshot
// before any live path changes: fingerprint mismatch refuses the swap.
func (b *Backend) ApplyRecoveryRestore(ctx context.Context, opID string, req apitypes.RecoveryRestoreRequest) (RecoveryRestoreStatus, error) {
	j, err := loadRecoveryRestoreJournal(b.cfg.StateDir, strings.TrimSpace(opID))
	if err != nil {
		return RecoveryRestoreStatus{}, err
	}
	if j.Operation.Status != OpCompleted || j.Operation.Stage != "staged" {
		return RecoveryRestoreStatus{}, translateError(fmt.Errorf("%w: recovery restore %q is not staged (stage %q)", store.ErrConflict, opID, j.Operation.Stage))
	}
	seed, err := phraseSeedFromRequest(req.Phrase)
	if err != nil {
		return RecoveryRestoreStatus{}, err
	}
	fp := recoverySeedFingerprint(seed)
	if !strings.EqualFold(fp, j.Source.PhraseFP) {
		return RecoveryRestoreStatus{}, translateError(fmt.Errorf("%w: phrase fingerprint changed between staging and apply; re-stage with the original phrase", store.ErrValidation))
	}
	manifest, err := backups.ServerConfigManifest(j.StagingDir)
	if err != nil {
		return b.failRecoveryRestore(&j, err.Error())
	}
	if err := backups.CheckRecoveryFingerprint(manifest, fp); err != nil {
		return b.failRecoveryRestore(&j, err.Error())
	}
	// Unwrap the master key from the staged kit before touching anything.
	master, err := unwrapStagedMasterKey(j.StagingDir, b.cfg.StateDir, seed)
	if err != nil {
		return b.failRecoveryRestore(&j, err.Error())
	}
	defer zeroBytes(master[:])
	plan, err := backups.PlanServerConfigSwap(j.StagingDir, b.cfg.StateDir, manifest)
	if err != nil {
		return b.failRecoveryRestore(&j, "swap plan: "+err.Error())
	}
	j.Operation.Stage = "swapping"
	_ = writeRecoveryRestoreJournal(b.cfg.StateDir, j)
	rollback, cleanup, err := backups.ApplyServerConfigSwap(ctx, plan, master[:])
	if err != nil {
		j.Operation.Stage = "rolling_back"
		_ = writeRecoveryRestoreJournal(b.cfg.StateDir, j)
		if rbErr := rollback(); rbErr != nil {
			return b.failRecoveryRestore(&j, fmt.Sprintf("swap failed (%v) and rollback failed (%v); state may be partially swapped — inspect %s", err, rbErr, j.StagingDir))
		}
		return b.failRecoveryRestore(&j, "swap failed and rolled back: "+err.Error())
	}
	defer cleanup()
	j.Operation.Stage = "completed"
	j.Operation.Status = OpCompleted
	j.Operation.Progress = 100
	_ = writeRecoveryRestoreJournal(b.cfg.StateDir, j)
	return j.Operation, nil
}

// ApplyStagedRecoveryRestore is the entry point for the omahab-restore
// oneshot unit and `omahab recovery apply --apply-staged`: it reads the
// tmpfs phrase handoff written at trigger time, applies the staged swap,
// and removes the handoff first thing regardless of outcome. omahabd is
// expected to be stopped by the unit (ExecStartPre).
func (b *Backend) ApplyStagedRecoveryRestore(ctx context.Context, opID string) (RecoveryRestoreStatus, error) {
	phrase, stagedOpID, err := readRecoveryPhraseHandoff()
	if err != nil {
		return RecoveryRestoreStatus{}, err
	}
	if opID == "" {
		opID = stagedOpID
	}
	op, err := b.ApplyRecoveryRestore(ctx, opID, apitypes.RecoveryRestoreRequest{Phrase: phrase})
	return op, err
}

// readRecoveryPhraseHandoff consumes (and removes) the tmpfs phrase file.
func readRecoveryPhraseHandoff() (phrase, opID string, err error) {
	path := recoveryPhraseHandoffPath()
	raw, rerr := os.ReadFile(path)
	// Remove first: the phrase must never survive the read, success or
	// not.
	_ = os.Remove(path)
	if rerr != nil {
		return "", "", fmt.Errorf("%w: recovery phrase handoff missing (expected %s from the trigger)", store.ErrNotFound, path)
	}
	var h struct {
		OperationID string `json:"operation_id"`
		Phrase      string `json:"phrase"`
	}
	if err := json.Unmarshal(raw, &h); err != nil {
		return "", "", fmt.Errorf("%w: parse phrase handoff: %v", store.ErrValidation, err)
	}
	return h.Phrase, h.OperationID, nil
}

// GetRecoveryRestoreStatus polls one offline restore by operation id.
func (b *Backend) GetRecoveryRestoreStatus(ctx context.Context, opID string) (RecoveryRestoreStatus, error) {
	j, err := loadRecoveryRestoreJournal(b.cfg.StateDir, strings.TrimSpace(opID))
	if err != nil {
		return RecoveryRestoreStatus{}, err
	}
	return j.Operation, nil
}

// TriggerRecoveryRestoreUnit is the daemon-side handoff for the setup UI:
// the restore has been staged and validated; this starts the
// omahab-restore oneshot unit (defined in nix/module.nix) which stops
// omahabd, applies the swap as root, and restarts omahabd. Progress is
// observable afterwards through GetRecoveryRestoreStatus once the daemon
// is back.
func (b *Backend) TriggerRecoveryRestoreUnit(ctx context.Context, opID, phrase string) (RecoveryRestoreStatus, error) {
	j, err := loadRecoveryRestoreJournal(b.cfg.StateDir, strings.TrimSpace(opID))
	if err != nil {
		return RecoveryRestoreStatus{}, err
	}
	if j.Operation.Stage != "staged" || j.Operation.Status != OpCompleted {
		return RecoveryRestoreStatus{}, translateError(fmt.Errorf("%w: recovery restore %q is not staged", store.ErrConflict, opID))
	}
	// Phrase handoff: the oneshot unit runs as root after this daemon
	// stops, so the phrase travels through a 0600 file on /run (tmpfs —
	// RAM-backed, never disk) that the apply step reads and removes
	// first thing. The journal never stores it.
	if err := writeRecoveryPhraseHandoff(j.Operation.ID, phrase); err != nil {
		return RecoveryRestoreStatus{}, translateError(err)
	}
	out, err := execCommand("systemctl", "start", "omahab-restore.service")
	if err != nil {
		_ = os.Remove(recoveryPhraseHandoffPath())
		return RecoveryRestoreStatus{}, translateError(fmt.Errorf("start omahab-restore.service: %v: %s", err, strings.TrimSpace(out)))
	}
	j.Operation.Stage = "unit-started"
	j.Operation.Status = OpRunning
	j.Operation.CanResume = false
	_ = writeRecoveryRestoreJournal(b.cfg.StateDir, j)
	return j.Operation, nil
}

// recoveryPhraseHandoffPath is the tmpfs phrase handoff consumed by
// `omahab recovery apply --apply-staged`. /run is the daemon's declared
// ReadWritePaths root and is wiped at boot.
func recoveryPhraseHandoffPath() string {
	if v := strings.TrimSpace(os.Getenv("OMAHAB_RUNTIME_DIR")); v != "" {
		return filepath.Join(v, "recovery-phrase")
	}
	return "/run/omahab/recovery-phrase"
}

func writeRecoveryPhraseHandoff(opID, phrase string) error {
	if strings.TrimSpace(phrase) == "" {
		return fmt.Errorf("%w: recovery phrase is required to apply the staged restore", store.ErrValidation)
	}
	path := recoveryPhraseHandoffPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create runtime dir: %w", err)
	}
	body, err := json.Marshal(map[string]string{"operation_id": opID, "phrase": phrase})
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return fmt.Errorf("write phrase handoff: %w", err)
	}
	return nil
}

// recoveryRestoreJournal persists an offline recovery restore across the
// daemon restart the swap itself causes. It never holds the phrase, seed,
// or repository password — only the fingerprint and paths.
type recoveryRestoreJournal struct {
	Operation  Operation                 `json:"operation"`
	Source     recoveryJournalSource     `json:"source"`
	StagingDir string                    `json:"staging_dir"`
	Manifest   *backups.SnapshotManifest `json:"manifest,omitempty"`
	Error      string                    `json:"error,omitempty"`
}

type recoveryJournalSource struct {
	Location    string   `json:"location"`
	SnapshotID  string   `json:"snapshot_id"`
	PhraseFP    string   `json:"phrase_fingerprint"`
	Scopes      []string `json:"scopes,omitempty"`
	IncludeApps bool     `json:"include_apps,omitempty"`
}

func writeRecoveryRestoreJournal(stateDir string, j recoveryRestoreJournal) error {
	j.Operation.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	raw, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	path := operationJournalPath(stateDir, recoveryRestoreJournalKind, j.Operation.ID)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return syncWriteFile(path, append(raw, '\n'), 0o600)
}

func loadRecoveryRestoreJournal(stateDir, id string) (recoveryRestoreJournal, error) {
	raw, err := os.ReadFile(operationJournalPath(stateDir, recoveryRestoreJournalKind, id))
	if err != nil {
		return recoveryRestoreJournal{}, fmt.Errorf("%w: recovery restore %q not found", apitypes.ErrNotFound, id)
	}
	var j recoveryRestoreJournal
	if err := json.Unmarshal(raw, &j); err != nil {
		return recoveryRestoreJournal{}, fmt.Errorf("parse recovery restore journal: %w", err)
	}
	return j, nil
}

func (b *Backend) failRecoveryRestore(j *recoveryRestoreJournal, msg string) (RecoveryRestoreStatus, error) {
	j.Operation.Status = OpFailed
	j.Operation.Stage = OpFailed
	j.Operation.Error = msg
	j.Error = msg
	_ = writeRecoveryRestoreJournal(b.cfg.StateDir, *j)
	return j.Operation, translateError(fmt.Errorf("%w: %s", store.ErrValidation, msg))
}

// phraseSeedFromRequest validates the 24-word phrase from a request body.
// The phrase is used in memory only.
func phraseSeedFromRequest(phrase string) ([32]byte, error) {
	phrase = strings.TrimSpace(phrase)
	if phrase == "" {
		return [32]byte{}, translateError(fmt.Errorf("%w: recovery phrase is required", store.ErrValidation))
	}
	words := strings.Fields(phrase)
	if len(words) != 24 {
		return [32]byte{}, translateError(fmt.Errorf("%w: phrase must have 24 words, got %d", store.ErrValidation, len(words)))
	}
	seed, err := secrets.PhraseToSeed(words)
	if err != nil {
		return [32]byte{}, translateError(fmt.Errorf("%w: invalid recovery phrase", store.ErrValidation))
	}
	return seed, nil
}

func recoverySeedFingerprint(seed [32]byte) string {
	return seedFingerprint(seed)
}

// unwrapStagedMasterKey unwraps master.key material from the staged
// snapshot's recovery.kit using the phrase seed.
func unwrapStagedMasterKey(staging, stateDir string, seed [32]byte) ([32]byte, error) {
	kitPath, err := backups.StagedRecoveryKitPath(staging, stateDir)
	if err != nil {
		return [32]byte{}, err
	}
	kit, blob, err := readRecoveryKitFile(kitPath)
	if err != nil {
		return [32]byte{}, fmt.Errorf("read staged recovery.kit: %w", err)
	}
	rk := secrets.DeriveRecoveryKey(seed)
	master, err := secrets.UnwrapMasterKey(blob, rk)
	if err != nil {
		return [32]byte{}, fmt.Errorf("%w: the recovery phrase does not unwrap the staged kit", store.ErrValidation)
	}
	fp := recoverySeedFingerprint(seed)
	if !strings.EqualFold(strings.TrimSpace(kit.Fingerprint), fp) {
		return [32]byte{}, fmt.Errorf("%w: staged kit fingerprint %s does not match the phrase (%s)", store.ErrValidation, kit.Fingerprint, fp)
	}
	return master, nil
}

func zeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
