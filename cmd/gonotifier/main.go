// Command gonotifier is a self-hosted event reminder service: it pushes reminders
// through ntfy, serves an htmx web UI and JSON API, and publishes an iCal feed.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sebibar/gonotifier/internal/config"
	"github.com/sebibar/gonotifier/internal/scheduler"
	"github.com/sebibar/gonotifier/internal/store"
	"github.com/sebibar/gonotifier/internal/web"
)

func main() {
	dryRun := flag.Bool("dry-run", false, "run a single check, log what would be sent, then exit")
	healthcheck := flag.Bool("healthcheck", false, "exit 0 if the running server's /health is ok (for Docker HEALTHCHECK)")
	flag.Parse()

	if *healthcheck {
		os.Exit(checkHealth())
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error:\n"+err.Error())
		os.Exit(1)
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel})))

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		slog.Error("open database", "path", cfg.DBPath, "err", err)
		os.Exit(1)
	}
	defer st.Close()
	sched := scheduler.New(cfg, st)

	if *dryRun {
		n, next, err := sched.Check(true)
		if err != nil {
			slog.Error("dry run failed", "err", err)
			os.Exit(1)
		}
		slog.Info("dry run complete", "would_send", n, "next_reminder", next)
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           logRequests(web.Handler(cfg, st, sched)),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go sched.Run(ctx)

	go func() {
		slog.Info("gonotifier listening", "port", cfg.Port, "db", cfg.DBPath, "export", cfg.ExportDir, "tz", cfg.TZ.String())
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("http server", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(shutdownCtx)
}

// checkHealth asks the server running in this container for /health. The image has
// no shell or wget, so the Docker HEALTHCHECK calls the binary itself.
func checkHealth() int {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	client := http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/health")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "health:", resp.Status)
		return 1
	}
	return 0
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		slog.Debug("http", "method", r.Method, "path", r.URL.Path, "dur", time.Since(start))
	})
}
