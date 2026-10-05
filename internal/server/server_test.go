package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/sainad2222/hopclip/internal/auth"
	"github.com/sainad2222/hopclip/internal/config"
	"github.com/sainad2222/hopclip/internal/store"
)

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

type testEnv struct {
	t   *testing.T
	cfg *config.Config
	st  *store.Store
	srv *Server
	ts  *httptest.Server
}

func newEnv(t *testing.T, mutate func(*config.Config)) *testEnv {
	t.Helper()
	cfg := &config.Config{
		DataDir:          t.TempDir(),
		SessionTTL:       24 * time.Hour,
		CookieSecure:     true,
		MaxUploadBytes:   1 << 20,
		UserQuotaBytes:   0,
		MaxClipBytes:     1024,
		ClipHistoryLimit: 50,
		LoginMaxAttempts: 100,
		LoginWindow:      time.Minute,
	}
	if mutate != nil {
		mutate(cfg)
	}
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	static := fstest.MapFS{
		"index.html": {Data: []byte("<!doctype html><title>Hopclip</title>")},
		"app.js":     {Data: []byte("console.log(1)")},
	}
	srv, err := New(cfg, st, static)
	if err != nil {
		t.Fatal(err)
	}
	// TLS so that Secure, __Host- cookies behave as they do in production.
	ts := httptest.NewTLSServer(srv.Handler())
	t.Cleanup(func() {
		srv.hub.closeAll()
		ts.Close()
		st.Close()
	})
	return &testEnv{t: t, cfg: cfg, st: st, srv: srv, ts: ts}
}

func (e *testEnv) addUser(name, pw string, admin bool) *store.User {
	e.t.Helper()
	hash, err := auth.HashPassword(pw)
	if err != nil {
		e.t.Fatal(err)
	}
	u, err := e.st.CreateUser(context.Background(), name, hash, admin)
	if err != nil {
		e.t.Fatal(err)
	}
	return u
}

type client struct {
	t  *testing.T
	e  *testEnv
	hc *http.Client
	me meResponse
}

func (e *testEnv) anon() *client {
	jar, _ := cookiejar.New(nil)
	// ts.Client() is shared; each simulated device needs its own cookie jar.
	hc := &http.Client{Transport: e.ts.Client().Transport, Jar: jar}
	return &client{t: e.t, e: e, hc: hc}
}

func (e *testEnv) login(name, pw, device string) *client {
	e.t.Helper()
	c := e.anon()
	res := c.do("POST", "/api/login", map[string]string{"username": name, "password": pw, "device_name": device})
	if res.StatusCode != http.StatusOK {
		e.t.Fatalf("login %s: status %d: %s", name, res.StatusCode, readBody(res))
	}
	decode(e.t, res, &c.me)
	return c
}

type hdr map[string]string

func (c *client) request(method, path string, body io.Reader, headers hdr) *http.Response {
	c.t.Helper()
	req, err := http.NewRequest(method, c.e.ts.URL+path, body)
	if err != nil {
		c.t.Fatal(err)
	}
	if c.me.CSRFToken != "" {
		req.Header.Set(csrfHeader, c.me.CSRFToken)
	}
	for k, v := range headers {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	res, err := c.hc.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	return res
}

func (c *client) do(method, path string, body any, headers ...hdr) *http.Response {
	c.t.Helper()
	h := hdr{}
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
		h["Content-Type"] = "application/json"
	}
	for _, extra := range headers {
		for k, v := range extra {
			h[k] = v
		}
	}
	return c.request(method, path, r, h)
}

func (c *client) expect(status int, method, path string, body any, out any) {
	c.t.Helper()
	res := c.do(method, path, body)
	if res.StatusCode != status {
		c.t.Fatalf("%s %s: got %d, want %d: %s", method, path, res.StatusCode, status, readBody(res))
	}
	if out != nil {
		decode(c.t, res, out)
	} else {
		res.Body.Close()
	}
}

func (c *client) upload(name, contentType string, data []byte) *http.Response {
	c.t.Helper()
	return c.request("POST", "/api/files?name="+urlEncode(name), bytes.NewReader(data), hdr{"Content-Type": contentType})
}

