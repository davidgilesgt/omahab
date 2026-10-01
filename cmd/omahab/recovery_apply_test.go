package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omahab/omahab/internal/controlplane"
	"github.com/omahab/omahab/internal/secrets"
)

// The recovery CLI must never let a malformed one-time tmpfs handoff
// linger: the panel trigger writes it exactly once, and a parse failure
// that kept the file would poison every retry with the same garbage.
func TestReadPhraseHandoffCLIMalformedIsDeleted(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OMAHAB_RUNTIME_DIR", dir)
	path := filepath.Join(dir, "recovery-phrase")
	if err := os.WriteFile(path, []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	phrase, opID, pw, err := readPhraseHandoffCLI()
	if err == nil {
		t.Fatal("malformed handoff must fail")
	}
	if phrase != "" || opID != "" || pw != "" {
		t.Fatalf("malformed handoff must not yield values: %q %q %q", phrase, opID, pw)
	}
	if _, serr := os.Stat(path); !os.IsNotExist(serr) {
		t.Fatalf("malformed handoff must be deleted, stat err = %v", serr)
	}
}

// A successful read consumes the secret handoff exactly once.
func TestReadPhraseHandoffCLISuccessIsConsumedOnce(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OMAHAB_RUNTIME_DIR", dir)
	path := filepath.Join(dir, "recovery-phrase")

	raw, err := json.Marshal(map[string]any{
		"operation_id": "op-1",
		"phrase":       "alpha bravo charlie delta echo",
		"password":     "generic-repo-password",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, _, err = readPhraseHandoffCLI()
	if err != nil {
		t.Fatal(err)
	}
	if _, serr := os.Stat(path); !os.IsNotExist(serr) {
		t.Fatalf("consumed handoff must be deleted, stat err = %v", serr)
	}

	if _, _, _, err := readPhraseHandoffCLI(); err == nil {
		t.Fatal("a consumed secret handoff must not be reusable")
	}
}

// The interactive apply must reuse the persisted operation of an
// interrupted run: the same command (repository, phrase, selection)
// resolves its original operation instead of staging a new one from the
// latest snapshot. This pins the consumer-visible interrupted-state
// boundary the CLI's resume path depends on: a staged-not-applied
// operation counts as unfinished, snapshot matching is enforced only
// when the caller pins one, and a finished restore is never resumed.
func TestRecoveryApplyResumesInterruptedOperation(t *testing.T) {
	dir := t.TempDir()
	writeJournal := func(id, status, stage, snap string) string {
		opDir := filepath.Join(dir, "recovery-restore-operations", id)
		if err := os.MkdirAll(opDir, 0o700); err != nil {
			t.Fatal(err)
		}
		journal := map[string]any{
			"operation": map[string]any{
				"id":         id,
				"kind":       "recovery-restore",
				"status":     status,
				"stage":      stage,
				"created_at": "2026-10-01T00:00:0" + id[len(id)-1:] + "Z",
				"updated_at": "2026-10-01T00:00:01Z",
			},
			"source": map[string]any{
				"location":           "/tmp/omahab-recovery-repo",
				"snapshot_id":        snap,
				"phrase_fingerprint": "abcdef0123456789",
			},
			"staging_dir": filepath.Join(dir, "recovery-restore-staging", "apply-"+snap),
		}
		raw, err := json.Marshal(journal)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(opDir, "journal.json"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
		return id
	}
	writeJournal("op-interrupted", "completed", "staged", "snap-original")
	writeJournal("op-finished", "completed", "completed", "snap-old")

	// The staged-not-applied operation is the one the CLI resumes, with
	// or without a pinned snapshot; the finished restore is skipped.
	op, snapshot, err := controlplane.FindOfflineRecoveryOperation(dir, "/tmp/omahab-recovery-repo", "", "abcdef0123456789", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if op.ID != "op-interrupted" || snapshot != "snap-original" {
		t.Fatalf("resume lookup = %q %q, want original operation and snapshot", op.ID, snapshot)
	}
	op, snapshot, err = controlplane.FindOfflineRecoveryOperation(dir, "/tmp/omahab-recovery-repo", "snap-original", strings.ToUpper("abcdef0123456789"), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if op.ID != "op-interrupted" || snapshot != "snap-original" {
		t.Fatalf("pinned lookup = %q %q, want original operation and snapshot", op.ID, snapshot)
	}
	// A different repository never resumes someone else's operation.
	op, _, err = controlplane.FindOfflineRecoveryOperation(dir, "/tmp/other-repo", "", "abcdef0123456789", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if op.ID != "" {
		t.Fatalf("foreign repository lookup = %q, want none", op.ID)
	}
}

func TestRecoveryApplyStageResumesWithoutRepositoryAccess(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OMAHAB_STATE_DIR", dir)
	t.Setenv("PATH", "")
	words, seed, err := secrets.GenerateRecoveryPhrase()
	if err != nil {
		t.Fatal(err)
	}
	repository := filepath.Join(dir, "unavailable-repository")
	journal := map[string]any{
		"operation": map[string]any{
			"id": "cli-resume", "kind": "recovery-restore",
			"status": "completed", "stage": "staged", "can_resume": true,
			"created_at": "2026-10-01T00:00:00Z",
		},
		"source": map[string]any{
			"location": repository, "snapshot_id": "snapshot-original",
			"phrase_fingerprint": phraseFingerprintCLI(seed),
		},
	}
	opDir := filepath.Join(dir, "recovery-restore-operations", "cli-resume")
	if err := os.MkdirAll(opDir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(journal)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(opDir, "journal.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	input, err := os.CreateTemp(dir, "phrase-")
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	if _, err := input.WriteString(strings.Join(words, " ")); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	oldStdin, oldStdout, oldJSON := os.Stdin, os.Stdout, flagJSON
	os.Stdin, os.Stdout, flagJSON = input, writer, true
	t.Cleanup(func() { os.Stdin, os.Stdout, flagJSON = oldStdin, oldStdout, oldJSON })
	cmd := newRecoveryApplyCmd()
	if err := cmd.ParseFlags([]string{"--repository", repository, "--stage"}); err != nil {
		t.Fatal(err)
	}
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("staged resume required repository access: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]string
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatal(err)
	}
	if result["operation"] != "cli-resume" || result["snapshot"] != "snapshot-original" || result["stage"] != "staged" {
		t.Fatalf("resume selected a different operation, snapshot, or stage: %v", result)
	}
}
