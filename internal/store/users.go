package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

type User struct {
	ID           int64  `json:"id"`
	Username     string `json:"username"`
	PasswordHash string `json:"-"`
	IsAdmin      bool   `json:"is_admin"`
	CreatedAt    int64  `json:"created_at"`
}

const userCols = "id, username, password_hash, is_admin, created_at"

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	u := &User{}
	err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.IsAdmin, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return u, err
}

func (s *Store) CreateUser(ctx context.Context, username, passwordHash string, isAdmin bool) (*User, error) {
	u := &User{Username: username, PasswordHash: passwordHash, IsAdmin: isAdmin, CreatedAt: nowMs()}
	res, err := s.db.ExecContext(ctx,
		"INSERT INTO users (username, password_hash, is_admin, created_at) VALUES (?, ?, ?, ?)",
		u.Username, u.PasswordHash, u.IsAdmin, u.CreatedAt)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return nil, ErrConflict
		}
		return nil, err
	}
	u.ID, err = res.LastInsertId()
	return u, err
}

func (s *Store) UserByName(ctx context.Context, username string) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx, "SELECT "+userCols+" FROM users WHERE username = ?", username))
}

func (s *Store) UserByID(ctx context.Context, id int64) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx, "SELECT "+userCols+" FROM users WHERE id = ?", id))
}

func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+userCols+" FROM users ORDER BY username")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	users := []User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, *u)
	}
	return users, rows.Err()
}

func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM users").Scan(&n)
	return n, err
}

func (s *Store) SetPassword(ctx context.Context, userID int64, passwordHash string) error {
	return one(s.db.ExecContext(ctx, "UPDATE users SET password_hash = ? WHERE id = ?", passwordHash, userID))
}

func (s *Store) SetAdmin(ctx context.Context, userID int64, isAdmin bool) error {
	return one(s.db.ExecContext(ctx, "UPDATE users SET is_admin = ? WHERE id = ?", isAdmin, userID))
}

// DeleteUser removes the user, their sessions, clips and file records, then
// deletes the file blobs from disk.
func (s *Store) DeleteUser(ctx context.Context, userID int64) error {
	ids, err := s.fileIDs(ctx, "SELECT id FROM files WHERE user_id = ?", userID)
	if err != nil {
		return err
	}
	if err := one(s.db.ExecContext(ctx, "DELETE FROM users WHERE id = ?", userID)); err != nil {
		return err
	}
	s.removeBlobs(ids)
	return nil
}