func urlEncode(s string) string {
	r := strings.NewReplacer("%", "%25", "/", "%2F", "?", "%3F", "&", "%26", "#", "%23", " ", "%20", "+", "%2B")
	return r.Replace(s)
}

func readBody(res *http.Response) string {
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return string(b)
}

func decode(t *testing.T, res *http.Response, out any) {
	t.Helper()
	defer res.Body.Close()
	if err := json.NewDecoder(res.Body).Decode(out); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

// stream opens the SSE endpoint and delivers parsed events on a channel. The
// channel is closed when the server ends the stream.
func (c *client) stream() <-chan event {
	c.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	c.t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, "GET", c.e.ts.URL+"/api/events", nil)
	res, err := c.hc.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		c.t.Fatalf("events: status %d", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		c.t.Fatalf("events content type %q", ct)
	}
	ch := make(chan event, 16)
	go func() {
		defer close(ch)
		defer res.Body.Close()
		sc := bufio.NewScanner(res.Body)
		for sc.Scan() {
			line := sc.Text()
			if data, ok := strings.CutPrefix(line, "data: "); ok {
				var ev event
				if json.Unmarshal([]byte(data), &ev) == nil {
					ch <- ev
				}
			}
		}
	}()
	if ev := next(c.t, ch); ev.Type != "hello" {
		c.t.Fatalf("first event %q, want hello", ev.Type)
	}
	return ch
}

func next(t *testing.T, ch <-chan event) event {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("event stream closed")
		}
		return ev
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for event")
	}
	return event{}
}

func waitClosed(t *testing.T, ch <-chan event) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("event stream was not closed")
		}
	}
}

func TestLoginSetsHardenedCookie(t *testing.T) {
	e := newEnv(t, nil)
	e.addUser("alice", "correct horse", false)
	c := e.anon()
	res := c.do("POST", "/api/login", map[string]string{"username": "alice", "password": "correct horse"})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d", res.StatusCode)
	}
	res.Body.Close()
	sc := res.Header.Get("Set-Cookie")
	for _, want := range []string{"__Host-hopclip=", "Path=/", "HttpOnly", "Secure", "SameSite=Strict", "Max-Age=86400"} {
		if !strings.Contains(sc, want) {
			t.Errorf("Set-Cookie %q missing %q", sc, want)
		}
	}
	if strings.Contains(strings.ToLower(sc), "domain=") {
		t.Errorf("Set-Cookie must be host-only: %q", sc)
	}
	var me meResponse
	c.expect(http.StatusOK, "GET", "/api/me", nil, &me)
	if me.User.Username != "alice" || me.CSRFToken == "" || me.Device == "" {
		t.Fatalf("unexpected me: %+v", me)
	}
}

func TestLoginFailures(t *testing.T) {
	e := newEnv(t, nil)
	e.addUser("alice", "correct horse", false)
	c := e.anon()
	for _, tc := range []struct {
		name   string
		body   any
		hdr    hdr
		status int
	}{
		{"wrong password", map[string]string{"username": "alice", "password": "nope nope"}, nil, 401},
		{"unknown user", map[string]string{"username": "bob", "password": "whatever1"}, nil, 401},
		{"missing fields", map[string]string{"username": "alice"}, nil, 400},
		{"unknown field", map[string]string{"username": "alice", "password": "x", "admin": "1"}, nil, 400},
		{"wrong content type", map[string]string{"username": "alice", "password": "correct horse"}, hdr{"Content-Type": "text/plain"}, 415},
		{"cross-site", map[string]string{"username": "alice", "password": "correct horse"}, hdr{"Sec-Fetch-Site": "cross-site"}, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := c.do("POST", "/api/login", tc.body, tc.hdr)
			if res.StatusCode != tc.status {
				t.Fatalf("got %d want %d: %s", res.StatusCode, tc.status, readBody(res))
			}
			res.Body.Close()
		})
	}
	c.expect(http.StatusUnauthorized, "GET", "/api/me", nil, nil)
}

func TestLoginCaseInsensitiveUsername(t *testing.T) {
	e := newEnv(t, nil)
	e.addUser("Alice", "correct horse", false)
	c := e.login("alice", "correct horse", "")
	if c.me.User.Username != "Alice" {
		t.Fatalf("username %q", c.me.User.Username)
	}
}

