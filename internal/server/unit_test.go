package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sainad2222/hopclip/internal/config"
)

func TestCleanLabel(t *testing.T) {
	for in, want := range map[string]string{
		"  My   Phone  ":         "My Phone",
		"tab\there\nnewline":     "tab here newline",
		"nul\x00byte":            "nulbyte",
		"rtl\u202eoverride":      "rtloverride",
		"bad\xffutf8":            "badutf8",
		strings.Repeat("é", 100): strings.Repeat("é", 64),
	} {
		if got := cleanLabel(in, 64); got != want {
			t.Errorf("cleanLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeFilename(t *testing.T) {
	for in, want := range map[string]string{
		"report.pdf":             "report.pdf",
		"../../etc/passwd":       "_.._etc_passwd",
		`C:\Users\me\file.txt`:   `C:_Users_me_file.txt`,
		"   ":                    "file",
		"..":                     "file",
		".hidden":                "hidden",
		"photo\r\n.jpg":          "photo .jpg",
		"日本語のファイル.txt":           "日本語のファイル.txt",
		strings.Repeat("a", 300): strings.Repeat("a", 200),
	} {
		if got := sanitizeFilename(in); got != want {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGuessDevice(t *testing.T) {
	for ua, want := range map[string]string{
		"Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1": "iPhone Safari",
		"Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Mobile Safari/537.36":                       "Android Chrome",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36 Edg/120.0":                   "Windows Edge",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 14.0; rv:120.0) Gecko/20100101 Firefox/120.0":                                                     "Mac Firefox",
		"curl/8.5.0": "curl",
		"":           "Unknown device",
	} {
		if got := guessDevice(ua); got != want {
			t.Errorf("guessDevice(%q) = %q, want %q", ua, got, want)
		}
	}
}

func TestClientIP(t *testing.T) {
	req := func(remote string, h map[string]string) *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = remote
		for k, v := range h {
			r.Header.Add(k, v)
		}
		return r
	}
	for _, tc := range []struct {
		name, header string
		r            *http.Request
		want         string
	}{
		{"no header configured ignores XFF", "", req("10.0.0.2:5555", map[string]string{"X-Forwarded-For": "1.2.3.4"}), "10.0.0.2"},
		{"XFF takes right-most hop", "X-Forwarded-For", req("10.0.0.2:5555", map[string]string{"X-Forwarded-For": "6.6.6.6, 203.0.113.9"}), "203.0.113.9"},
		{"cloudflare header", "Cf-Connecting-Ip", req("10.0.0.2:5555", map[string]string{"Cf-Connecting-Ip": "2001:db8::1"}), "2001:db8::1"},
		{"garbage falls back to socket", "X-Forwarded-For", req("10.0.0.2:5555", map[string]string{"X-Forwarded-For": "not-an-ip"}), "10.0.0.2"},
		{"ipv4-mapped is unmapped", "X-Real-Ip", req("[::1]:1", map[string]string{"X-Real-Ip": "::ffff:198.51.100.7"}), "198.51.100.7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{cfg: &config.Config{ClientIPHeader: tc.header}}
			if got := s.clientIP(tc.r); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestFailureLimiter(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newFailureLimiter(3, time.Minute)
	l.now = func() time.Time { return now }

	for range 2 {
		l.fail("k")
	}
	if b, _ := l.blocked("k"); b {
		t.Fatal("blocked too early")
	}
	l.fail("k")
	b, wait := l.blocked("k")
	if !b || wait != time.Minute {
		t.Fatalf("blocked=%v wait=%v", b, wait)
	}
	if b, _ := l.blocked("other"); b {
		t.Fatal("keys must be independent")
	}
	now = now.Add(61 * time.Second)
	if b, _ := l.blocked("k"); b {
		t.Fatal("window did not expire")
	}
	l.fail("k")
	l.reset("k")
	if b, _ := l.blocked("k"); b {
		t.Fatal("reset did not clear")
	}
	l.fail("gc")
	now = now.Add(2 * time.Minute)
	l.gc()
	if len(l.entries) != 0 {
		t.Fatalf("gc left %d entries", len(l.entries))
	}
}

func TestHubDropsStalledSubscriber(t *testing.T) {
	h := newHub()
	slow := h.subscribe(1, "a")
	for range cap(slow.ch) + 1 {
		h.publish(1, event{Type: "x"})
	}
	select {
	case <-slow.done:
	default:
		t.Fatal("stalled subscriber not dropped")
	}
	if len(h.onlineSessions(1)) != 0 {
		t.Fatal("subscriber still registered")
	}
}
