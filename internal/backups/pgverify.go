package backups

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// StagingReport is the isolated validation outcome for one extracted tree.
type StagingReport struct {
	// Manifest is the validated v2 manifest read from the fixed staging
	// path. Present only when the tree carries one.
	Manifest      *SnapshotManifest `json:"-"`
	SQLiteChecked int
	SQLiteOK      int
	PGChecked     int
	PGOK          int
	Files         int64
	Bytes         int64
}

// ValidateStaging checks an extracted tree without touching live apps:
// v2 manifest validation with per-artifact sha256 verification,
// staging-escape refusal, SQLite integrity and disposable-PostgreSQL
// dump restores. It never executes commands embedded in an imported
// manifest and never invokes live app hooks. A tree without a v2
// manifest is an error: scope boundaries and artifact hashes are the
// restore contract, not a directory listing guess.
func ValidateStaging(ctx context.Context, target string) (StagingReport, error) {
	var rep StagingReport
	files, bytes, err := walkStats(target)
	if err != nil {
		return rep, err
	}
	if files == 0 {
		return rep, fmt.Errorf("%w: staged tree produced zero files", ErrInvalid)
	}
	rep.Files, rep.Bytes = files, bytes
	manifest, err := ReadStagedManifest(target)
	if err != nil {
		return rep, err
	}
	rep.Manifest = &manifest
	if err := checkStagingEscapes(target); err != nil {
		return rep, err
	}
	// Every artifact the manifest records must exist with the exact
	// recorded content hash: restic's integrity proof covers repository
	// data, this covers capture-time staging corruption and drift.
	if err := verifyManifestArtifacts(target, manifest); err != nil {
		return rep, err
	}
	checked, ok, err := checkSQLiteIntegrity(target)
	if err != nil {
		return rep, err
	}
	rep.SQLiteChecked, rep.SQLiteOK = checked, ok
	dumps := findPGDumps(target)
	pgChecked, pgOK, _, err := checkPGDumps(ctx, dumps)
	if err != nil {
		return rep, fmt.Errorf("disposable pg validation: %w", err)
	}
	rep.PGChecked, rep.PGOK = pgChecked, pgOK
	return rep, nil
}

// stagingDirCandidates lists where a restored tree can hold the manifest:
// every capture used the fixed backup-staging path, so the manifest
// lives at <target>/<that absolute path>/manifest.json. The staging root
// itself (target == staging dir) is also accepted for in-place checks.
func stagingDirCandidates(target string) []string {
	return []string{
		filepath.Join(target, backupStagingDir, manifestFileName),
		filepath.Join(target, manifestFileName),
	}
}

// ReadStagedManifest reads and validates the v2 manifest from a restored
// or staged tree. Exactly one manifest is expected; its absence is an
// error naming the v2 contract.
func ReadStagedManifest(target string) (SnapshotManifest, error) {
	var (
		found string
		m     SnapshotManifest
	)
	for _, p := range stagingDirCandidates(target) {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		if found != "" {
			return SnapshotManifest{}, fmt.Errorf("%w: staged tree holds more than one manifest (%s, %s)", ErrInvalid, found, p)
		}
		found = p
	}
	if found == "" {
		return SnapshotManifest{}, fmt.Errorf("%w: snapshot has no v2 manifest at %s (pre-v2 snapshots are not restoreable; use manual recovery)",
			ErrInvalid, backupStagingDirPath+"/manifest.json")
	}
	m, err := ReadManifest(filepath.Dir(found))
	if err != nil {
		return SnapshotManifest{}, fmt.Errorf("manifest: %w", err)
	}
	return m, nil
}

// verifyManifestArtifacts checks every recorded artifact hash against
// the staged content. Artifact paths are relative to the manifest's
// directory and are refused when they escape the staging tree. Database
// dumps get the same treatment through verifyStagedManifestFile.
func verifyManifestArtifacts(target string, m SnapshotManifest) error {
	for _, scope := range m.Scopes {
		for _, a := range scope.Artifacts {
			if err := verifyStagedManifestFile(target, scope.Name, "artifact", a.File, a.SHA256); err != nil {
				return err
			}
		}
		for _, d := range scope.Databases {
			if err := verifyStagedManifestFile(target, scope.Name, "database", d.File, d.SHA256); err != nil {
				return err
			}
		}
	}
	return nil
}

// verifyStagedManifestFile is the scope-aware core of the hash check;
func verifyStagedManifestFile(target, scope, kind, file, sha string) error {
	base := filepath.Join(target, backupStagingDir)
	path := filepath.Join(base, filepath.FromSlash(file))
	if !strings.HasPrefix(filepath.Clean(path), filepath.Clean(base)+string(filepath.Separator)) &&
		filepath.Clean(path) != filepath.Clean(base) {
		return fmt.Errorf("%w: scope %s %s %q escapes staging", ErrInvalid, scope, kind, file)
	}
	got, err := SHA256File(path)
	if err != nil {
		return fmt.Errorf("%w: scope %s %s %s: %v", ErrInvalid, scope, kind, file, err)
	}
	if !strings.EqualFold(got, sha) {
		return fmt.Errorf("%w: scope %s %s %s sha256 mismatch: staged %s, manifest %s",
			ErrInvalid, scope, kind, file, got, sha)
	}
	return nil
}

