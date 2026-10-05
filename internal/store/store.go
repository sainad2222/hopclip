package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("already exists")
)

type Store struct {
	db      *sql.DB
	blobDir string
	tmpDir  string
}

func Open(dataDir string) (*Store, error) {
	s := &Store{
		blobDir: filepath.Join(dataDir, "files"),
		tmpDir:  filepath.Join(dataDir, "tmp"),
	}
	for _, d := range []string{dataDir, s.blobDir, s.tmpDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	// Leftovers from uploads interrupted by a crash or restart.
	if entries, err := os.ReadDir(s.tmpDir); err == nil {
		for _, e := range entries {
			_ = os.Remove(filepath.Join(s.tmpDir, e.Name()))
		}
	}

	dsn := "file:" + filepath.Join(dataDir, "hopclip.db") +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	s.db = db
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

var migrations = []string{
	`
CREATE TABLE users (
	id            INTEGER PRIMARY KEY,
	username      TEXT    NOT NULL UNIQUE COLLATE NOCASE,
	password_hash TEXT    NOT NULL,
	is_admin      INTEGER NOT NULL DEFAULT 0,
	created_at    INTEGER NOT NULL
);
CREATE TABLE sessions (
	id          TEXT    PRIMARY KEY,
	token_hash  BLOB    NOT NULL UNIQUE,
	user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	csrf_token  TEXT    NOT NULL,
	device_name TEXT    NOT NULL,
	user_agent  TEXT    NOT NULL,
	ip          TEXT    NOT NULL,
	created_at  INTEGER NOT NULL,
	last_seen   INTEGER NOT NULL,
	expires_at  INTEGER NOT NULL
);
CREATE INDEX sessions_user ON sessions(user_id);
CREATE TABLE clips (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	content     TEXT    NOT NULL,
	device_name TEXT    NOT NULL,
	session_id  TEXT    NOT NULL,
	created_at  INTEGER NOT NULL
);
CREATE INDEX clips_user ON clips(user_id, id DESC);
CREATE TABLE files (
	id           TEXT    PRIMARY KEY,
	user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	name         TEXT    NOT NULL,
	size         INTEGER NOT NULL,
	content_type TEXT    NOT NULL,
	sniffed_type TEXT    NOT NULL,
	device_name  TEXT    NOT NULL,
	session_id   TEXT    NOT NULL,
	created_at   INTEGER NOT NULL
);
CREATE INDEX files_user ON files(user_id, created_at DESC);
`,
}

func (s *Store) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	for i := version; i < len(migrations); i++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func RandomHex(nbytes int) string {
	b := make([]byte, nbytes)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func nowMs() int64 { return time.Now().UnixMilli() }

func one(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
