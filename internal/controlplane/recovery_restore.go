package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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

var recoveryDispatchMu sync.Mutex

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
			Scopes: snapshotScopesFromTags(s.Tags),
		})
	}
	return apitypes.RecoverySnapshotList{Snapshots: out}, nil
}

// ListRecoveryInstances implements the instance-discovery step of the
// first-restore flow: derive the restic password from the phrase, then
// enumerate every earlier Omahab instance under the backup account
// (Hetzner form: SFTP listing of ./omahab/ on port 23, with the backup
// SSH key uploaded first on a fresh machine). A generic location is
// probed directly: sftp roots enumerate through the same listing; other
// schemes report the single location. Each instance's newest snapshot
// time and manifest domain surface when the password opens it; locked
// instances report as locked instead of failing the listing.
func (b *Backend) ListRecoveryInstances(ctx context.Context, req apitypes.RecoveryListRequest) (apitypes.RecoveryInstanceList, error) {
	seed, err := phraseSeedFromRequest(req.Phrase)
	if err != nil {
		return apitypes.RecoveryInstanceList{}, err
	}
	// A fresh machine may have no backup SSH key yet: Hetzner
	// credentials upload the generated key once (the same setup flow)
	// before any SFTP listing.
	if req.Username != "" && req.Host != "" && req.SubAccountPassword != "" {
		if err := b.prepareBackupSSHKey(ctx, req.Host, req.Username, req.SubAccountPassword); err != nil {
			return apitypes.RecoveryInstanceList{}, err
		}
	}
	// Password: phrase-derived unless the caller supplied one (generic
	// repositories with their own password).
	password := strings.TrimSpace(req.Password)
	if password == "" {
		password = secrets.DeriveResticPassword(seed)
	}
	runner := recoveryRunner(b.cfg.StateDir)

	// Hetzner account form: list instance directories under
	// sftp://<user>@<host>:23/./omahab.
	if strings.TrimSpace(req.Location) == "" {
		if strings.TrimSpace(req.Username) == "" || strings.TrimSpace(req.Host) == "" {
			return apitypes.RecoveryInstanceList{}, translateError(fmt.Errorf("%w: recovery instance discovery needs the Hetzner account (username + host) or an exact location", store.ErrValidation))
		}
		root := fmt.Sprintf("sftp://%s@%s:%s/./omahab", strings.TrimSpace(req.Username), strings.TrimSpace(req.Host), backupSSHPort)
		lister := sftpInstanceLister{stateDir: b.cfg.StateDir}
		entries, err := backups.ListInstances(ctx, lister, runner, root, password)
		if err != nil {
			return apitypes.RecoveryInstanceList{}, translateError(err)
		}
		return recoveryInstanceListResponse(entries), nil
	}

	// Exact location given: sftp roots enumerate like the account form
	// (the caller pointed at a parent directory); every other scheme is
	// a single repository.
	loc := strings.TrimSpace(req.Location)
	if strings.HasPrefix(strings.ToLower(loc), "sftp://") {
		lister := sftpInstanceLister{stateDir: b.cfg.StateDir}
		entries, err := backups.ListInstances(ctx, lister, runner, loc, password)
		if err != nil {
			return apitypes.RecoveryInstanceList{}, translateError(err)
		}
		return recoveryInstanceListResponse(entries), nil
	}
	entry := backups.ListSingleInstance(ctx, runner, loc, password)
	return recoveryInstanceListResponse([]backups.InstanceListEntry{entry}), nil
}

// recoveryInstanceListResponse converts discovered instances to their
// API shape; the list is always non-null on the wire.
func recoveryInstanceListResponse(entries []backups.InstanceListEntry) apitypes.RecoveryInstanceList {
	out := make([]apitypes.RecoveryInstance, 0, len(entries))
	for _, e := range entries {
		out = append(out, apitypes.RecoveryInstance{
			InstanceID:         e.InstanceID,
			Location:           e.Location,
			LatestSnapshotTime: e.LatestSnapshotTime,
			Domain:             e.Domain,
			Locked:             e.Locked,
		})
	}
	return apitypes.RecoveryInstanceList{Instances: out}
}