func TestLoginRateLimit(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.LoginMaxAttempts = 3 })
	e.addUser("alice", "correct horse", false)
	c := e.anon()
	for range 3 {
		c.expect(http.StatusUnauthorized, "POST", "/api/login", map[string]string{"username": "alice", "password": "wrong pass"}, nil)
	}
	res := c.do("POST", "/api/login", map[string]string{"username": "alice", "password": "correct horse"})
	if res.StatusCode != http.StatusTooManyRequests || res.Header.Get("Retry-After") == "" {
		t.Fatalf("got %d (Retry-After %q), want 429", res.StatusCode, res.Header.Get("Retry-After"))
	}
	res.Body.Close()
}

func TestAPIRequiresAuth(t *testing.T) {
	e := newEnv(t, nil)
	c := e.anon()
	for _, p := range []string{"/api/me", "/api/clips", "/api/files", "/api/files/abc", "/api/events", "/api/sessions", "/api/admin/users"} {
		c.expect(http.StatusUnauthorized, "GET", p, nil, nil)
	}
	c.expect(http.StatusUnauthorized, "POST", "/api/clips", map[string]string{"content": "x"}, nil)

	res := c.request("GET", "/api/me", nil, hdr{"Cookie": "__Host-hopclip=forged"})
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("forged cookie: %d", res.StatusCode)
	}
	res.Body.Close()
	c.expect(http.StatusOK, "GET", "/healthz", nil, nil)
	c.expect(http.StatusNotFound, "GET", "/api/nope", nil, nil)
}

func TestCSRFProtection(t *testing.T) {
	e := newEnv(t, nil)
	e.addUser("alice", "correct horse", false)
	c := e.login("alice", "correct horse", "laptop")
	body := map[string]string{"content": "hello"}

	for _, tc := range []struct {
		name string
		h    hdr
	}{
		{"missing token", hdr{csrfHeader: ""}},
		{"wrong token", hdr{csrfHeader: "deadbeef"}},
		{"cross-site with token", hdr{"Sec-Fetch-Site": "cross-site"}},
		{"same-site subdomain", hdr{"Sec-Fetch-Site": "same-site"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := c.do("POST", "/api/clips", body, tc.h)
			if res.StatusCode != http.StatusForbidden {
				t.Fatalf("got %d, want 403", res.StatusCode)
			}
			res.Body.Close()
		})
	}
	res := c.do("POST", "/api/clips", body, hdr{"Sec-Fetch-Site": "same-origin"})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("valid request: %d %s", res.StatusCode, readBody(res))
	}
	res.Body.Close()

	// A token from another session must not work for this one.
	other := e.login("alice", "correct horse", "phone")
	res = c.do("DELETE", "/api/clips", nil, hdr{csrfHeader: other.me.CSRFToken})
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign token accepted: %d", res.StatusCode)
	}
	res.Body.Close()
}

func TestSecurityHeaders(t *testing.T) {
	e := newEnv(t, nil)
	res := e.anon().do("GET", "/", nil)
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d", res.StatusCode)
	}
	for k, want := range map[string]string{
		"X-Content-Type-Options":    "nosniff",
		"X-Frame-Options":           "DENY",
		"Referrer-Policy":           "no-referrer",
		"Strict-Transport-Security": "max-age=31536000",
	} {
		if got := res.Header.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	csp := res.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "unsafe-inline") {
		t.Errorf("weak CSP %q", csp)
	}
}

func TestStaticETag(t *testing.T) {
	e := newEnv(t, nil)
	c := e.anon()
	res := c.do("GET", "/app.js", nil)
	res.Body.Close()
	etag := res.Header.Get("ETag")
	if res.StatusCode != 200 || etag == "" {
		t.Fatalf("status %d etag %q", res.StatusCode, etag)
	}
	res = c.do("GET", "/app.js", nil, hdr{"If-None-Match": etag})
	res.Body.Close()
	if res.StatusCode != http.StatusNotModified {
		t.Fatalf("revalidation got %d", res.StatusCode)
	}
}

