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
		cfg.TZ.String() != "UTC" || cfg.DefaultNotifyTime.String() != "00:00" || cfg.ExportDir != "/data/exports" {
		t.Errorf("unexpected defaults: %+v", cfg)
	}
}

func TestLoad_RequiresNtfyURL(t *testing.T) {
	t.Setenv("NTFY_URL", "")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "NTFY_URL") {
		t.Errorf("err = %v", err)
	}
}

func TestLoad_ExportDirOff(t *testing.T) {
	t.Setenv("NTFY_URL", "http://ntfy:80")
	t.Setenv("EXPORT_DIR", "off")
	cfg, err := Load()
	if err != nil || cfg.ExportDir != "" {
		t.Errorf("ExportDir = %q, %v", cfg.ExportDir, err)
	}
}

func TestLoad_FeedURL(t *testing.T) {
	t.Setenv("NTFY_URL", "http://ntfy:80")
	t.Setenv("FEED_URL", "https://cal.example.com/")
	if cfg, err := Load(); err != nil || cfg.FeedURL != "https://cal.example.com" {
		t.Errorf("FeedURL = %q, %v", cfg.FeedURL, err)
	}
	for _, bad := range []string{"cal.example.com", "ftp://cal.example.com", "https://"} {
		t.Setenv("FEED_URL", bad)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "FEED_URL") {
			t.Errorf("FEED_URL=%q: err = %v", bad, err)
		}
	}
}
