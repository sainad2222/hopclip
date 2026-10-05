package server

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"time"

	"github.com/sainad2222/hopclip/internal/store"
)

// Types that may be shown inline. Only raster images whose bytes were sniffed
// as such qualify: SVG and HTML can carry script and are always downloaded.
var previewableTypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/gif":  true,
	"image/webp": true,
	"image/bmp":  true,
}

type fileView struct {
	store.File
	Previewable bool `json:"previewable"`
}

func viewOf(f *store.File) fileView {
	return fileView{File: *f, Previewable: previewableTypes[f.SniffedType]}
}

func (s *Server) handleListFiles(w http.ResponseWriter, r *http.Request) {
	a := authFrom(r)
	files, err := s.store.ListFiles(r.Context(), a.user.ID)
	if err != nil {
		fail(w, r, err)
		return
	}
	out := make([]fileView, len(files))
	for i := range files {
		out[i] = viewOf(&files[i])
	}
	writeJSON(w, http.StatusOK, out)
}

func formatMB(n int64) string { return fmt.Sprintf("%d MB", n>>20) }

// handleUploadFile takes the raw file as the request body, with the filename
// in the "name" query parameter. Streaming the body straight to disk avoids
// multipart buffering and lets browsers report upload progress.
func (s *Server) handleUploadFile(w http.ResponseWriter, r *http.Request) {
	a := authFrom(r)
	name := sanitizeFilename(r.URL.Query().Get("name"))

	contentType := "application/octet-stream"
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err == nil && len(mt) <= 127 {
		contentType = mt
	}

	limit := s.cfg.MaxUploadBytes
	limitMsg := "file exceeds the " + formatMB(s.cfg.MaxUploadBytes) + " upload limit"
	if s.cfg.UserQuotaBytes > 0 {
		used, err := s.store.UsedBytes(r.Context(), a.user.ID)
		if err != nil {
			fail(w, r, err)
			return
		}
		if left := s.cfg.UserQuotaBytes - used; left < limit {
			limit = max(left, 0)
			limitMsg = "not enough storage left (" + formatMB(s.cfg.UserQuotaBytes) + " quota); delete some files first"
		}
	}
	if r.ContentLength > limit {
		writeError(w, http.StatusRequestEntityTooLarge, limitMsg)
		return
	}

	tmp, err := s.store.TempFile()
	if err != nil {
		fail(w, r, err)
		return
	}
	committed := false
	defer func() {
		if !committed {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()

	// Uploads can legitimately take a long time on slow links; give the body
	// read a generous but finite deadline.
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(6 * time.Hour))

	body := http.MaxBytesReader(w, r.Body, limit)
	head := make([]byte, 512)
	n, err := io.ReadFull(body, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		uploadFailed(w, r, err, limitMsg)
		return
	}
	head = head[:n]
	sniffed := http.DetectContentType(head)
	if mt, _, err := mime.ParseMediaType(sniffed); err == nil {
		sniffed = mt
	}
	if _, err := tmp.Write(head); err != nil {
		fail(w, r, err)
		return
	}
	rest, err := io.Copy(tmp, body)
	if err != nil {
		uploadFailed(w, r, err, limitMsg)
		return
	}
	if err := tmp.Sync(); err != nil {
		fail(w, r, err)
		return
	}
	if err := tmp.Close(); err != nil {
		fail(w, r, err)
		return
	}

	f := &store.File{
		UserID:      a.user.ID,
		Name:        name,
		Size:        int64(n) + rest,
		ContentType: contentType,
		SniffedType: sniffed,
		DeviceName:  a.session.DeviceName,
		SessionID:   a.session.ID,
	}
	if err := s.store.CommitFile(r.Context(), tmp.Name(), f); err != nil {
		fail(w, r, err)
		return
	}
	committed = true
	slog.Info("file uploaded", "user", a.user.Username, "size", f.Size, "device", f.DeviceName)
	view := viewOf(f)
	s.hub.publish(a.user.ID, event{Type: "file", Data: view, From: a.session.ID})
	writeJSON(w, http.StatusCreated, view)
}

func uploadFailed(w http.ResponseWriter, r *http.Request, err error, limitMsg string) {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		writeError(w, http.StatusRequestEntityTooLarge, limitMsg)
		return
	}
	slog.Warn("upload aborted", "err", err)
	writeError(w, http.StatusBadRequest, "upload interrupted")
}

func (s *Server) handleDownloadFile(w http.ResponseWriter, r *http.Request) {
	a := authFrom(r)
	f, err := s.store.GetFile(r.Context(), a.user.ID, r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "file not found")
		return
	} else if err != nil {
		fail(w, r, err)
		return
	}
	blob, err := os.Open(s.store.BlobPath(f.ID))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusNotFound, "file data missing")
			return
		}
		fail(w, r, err)
		return
	}
	defer blob.Close()

	h := w.Header()
	inline := r.URL.Query().Get("inline") == "1" && previewableTypes[f.SniffedType]
	disposition := "attachment"
	contentType := f.ContentType
	if inline {
		disposition = "inline"
		contentType = f.SniffedType
	}
	h.Set("Content-Type", contentType)
	h.Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": f.Name}))
	// Belt and braces: even if a browser ever rendered a file, it would run
	// in an opaque origin with no script access to this app.
	h.Set("Content-Security-Policy", "default-src 'none'; img-src 'self'; style-src 'unsafe-inline'; sandbox")
	h.Set("Cache-Control", "private, no-cache")
	http.ServeContent(w, r, "", time.UnixMilli(f.CreatedAt), blob)
}

func (s *Server) handleDeleteFile(w http.ResponseWriter, r *http.Request) {
	a := authFrom(r)
	id := r.PathValue("id")
	err := s.store.DeleteFile(r.Context(), a.user.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "file not found")
		return
	} else if err != nil {
		fail(w, r, err)
		return
	}
	s.hub.publish(a.user.ID, event{Type: "file_deleted", Data: map[string]string{"id": id}, From: a.session.ID})
	w.WriteHeader(http.StatusNoContent)
}