func TestClipSyncAcrossDevices(t *testing.T) {
	e := newEnv(t, nil)
	e.addUser("alice", "correct horse", false)
	e.addUser("bob", "battery staple", false)
	laptop := e.login("alice", "correct horse", "Laptop")
	phone := e.login("alice", "correct horse", "Phone")
	bob := e.login("bob", "battery staple", "Bob PC")

	phoneEvents := phone.stream()
	laptopEvents := laptop.stream()
	bobEvents := bob.stream()

	var clip store.Clip
	laptop.expect(http.StatusCreated, "POST", "/api/clips", map[string]string{"content": "secret 🔑 text\nline 2"}, &clip)

	for _, ch := range []<-chan event{phoneEvents, laptopEvents} {
		ev := next(t, ch)
		if ev.Type != "clip" || ev.From != laptop.me.SessionID {
			t.Fatalf("got %+v", ev)
		}
		data := ev.Data.(map[string]any)
		if data["content"] != "secret 🔑 text\nline 2" || data["device_name"] != "Laptop" {
			t.Fatalf("clip payload %+v", data)
		}
	}

	// Bob must never see Alice's clip: the first event he receives is his own.
	bob.expect(http.StatusCreated, "POST", "/api/clips", map[string]string{"content": "bob's"}, nil)
	if ev := next(t, bobEvents); ev.Data.(map[string]any)["content"] != "bob's" {
		t.Fatalf("bob received %+v", ev)
	}

	var clips []store.Clip
	phone.expect(http.StatusOK, "GET", "/api/clips", nil, &clips)
	if len(clips) != 1 || clips[0].ID != clip.ID {
		t.Fatalf("phone history %+v", clips)
	}

	phone.expect(http.StatusNoContent, "DELETE", fmt.Sprintf("/api/clips/%d", clip.ID), nil, nil)
	for _, ch := range []<-chan event{phoneEvents, laptopEvents} {
		if ev := next(t, ch); ev.Type != "clip_deleted" || ev.From != phone.me.SessionID {
			t.Fatalf("got %+v", ev)
		}
	}
	laptop.expect(http.StatusCreated, "POST", "/api/clips", map[string]string{"content": "x"}, nil)
	if ev := next(t, phoneEvents); ev.Type != "clip" {
		t.Fatalf("got %+v", ev)
	}
	laptop.expect(http.StatusNoContent, "DELETE", "/api/clips", nil, nil)
	if ev := next(t, phoneEvents); ev.Type != "clips_cleared" {
		t.Fatalf("got %+v", ev)
	}
}

func TestClipValidationAndHistoryLimit(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.ClipHistoryLimit = 3; c.MaxClipBytes = 100 })
	e.addUser("alice", "correct horse", false)
	c := e.login("alice", "correct horse", "")

	c.expect(http.StatusBadRequest, "POST", "/api/clips", map[string]string{"content": ""}, nil)
	c.expect(http.StatusRequestEntityTooLarge, "POST", "/api/clips", map[string]string{"content": strings.Repeat("a", 101)}, nil)
	c.expect(http.StatusCreated, "POST", "/api/clips", map[string]string{"content": strings.Repeat("a", 100)}, nil)
	// Escaped JSON is larger than the decoded text and must still be accepted.
	c.expect(http.StatusCreated, "POST", "/api/clips", map[string]string{"content": strings.Repeat("\u0001", 100)}, nil)

	for i := range 5 {
		c.expect(http.StatusCreated, "POST", "/api/clips", map[string]string{"content": fmt.Sprint("clip ", i)}, nil)
	}
	var clips []store.Clip
	c.expect(http.StatusOK, "GET", "/api/clips", nil, &clips)
	if len(clips) != 3 || clips[0].Content != "clip 4" || clips[2].Content != "clip 2" {
		t.Fatalf("history %+v", clips)
	}
	c.expect(http.StatusOK, "GET", "/api/clips?limit=1", nil, &clips)
	if len(clips) != 1 {
		t.Fatalf("limit ignored: %d", len(clips))
	}
	c.expect(http.StatusBadRequest, "GET", "/api/clips?limit=-1", nil, nil)
	c.expect(http.StatusNotFound, "DELETE", "/api/clips/999999", nil, nil)
	c.expect(http.StatusNotFound, "DELETE", "/api/clips/abc", nil, nil)
}

var pngHeader = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x02\x00\x00\x00")

