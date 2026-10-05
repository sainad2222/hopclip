package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/sainad2222/hopclip/internal/config"
	"github.com/sainad2222/hopclip/internal/store"
)

type Server struct {
	cfg     *config.Config
	store   *store.Store
	hub     *hub
	limiter *failureLimiter
	static  fs.FS
	etags   map[string]string
}

func New(cfg *config.Config, st *store.Store, static fs.FS) (*Server, error) {
	s := &Server{
		cfg:     cfg,
		store:   st,
		hub:     newHub(),
		limiter: newFailureLimiter(cfg.LoginMaxAttempts, cfg.LoginWindow),
		static:  static,
		etags:   map[string]string{},
	}
	err := fs.WalkDir(static, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(static, path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		s.etags["/"+path] = `"` + hex.EncodeToString(sum[:8]) + `"`
		return nil
	})
	return s, err
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.Handle("/", s.staticHandler())

	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not found")
	})

	authed := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.requireAuth(h)) }
	admin := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.requireAuth(s.requireAdmin(h))) }

	authed("GET /api/me", s.handleMe)
	authed("POST /api/logout", s.handleLogout)
	authed("POST /api/password", s.handleChangePassword)
	authed("GET /api/sessions", s.handleListSessions)
	authed("PATCH /api/sessions/{id}", s.handleRenameSession)
	authed("DELETE /api/sessions/{id}", s.handleDeleteSession)

	authed("GET /api/events", s.handleEvents)

	authed("GET /api/clips", s.handleListClips)
	authed("POST /api/clips", s.handleCreateClip)
	authed("DELETE /api/clips", s.handleClearClips)
	authed("DELETE /api/clips/{id}", s.handleDeleteClip)

	authed("GET /api/files", s.handleListFiles)
	authed("POST /api/files", s.handleUploadFile)
	authed("GET /api/files/{id}", s.handleDownloadFile)
	authed("DELETE /api/files/{id}", s.handleDeleteFile)

	admin("GET /api/admin/users", s.handleAdminListUsers)
	admin("POST /api/admin/users", s.handleAdminCreateUser)
	admin("PATCH /api/admin/users/{id}", s.handleAdminUpdateUser)
	admin("DELETE /api/admin/users/{id}", s.handleAdminDeleteUser)

	return s.logRequests(s.securityHeaders(mux))
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) staticHandler() http.Handler {
	files := http.FileServerFS(s.static)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		key := r.URL.Path
		if key == "/" {
			key = "/index.html"
		}
		// The embedded FS has no modtimes, so a content hash ETag lets
		// browsers revalidate instead of re-downloading on every visit.
		if etag, ok := s.etags[key]; ok {
			w.Header().Set("ETag", etag)
		}
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
}

// Run serves until ctx is cancelled, then shuts down gracefully.
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.cfg.ListenAddr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
		// No Read/WriteTimeout: event streams and large transfers are long-lived.
	}
	srv.RegisterOnShutdown(s.hub.closeAll)

	go s.janitor(ctx)

	errCh := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", s.cfg.ListenAddr)
		errCh <- srv.ListenAndServe()
	}()
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}
	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

func (s *Server) janitor(ctx context.Context) {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		s.sweep(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Server) sweep(ctx context.Context) {
	s.limiter.gc()
	if n, err := s.store.DeleteExpiredSessions(ctx); err != nil {
		slog.Error("sweep sessions", "err", err)
	} else if n > 0 {
		slog.Info("removed expired sessions", "count", n)
	}
	if s.cfg.FileRetention > 0 {
		removed, err := s.store.DeleteFilesOlderThan(ctx, s.cfg.FileRetention)
		if err != nil {
			slog.Error("sweep files", "err", err)
		}
		for id, uid := range removed {
			s.hub.publish(uid, event{Type: "file_deleted", Data: map[string]string{"id": id}})
		}
		if len(removed) > 0 {
			slog.Info("removed expired files", "count", len(removed))
		}
	}
	if s.cfg.ClipRetention > 0 {
		n, err := s.store.DeleteClipsOlderThan(ctx, s.cfg.ClipRetention)
		if err != nil {
			slog.Error("sweep clips", "err", err)
		} else if n > 0 {
			slog.Info("removed expired clips", "count", n)
			s.hub.publishAll(event{Type: "resync"})
		}
	}
}
