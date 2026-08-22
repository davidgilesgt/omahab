// Package store provides the shared SQLite control-plane state store for
// Omahab controllers, together with shared identifiers and API-mappable
// errors.
//
// Durable control-plane state lives in SQLite (DESIGN.md §4.1–4.2), opened
// with the pure-Go modernc.org/sqlite driver so no C toolchain is required.
// Every pooled connection is configured through the DSN, which is the only
// way to guarantee that per-connection pragmas such as foreign_keys and
// busy_timeout apply no matter how many connections database/sql opens.
//
// Controllers own their tables. Each exposes Migrations() []Migration and
// passes them to (*Store).Migrate, which records applied migrations in
// schema_migrations and is safe to call with independent, overlapping
// migration sets.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver
)

// DriverName is the database/sql driver name registered by modernc.org/sqlite.
const DriverName = "sqlite"

const (
	// busyTimeoutMS is how long a connection waits for a lock before
	// failing with SQLITE_BUSY.
	busyTimeoutMS = 10_000

	// maxOpenConns bounds the connection pool. SQLite allows a single
	// writer; transactions begin IMMEDIATE (see the _txlock DSN parameter)
	// and the busy timeout serializes writers, while WAL mode lets readers
	// proceed concurrently.
	maxOpenConns = 4

	// openTimeout bounds the initial connectivity check in Open.
	openTimeout = 30 * time.Second
)

// Store is a SQLite-backed control-plane state store.
type Store struct {
	db *sql.DB
}

// Open opens the SQLite database at path, creating it (and its parent
// directory) if necessary, and verifies the database is usable.
//
// path may be a plain filesystem path, ":memory:" for a private in-memory
// database, or a SQLite "file:" URI. The database file is created with
// 0600 permissions so control-plane state is private by default; its
// WAL sidecar files inherit the same mode.
func Open(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, Validation("database path is required")
	}
	dsn, memory := buildDSN(path)

	if !memory && !strings.HasPrefix(path, "file:") {
		// Create the file privately before SQLite touches it, so the
		// database and its WAL sidecars never appear world-readable.
		if dir := filepath.Dir(path); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o750); err != nil {
				return nil, fmt.Errorf("store: create database directory %s: %w", dir, err)
			}
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, fmt.Errorf("store: create database file %s: %w", path, err)
		}
		_ = f.Close()
		if err := os.Chmod(path, 0o600); err != nil {
			return nil, fmt.Errorf("store: secure database file %s: %w", path, err)
		}
	}

	db, err := sql.Open(DriverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open sqlite: %w", err)
	}
	db.SetMaxOpenConns(maxOpenConns)
	db.SetMaxIdleConns(maxOpenConns)
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)
	if memory {
		// An in-memory database is one shared-cache database: the pool
		// must never hold a second connection to it.
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
	}

	// Force a real connection so pragma failures and unusable paths
	// surface here rather than at first use.
	ctx, cancel := context.WithTimeout(context.Background(), openTimeout)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: open database %s: %w", path, err)
	}
	return &Store{db: db}, nil
}

// DB returns the underlying *sql.DB for controllers that run their own
// queries. Prefer the Store helpers where they exist.
func (s *Store) DB() *sql.DB { return s.db }

// Queries returns a sqlc-generated Queries backed by the store's database.
// It is the typed entry point for instance and migration queries; callers
// should prefer it over hand-written database/sql strings.
// For transactional work, use QueriesWithTx.
// The integrator entry point is store.New(store.DB()) or s.Queries().
func (s *Store) Queries() *Queries {
	if s == nil || s.db == nil {
		return &Queries{db: nil}
	}
	return New(s.db)
}

// QueriesWithTx returns a Queries bound to tx, for use inside an explicit
// transaction (BeginTx / Commit / Rollback). The transaction's isolation is
// IMMEDIATE, matching the store's DSN _txlock.
func (s *Store) QueriesWithTx(tx *sql.Tx) *Queries { return New(tx) }

// Close closes the store. It is safe to call once; afterwards the store must
// not be used again.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// buildDSN returns the driver DSN for path and whether it addresses an
// in-memory database. Secure pragmas ride in the DSN query so they are
// applied to every pooled connection by the driver.
func buildDSN(path string) (dsn string, memory bool) {
	q := url.Values{}
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeoutMS))
	q.Add("_pragma", "foreign_keys(1)") // enforce FK constraints everywhere
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "trusted_schema(0)") // harden schema against untrusted SQL
	q.Add("_txlock", "immediate")         // writers serialize without BUSY snapshots

	if path == ":memory:" {
		// Unique name: separate in-memory stores stay isolated while the
		// shared cache keeps all statements of this store on one database.
		q.Set("mode", "memory")
		q.Set("cache", "shared")
		return "file:omahab-mem-" + NewID() + "?" + q.Encode(), true
	}
	if strings.HasPrefix(path, "file:") {
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		return path + sep + q.Encode(), false
	}
	return path + "?" + q.Encode(), false
}

// FormatTime formats t as the store's canonical timestamp representation: a
// UTC RFC 3339 string with nanosecond precision. Controllers persisting
// timestamps as text should use it so all tables share one format.
func FormatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// ParseTime parses a timestamp persisted with FormatTime.
func ParseTime(s string) (time.Time, error) { return time.Parse(time.RFC3339Nano, s) }
