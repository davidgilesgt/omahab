package backups

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

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
	pgChecked, pgOK, err := checkPGDumps(ctx, dumps)
	if err != nil {
		return rep, fmt.Errorf("disposable pg validation: %w", err)
	}
	rep.PGChecked, rep.PGOK = pgChecked, pgOK
	return rep, nil
}

// stagingManifestCandidates lists the exact locations a restored tree can
// hold the v2 manifest. Real captures snapshot the staging directory at
// its absolute path, so an extracted snapshot puts the manifest at
// <target>/<stateDir>/backup-staging/manifest.json; an in-place
// backup-staging tree puts it at <target>/backup-staging/manifest.json
// (target being the restore root) or <target>/manifest.json (target being
// the staging directory itself). No other depth is accepted: scope
// boundaries are the restore contract, not a directory-listing guess.
func stagingManifestCandidates(target, stateDir string) []string {
	candidates := []string{
		filepath.Join(target, backupStagingDir, manifestFileName),
	}
	if stateDir != "" {
		candidates = append(candidates, filepath.Join(target, filepath.Clean("/"+stateDir), backupStagingDir, manifestFileName))
	}
	return append(candidates, filepath.Join(target, manifestFileName))
}

// StagedManifestDirFor returns the absolute directory holding the v2
// manifest for one restored or staged tree, resolving the extracted
// absolute-capture layout with the stateDir the capture ran with
// (<target>/<stateDir>/backup-staging) plus the in-place backup-staging
// shapes. All artifact, custom-definition, and checkpoint paths resolve
// relative to the returned directory. Exactly one manifest must exist;
// ambiguity, absence, and directories escaping the target are refused.
func StagedManifestDirFor(target, stateDir string) (string, error) {
	clean := filepath.Clean(target)
	if !filepath.IsAbs(clean) {
		return "", fmt.Errorf("%w: staging target %q must be absolute", ErrInvalid, target)
	}
	var found string
	for _, p := range stagingManifestCandidates(clean, stateDir) {
		if fi, err := os.Lstat(p); err != nil || fi.IsDir() {
			continue
		}
		if found != "" {
			return "", fmt.Errorf("%w: staged tree holds more than one manifest (%s, %s)", ErrInvalid, found, p)
		}
		found = p
	}
	if found == "" {
		return "", fmt.Errorf("%w: snapshot has no v2 manifest under %s (pre-v2 snapshots are not restoreable; use manual recovery)",
			ErrInvalid, clean)
	}
	dir := filepath.Dir(found)
	if dir != clean && !strings.HasPrefix(dir, clean+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: manifest directory %s escapes staging %s", ErrInvalid, dir, clean)
	}
	return dir, nil
}

// StagedManifestDir is StagedManifestDirFor with the production state
// directory: every installed capture stages at
// /var/lib/omahab/backup-staging, so an extracted snapshot carries the
// manifest at <target>/var/lib/omahab/backup-staging/manifest.json.
func StagedManifestDir(target string) (string, error) {
	return StagedManifestDirFor(target, backupStagingDirPathParent)
}

// ReadStagedManifest reads and validates the v2 manifest from a restored
// or staged tree. Exactly one manifest is expected; its absence is an
// error naming the v2 contract.
func ReadStagedManifest(target string) (SnapshotManifest, error) {
	m, err := readStagedManifestFor(target, backupStagingDirPathParent)
	if err != nil {
		return SnapshotManifest{}, err
	}
	return m, nil
}

// readStagedManifestFor is ReadStagedManifest with an explicit capture
// stateDir for non-default state layouts (tests, relocated state).
func readStagedManifestFor(target, stateDir string) (SnapshotManifest, error) {
	dir, err := StagedManifestDirFor(target, stateDir)
	if err != nil {
		return SnapshotManifest{}, err
	}
	m, err := ReadManifest(dir)
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
	dir, err := StagedManifestDir(target)
	if err != nil {
		return err
	}
	for _, scope := range m.Scopes {
		for _, a := range scope.Artifacts {
			if err := verifyStagedManifestFile(dir, target, scope.Name, "artifact", a.File, a.SHA256); err != nil {
				return err
			}
		}
		for _, d := range scope.Databases {
			if err := verifyStagedManifestFile(dir, target, scope.Name, "database", d.File, d.SHA256); err != nil {
				return err
			}
		}
	}
	return nil
}

