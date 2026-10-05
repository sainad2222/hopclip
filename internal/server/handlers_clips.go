package server

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"unicode/utf8"

	"github.com/sainad2222/hopclip/internal/store"
)

func (s *Server) handleListClips(w http.ResponseWriter, r *http.Request) {
	a := authFrom(r)
	limit := s.cfg.ClipHistoryLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "invalid limit")
			return
		}
		limit = min(n, limit)
	}
	clips, err := s.store.ListClips(r.Context(), a.user.ID, limit)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, clips)
}

func (s *Server) handleCreateClip(w http.ResponseWriter, r *http.Request) {
	a := authFrom(r)
	var req struct {
		Content string `json:"content"`
	}
	// JSON escaping can inflate text up to 6x (\uXXXX), so the body limit is
	// looser than the content limit checked below.
	if err := decodeJSON(w, r, int64(s.cfg.MaxClipBytes)*6+1024, &req); err != nil {
		fail(w, r, err)
		return
	}
	switch {
	case req.Content == "":
		writeError(w, http.StatusBadRequest, "clip is empty")
		return
	case len(req.Content) > s.cfg.MaxClipBytes:
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("clip exceeds %d KB limit", s.cfg.MaxClipBytes/1024))
		return
	case !utf8.ValidString(req.Content):
		writeError(w, http.StatusBadRequest, "clip must be valid UTF-8")
		return
	}
	clip, err := s.store.AddClip(r.Context(), a.user.ID, req.Content, a.session.DeviceName, a.session.ID, s.cfg.ClipHistoryLimit)
	if err != nil {
		fail(w, r, err)
		return
	}
	s.hub.publish(a.user.ID, event{Type: "clip", Data: clip, From: a.session.ID})
	writeJSON(w, http.StatusCreated, clip)
}

func (s *Server) handleDeleteClip(w http.ResponseWriter, r *http.Request) {
	a := authFrom(r)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, "clip not found")
		return
	}
	err = s.store.DeleteClip(r.Context(), a.user.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "clip not found")
		return
	} else if err != nil {
		fail(w, r, err)
		return
	}
	s.hub.publish(a.user.ID, event{Type: "clip_deleted", Data: map[string]int64{"id": id}, From: a.session.ID})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleClearClips(w http.ResponseWriter, r *http.Request) {
	a := authFrom(r)
	if err := s.store.ClearClips(r.Context(), a.user.ID); err != nil {
		fail(w, r, err)
		return
	}
	s.hub.publish(a.user.ID, event{Type: "clips_cleared", From: a.session.ID})
	w.WriteHeader(http.StatusNoContent)
}
