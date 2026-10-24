// skycam-web — the relay at skycam.mchugh.au. The observatory Pi pushes its
// frames and night reports here; you watch from anywhere. See
// internal/relay for the protocol.
//
// Configured from the environment (EnvironmentFile=/var/www/skycam/.env):
//
//	PORT              listen port on 127.0.0.1 (default 8996)
//	SKYCAM_TOKEN      bearer token the Pi sends
//	SKYCAM_USER       viewer login (default "james")
//	SKYCAM_PASSWORD   viewer password
//	DATA_DIR          latest frame and night list (default ./data)
//	REPORT_DIR        deepspaceplace's reports/skycam folder ("" to skip)
//	REPORT_BASE_URL   where those reports are served
//	MONITOR_URL, MONITOR_API_KEY   ship WARN+ logs to monitor
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "time/tzdata"

	"github.com/exploded/monitor/pkg/logship"

	"github.com/exploded/skycam/internal/relay"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	text := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})
	log := slog.New(text)
	if u, k := os.Getenv("MONITOR_URL"), os.Getenv("MONITOR_API_KEY"); u != "" && k != "" {
		ship := logship.New(logship.Options{Endpoint: u + "/api/logs", APIKey: k, App: "skycam-web", Level: slog.LevelWarn})
		defer ship.Shutdown()
		log = slog.New(logship.Multi(text, ship))
		log.Warn("skycam-web started, log shipping active", "endpoint", u+"/api/logs")
	}
	slog.SetDefault(log)

	loc, err := time.LoadLocation(env("TZ_OBSERVATORY", "Australia/Melbourne"))
	if err != nil {
		log.Error("timezone", "err", err)
		os.Exit(2)
	}
	cfg := relay.Config{
		Token:         os.Getenv("SKYCAM_TOKEN"),
		User:          env("SKYCAM_USER", "james"),
		Password:      os.Getenv("SKYCAM_PASSWORD"),
		DataDir:       env("DATA_DIR", "data"),
		ReportDir:     os.Getenv("REPORT_DIR"),
		ReportBaseURL: os.Getenv("REPORT_BASE_URL"),
		Loc:           loc,
		Log:           log,
	}
	if cfg.Token == "" || cfg.Password == "" {
		log.Error("SKYCAM_TOKEN and SKYCAM_PASSWORD must be set")
		os.Exit(2)
	}
	srv, err := relay.New(cfg)
	if err != nil {
		log.Error("starting", "err", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	addr := "127.0.0.1:" + env("PORT", "8996")
	if err := relay.Serve(ctx, addr, srv.Handler(), log); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("serving", "err", err)
		os.Exit(1)
	}
}
