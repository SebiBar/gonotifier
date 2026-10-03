// Package config loads gonotifier's settings from environment variables.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // embedded zoneinfo fallback, so TZ works without system tzdata

	"github.com/sebibar/gonotifier/internal/events"
)

type Config struct {
	NtfyURL           string
	NtfyToken         string
	NtfyDefaultTopic  string
	DBPath            string
	ExportFile        string // read-only JSON snapshot of all events; "" = disabled
	FeedToken         string // if set, the iCal feed is only served at /feed/<token>.ics
	TZ                *time.Location
	CatchupWindow     time.Duration
	DefaultNotifyTime events.TimeOnly
	Port              int
	LogLevel          slog.Level
}

// reFeedToken keeps the secret feed URL hard to guess and safe in a URL path.
var reFeedToken = regexp.MustCompile(`^[A-Za-z0-9_-]{16,}$`)

// FeedPath is the URL path the calendar feed is served at.
func (c *Config) FeedPath() string {
	if c.FeedToken == "" {
		return "/feed.ics"
	}
	return "/feed/" + c.FeedToken + ".ics"
}

// Load reads environment variables, validates them and returns the Config.
func Load() (*Config, error) {
	var errs []error
	cfg := &Config{
		NtfyURL:          strings.TrimRight(os.Getenv("NTFY_URL"), "/"),
		NtfyToken:        os.Getenv("NTFY_TOKEN"),
		NtfyDefaultTopic: envOr("NTFY_DEFAULT_TOPIC", "reminders"),
		DBPath:           envOr("DB_PATH", "/data/gonotifier.db"),
	}
	if cfg.NtfyURL == "" {
		errs = append(errs, errors.New("NTFY_URL is required"))
	}
	cfg.ExportFile = envOr("EXPORT_FILE", filepath.Join(filepath.Dir(cfg.DBPath), "events-export.json"))
	if cfg.ExportFile == "off" {
		cfg.ExportFile = ""
	}
	cfg.FeedToken = envOr("FEED_TOKEN", "")
	if cfg.FeedToken != "" && !reFeedToken.MatchString(cfg.FeedToken) {
		errs = append(errs, errors.New("FEED_TOKEN must be at least 16 characters of A-Z, a-z, 0-9, - or _ (e.g. openssl rand -hex 24)"))
	}

	loc, err := time.LoadLocation(envOr("TZ", "UTC"))
	if err != nil {
		errs = append(errs, fmt.Errorf("TZ: %w", err))
	}
	cfg.TZ = loc

	if cfg.CatchupWindow, err = time.ParseDuration(envOr("CATCHUP_WINDOW", "24h")); err != nil || cfg.CatchupWindow < 0 {
		errs = append(errs, fmt.Errorf("CATCHUP_WINDOW: invalid duration"))
	}
	if cfg.DefaultNotifyTime, err = events.ParseTimeOnly(envOr("DEFAULT_NOTIFY_TIME", "09:00")); err != nil {
		errs = append(errs, fmt.Errorf("DEFAULT_NOTIFY_TIME: %w", err))
	}
	if cfg.Port, err = strconv.Atoi(envOr("PORT", "8080")); err != nil || cfg.Port <= 0 || cfg.Port > 65535 {
		errs = append(errs, fmt.Errorf("PORT: invalid port"))
	}
	if err := cfg.LogLevel.UnmarshalText([]byte(envOr("LOG_LEVEL", "info"))); err != nil {
		errs = append(errs, fmt.Errorf("LOG_LEVEL: %w", err))
	}

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return cfg, nil
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}
