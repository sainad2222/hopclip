package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"
)

type apiError struct {
	status int
	msg    string
}

func (e *apiError) Error() string { return e.msg }

func errStatus(status int, msg string) error { return &apiError{status, msg} }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// fail maps an error to a response. apiErrors carry their own status;
// anything else is logged and reported as a 500 without detail.
func fail(w http.ResponseWriter, r *http.Request, err error) {
	var ae *apiError
	if errors.As(err, &ae) {
		writeError(w, ae.status, ae.msg)
		return
	}
	slog.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

func decodeJSON(w http.ResponseWriter, r *http.Request, maxBytes int64, dst any) error {
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mt != "application/json" {
		return errStatus(http.StatusUnsupportedMediaType, "expected application/json")
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return errStatus(http.StatusRequestEntityTooLarge, "request body too large")
		}
		return errStatus(http.StatusBadRequest, "invalid JSON body")
	}
	if dec.More() {
		return errStatus(http.StatusBadRequest, "invalid JSON body")
	}
	return nil
}

// cleanLabel normalises user-supplied single-line labels such as device and
// file names: control characters are dropped, whitespace collapsed, and the
// result truncated to maxRunes.
func cleanLabel(s string, maxRunes int) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case unicode.IsSpace(r):
			return ' '
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r):
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, ""))
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > maxRunes {
		s = strings.TrimSpace(string([]rune(s)[:maxRunes]))
	}
	return s
}

func sanitizeFilename(name string) string {
	name = strings.NewReplacer("/", "_", "\\", "_").Replace(name)
	name = cleanLabel(name, 200)
	name = strings.Trim(name, " .")
	if name == "" {
		return "file"
	}
	return name
}

func guessDevice(ua string) string {
	has := func(s string) bool { return strings.Contains(ua, s) }
	os := "Unknown OS"
	switch {
	case has("iPhone"):
		os = "iPhone"
	case has("iPad"):
		os = "iPad"
	case has("Android"):
		os = "Android"
	case has("Windows"):
		os = "Windows"
	case has("Mac OS X"), has("Macintosh"):
		os = "Mac"
	case has("CrOS"):
		os = "ChromeOS"
	case has("Linux"):
		os = "Linux"
	}
	browser := ""
	switch {
	case has("Edg/"):
		browser = "Edge"
	case has("Firefox/"), has("FxiOS"):
		browser = "Firefox"
	case has("Chrome/"), has("CriOS"):
		browser = "Chrome"
	case has("Safari/"):
		browser = "Safari"
	case has("curl/"):
		return "curl"
	}
	if browser == "" {
		if os == "Unknown OS" {
			return "Unknown device"
		}
		return os
	}
	return os + " " + browser
}