func TestFileUploadDownload(t *testing.T) {
	e := newEnv(t, nil)
	e.addUser("alice", "correct horse", false)
	laptop := e.login("alice", "correct horse", "Laptop")
	phone := e.login("alice", "correct horse", "Phone")
	events := phone.stream()

	data := bytes.Repeat([]byte("0123456789"), 5000)
	res := laptop.upload("../../etc/rëport final.txt", "text/plain; charset=utf-8", data)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("upload: %d %s", res.StatusCode, readBody(res))
	}
	var f fileView
	decode(t, res, &f)
	if strings.ContainsAny(f.Name, "/\\") || !strings.HasSuffix(f.Name, "rëport final.txt") {
		t.Fatalf("name not sanitised: %q", f.Name)
	}
	if f.Size != int64(len(data)) || f.ContentType != "text/plain" || f.Previewable {
		t.Fatalf("file %+v", f)
	}
	if ev := next(t, events); ev.Type != "file" || ev.Data.(map[string]any)["id"] != f.ID {
		t.Fatalf("event %+v", ev)
	}

	res = phone.do("GET", "/api/files/"+f.ID, nil)
	got := readBody(res)
	if res.StatusCode != 200 || got != string(data) {
		t.Fatalf("download status %d len %d", res.StatusCode, len(got))
	}
	cd := res.Header.Get("Content-Disposition")
	if !strings.HasPrefix(cd, "attachment;") || !strings.Contains(cd, "filename*=utf-8''") {
		t.Fatalf("Content-Disposition %q", cd)
	}
	if !strings.Contains(res.Header.Get("Content-Security-Policy"), "sandbox") {
		t.Fatal("download missing sandbox CSP")
	}

	res = phone.do("GET", "/api/files/"+f.ID, nil, hdr{"Range": "bytes=10-19"})
	if body := readBody(res); res.StatusCode != http.StatusPartialContent || body != "0123456789" {
		t.Fatalf("range: %d %q", res.StatusCode, body)
	}

	var files []fileView
	phone.expect(http.StatusOK, "GET", "/api/files", nil, &files)
	if len(files) != 1 || files[0].ID != f.ID {
		t.Fatalf("list %+v", files)
	}
	phone.expect(http.StatusNoContent, "DELETE", "/api/files/"+f.ID, nil, nil)
	if ev := next(t, events); ev.Type != "file_deleted" {
		t.Fatalf("event %+v", ev)
	}
	if _, err := os.Stat(e.st.BlobPath(f.ID)); !os.IsNotExist(err) {
		t.Fatalf("blob still on disk: %v", err)
	}
	phone.expect(http.StatusNotFound, "GET", "/api/files/"+f.ID, nil, nil)
}

func TestInlinePreviewOnlyForSniffedImages(t *testing.T) {
	e := newEnv(t, nil)
	e.addUser("alice", "correct horse", false)
	c := e.login("alice", "correct horse", "")

	var png, fakePNG, html fileView
	decode(t, c.upload("pic.png", "image/png", pngHeader), &png)
	decode(t, c.upload("evil.png", "image/png", []byte("<html><script>alert(1)</script>")), &fakePNG)
	decode(t, c.upload("page.html", "text/html", []byte("<html><script>alert(1)</script>")), &html)

	if !png.Previewable || fakePNG.Previewable || html.Previewable {
		t.Fatalf("previewable flags: png=%v fake=%v html=%v", png.Previewable, fakePNG.Previewable, html.Previewable)
	}
	res := c.do("GET", "/api/files/"+png.ID+"?inline=1", nil)
	res.Body.Close()
	if res.Header.Get("Content-Type") != "image/png" || !strings.HasPrefix(res.Header.Get("Content-Disposition"), "inline") {
		t.Fatalf("png inline headers %v", res.Header)
	}
	for _, id := range []string{fakePNG.ID, html.ID} {
		res := c.do("GET", "/api/files/"+id+"?inline=1", nil)
		res.Body.Close()
		if !strings.HasPrefix(res.Header.Get("Content-Disposition"), "attachment") {
			t.Fatalf("non-image served inline: %v", res.Header)
		}
	}
}