// verifyStagedManifestFile is the scope-aware core of the hash check;
// base is the manifest directory, target the tree root that bounds it.
func verifyStagedManifestFile(base, target, scope, kind, file, sha string) error {
	path := filepath.Join(base, filepath.FromSlash(file))
	if !strings.HasPrefix(filepath.Clean(path), filepath.Clean(base)+string(filepath.Separator)) &&
		filepath.Clean(path) != filepath.Clean(base) {
		return fmt.Errorf("%w: scope %s %s %q escapes staging", ErrInvalid, scope, kind, file)
	}
	if !strings.HasPrefix(filepath.Clean(path), filepath.Clean(target)+string(filepath.Separator)) &&
		filepath.Clean(path) != filepath.Clean(target) {
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

// pgBinariesPresent locates the PostgreSQL tooling: initdb, pg_ctl, and
// pg_restore. A single missing binary is a hard error — a validator that
// silently skips database proof is not a validator.
func pgBinariesPresent() (initdb, pgCtl, pgRestore string, err error) {
	var missing []string
	if initdb, err = exec.LookPath("initdb"); err != nil {
		missing = append(missing, "initdb")
	}
	if pgCtl, err = exec.LookPath("pg_ctl"); err != nil {
		missing = append(missing, "pg_ctl")
	}
	if pgRestore, err = exec.LookPath("pg_restore"); err != nil {
		missing = append(missing, "pg_restore")
	}
	if len(missing) > 0 {
		return "", "", "", fmt.Errorf("%w: postgresql binaries required for dump validation but not found: %s",
			ErrInvalid, strings.Join(missing, ", "))
	}
	return initdb, pgCtl, pgRestore, nil
}

// pgVerifyUser is the OS account the disposable cluster runs as: initdb
// refuses to run as root, and the postgres user already owns the
// production cluster data with a readable PostgreSQL installation.
const pgVerifyUser = "postgres"

// runPgCommand executes one cluster command with stdout/stderr captured in
// a log file instead of in-memory pipes: pg_ctl and its postgres child
// inherit the descriptors and hold them for the server's lifetime, which
// deadlocks CombinedOutput pipes.
func runPgCommand(ctx context.Context, logDir string, input io.Reader, name string, args ...string) (string, error) {
	logFile := filepath.Join(logDir, fmt.Sprintf("%s-%d.log", filepath.Base(name), time.Now().UnixNano()))
	f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	defer f.Close()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = input
	cmd.Env = append(os.Environ(), "HOME="+filepath.Dir(logDir), "LC_ALL=C")
	cmd.Dir = filepath.Dir(logDir)
	cmd.Stdout = f
	cmd.Stderr = f
	runErr := cmd.Run()
	out, _ := os.ReadFile(logFile)
	detail := strings.TrimSpace(string(out))
	if len(detail) > 2000 {
		detail = detail[:2000]
	}
	return detail, runErr
}

// dropToPostgres wraps args so the command runs as the postgres OS user:
// setpriv is in util-linux, present on every installed system.
func dropToPostgres(ctx context.Context, logDir string, input io.Reader, name string, args ...string) (string, error) {
	setpriv, err := exec.LookPath("setpriv")
	if err != nil {
		return "", fmt.Errorf("%w: setpriv (util-linux) required to run %s as %s: %v", ErrInvalid, name, pgVerifyUser, err)
	}
	full := append([]string{"--reuid", pgVerifyUser, "--regid", pgVerifyUser, "--init-groups", "--"}, name)
	full = append(full, args...)
	return runPgCommand(ctx, logDir, input, setpriv, full...)
}

// checkPGDumps restores dump-declared schemas and extensions in a disposable
// PostgreSQL cluster. The running cluster is never used for validation.
// Root opens each private dump before dropping to the postgres account.
func checkPGDumps(ctx context.Context, dumps []string) (checked, ok int, err error) {
	if len(dumps) == 0 {
		return 0, 0, nil
	}
	initdb, pgCtl, pgRestore, err := pgBinariesPresent()
	if err != nil {
		return 0, 0, err
	}
	account, err := user.Lookup(pgVerifyUser)
	if err != nil {
		return 0, 0, fmt.Errorf("%w: %s OS user required for disposable validation: %v", ErrInvalid, pgVerifyUser, err)
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil {
		return 0, 0, err
	}
	gid, err := strconv.Atoi(account.Gid)
	if err != nil {
		return 0, 0, err
	}
	tmp, err := os.MkdirTemp("", "omahab-pgverify-*")
	if err != nil {
		return 0, 0, err
	}
	defer os.RemoveAll(tmp)
	dataDir := filepath.Join(tmp, "data")
	sockDir := filepath.Join(tmp, "sock")
	logDir := filepath.Join(tmp, "logs")
	for _, dir := range []string{tmp, dataDir, sockDir, logDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return 0, 0, err
		}
		if err := os.Chown(dir, uid, gid); err != nil {
			return 0, 0, err
		}
	}
	if detail, err := dropToPostgres(ctx, logDir, nil, initdb, "-D", dataDir, "-E", "UTF8", "--no-locale", "--auth=trust"); err != nil {
		return 0, 0, fmt.Errorf("initdb: %v: %s", err, detail)
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_, _ = dropToPostgres(stopCtx, logDir, nil, pgCtl, "-D", dataDir, "-m", "immediate", "-w", "stop")
	}()
	serverOptions := "-k " + strconv.Quote(sockDir) + " -c listen_addresses=''"
	if preload := os.Getenv("OMAHAB_PG_PRELOAD_LIBRARIES"); preload != "" {
		serverOptions += " -c shared_preload_libraries=" + strconv.Quote(preload)
	}
	if detail, err := dropToPostgres(ctx, logDir, nil, pgCtl, "-D", dataDir,
		"-l", filepath.Join(tmp, "postgres.log"), "-o", serverOptions, "-w", "start"); err != nil {
		return 0, 0, fmt.Errorf("pg_ctl start: %v: %s", err, detail)
	}
	createdb := filepath.Join(filepath.Dir(pgRestore), "createdb")
	for _, dump := range dumps {
		checked++
		dbName := "verify" + strconv.Itoa(checked)
		if detail, err := dropToPostgres(ctx, logDir, nil, createdb, "-h", sockDir, dbName); err != nil {
			return checked, ok, fmt.Errorf("createdb: %v: %s", err, detail)
		}
		input, err := os.Open(dump)
		if err != nil {
			return checked, ok, err
		}
		detail, restoreErr := dropToPostgres(ctx, logDir, input, pgRestore, "-h", sockDir, "-d", dbName, "--no-owner", "--no-privileges", "--exit-on-error")
		_ = input.Close()
		if restoreErr != nil {
			return checked, ok, fmt.Errorf("pg_restore %s: %v: %s", dump, restoreErr, detail)
		}
		ok++
	}
	return checked, ok, nil
}
