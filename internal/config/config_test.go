package config

import (
	"strings"
	"testing"
)

func TestLoad_Defaults(t *testing.T) {
	t.Setenv("NTFY_URL", "http://ntfy:80/")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.NtfyURL != "http://ntfy:80" || cfg.DBPath != "/data/gonotifier.db" || cfg.Port != 8080 ||
		cfg.TZ.String() != "UTC" || cfg.DefaultNotifyTime.String() != "09:00" || cfg.FeedPath() != "/feed.ics" {
		t.Errorf("unexpected defaults: %+v", cfg)
	}
}

func TestLoad_RequiresNtfyURL(t *testing.T) {
	t.Setenv("NTFY_URL", "")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "NTFY_URL") {
		t.Errorf("err = %v", err)
	}
}

func TestLoad_FeedToken(t *testing.T) {
	t.Setenv("NTFY_URL", "http://ntfy:80")
	t.Setenv("FEED_TOKEN", "0123456789abcdef_-XYZ")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.FeedPath() != "/feed/0123456789abcdef_-XYZ.ics" {
		t.Errorf("FeedPath = %q", cfg.FeedPath())
	}
	for _, bad := range []string{"short", "has spaces in it ok?", "slash/inside/the/token"} {
		t.Setenv("FEED_TOKEN", bad)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "FEED_TOKEN") {
			t.Errorf("FEED_TOKEN=%q: err = %v", bad, err)
		}
	}
}