func tmpEntries(t *testing.T, e *testEnv) int {
	entries, err := os.ReadDir(filepath.Join(e.cfg.DataDir, "tmp"))
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

func TestUploadSizeLimit(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.MaxUploadBytes = 1000 })
	e.addUser("alice", "correct horse", false)
	c := e.login("alice", "correct horse", "")

	res := c.upload("big.bin", "application/octet-stream", make([]byte, 1001))
	if res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("got %d", res.StatusCode)
	}
	res.Body.Close()

	// Without Content-Length the limit must be enforced while streaming.
	body := io.MultiReader(bytes.NewReader(make([]byte, 600)), bytes.NewReader(make([]byte, 600)))
	res = c.request("POST", "/api/files?name=chunked.bin", body, hdr{"Content-Type": "application/octet-stream"})
	if res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("chunked: got %d", res.StatusCode)
	}
	res.Body.Close()
	if n := tmpEntries(t, e); n != 0 {
		t.Fatalf("%d temp files left behind", n)
	}

	res = c.upload("ok.bin", "", make([]byte, 1000))
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("at-limit upload: %d", res.StatusCode)
	}
	var f fileView
	decode(t, res, &f)
	if f.ContentType != "application/octet-stream" || f.Name != "ok.bin" {
		t.Fatalf("file %+v", f)
	}
	res = c.upload("", "", []byte{})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("empty upload: %d", res.StatusCode)
	}
	decode(t, res, &f)
	if f.Name != "file" || f.Size != 0 {
		t.Fatalf("empty file %+v", f)
	}
}

func TestUserQuota(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.UserQuotaBytes = 3000 })
	e.addUser("alice", "correct horse", false)
	e.addUser("bob", "battery staple", false)
	alice := e.login("alice", "correct horse", "")
	bob := e.login("bob", "battery staple", "")

	var f fileView
	res := alice.upload("a", "", make([]byte, 2000))
	decode(t, res, &f)
	res = alice.upload("b", "", make([]byte, 1001))
	if res.StatusCode != http.StatusRequestEntityTooLarge || !strings.Contains(readBody(res), "quota") {
		t.Fatalf("over quota: %d", res.StatusCode)
	}
	res = bob.upload("b", "", make([]byte, 2000))
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("quota is per user: %d", res.StatusCode)
	}
	res.Body.Close()

	var me meResponse
	alice.expect(http.StatusOK, "GET", "/api/me", nil, &me)
	if me.UsedBytes != 2000 || me.Limits.UserQuotaBytes != 3000 {
		t.Fatalf("me %+v", me)
	}
	alice.expect(http.StatusNoContent, "DELETE", "/api/files/"+f.ID, nil, nil)
	res = alice.upload("b", "", make([]byte, 3000))
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("after freeing space: %d", res.StatusCode)
	}
	res.Body.Close()
}

func TestUserIsolation(t *testing.T) {
	e := newEnv(t, nil)
	e.addUser("alice", "correct horse", false)
	e.addUser("bob", "battery staple", false)
	alice := e.login("alice", "correct horse", "")
	bob := e.login("bob", "battery staple", "")

	var clip store.Clip
	alice.expect(http.StatusCreated, "POST", "/api/clips", map[string]string{"content": "mine"}, &clip)
	var f fileView
	decode(t, alice.upload("x.txt", "text/plain", []byte("private")), &f)

	var clips []store.Clip
	bob.expect(http.StatusOK, "GET", "/api/clips", nil, &clips)
	var files []fileView
	bob.expect(http.StatusOK, "GET", "/api/files", nil, &files)
	if len(clips) != 0 || len(files) != 0 {
		t.Fatalf("bob sees %d clips %d files", len(clips), len(files))
	}
	bob.expect(http.StatusNotFound, "GET", "/api/files/"+f.ID, nil, nil)
	bob.expect(http.StatusNotFound, "DELETE", "/api/files/"+f.ID, nil, nil)
	bob.expect(http.StatusNotFound, "DELETE", fmt.Sprintf("/api/clips/%d", clip.ID), nil, nil)
	bob.expect(http.StatusNotFound, "DELETE", "/api/sessions/"+alice.me.SessionID, nil, nil)
	bob.expect(http.StatusNotFound, "PATCH", "/api/sessions/"+alice.me.SessionID, map[string]string{"device_name": "pwned"}, nil)
	alice.expect(http.StatusOK, "GET", "/api/files/"+f.ID, nil, nil)
}