// checkStagingEscapes refuses path traversal and symlinks escaping staging.
// Restores must never follow links out of the isolated target.
func checkStagingEscapes(target string) error {
	root := filepath.Clean(target)
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			dst, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if !filepath.IsAbs(dst) && !strings.HasPrefix(filepath.Clean(filepath.Join(filepath.Dir(path), dst)), root+string(filepath.Separator)) {
				return fmt.Errorf("%w: symlink %q escapes staging", ErrInvalid, path)
			}
			if filepath.IsAbs(dst) && !strings.HasPrefix(filepath.Clean(dst), root+string(filepath.Separator)) {
				return fmt.Errorf("%w: symlink %q escapes staging", ErrInvalid, path)
			}
		}
		return nil
	})
}

func isSQLiteFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	hdr := make([]byte, 16)
	if _, err := io.ReadFull(f, hdr); err != nil {
		return false
	}
	return string(hdr) == "SQLite format 3\x00"
}

// checkSQLiteIntegrity runs integrity_check on every restored SQLite file.
// WAL sidecars are validated with their main database, not alone.
func checkSQLiteIntegrity(target string) (checked, ok int, err error) {
	dbs := map[string]bool{}
	_ = filepath.Walk(target, func(path string, info os.FileInfo, werr error) error {
		if werr != nil || info.IsDir() {
			return nil
		}
		name := info.Name()
		if name == "control.db" || strings.HasSuffix(name, ".db") {
			if isSQLiteFile(path) {
				dbs[path] = true
			}
		}
		return nil
	})
	for path := range dbs {
		checked++
		if err := sqliteIntegrityCheck(path); err == nil {
			ok++
		} else {
			return checked, ok, err
		}
	}
	return checked, ok, nil
}

func sqliteIntegrityCheck(path string) error {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return err
	}
	defer db.Close()
	var result string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("integrity_check: %s", result)
	}
	return nil
}

func findPGDumps(target string) []string {
	var out []string
	_ = filepath.Walk(target, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if strings.HasSuffix(info.Name(), ".pgdump") {
			out = append(out, path)
		}
		return nil
	})
	return out
}

func pgBinariesPresent() (initdb, pgCtl, pgRestore string, ok bool) {
	initdb, err1 := exec.LookPath("initdb")
	pgCtl, err2 := exec.LookPath("pg_ctl")
	pgRestore, err3 := exec.LookPath("pg_restore")
	return initdb, pgCtl, pgRestore, err1 == nil && err2 == nil && err3 == nil
}

// checkPGDumps restores every staged dump into a disposable validation
// database inside an isolated temporary cluster on a Unix socket with no TCP
// listener, including the installed app extensions. A backup's SQL is never
// loaded into the running production cluster merely to verify it.
func checkPGDumps(ctx context.Context, dumps []string) (checked, ok int, skipped string, err error) {
	if len(dumps) == 0 {
		return 0, 0, "", nil
	}
	initdb, pgCtl, pgRestore, okBins := pgBinariesPresent()
	if !okBins {
		return 0, 0, "postgresql binaries not available", nil
	}
	tmp, err := os.MkdirTemp("", "omahab-pgverify-*")
	if err != nil {
		return 0, 0, "", err
	}
	defer os.RemoveAll(tmp)
	dataDir := filepath.Join(tmp, "data")
	if out, err := exec.CommandContext(ctx, initdb, "-D", dataDir, "-E", "UTF8").CombinedOutput(); err != nil {
		return 0, 0, "", fmt.Errorf("initdb: %v: %s", err, strings.TrimSpace(string(out)))
	}
	sockDir := filepath.Join(tmp, "sock")
	if err := os.MkdirAll(sockDir, 0o700); err != nil {
		return 0, 0, "", err
	}
	if out, err := exec.CommandContext(ctx, pgCtl, "-D", dataDir, "-o", "-k "+sockDir+" -c listen_addresses=''", "-w", "start").CombinedOutput(); err != nil {
		return 0, 0, "", fmt.Errorf("pg_ctl start: %v: %s", err, strings.TrimSpace(string(out)))
	}
	defer func() {
		_, _ = exec.CommandContext(context.WithoutCancel(ctx), pgCtl, "-D", dataDir, "-m", "immediate", "stop").CombinedOutput()
	}()
	for _, dump := range dumps {
		checked++
		dbName := "verify" + fmt.Sprint(checked)
		if out, err := exec.CommandContext(ctx, "createdb", "-h", sockDir, dbName).CombinedOutput(); err != nil {
			return checked, ok, "", fmt.Errorf("createdb: %v: %s", err, strings.TrimSpace(string(out)))
		}
		if out, err := exec.CommandContext(ctx, pgRestore, "-h", sockDir, "-d", dbName, "--exit-on-error", dump).CombinedOutput(); err != nil {
			return checked, ok, "", fmt.Errorf("pg_restore %s: %v: %s", dump, err, strings.TrimSpace(string(out)))
		}
		ok++
	}
	return checked, ok, "", nil
}