// snapshotScopesFromTags extracts the scope:<name> tags restic records on
// every manifest-bearing snapshot, dropping the bare omahab marker.
func snapshotScopesFromTags(tags []string) []string {
	var scopes []string
	for _, t := range tags {
		if name, ok := strings.CutPrefix(t, "scope:"); ok && strings.TrimSpace(name) != "" {
			scopes = append(scopes, strings.TrimSpace(name))
		}
	}
	return scopes
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
	// The phrase-derived repository password resolves BEFORE any source
	// validation touches the repository: Hetzner-style locations have no
	// explicit password, and validating the location without the derived
	// credential would reject the exact repository the phrase opens.
	src, err := deriveRecoverySource(req, seed)
	if err != nil {
		return RecoveryRestoreStatus{}, translateError(err)
	}
	if strings.TrimSpace(req.Password) == "" {
		src.Password = secrets.DeriveResticPassword(seed)
	}
	op := newOperation(recoveryRestoreJournalKind, nil)
	op.Status = OpRunning
	op.Stage = recoveryStageListing
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
	b.markRestoreSetupState("in_progress", op.ID, "")
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
	rt := offlineRecoveryRuntime{
		stateDir:   b.cfg.StateDir,
		dataDir:    b.cfg.DataDir,
		catalog:    b.cfg.CatalogPath,
		layoutPath: b.cfg.NativeLayoutPath,
	}
	if err := offlineStageAndValidate(ctx, rt, recoveryRunner(b.cfg.StateDir), src, snap.ID, staging, &j); err != nil {
		return b.failRecoveryRestore(&j, err.Error())
	}
	return j.Operation, nil
}

// restoreReleaseRefNote warns when the staged snapshot was captured from a
// different system release than the running one. The swap itself still
// applies (control-plane state is release-independent); app payloads from
// the snapshot restore later through the scoped path, which refuses
// version mismatches on its own.
func restoreReleaseRefNote(manifest backups.SnapshotManifest) string {
	want := strings.TrimSpace(manifest.System.ReleaseRef)
	if want == "" {
		return ""
	}
	if current := strings.TrimSpace(systemReleaseRef()); current != "" && current != want {
		return fmt.Sprintf("Snapshot was captured from release %s; this system runs %s. Control-plane state restores regardless; app data from this snapshot may need the matching release before it restores.", want, current)
	}
	return ""
}

