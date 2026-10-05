package config

import (
	"strings"
	"testing"
	"time"
)

func TestDefaults(t *testing.T) {
	t.Setenv("DATA_DIR", t.TempDir())
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.ListenAddr != ":8080" || !c.CookieSecure || c.SessionTTL != 720*time.Hour ||
		c.MaxUploadBytes != 100<<20 || c.UserQuotaBytes != 2048<<20 || c.MaxClipBytes != 256<<10 ||
		c.ClipHistoryLimit != 200 || c.ClientIPHeader != "" {
		t.Fatalf("unexpected defaults %+v", c)
	}
}

func TestOverrides(t *testing.T) {
	t.Setenv("MAX_UPLOAD_MB", "5")
	t.Setenv("COOKIE_SECURE", "false")
	t.Setenv("CLIENT_IP_HEADER", "cf-connecting-ip")
	t.Setenv("FILE_RETENTION_DAYS", "7")
	t.Setenv("SESSION_TTL", "48h")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxUploadBytes != 5<<20 || c.CookieSecure || c.ClientIPHeader != "Cf-Connecting-Ip" ||
		c.FileRetention != 7*24*time.Hour || c.SessionTTL != 48*time.Hour {
		t.Fatalf("overrides not applied %+v", c)
	}
}

func TestInvalid(t *testing.T) {
	for k, v := range map[string]string{
		"MAX_UPLOAD_MB":  "0",
		"MAX_CLIP_KB":    "abc",
		"SESSION_TTL":    "5m",
		"COOKIE_SECURE":  "maybe",
		"USER_QUOTA_MB":  "-1",
		"ADMIN_USERNAME": "admin",
	} {
		t.Run(k, func(t *testing.T) {
			t.Setenv(k, v)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), strings.SplitN(k, "_", 2)[0]) {
				t.Fatalf("%s=%s: err %v", k, v, err)
			}
		})
	}
}