func TestSessionsAndRevocation(t *testing.T) {
	e := newEnv(t, nil)
	e.addUser("alice", "correct horse", false)
	laptop := e.login("alice", "correct horse", "Laptop")
	phone := e.login("alice", "correct horse", "  Phone\x00\n  of\tAlice ")
	if phone.me.Device != "Phone of Alice" {
		t.Fatalf("device name not cleaned: %q", phone.me.Device)
	}
	phoneEvents := phone.stream()

	var sessions []sessionView
	laptop.expect(http.StatusOK, "GET", "/api/sessions", nil, &sessions)
	if len(sessions) != 2 {
		t.Fatalf("sessions %+v", sessions)
	}
	for _, s := range sessions {
		if s.Current != (s.ID == laptop.me.SessionID) || s.Online != (s.ID == phone.me.SessionID) {
			t.Fatalf("flags wrong %+v", s)
		}
	}

	laptop.expect(http.StatusOK, "PATCH", "/api/sessions/"+phone.me.SessionID, map[string]string{"device_name": "Pixel"}, nil)
	if ev := next(t, phoneEvents); ev.Type != "devices_changed" {
		t.Fatalf("got %+v", ev)
	}
	laptop.expect(http.StatusBadRequest, "PATCH", "/api/sessions/"+phone.me.SessionID, map[string]string{"device_name": " \t "}, nil)

	laptop.expect(http.StatusNoContent, "DELETE", "/api/sessions/"+phone.me.SessionID, nil, nil)
	waitClosed(t, phoneEvents)
	phone.expect(http.StatusUnauthorized, "GET", "/api/me", nil, nil)

	laptop.expect(http.StatusNoContent, "POST", "/api/logout", nil, nil)
	laptop.expect(http.StatusUnauthorized, "GET", "/api/me", nil, nil)
}

func TestEventStreamEndsWhenSessionRemovedOutOfBand(t *testing.T) {
	old := heartbeatInterval
	heartbeatInterval = 50 * time.Millisecond
	t.Cleanup(func() { heartbeatInterval = old })

	e := newEnv(t, nil)
	u := e.addUser("alice", "correct horse", false)
	c := e.login("alice", "correct horse", "")
	events := c.stream()
	// What `hopclip user passwd` does from a separate process.
	if _, err := e.st.DeleteOtherSessions(context.Background(), u.ID, ""); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, events)
}

func TestSessionExpiry(t *testing.T) {
	e := newEnv(t, nil)
	e.addUser("alice", "correct horse", false)
	c := e.login("alice", "correct horse", "")
	// Force expiry by backdating the row.
	if err := e.st.TouchSession(context.Background(), c.me.SessionID, "x", -time.Hour); err != nil {
		t.Fatal(err)
	}
	c.expect(http.StatusUnauthorized, "GET", "/api/me", nil, nil)
}

func TestChangePassword(t *testing.T) {
	e := newEnv(t, nil)
	e.addUser("alice", "correct horse", false)
	laptop := e.login("alice", "correct horse", "Laptop")
	phone := e.login("alice", "correct horse", "Phone")
	phoneEvents := phone.stream()

	laptop.expect(http.StatusForbidden, "POST", "/api/password", map[string]string{"current_password": "wrong one", "new_password": "new password"}, nil)
	laptop.expect(http.StatusBadRequest, "POST", "/api/password", map[string]string{"current_password": "correct horse", "new_password": "short"}, nil)
	var out map[string]int
	laptop.expect(http.StatusOK, "POST", "/api/password", map[string]string{"current_password": "correct horse", "new_password": "new password"}, &out)
	if out["revoked_sessions"] != 1 {
		t.Fatalf("revoked %v", out)
	}
	waitClosed(t, phoneEvents)
	phone.expect(http.StatusUnauthorized, "GET", "/api/me", nil, nil)
	laptop.expect(http.StatusOK, "GET", "/api/me", nil, nil)

	e.anon().expect(http.StatusUnauthorized, "POST", "/api/login", map[string]string{"username": "alice", "password": "correct horse"}, nil)
	e.login("alice", "new password", "")
}

