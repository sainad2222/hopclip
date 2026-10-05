package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/sainad2222/hopclip/internal/store"
)

const csrfHeader = "X-CSRF-Token"

// Re-validating every request is cheap, but writing last_seen on each one is
// not; activity is recorded at most this often per session.
const touchInterval = 5 * time.Minute

type authInfo struct {
	user    *store.User
	session *store.Session
}

type ctxKey struct{}

func authFrom(r *http.Request) *authInfo { return r.Context().Value(ctxKey{}).(*authInfo) }

func (s *Server) cookieName() string {
	if s.cfg.CookieSecure {
		// The __Host- prefix makes browsers refuse the cookie unless it is
		// Secure, host-only and Path=/, so subdomains cannot plant or shadow it.
		return "__Host-hopclip"
	}
	return "hopclip"
}

func (s *Server) setSessionCookie(w http.ResponseWriter, token string, maxAge time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.cookieName(),
		Value:    token,
		Path:     "/",
		MaxAge:   int(maxAge.Seconds()),
		HttpOnly: true,
		Secure:   s.cfg.CookieSecure,
		SameSite: http.SameSiteStrictMode,
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: s.cookieName(), Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.cfg.CookieSecure, SameSite: http.SameSiteStrictMode,
	})
}

func (s *Server) clientIP(r *http.Request) string {
	if h := s.cfg.ClientIPHeader; h != "" {
		v := r.Header.Get(h)
		if h == "X-Forwarded-For" {
			// The right-most entry is the one appended by our own proxy;
			// anything to its left is client-controlled.
			if vals := r.Header.Values(h); len(vals) > 0 {
				parts := strings.Split(vals[len(vals)-1], ",")
				v = parts[len(parts)-1]
			}
		}
		if addr, err := netip.ParseAddr(strings.TrimSpace(v)); err == nil {
			return addr.Unmap().String()
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// sameOriginRequest rejects requests that browsers mark as coming from
// another site. Non-browser clients do not send the header and are allowed.
func sameOriginRequest(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
		return true
	}
	return false
}

func isSafeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(s.cookieName())
		if err != nil || c.Value == "" {
			writeError(w, http.StatusUnauthorized, "not logged in")
			return
		}
		sess, err := s.store.SessionByToken(r.Context(), c.Value)
		if err == nil {
			var user *store.User
			if user, err = s.store.UserByID(r.Context(), sess.UserID); err == nil {
				s.serveAuthed(w, r, next, &authInfo{user: user, session: sess}, c.Value)
				return
			}
		}
		if errors.Is(err, store.ErrNotFound) {
			s.clearSessionCookie(w)
			writeError(w, http.StatusUnauthorized, "session expired")
			return
		}
		fail(w, r, err)
	})
}

func (s *Server) serveAuthed(w http.ResponseWriter, r *http.Request, next http.Handler, a *authInfo, token string) {
	if !isSafeMethod(r.Method) {
		got := r.Header.Get(csrfHeader)
		if !sameOriginRequest(r) || subtle.ConstantTimeCompare([]byte(got), []byte(a.session.CSRFToken)) != 1 {
			writeError(w, http.StatusForbidden, "CSRF check failed")
			return
		}
	}
	ip := s.clientIP(r)
	if time.Since(time.UnixMilli(a.session.LastSeen)) > touchInterval || ip != a.session.IP {
		if err := s.store.TouchSession(r.Context(), a.session.ID, ip, s.cfg.SessionTTL); err != nil {
			slog.Warn("touch session", "err", err)
		} else {
			s.setSessionCookie(w, token, s.cfg.SessionTTL)
		}
	}
	next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, a)))
}

func (s *Server) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !authFrom(r).user.IsAdmin {
			writeError(w, http.StatusForbidden, "admin only")
			return
		}
		next.ServeHTTP(w, r)
	})
}

const contentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data: blob:; " +
	"connect-src 'self'; font-src 'self'; manifest-src 'self'; object-src 'none'; base-uri 'none'; " +
	"form-action 'self'; frame-ancestors 'none'"

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
		if s.cfg.CookieSecure {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		slog.Info("request", "method", r.Method, "path", r.URL.Path, "status", rec.status,
			"bytes", rec.bytes, "dur", time.Since(start).Round(time.Millisecond), "ip", s.clientIP(r))
	})
}
