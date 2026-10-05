package store

import (
	"context"
	"time"
)

type Clip struct {
	ID         int64  `json:"id"`
	Content    string `json:"content"`
	DeviceName string `json:"device_name"`
	SessionID  string `json:"session_id"`
	CreatedAt  int64  `json:"created_at"`
}

// AddClip stores a clip and trims the user's history to keep entries.
func (s *Store) AddClip(ctx context.Context, userID int64, content, deviceName, sessionID string, keep int) (*Clip, error) {
	c := &Clip{Content: content, DeviceName: deviceName, SessionID: sessionID, CreatedAt: nowMs()}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	err = tx.QueryRowContext(ctx,
		"INSERT INTO clips (user_id, content, device_name, session_id, created_at) VALUES (?, ?, ?, ?, ?) RETURNING id",
		userID, c.Content, c.DeviceName, c.SessionID, c.CreatedAt).Scan(&c.ID)
	if err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx,
		"DELETE FROM clips WHERE user_id = ? AND id <= (SELECT id FROM clips WHERE user_id = ? ORDER BY id DESC LIMIT 1 OFFSET ?)",
		userID, userID, keep)
	if err != nil {
		return nil, err
	}
	return c, tx.Commit()
}

func (s *Store) ListClips(ctx context.Context, userID int64, limit int) ([]Clip, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT id, content, device_name, session_id, created_at FROM clips WHERE user_id = ? ORDER BY id DESC LIMIT ?", userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Clip{}
	for rows.Next() {
		var c Clip
		if err := rows.Scan(&c.ID, &c.Content, &c.DeviceName, &c.SessionID, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) DeleteClip(ctx context.Context, userID, id int64) error {
	return one(s.db.ExecContext(ctx, "DELETE FROM clips WHERE id = ? AND user_id = ?", id, userID))
}

func (s *Store) ClearClips(ctx context.Context, userID int64) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM clips WHERE user_id = ?", userID)
	return err
}

func (s *Store) DeleteClipsOlderThan(ctx context.Context, age time.Duration) (int64, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM clips WHERE created_at < ?", nowMs()-age.Milliseconds())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
