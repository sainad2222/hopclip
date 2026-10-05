package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"time"
)

type Session struct {
	ID         string `json:"id"`
	UserID     int64  `json:"-"`
	CSRFToken  string `json:"-"`
	DeviceName string `json:"device_name"`
	UserAgent  string `json:"user_agent"`
	IP         string `json:"ip"`
	CreatedAt  int64  `json:"created_at"`
	LastSeen   int64  `json:"last_seen"`
	ExpiresAt  int64  `json:"expires_at"`
}

const sessionCols = "id, user_id, csrf_token, device_name, user_agent, ip, created_at, last_seen, expires_at"

func scanSession(row interface{ Scan(...any) error }) (*Session, error) {
	s := &Session{}
	err := row.Scan(&s.ID, &s.UserID, &s.CSRFToken, &s.DeviceName, &s.UserAgent, &s.IP, &s.CreatedAt, &s.LastSeen, &s.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return s, err
}

func hashToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// CreateSession returns the session and the raw bearer token. Only the
// token's SHA-256 is stored, so a leaked database cannot be replayed as cookies.
func (s *Store) CreateSession(ctx context.Context, userID int64, deviceName, userAgent, ip string, ttl time.Duration) (*Session, string, error) {
	token := RandomHex(32)
	now := nowMs()
	sess := &Session{
		ID:         RandomHex(12),
		UserID:     userID,
		CSRFToken:  RandomHex(24),
		DeviceName: deviceName,
		UserAgent:  userAgent,
		IP:         ip,
		CreatedAt:  now,
		LastSeen:   now,
		ExpiresAt:  now + ttl.Milliseconds(),
	}
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO sessions (id, token_hash, user_id, csrf_token, device_name, user_agent, ip, created_at, last_seen, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		sess.ID, hashToken(token), sess.UserID, sess.CSRFToken, sess.DeviceName, sess.UserAgent, sess.IP, sess.CreatedAt, sess.LastSeen, sess.ExpiresAt)
	if err != nil {
		return nil, "", err
	}
	return sess, token, nil
}

// SessionByToken returns the live session for token, or ErrNotFound if it is
// unknown or expired.
func (s *Store) SessionByToken(ctx context.Context, token string) (*Session, error) {
	sess, err := scanSession(s.db.QueryRowContext(ctx,
		"SELECT "+sessionCols+" FROM sessions WHERE token_hash = ? AND expires_at > ?", hashToken(token), nowMs()))
	return sess, err
}

func (s *Store) SessionActive(ctx context.Context, id string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, "SELECT 1 FROM sessions WHERE id = ? AND expires_at > ?", id, nowMs()).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// TouchSession records activity and slides the expiry forward.
func (s *Store) TouchSession(ctx context.Context, id, ip string, ttl time.Duration) error {
	now := nowMs()
	_, err := s.db.ExecContext(ctx, "UPDATE sessions SET last_seen = ?, ip = ?, expires_at = ? WHERE id = ?",
		now, ip, now+ttl.Milliseconds(), id)
	return err
}

func (s *Store) RenameSession(ctx context.Context, userID int64, id, name string) error {
	return one(s.db.ExecContext(ctx, "UPDATE sessions SET device_name = ? WHERE id = ? AND user_id = ?", name, id, userID))
}

func (s *Store) ListSessions(ctx context.Context, userID int64) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+sessionCols+" FROM sessions WHERE user_id = ? AND expires_at > ? ORDER BY last_seen DESC", userID, nowMs())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Session{}
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *sess)
	}
	return out, rows.Err()
}

func (s *Store) DeleteSession(ctx context.Context, userID int64, id string) error {
	return one(s.db.ExecContext(ctx, "DELETE FROM sessions WHERE id = ? AND user_id = ?", id, userID))
}

// DeleteOtherSessions removes every session of the user except keepID and
// returns the IDs it removed.
func (s *Store) DeleteOtherSessions(ctx context.Context, userID int64, keepID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "DELETE FROM sessions WHERE user_id = ? AND id != ? RETURNING id", userID, keepID)
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

func (s *Store) DeleteExpiredSessions(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM sessions WHERE expires_at <= ?", nowMs())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
