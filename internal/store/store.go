// Package store persists clipboard history and snippets in SQLite.
//
// One *Store owns a single database connection (database/sql is limited to
// one open connection on purpose): the daemon and the TUI are separate
// processes that share the file through WAL mode and a busy timeout, so
// concurrent writers never lose data.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ErrInvalid is returned (wrapped) when user input fails validation.
var ErrInvalid = errors.New("invalid input")

// Store is a handle on the suchi database.
type Store struct {
	db *sql.DB
}

// migrations are applied in order inside one exclusive transaction; the
// applied versions are recorded in the schema_migrations table. Never edit an
// existing entry, only append. Every statement is idempotent, so a database
// left half-migrated by an older suchi heals itself.
var migrations = []string{
	// 1: text history and snippets
	`CREATE TABLE IF NOT EXISTS clips (
		id        INTEGER PRIMARY KEY,
		text      TEXT    NOT NULL UNIQUE,
		pinned    INTEGER NOT NULL DEFAULT 0,
		copied_at INTEGER NOT NULL,
		used_at   INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX IF NOT EXISTS clips_recent ON clips (pinned, copied_at);
	CREATE TABLE IF NOT EXISTS snippets (
		id         INTEGER PRIMARY KEY,
		name       TEXT    NOT NULL,
		body       TEXT    NOT NULL,
		kind       TEXT    NOT NULL DEFAULT 'text' CHECK (kind IN ('text','command')),
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	);
	CREATE INDEX IF NOT EXISTS snippets_name ON snippets (name COLLATE NOCASE);`,

	// 2: media. clips gains a kind (text / image / file), the image bytes and
	// a content hash; uniqueness moves to partial indexes (text and file paths
	// are unique by text, images by hash). SQLite cannot drop a column-level
	// UNIQUE, so the table is rebuilt.
	`CREATE TABLE clips_v2 (
		id        INTEGER PRIMARY KEY,
		kind      TEXT    NOT NULL DEFAULT 'text' CHECK (kind IN ('text','image','file')),
		text      TEXT    NOT NULL,
		mime      TEXT    NOT NULL DEFAULT '',
		data      BLOB,
		size      INTEGER NOT NULL DEFAULT 0,
		hash      TEXT    NOT NULL DEFAULT '',
		pinned    INTEGER NOT NULL DEFAULT 0,
		copied_at INTEGER NOT NULL,
		used_at   INTEGER NOT NULL DEFAULT 0
	);
	INSERT INTO clips_v2 (id, text, size, pinned, copied_at, used_at)
		SELECT id, text, length(CAST(text AS BLOB)), pinned, copied_at, used_at FROM clips;
	DROP TABLE clips;
	ALTER TABLE clips_v2 RENAME TO clips;
	CREATE INDEX clips_recent ON clips (pinned, copied_at);
	CREATE UNIQUE INDEX clips_uq_text ON clips (kind, text) WHERE kind <> 'image';
	CREATE UNIQUE INDEX clips_uq_hash ON clips (hash) WHERE kind = 'image';`,

	// 3: texts suchi itself just put on the clipboard (a chosen snippet) that
	// the daemon must not record as history.
	`CREATE TABLE IF NOT EXISTS ignored (
		hash       TEXT    PRIMARY KEY,
		expires_at INTEGER NOT NULL
	);`,
}

// Open opens (creating if needed) the database at path. The file is created
// 0600 inside a 0700 directory because clipboard history routinely holds
// passwords and tokens.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open database file: %w", err)
	}
	_ = f.Close()

	db, err := sql.Open(driverName, path)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Pragmas are per-connection; there is exactly one connection.
	for _, p := range []string{
		"PRAGMA busy_timeout = 5000",
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = NORMAL",
	} {
		if _, err := db.ExecContext(ctx, p); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("%s: %w", p, err)
		}
	}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// migrate brings the schema up to date. BEGIN IMMEDIATE takes the write lock
// before the version is read, so several suchi processes opening a fresh
// database at once (daemon, UI, systemd unit) queue up on the busy timeout
// instead of all trying to run the same migration.
func (s *Store) migrate(ctx context.Context) (err error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("lock database for migration: %w", err)
	}
	defer func() {
		if err != nil {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	if _, err = conn.ExecContext(ctx,
		"CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY)"); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	var v int
	if err = conn.QueryRowContext(ctx, "SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&v); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if v > len(migrations) {
		return fmt.Errorf("database schema v%d is newer than this suchi (v%d); upgrade suchi", v, len(migrations))
	}
	for i := v; i < len(migrations); i++ {
		if _, err = conn.ExecContext(ctx, migrations[i]); err != nil {
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err = conn.ExecContext(ctx, "INSERT INTO schema_migrations (version) VALUES (?)", i+1); err != nil {
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("commit migrations: %w", err)
	}
	return nil
}

// DataVersion changes whenever ANOTHER connection (e.g. the daemon) commits.
// The TUI polls it to refresh live.
func (s *Store) DataVersion(ctx context.Context) (int64, error) {
	var v int64
	err := s.db.QueryRowContext(ctx, "PRAGMA data_version").Scan(&v)
	return v, err
}

func nowMS() int64 { return time.Now().UnixMilli() }

func fromMS(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}
