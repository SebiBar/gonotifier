// Package config loads gonotifier's settings from environment variables.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // embedded zoneinfo fallback, so TZ works without system tzdata

	"github.com/sebibar/gonotifier/internal/events"
)

type Config struct {
	NtfyURL         string // also where users log in: their username and password are checked by ntfy
	DBPath          string
	ExportDir       string // read-only JSON snapshot of each user's events, <username>.json; "" = disabled
	FeedURL         string // public base URL the UI shows calendar feed links on, e.g. https://cal.example.com; "" = this server
	TZ              *time.Location
	CatchupWindow   time.Duration
	DefaultDayStart events.TimeOnly
	Port            int
	LogLevel        slog.Level
}

// Load reads environment variables, validates them and returns the Config.
func Load() (*Config, error) {
	var errs []error
	cfg := &Config{
		NtfyURL: strings.TrimRight(os.Getenv("NTFY_URL"), "/"),
		DBPath:  envOr("DB_PATH", "/data/gonotifier.db"),
	}
	if cfg.NtfyURL == "" {
		errs = append(errs, errors.New("NTFY_URL is required"))
	}
	cfg.ExportDir = envOr("EXPORT_DIR", filepath.Join(filepath.Dir(cfg.DBPath), "exports"))
	if cfg.ExportDir == "off" {
		cfg.ExportDir = ""
	}
	cfg.FeedURL = strings.TrimRight(envOr("FEED_URL", ""), "/")
	if u, err := url.Parse(cfg.FeedURL); cfg.FeedURL != "" && (err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "") {
		errs = append(errs, errors.New("FEED_URL must be an http(s) URL, e.g. https://cal.example.com"))
	}

	loc, err := time.LoadLocation(envOr("TZ", "UTC"))
	if err != nil {
		errs = append(errs, fmt.Errorf("TZ: %w", err))
	}
	cfg.TZ = loc

	if cfg.CatchupWindow, err = time.ParseDuration(envOr("CATCHUP_WINDOW", "24h")); err != nil || cfg.CatchupWindow < 0 {
		errs = append(errs, fmt.Errorf("CATCHUP_WINDOW: invalid duration"))
	}
	if cfg.DefaultDayStart, err = events.ParseTimeOnly(envOr("DAY_START", "00:00")); err != nil {
		errs = append(errs, fmt.Errorf("DAY_START: %w", err))
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
