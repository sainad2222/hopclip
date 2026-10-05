package config

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	ListenAddr string
	DataDir    string

	AdminUsername string
	AdminPassword string

	SessionTTL     time.Duration
	CookieSecure   bool
	ClientIPHeader string

	MaxUploadBytes   int64
	UserQuotaBytes   int64
	MaxClipBytes     int
	ClipHistoryLimit int
	FileRetention    time.Duration
	ClipRetention    time.Duration

	LoginMaxAttempts int
	LoginWindow      time.Duration
}

const mb = 1 << 20

func Load() (*Config, error) {
	c := &Config{
		ListenAddr:     env("LISTEN_ADDR", ":8080"),
		DataDir:        env("DATA_DIR", "./data"),
		AdminUsername:  strings.TrimSpace(os.Getenv("ADMIN_USERNAME")),
		AdminPassword:  os.Getenv("ADMIN_PASSWORD"),
		ClientIPHeader: http.CanonicalHeaderKey(strings.TrimSpace(os.Getenv("CLIENT_IP_HEADER"))),
	}
	var err error
	p := parser{}
	c.SessionTTL = p.duration("SESSION_TTL", 30*24*time.Hour)
	c.CookieSecure = p.boolean("COOKIE_SECURE", true)
	c.MaxUploadBytes = int64(p.integer("MAX_UPLOAD_MB", 100)) * mb
	c.UserQuotaBytes = int64(p.integer("USER_QUOTA_MB", 2048)) * mb
	c.MaxClipBytes = p.integer("MAX_CLIP_KB", 256) * 1024
	c.ClipHistoryLimit = p.integer("CLIP_HISTORY_LIMIT", 200)
	c.FileRetention = time.Duration(p.integer("FILE_RETENTION_DAYS", 0)) * 24 * time.Hour
	c.ClipRetention = time.Duration(p.integer("CLIP_RETENTION_DAYS", 0)) * 24 * time.Hour
	c.LoginMaxAttempts = p.integer("LOGIN_MAX_ATTEMPTS", 10)
	c.LoginWindow = p.duration("LOGIN_WINDOW", 15*time.Minute)
	if p.err != nil {
		return nil, p.err
	}

	if c.DataDir, err = filepath.Abs(c.DataDir); err != nil {
		return nil, err
	}
	switch {
	case c.SessionTTL < time.Hour:
		return nil, fmt.Errorf("SESSION_TTL must be at least 1h")
	case c.MaxUploadBytes <= 0:
		return nil, fmt.Errorf("MAX_UPLOAD_MB must be positive")
	case c.UserQuotaBytes < 0:
		return nil, fmt.Errorf("USER_QUOTA_MB must be >= 0 (0 = unlimited)")
	case c.MaxClipBytes <= 0 || c.MaxClipBytes > 16*mb:
		return nil, fmt.Errorf("MAX_CLIP_KB must be between 1 and 16384")
	case c.ClipHistoryLimit <= 0:
		return nil, fmt.Errorf("CLIP_HISTORY_LIMIT must be positive")
	case c.FileRetention < 0 || c.ClipRetention < 0:
		return nil, fmt.Errorf("retention days must be >= 0")
	case c.LoginMaxAttempts <= 0:
		return nil, fmt.Errorf("LOGIN_MAX_ATTEMPTS must be positive")
	case (c.AdminUsername == "") != (c.AdminPassword == ""):
		return nil, fmt.Errorf("set both ADMIN_USERNAME and ADMIN_PASSWORD, or neither")
	}
	return c, nil
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

type parser struct{ err error }

func (p *parser) fail(key, v string, err error) {
	if p.err == nil {
		p.err = fmt.Errorf("invalid %s=%q: %v", key, v, err)
	}
}

func (p *parser) integer(key string, def int) int {
	v := env(key, "")
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		p.fail(key, v, err)
	}
	return n
}

func (p *parser) boolean(key string, def bool) bool {
	v := env(key, "")
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		p.fail(key, v, err)
	}
	return b
}

func (p *parser) duration(key string, def time.Duration) time.Duration {
	v := env(key, "")
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		p.fail(key, v, err)
	}
	return d
}
