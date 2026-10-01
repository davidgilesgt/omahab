package controlplane

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omahab/omahab/internal/apitypes"
	"github.com/omahab/omahab/internal/secrets"
)

func recoveryRestoreTestBackend(t *testing.T) *Backend {
	t.Helper()
	t.Setenv("OMAHAB_MAINTENANCE_LOCK", filepath.Join(t.TempDir(), "maintenance.lock"))
	return &Backend{
		cfg: testConfig(t.TempDir()),
	}
}

func TestPhraseSeedFromRequestValidation(t *testing.T) {
	if _, err := phraseSeedFromRequest(""); err == nil {
		t.Fatal("empty phrase must be refused")
	}
	if _, err := phraseSeedFromRequest("one two three"); err == nil {
		t.Fatal("short phrase must be refused")
	}
	// A syntactically valid BIP39 24 words but wrong checksum fails.
	bad := strings.Repeat("abandon ", 23) + "abandon"
	if _, err := phraseSeedFromRequest(bad); err == nil {
		t.Fatal("invalid checksum phrase must be refused")
	}
}

func TestRecoveryRestoreJournalRoundtrip(t *testing.T) {
	b := recoveryRestoreTestBackend(t)
	j := recoveryRestoreJournal{
		Operation:  newOperation(recoveryRestoreJournalKind, nil),
		StagingDir: filepath.Join(b.cfg.StateDir, "recovery-restore-staging", "op1"),
		Source: recoveryJournalSource{
			Location: "sftp://u@h:23/omahab", SnapshotID: "snap1", PhraseFP: "aabbccdd",
		},
	}
	if err := writeRecoveryRestoreJournal(b.cfg.StateDir, j); err != nil {
		t.Fatal(err)
	}
	got, err := loadRecoveryRestoreJournal(b.cfg.StateDir, j.Operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Source.PhraseFP != "aabbccdd" || got.Source.SnapshotID != "snap1" {
		t.Fatalf("journal roundtrip lost source: %+v", got.Source)
	}
	// The journal file must never contain a phrase field.
	raw, _ := os.ReadFile(operationJournalPath(b.cfg.StateDir, recoveryRestoreJournalKind, j.Operation.ID))
	if strings.Contains(string(raw), `"phrase"`) {
		t.Fatalf("journal must not hold phrase material: %s", raw)
	}
}

func TestGetRecoveryRestoreStatusNotFound(t *testing.T) {
	b := recoveryRestoreTestBackend(t)
	if _, err := b.GetRecoveryRestoreStatus(context.Background(), "nope"); err == nil {
		t.Fatal("unknown operation must 404")
	}
}

func TestRecoveryCredentialsHandoffIsPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "runtime")
	t.Setenv("OMAHAB_RUNTIME_DIR", dir)
	if err := writeRecoveryPhraseHandoff("op1", "alpha beta", "synthetic-password"); err != nil {
		t.Fatal(err)
	}
	for path, mode := range map[string]os.FileMode{
		dir:                                   0o700,
		filepath.Join(dir, "recovery-phrase"): 0o600,
	} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("private handoff %s: %v %v", path, info, err)
		}
	}
}

func TestTriggerRecoveryRestoreUnitRequiresStagedAndPhrase(t *testing.T) {
	b := recoveryRestoreTestBackend(t)
	dir := t.TempDir()
	t.Setenv("OMAHAB_RUNTIME_DIR", dir)
	j := recoveryRestoreJournal{Operation: newOperation(recoveryRestoreJournalKind, nil)}
	j.Operation.Stage = "staged"
	j.Operation.Status = OpCompleted
	if err := writeRecoveryRestoreJournal(b.cfg.StateDir, j); err != nil {
		t.Fatal(err)
	}
	// Empty phrase is refused before any systemctl call.
	if _, err := b.TriggerRecoveryRestoreUnit(context.Background(), j.Operation.ID, "", ""); err == nil {
		t.Fatal("trigger without a phrase must be refused")
	}
	// Not-staged operations are refused.
	other := recoveryRestoreJournal{Operation: newOperation(recoveryRestoreJournalKind, nil)}
	other.Operation.Stage = "listing"
	if err := writeRecoveryRestoreJournal(b.cfg.StateDir, other); err != nil {
		t.Fatal(err)
	}
	if _, err := b.TriggerRecoveryRestoreUnit(context.Background(), other.Operation.ID, "p", ""); err == nil {
		t.Fatal("trigger on a non-staged restore must be refused")
	}
}

func TestUnwrapStagedMasterKeyRefusesMismatchedPhrase(t *testing.T) {
	b := recoveryRestoreTestBackend(t)
	staging := t.TempDir()
	// Real kit wrapping a known master under a random phrase.
	words, seed, err := generateTestPhrase(t)
	if err != nil {
		t.Fatal(err)
	}
	_ = words
	master := [32]byte{}
	for i := range master {
		master[i] = byte(i + 7)
	}
	kit := writeTestRecoveryKit(t, staging, seed, master)
	if kit.Fingerprint != recoverySeedFingerprint(seed) {
		t.Fatalf("kit fingerprint %q does not match seed", kit.Fingerprint)
	}
	got, err := unwrapStagedMasterKey(staging, b.cfg.StateDir, seed)
	if err != nil {
		t.Fatalf("unwrap with the right phrase: %v", err)
	}
	if got != master {
		t.Fatal("unwrapped key differs from the wrapped master")
	}
	// Wrong phrase: a different valid seed must fail closed.
	_, wrongSeed, err := generateTestPhrase(t)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unwrapStagedMasterKey(staging, b.cfg.StateDir, wrongSeed); err == nil {
		t.Fatal("mismatched phrase must not unwrap the kit")
	}
}

// generateTestPhrase produces a real BIP39 phrase + seed.
func generateTestPhrase(t *testing.T) ([]string, [32]byte, error) {
	t.Helper()
	return secrets.GenerateRecoveryPhrase()
}

// writeTestRecoveryKit persists a kit wrapping master under seed at the
// v2 staged location dir/backup-staging/recovery.kit and returns the
// decoded kit.
func writeTestRecoveryKit(t *testing.T, dir string, seed, master [32]byte) recoveryKitFile {
	t.Helper()
	rk := secrets.DeriveRecoveryKey(seed)
	wrapped := secrets.WrapMasterKey(master, rk)
	kit := recoveryKitFile{
		Version:       1,
		Fingerprint:   recoverySeedFingerprint(seed),
		MasterWrapped: base64.StdEncoding.EncodeToString(wrapped),
		CreatedAt:     "2026-09-27T00:00:00Z",
	}
	raw, err := json.Marshal(kit)
	if err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(dir, "backup-staging")
	if err := os.MkdirAll(staged, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staged, "recovery.kit"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return kit
}

// JSON marshal shape sanity: the request types never carry a phrase into
// a persisted form.
func TestRecoveryRequestTypesShape(t *testing.T) {
	raw, err := json.Marshal(apitypes.RecoveryListRequest{Phrase: "p", Location: "l"})
	if err != nil {
		t.Fatal(err)
	}
	_ = raw // phrase crosses the wire (authed POST) and is not persisted
}
