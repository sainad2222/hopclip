package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"time"
)

type File struct {
	ID          string `json:"id"`
	UserID      int64  `json:"-"`
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	ContentType string `json:"content_type"`
	SniffedType string `json:"-"`
	DeviceName  string `json:"device_name"`
	SessionID   string `json:"session_id"`
	CreatedAt   int64  `json:"created_at"`
}

const fileCols = "id, user_id, name, size, content_type, sniffed_type, device_name, session_id, created_at"

func scanFile(row interface{ Scan(...any) error }) (*File, error) {
	f := &File{}
	err := row.Scan(&f.ID, &f.UserID, &f.Name, &f.Size, &f.ContentType, &f.SniffedType, &f.DeviceName, &f.SessionID, &f.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return f, err
}

func (s *Store) BlobPath(id string) string { return filepath.Join(s.blobDir, id) }

func (s *Store) TempFile() (*os.File, error) { return os.CreateTemp(s.tmpDir, "upload-*") }

// CommitFile moves a finished upload from tmpPath into blob storage and
// records it. f.ID and f.CreatedAt are filled in.
func (s *Store) CommitFile(ctx context.Context, tmpPath string, f *File) error {
	f.ID = RandomHex(16)
	f.CreatedAt = nowMs()
	if err := os.Rename(tmpPath, s.BlobPath(f.ID)); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO files ("+fileCols+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
		f.ID, f.UserID, f.Name, f.Size, f.ContentType, f.SniffedType, f.DeviceName, f.SessionID, f.CreatedAt)
	if err != nil {
		_ = os.Remove(s.BlobPath(f.ID))
	}
	return err
}

func (s *Store) ListFiles(ctx context.Context, userID int64) ([]File, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+fileCols+" FROM files WHERE user_id = ? ORDER BY created_at DESC", userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []File{}
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *f)
	}
	return out, rows.Err()
}

func (s *Store) GetFile(ctx context.Context, userID int64, id string) (*File, error) {
	return scanFile(s.db.QueryRowContext(ctx, "SELECT "+fileCols+" FROM files WHERE id = ? AND user_id = ?", id, userID))
}

func (s *Store) UsedBytes(ctx context.Context, userID int64) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, "SELECT COALESCE(SUM(size), 0) FROM files WHERE user_id = ?", userID).Scan(&n)
	return n, err
}

func (s *Store) DeleteFile(ctx context.Context, userID int64, id string) error {
	if err := one(s.db.ExecContext(ctx, "DELETE FROM files WHERE id = ? AND user_id = ?", id, userID)); err != nil {
		return err
	}
	s.removeBlobs([]string{id})
	return nil
}

// DeleteFilesOlderThan removes expired files and returns (fileID, userID)
// pairs so callers can notify connected devices.
func (s *Store) DeleteFilesOlderThan(ctx context.Context, age time.Duration) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, "DELETE FROM files WHERE created_at < ? RETURNING id, user_id", nowMs()-age.Milliseconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	var ids []string
	for rows.Next() {
		var id string
		var uid int64
		if err := rows.Scan(&id, &uid); err != nil {
			return nil, err
		}
		out[id] = uid
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	s.removeBlobs(ids)
	return out, nil
}

func (s *Store) fileIDs(ctx context.Context, query string, args ...any) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Store) removeBlobs(ids []string) {
	for _, id := range ids {
		_ = os.Remove(s.BlobPath(id))
	}
}
