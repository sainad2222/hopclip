package server

import (
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"

	"github.com/sainad2222/hopclip/internal/auth"
	"github.com/sainad2222/hopclip/internal/store"
)

var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,31}$`)

func ValidateUsername(name string) error {
	if !usernamePattern.MatchString(name) {
		return errors.New("username must be 1-32 characters: letters, digits, '.', '_' or '-'")
	}
	return nil
}

type adminUserView struct {
	store.User
	UsedBytes int64 `json:"used_bytes"`
}

func (s *Server) handleAdminListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.store.ListUsers(r.Context())
	if err != nil {
		fail(w, r, err)
		return
	}
	out := make([]adminUserView, len(users))
	for i, u := range users {
		used, err := s.store.UsedBytes(r.Context(), u.ID)
		if err != nil {
			fail(w, r, err)
			return
		}
		out[i] = adminUserView{User: u, UsedBytes: used}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleAdminCreateUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
		IsAdmin  bool   `json:"is_admin"`
	}
	if err := decodeJSON(w, r, smallBody, &req); err != nil {
		fail(w, r, err)
		return
	}
	if err := ValidateUsername(req.Username); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := auth.ValidatePassword(req.Password); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		fail(w, r, err)
		return
	}
	u, err := s.store.CreateUser(r.Context(), req.Username, hash, req.IsAdmin)
	if errors.Is(err, store.ErrConflict) {
		writeError(w, http.StatusConflict, "username already taken")
		return
	} else if err != nil {
		fail(w, r, err)
		return
	}
	slog.Info("user created", "by", authFrom(r).user.Username, "username", u.Username, "admin", u.IsAdmin)
	writeJSON(w, http.StatusCreated, u)
}

func (s *Server) targetUser(w http.ResponseWriter, r *http.Request) (*store.User, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, "user not found")
		return nil, false
	}
	u, err := s.store.UserByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "user not found")
		return nil, false
	} else if err != nil {
		fail(w, r, err)
		return nil, false
	}
	return u, true
}

// handleAdminUpdateUser changes another user's admin flag and/or resets their
// password. A reset signs the user out everywhere.
func (s *Server) handleAdminUpdateUser(w http.ResponseWriter, r *http.Request) {
	a := authFrom(r)
	u, ok := s.targetUser(w, r)
	if !ok {
		return
	}
	var req struct {
		Password *string `json:"password"`
		IsAdmin  *bool   `json:"is_admin"`
	}
	if err := decodeJSON(w, r, smallBody, &req); err != nil {
		fail(w, r, err)
		return
	}
	if u.ID == a.user.ID {
		writeError(w, http.StatusBadRequest, "use the account settings to change your own password; admins cannot demote themselves")
		return
	}
	if req.Password != nil {
		if err := auth.ValidatePassword(*req.Password); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		hash, err := auth.HashPassword(*req.Password)
		if err != nil {
			fail(w, r, err)
			return
		}
		if err := s.store.SetPassword(r.Context(), u.ID, hash); err != nil {
			fail(w, r, err)
			return
		}
		if _, err := s.store.DeleteOtherSessions(r.Context(), u.ID, ""); err != nil {
			fail(w, r, err)
			return
		}
		s.hub.kickUser(u.ID)
		slog.Info("password reset by admin", "by", a.user.Username, "username", u.Username)
	}
	if req.IsAdmin != nil {
		if err := s.store.SetAdmin(r.Context(), u.ID, *req.IsAdmin); err != nil {
			fail(w, r, err)
			return
		}
		u.IsAdmin = *req.IsAdmin
		slog.Info("admin flag changed", "by", a.user.Username, "username", u.Username, "admin", u.IsAdmin)
	}
	writeJSON(w, http.StatusOK, u)
}

func (s *Server) handleAdminDeleteUser(w http.ResponseWriter, r *http.Request) {
	a := authFrom(r)
	u, ok := s.targetUser(w, r)
	if !ok {
		return
	}
	if u.ID == a.user.ID {
		writeError(w, http.StatusBadRequest, "you cannot delete your own account")
		return
	}
	if err := s.store.DeleteUser(r.Context(), u.ID); err != nil {
		fail(w, r, err)
		return
	}
	s.hub.kickUser(u.ID)
	slog.Info("user deleted", "by", a.user.Username, "username", u.Username)
	w.WriteHeader(http.StatusNoContent)
}
