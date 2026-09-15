package backups

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)
// StagingReport is the isolated validation outcome for one extracted tree.
type StagingReport struct {
	Manifests     []SnapshotManifest `json:"-"`
	ManifestFound bool
	Legacy        bool
	SQLiteChecked int
	SQLiteOK      int
	PGChecked     int
	PGOK          int
	Files         int64
	Bytes         int64
}

// ValidateStaging checks an extracted tree without touching live apps:
// manifest validation, staging-escape refusal, SQLite integrity and
// disposable-PostgreSQL dump restores. It never executes commands embedded
// in an imported manifest and never invokes live app hooks.
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
	for _, dir := range findManifests(target) {
		m, err := ReadManifest(dir)
		if err != nil {
			return rep, fmt.Errorf("manifest: %w", err)
		}
		rep.Manifests = append(rep.Manifests, m)
	}
	rep.ManifestFound = len(rep.Manifests) > 0
	rep.Legacy = len(rep.Manifests) == 0
	if err := checkStagingEscapes(target); err != nil {
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

// findManifests locates staged manifests under a restore target.
func findManifests(target string) []string {
	var out []string
	_ = filepath.Walk(target, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if info.Name() == manifestFileName {
			out = append(out, filepath.Dir(path))
		}
		return nil
	})
	return out
}

// checkStagingEscapes refuses path traversal and symlinks escaping staging.
// Restores must never follow links out of the isolated target.
func checkStagingEscapes(target string) error {
	root, err := filepath.EvalSymlinks(target)
	if err != nil {
		return err
	}
	var bad []string
	_ = filepath.Walk(target, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.Mode()&os.ModeSymlink == 0 {
			return nil
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			bad = append(bad, path+": dangling")
			return nil
		}
		if resolved != root && !strings.HasPrefix(resolved, root+string(os.PathSeparator)) {
			bad = append(bad, path+" -> "+resolved)
		}
		return nil
	})
	if len(bad) > 0 {
		return fmt.Errorf("%w: staging escapes: %s", ErrInvalid, strings.Join(bad, "; "))
	}
	return nil
}

func isSQLiteFile(path string) bool {
	lower := strings.ToLower(filepath.Base(path))
	return strings.HasSuffix(lower, ".db") || strings.HasSuffix(lower, ".sqlite") ||
		strings.HasSuffix(lower, ".sqlite3") || strings.HasSuffix(lower, ".db-wal")
}

// checkSQLiteIntegrity runs integrity_check on every restored SQLite file.
// WAL sidecars are validated with their main database, not alone.
func checkSQLiteIntegrity(target string) (checked, ok int, err error) {
	seen := map[string]bool{}
	err = filepath.Walk(target, func(path string, info os.FileInfo, werr error) error {
		if werr != nil || info.IsDir() || !isSQLiteFile(path) {
			return nil
		}
		main := path
		lower := strings.ToLower(path)
		if strings.HasSuffix(lower, "-wal") || strings.HasSuffix(lower, "-shm") || strings.HasSuffix(lower, "-journal") {
			return nil
		}
		if seen[main] {
			return nil
		}
		seen[main] = true
		checked++
		if cerr := sqliteIntegrityCheck(main); cerr != nil {
			return fmt.Errorf("sqlite %q: %w", strings.TrimPrefix(main, target), cerr)
		}
		ok++
		return nil
	})
	return checked, ok, err
}

func sqliteIntegrityCheck(path string) error {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=query_only(1)")
	if err != nil {
		return err
	}
	defer db.Close()
	rows, err := db.Query("PRAGMA integrity_check")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return err
		}
		if strings.TrimSpace(strings.ToLower(line)) != "ok" {
			return fmt.Errorf("integrity_check: %s", line)
		}
	}
	return rows.Err()
}

func findPGDumps(target string) []string {
	var out []string
	_ = filepath.Walk(target, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if strings.HasSuffix(strings.ToLower(info.Name()), ".pgdump") {
			out = append(out, path)
		}
		return nil
	})
	return out
}

func pgBinariesPresent() (initdb, pgCtl, pgRestore string, ok bool) {
	var err error
	if initdb, err = exec.LookPath("initdb"); err != nil {
		return "", "", "", false
	}
	if pgCtl, err = exec.LookPath("pg_ctl"); err != nil {
		return "", "", "", false
	}
	if pgRestore, err = exec.LookPath("pg_restore"); err != nil {
		return "", "", "", false
	}
	return initdb, pgCtl, pgRestore, true
}

// checkPGDumps restores every staged dump into a disposable validation
// database inside an isolated temporary cluster on a Unix socket with no TCP
// listener, including the installed app extensions. A backup's SQL is never
// loaded into the running production cluster merely to verify it.
func checkPGDumps(ctx context.Context, dumps []string) (checked, ok int, skipped string, err error) {
	if len(dumps) == 0 {
		return 0, 0, "", nil
	}
	initdb, pgCtl, pgRestore, present := pgBinariesPresent()
	if !present {
		return 0, 0, "", fmt.Errorf("pg dump validation requires initdb/pg_ctl/pg_restore")
	}
	cluster, err := os.MkdirTemp("", "omahab-pgverify-")
	if err != nil {
		return 0, 0, "", err
	}
	defer os.RemoveAll(cluster)
	sockDir := filepath.Join(cluster, "sockets")
	if err := os.MkdirAll(sockDir, 0o700); err != nil {
		return 0, 0, "", err
	}
	dataDir := filepath.Join(cluster, "data")
	if out, err := exec.CommandContext(ctx, initdb, "-D", dataDir, "-U", "postgres", "--auth=trust").CombinedOutput(); err != nil {
		return 0, 0, "", fmt.Errorf("initdb: %v: %s", err, strings.TrimSpace(string(out)))
	}
	startOpts := []string{"-D", dataDir, "-l", filepath.Join(cluster, "log"), "-o",
		"-c listen_addresses='' -k " + sockDir, "start"}
	if out, err := exec.CommandContext(ctx, pgCtl, startOpts...).CombinedOutput(); err != nil {
		return 0, 0, "", fmt.Errorf("pg_ctl start: %v: %s", err, strings.TrimSpace(string(out)))
	}
	defer func() {
		_ = exec.Command(pgCtl, "-D", dataDir, "-m", "fast", "stop").Run()
	}()
	psql := func(db, sqlText string) (string, error) {
		out, err := exec.CommandContext(ctx, "psql", "-h", sockDir, "-U", "postgres", "-d", db, "-c", sqlText).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	for i, dump := range dumps {
		name := fmt.Sprintf("verify%d", i)
		checked++
		if _, err := psql("postgres", "CREATE DATABASE "+name); err != nil {
			return checked, ok, "", fmt.Errorf("create validation db: %v", err)
		}
		// Installed app extensions first; unknown ones surface as restore
		// errors rather than silent skips.
		for _, ext := range []string{"vector", "pg_trgm", "unaccent", "pgcrypto", "uuid-ossp"} {
			_, _ = psql(name, "CREATE EXTENSION IF NOT EXISTS "+ext)
		}
		out, err := exec.CommandContext(ctx, pgRestore,
			"-h", sockDir, "-U", "postgres", "--clean", "--if-exists", "--exit-on-error",
			"--dbname="+name, dump).CombinedOutput()
		if err != nil {
			return checked, ok, "", fmt.Errorf("pg_restore %q: %v: %s", filepath.Base(dump), err, strings.TrimSpace(string(out)))
		}
		ok++
	}
	return checked, ok, "", nil
}