func TestAdmin(t *testing.T) {
	e := newEnv(t, nil)
	admin := e.addUser("root", "admin password", true)
	e.addUser("alice", "correct horse", false)
	root := e.login("root", "admin password", "")
	alice := e.login("alice", "correct horse", "")

	alice.expect(http.StatusForbidden, "GET", "/api/admin/users", nil, nil)
	alice.expect(http.StatusForbidden, "POST", "/api/admin/users", map[string]any{"username": "eve", "password": "eve password", "is_admin": true}, nil)

	var bob store.User
	root.expect(http.StatusCreated, "POST", "/api/admin/users", map[string]any{"username": "bob", "password": "bob password"}, &bob)
	root.expect(http.StatusConflict, "POST", "/api/admin/users", map[string]any{"username": "BOB", "password": "bob password"}, nil)
	root.expect(http.StatusBadRequest, "POST", "/api/admin/users", map[string]any{"username": "../x", "password": "bob password"}, nil)
	root.expect(http.StatusBadRequest, "POST", "/api/admin/users", map[string]any{"username": "carol", "password": "short"}, nil)

	bobc := e.login("bob", "bob password", "")
	var f fileView
	decode(t, bobc.upload("b.txt", "text/plain", []byte("bob data")), &f)
	bobEvents := bobc.stream()

	root.expect(http.StatusOK, "PATCH", fmt.Sprintf("/api/admin/users/%d", bob.ID), map[string]any{"password": "reset password"}, nil)
	waitClosed(t, bobEvents)
	bobc.expect(http.StatusUnauthorized, "GET", "/api/me", nil, nil)
	bobc = e.login("bob", "reset password", "")

	var updated store.User
	root.expect(http.StatusOK, "PATCH", fmt.Sprintf("/api/admin/users/%d", bob.ID), map[string]any{"is_admin": true}, &updated)
	if !updated.IsAdmin {
		t.Fatal("bob not promoted")
	}
	bobc.expect(http.StatusOK, "GET", "/api/admin/users", nil, nil)

	root.expect(http.StatusBadRequest, "DELETE", fmt.Sprintf("/api/admin/users/%d", admin.ID), nil, nil)
	root.expect(http.StatusBadRequest, "PATCH", fmt.Sprintf("/api/admin/users/%d", admin.ID), map[string]any{"is_admin": false}, nil)
	root.expect(http.StatusNotFound, "DELETE", "/api/admin/users/99999", nil, nil)

	var users []adminUserView
	root.expect(http.StatusOK, "GET", "/api/admin/users", nil, &users)
	if len(users) != 3 {
		t.Fatalf("users %+v", users)
	}
	for _, u := range users {
		if u.Username == "bob" && u.UsedBytes != int64(len("bob data")) {
			t.Fatalf("bob used %d", u.UsedBytes)
		}
	}

	root.expect(http.StatusNoContent, "DELETE", fmt.Sprintf("/api/admin/users/%d", bob.ID), nil, nil)
	bobc.expect(http.StatusUnauthorized, "GET", "/api/me", nil, nil)
	if _, err := os.Stat(e.st.BlobPath(f.ID)); !os.IsNotExist(err) {
		t.Fatalf("deleted user's blob still on disk: %v", err)
	}
}

func TestRetentionSweep(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.FileRetention = time.Millisecond; c.ClipRetention = time.Millisecond })
	e.addUser("alice", "correct horse", false)
	c := e.login("alice", "correct horse", "")
	var f fileView
	decode(t, c.upload("old.txt", "text/plain", []byte("x")), &f)
	c.expect(http.StatusCreated, "POST", "/api/clips", map[string]string{"content": "old"}, nil)
	events := c.stream()
	time.Sleep(5 * time.Millisecond)

	e.srv.sweep(context.Background())

	seen := map[string]bool{}
	for range 2 {
		seen[next(t, events).Type] = true
	}
	if !seen["file_deleted"] || !seen["resync"] {
		t.Fatalf("events %v", seen)
	}
	var clips []store.Clip
	c.expect(http.StatusOK, "GET", "/api/clips", nil, &clips)
	var files []fileView
	c.expect(http.StatusOK, "GET", "/api/files", nil, &files)
	if len(clips) != 0 || len(files) != 0 {
		t.Fatalf("not swept: %d clips %d files", len(clips), len(files))
	}
}