// SkipRecoveryRestore records the operator's explicit "start a new server"
// choice: the setup checklist's restore check turns skipped. It never
// touches staged restores or journals.
func (b *Backend) SkipRecoveryRestore(ctx context.Context) error {
	if _, err := loadRestoreSetupState(b.cfg.StateDir); err != nil {
		return err
	}
	b.markRestoreSetupState("skipped", "", "")
	return nil
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
// the restore has been staged and validated; this writes the tmpfs phrase
// handoff, persists the dispatched state, and starts the omahab-restore
// oneshot unit NONBLOCKING (systemctl --no-block). The unit stops
// omahabd, runs the shared offline executor as root, and restarts
// omahabd; progress stays observable through GetRecoveryRestoreStatus
// after the daemon is back. Persisting the started state BEFORE the
// dispatch means a lost API response (the unit stops this daemon) never
// hides an in-flight restore from a reload.
func (b *Backend) TriggerRecoveryRestoreUnit(ctx context.Context, opID, phrase, password string) (RecoveryRestoreStatus, error) {
	recoveryDispatchMu.Lock()
	defer recoveryDispatchMu.Unlock()
	j, err := loadRecoveryRestoreJournal(b.cfg.StateDir, strings.TrimSpace(opID))
	if err != nil {
		return RecoveryRestoreStatus{}, err
	}
	if j.Operation.Stage == recoveryStageCompleted && j.Operation.Status == OpCompleted {
		return j.Operation, nil
	}
	if !(j.Operation.Stage == recoveryStageStaged && j.Operation.Status == OpCompleted) && !j.Operation.CanResume {
		return RecoveryRestoreStatus{}, translateError(fmt.Errorf("%w: recovery restore %q cannot resume; stage it again", store.ErrConflict, opID))
	}
	if strings.TrimSpace(phrase) == "" {
		return RecoveryRestoreStatus{}, translateError(fmt.Errorf("%w: recovery phrase is required to apply the staged restore", store.ErrValidation))
	}
	unitState, err := exec.CommandContext(ctx, "systemctl", "show", "omahab-restore.service", "--property=ActiveState", "--value").Output()
	if err != nil {
		return RecoveryRestoreStatus{}, translateError(err)
	}
	switch strings.TrimSpace(string(unitState)) {
	case "active", "activating", "reloading", "deactivating":
		return RecoveryRestoreStatus{}, translateError(fmt.Errorf("%w: recovery restore unit is already running", store.ErrConflict))
	}
	// The phrase and optional password pass through a root-only tmpfs
	// handoff because the unit starts after this daemon stops.
	// The CLI consumes the file first. Neither secret enters the journal.
	// This is an exception to memory-only handling: root can read the file
	// during the apply window. Root already has equivalent
	// access to master.key and control.db on a running host (DESIGN
	// secret-storage boundary), so this grants no new capability.
	if err := writeRecoveryPhraseHandoff(j.Operation.ID, phrase, password); err != nil {
		return RecoveryRestoreStatus{}, translateError(err)
	}
	// Save the pending operation before dispatch can stop this daemon.
	j.Operation.Status = OpPending
	j.Operation.CanResume = true
	if err := writeRecoveryRestoreJournal(b.cfg.StateDir, j); err != nil {
		_ = os.Remove(recoveryPhraseHandoffPath())
		return RecoveryRestoreStatus{}, translateError(err)
	}
	out, err := exec.Command("systemctl", "--no-block", "start", "omahab-restore.service").CombinedOutput()
	if err != nil {
		_ = os.Remove(recoveryPhraseHandoffPath())
		j.Operation.Status = OpFailed
		j.Operation.Error = fmt.Sprintf("start omahab-restore.service: %v: %s", err, strings.TrimSpace(string(out)))
		_ = writeRecoveryRestoreJournal(b.cfg.StateDir, j)
		return RecoveryRestoreStatus{}, translateError(fmt.Errorf("%s", j.Operation.Error))
	}
	return j.Operation, nil
}

// recoveryPhraseHandoffPath is the tmpfs phrase handoff consumed by
// `omahab recovery apply --apply-staged`. /run is the daemon's declared
// ReadWritePaths root and is wiped at boot. The file exists because the
// apply unit runs after omahabd stops: no process holds the phrase in
// memory across that gap. It is 0600 root in a 0700 tmpfs directory and
// removed by the CLI first thing; it is readable by root during the apply
// window — the same trust boundary as master.key on a running host.
func recoveryPhraseHandoffPath() string {
	if v := strings.TrimSpace(os.Getenv("OMAHAB_RUNTIME_DIR")); v != "" {
		return filepath.Join(v, "recovery-phrase")
	}
	return "/run/omahab/recovery-phrase"
}

func writeRecoveryPhraseHandoff(opID, phrase, password string) error {
	path := recoveryPhraseHandoffPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create runtime dir: %w", err)
	}
	body, err := json.Marshal(struct {
		OperationID string `json:"operation_id"`
		Phrase      string `json:"phrase"`
		Password    string `json:"password,omitempty"`
	}{OperationID: opID, Phrase: phrase, Password: password})
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
	Operation  Operation             `json:"operation"`
	Source     recoveryJournalSource `json:"source"`
	StagingDir string                `json:"staging_dir"`
	// BundleApplyID is the bundle-apply operation this restore staged to
	// activate snapshot-carried custom bundle definitions. It is
	// persisted before the omahab-bundle-apply unit starts so an
	// interruption converges the exact apply instead of staging a
	// second one.
	BundleApplyID string                    `json:"bundle_apply_id,omitempty"`
	Manifest      *backups.SnapshotManifest `json:"manifest,omitempty"`
	Error         string                    `json:"error,omitempty"`
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

// restoreSetupState is the persisted first-boot restore posture behind the
// setup checklist's "restore" check: not_started | skipped | in_progress |
// completed | failed. It is written at the restore flow's transitions and
// survives the daemon restart the swap itself causes; "skipped" records the
// operator's explicit "new server" choice so the checklist stops asking.
type restoreSetupState struct {
	State     string `json:"state"`
	UpdatedAt string `json:"updated_at"`
	Operation string `json:"operation,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

func restoreSetupStatePath(stateDir string) string {
	return filepath.Join(stateDir, "restore_setup_state.json")
}

func writeRestoreSetupState(stateDir string, st restoreSetupState) error {
	st.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return syncWriteFile(restoreSetupStatePath(stateDir), append(raw, '\n'), 0o600)
}

func loadRestoreSetupState(stateDir string) (restoreSetupState, error) {
	raw, err := os.ReadFile(restoreSetupStatePath(stateDir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return restoreSetupState{State: "not_started"}, nil
		}
		return restoreSetupState{}, err
	}
	var st restoreSetupState
	if err := json.Unmarshal(raw, &st); err != nil {
		return restoreSetupState{}, fmt.Errorf("parse restore setup state: %w", err)
	}
	if strings.TrimSpace(st.State) == "" {
		return restoreSetupState{State: "not_started"}, nil
	}
	return st, nil
}

// markRestoreSetupState persists a restore-flow transition; a write error is
// logged, never fatal — the journal and staged tree remain the authority for
// the restore itself.
func (b *Backend) markRestoreSetupState(state, operation, detail string) {
	st := restoreSetupState{State: state, Operation: operation, Detail: detail}
	if err := writeRestoreSetupState(b.cfg.StateDir, st); err != nil {
		log.Printf("persist restore setup state %q: %v", state, err)
	}
}

func (b *Backend) failRecoveryRestore(j *recoveryRestoreJournal, msg string) (RecoveryRestoreStatus, error) {
	j.Operation.Status = OpFailed
	j.Operation.Stage = OpFailed
	j.Operation.Error = msg
	j.Error = msg
	b.markRestoreSetupState("failed", j.Operation.ID, msg)
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
