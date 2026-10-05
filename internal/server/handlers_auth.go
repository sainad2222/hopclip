package server

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strings"

	"github.com/sainad2222/hopclip/internal/auth"
	"github.com/sainad2222/hopclip/internal/store"
)

const (
	smallBody     = 16 << 10
	maxDeviceName = 64
)

type limits struct {
	MaxUploadBytes   int64 `json:"max_upload_bytes"`
	UserQuotaBytes   int64 `json:"user_quota_bytes"`
	MaxClipBytes     int   `json:"max_clip_bytes"`
	ClipHistoryLimit int   `json:"clip_history_limit"`
}

type meResponse struct {
	User      *store.User `json:"user"`
	SessionID string      `json:"session_id"`
	Device    string      `json:"device_name"`
	CSRFToken string      `json:"csrf_token"`
	Limits    limits      `json:"limits"`
	UsedBytes int64       `json:"used_bytes"`
}

func (s *Server) me(r *http.Request, u *store.User, sess *store.Session) (*meResponse, error) {
	used, err := s.store.UsedBytes(r.Context(), u.ID)
	if err != nil {
		return nil, err
	}
	return &meResponse{
		User:      u,
		SessionID: sess.ID,
		Device:    sess.DeviceName,
		CSRFToken: sess.CSRFToken,
		Limits: limits{
			MaxUploadBytes:   s.cfg.MaxUploadBytes,
			UserQuotaBytes:   s.cfg.UserQuotaBytes,
			MaxClipBytes:     s.cfg.MaxClipBytes,
			ClipHistoryLimit: s.cfg.ClipHistoryLimit,
		},
		UsedBytes: used,
	}, nil
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !sameOriginRequest(r) {
		writeError(w, http.StatusForbidden, "cross-site request rejected")
		return
	}
	var req struct {
		Username   string `json:"username"`
		Password   string `json:"password"`
		DeviceName string `json:"device_name"`
	}
	if err := decodeJSON(w, r, smallBody, &req); err != nil {
		fail(w, r, err)
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if req.Username == "" || req.Password == "" || len(req.Password) > auth.MaxPasswordLen {
		writeError(w, http.StatusBadRequest, "username and password are required")
		return
	}

	ip := s.clientIP(r)
	ipKey, userKey := "ip:"+ip, "user:"+strings.ToLower(req.Username)
	for _, k := range []string{ipKey, userKey} {
		if blocked, wait := s.limiter.blocked(k); blocked {
			w.Header().Set("Retry-After", fmt.Sprint(int(math.Ceil(wait.Seconds()))))
			writeError(w, http.StatusTooManyRequests, "too many failed attempts, try again later")
			return
		}
	}

	user, err := s.store.UserByName(r.Context(), req.Username)
	switch {
	case errors.Is(err, store.ErrNotFound):
		auth.BurnTime(req.Password)
	case err != nil:
		fail(w, r, err)
		return
	default:
		err = auth.VerifyPassword(user.PasswordHash, req.Password)
	}
	if err != nil {
		s.limiter.fail(ipKey)
		s.limiter.fail(userKey)
		slog.Warn("login failed", "username", req.Username, "ip", ip)
		writeError(w, http.StatusUnauthorized, "invalid username or password")
		return
	}
	s.limiter.reset(ipKey)
	s.limiter.reset(userKey)

	device := cleanLabel(req.DeviceName, maxDeviceName)
	if device == "" {
		device = guessDevice(r.UserAgent())
	}
	ua := cleanLabel(r.UserAgent(), 256)
	sess, token, err := s.store.CreateSession(r.Context(), user.ID, device, ua, ip, s.cfg.SessionTTL)
	if err != nil {
		fail(w, r, err)
		return
	}
	s.setSessionCookie(w, token, s.cfg.SessionTTL)
	slog.Info("login", "username", user.Username, "device", device, "ip", ip)

	resp, err := s.me(r, user, sess)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	a := authFrom(r)
	resp, err := s.me(r, a.user, a.session)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	a := authFrom(r)
	if err := s.store.DeleteSession(r.Context(), a.user.ID, a.session.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		fail(w, r, err)
		return
	}
	s.hub.kickSessions(a.user.ID, a.session.ID)
	s.clearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	a := authFrom(r)
	var req struct {
		Current string `json:"current_password"`
		New     string `json:"new_password"`
	}
	if err := decodeJSON(w, r, smallBody, &req); err != nil {
		fail(w, r, err)
		return
	}
	key := "user:" + strings.ToLower(a.user.Username)
	if blocked, _ := s.limiter.blocked(key); blocked {
		writeError(w, http.StatusTooManyRequests, "too many failed attempts, try again later")
		return
	}
	if len(req.Current) > auth.MaxPasswordLen || auth.VerifyPassword(a.user.PasswordHash, req.Current) != nil {
		s.limiter.fail(key)
		writeError(w, http.StatusForbidden, "current password is incorrect")
		return
	}
	if err := auth.ValidatePassword(req.New); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	hash, err := auth.HashPassword(req.New)
	if err != nil {
		fail(w, r, err)
		return
	}
	if err := s.store.SetPassword(r.Context(), a.user.ID, hash); err != nil {
		fail(w, r, err)
		return
	}
	revoked, err := s.store.DeleteOtherSessions(r.Context(), a.user.ID, a.session.ID)
	if err != nil {
		fail(w, r, err)
		return
	}
	s.hub.kickSessions(a.user.ID, revoked...)
	slog.Info("password changed", "username", a.user.Username, "revoked_sessions", len(revoked))
	writeJSON(w, http.StatusOK, map[string]int{"revoked_sessions": len(revoked)})
}

type sessionView struct {
	store.Session
	Current bool `json:"current"`
	Online  bool `json:"online"`
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	a := authFrom(r)
	sessions, err := s.store.ListSessions(r.Context(), a.user.ID)
	if err != nil {
		fail(w, r, err)
		return
	}
	online := s.hub.onlineSessions(a.user.ID)
	out := make([]sessionView, len(sessions))
	for i, sess := range sessions {
		out[i] = sessionView{Session: sess, Current: sess.ID == a.session.ID, Online: online[sess.ID]}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleRenameSession(w http.ResponseWriter, r *http.Request) {
	a := authFrom(r)
	var req struct {
		DeviceName string `json:"device_name"`
	}
	if err := decodeJSON(w, r, smallBody, &req); err != nil {
		fail(w, r, err)
		return
	}
	name := cleanLabel(req.DeviceName, maxDeviceName)
	if name == "" {
		writeError(w, http.StatusBadRequest, "device name is required")
		return
	}
	err := s.store.RenameSession(r.Context(), a.user.ID, r.PathValue("id"), name)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "session not found")
		return
	} else if err != nil {
		fail(w, r, err)
		return
	}
	s.hub.publish(a.user.ID, event{Type: "devices_changed", From: a.session.ID})
	writeJSON(w, http.StatusOK, map[string]string{"device_name": name})
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	a := authFrom(r)
	id := r.PathValue("id")
	err := s.store.DeleteSession(r.Context(), a.user.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "session not found")
		return
	} else if err != nil {
		fail(w, r, err)
		return
	}
	s.hub.kickSessions(a.user.ID, id)
	if id == a.session.ID {
		s.clearSessionCookie(w)
	} else {
		s.hub.publish(a.user.ID, event{Type: "devices_changed", From: a.session.ID})
	}
	w.WriteHeader(http.StatusNoContent)
}
