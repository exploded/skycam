// skycam — the observatory's all-sky camera: capture, overlay, cloud
// detection, timelapse, night report, live view and uploads, in place of
// allsky on the Pi.
//
//	skycam [-config /etc/skycam/config.json] run          capture and serve (the service)
//	skycam [-config ...] process -night 20261023          rebuild one night's products
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	// Embed the zone database; Pi OS Lite has one, but a missing tzdata
	// package must not turn the overlay clock into UTC.
	_ "time/tzdata"

	"github.com/exploded/monitor/pkg/logship"

	"github.com/exploded/skycam/internal/alpaca"
	"github.com/exploded/skycam/internal/astro"
	"github.com/exploded/skycam/internal/camera"
	"github.com/exploded/skycam/internal/camera/asisdk"
	"github.com/exploded/skycam/internal/camera/fake"
	"github.com/exploded/skycam/internal/capture"
	"github.com/exploded/skycam/internal/config"
	"github.com/exploded/skycam/internal/fsutil"
	"github.com/exploded/skycam/internal/night"
	"github.com/exploded/skycam/internal/observatory"
	"github.com/exploded/skycam/internal/overlay"
	"github.com/exploded/skycam/internal/report"
	"github.com/exploded/skycam/internal/store"
	"github.com/exploded/skycam/internal/store/db"
	"github.com/exploded/skycam/internal/upload"
	"github.com/exploded/skycam/internal/web"
)

func main() {
	cfgPath := flag.String("config", "/etc/skycam/config.json", "config file")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: skycam [-config path] run | process -night YYYYMMDD")
		flag.PrintDefaults()
	}
	flag.Parse()
	cmd := flag.Arg(0)
	if cmd == "" {
		cmd = "run"
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(2)
	}
	log, shutdownLog := logger(cfg)
	defer shutdownLog()

	loc, err := cfg.Location()
	if err != nil {
		log.Error("timezone", "err", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	app, err := newApp(ctx, cfg, loc, log)
	if err != nil {
		log.Error("starting", "err", err)
		os.Exit(1)
	}
	defer app.close()

	switch cmd {
	case "run":
		err = app.run(ctx)
	case "process":
		fs := flag.NewFlagSet("process", flag.ExitOnError)
		n := fs.String("night", "", "night date, YYYYMMDD")
		fs.Parse(flag.Args()[1:])
		if *n == "" {
			fs.Usage()
			os.Exit(2)
		}
		_, err = app.proc.Process(ctx, *n)
		app.wait()
	default:
		flag.Usage()
		os.Exit(2)
	}
	if err != nil {
		log.Error(cmd+" failed", "err", err)
		os.Exit(1)
	}
}

// logger ships WARN and above to monitor when configured, so a camera
// failure raises an alert instead of sitting in journalctl.
func logger(cfg config.Config) (*slog.Logger, func()) {
	text := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})
	if cfg.MonitorURL == "" || cfg.MonitorAPIKey == "" {
		l := slog.New(text)
		slog.SetDefault(l)
		return l, func() {}
	}
	ship := logship.New(logship.Options{
		Endpoint: cfg.MonitorURL + "/api/logs",
		APIKey:   cfg.MonitorAPIKey,
		App:      "skycam",
		Level:    slog.LevelWarn,
	})
	l := slog.New(logship.Multi(text, ship))
	slog.SetDefault(l)
	// WARN so it ships: the one line that proves shipping works.
	l.Warn("skycam started, log shipping active", "endpoint", cfg.MonitorURL+"/api/logs")
	return l, ship.Shutdown
}

type app struct {
	cfg     config.Config
	loc     *time.Location
	log     *slog.Logger
	sqlDB   interface{ Close() error }
	q       *db.Queries
	hub     *capture.Hub
	overlay *overlay.Renderer
	proc    *night.Processor
	up      *upload.Uploader
	bg      sync.WaitGroup
}

func newApp(ctx context.Context, cfg config.Config, loc *time.Location, log *slog.Logger) (*app, error) {
	if err := capture.EnsureDirs(cfg); err != nil {
		return nil, err
	}
	sqlDB, err := store.Open(cfg.DBPath())
	if err != nil {
		return nil, err
	}
	ov, err := overlay.New()
	if err != nil {
		sqlDB.Close()
		return nil, err
	}
	a := &app{cfg: cfg, loc: loc, log: log, sqlDB: sqlDB, q: db.New(sqlDB), hub: capture.NewHub(), overlay: ov}
	a.up, err = upload.New(ctx, cfg, loc, a.q, log)
	if err != nil {
		sqlDB.Close()
		return nil, err
	}
	pcfg := cfg
	if cfg.Camera.Driver == "fake" {
		// A replay is usually an old night; retention would delete it the
		// moment it was processed.
		pcfg.RetentionNights, pcfg.ProductDays = 0, 0
	}
	a.proc = &night.Processor{
		Cfg: pcfg, Loc: loc, DB: a.q, Overlay: ov, Log: log,
		Report: a.writeReport,
		Done: func(ctx context.Context, res night.Result) {
			a.bg.Add(1)
			defer a.bg.Done()
			a.up.Night(ctx, res)
		},
	}
	return a, nil
}

func (a *app) close() {
	a.sqlDB.Close()
}

func (a *app) wait() { a.bg.Wait() }

func (a *app) openCamera() (camera.Camera, error) {
	c := a.cfg.Camera
	switch c.Driver {
	case "fake":
		return fake.Open(c.FakeDir, a.loc, c.FakeRealtime)
	case "sdk":
		return asisdk.Open(c.SDKPath, asisdk.Options{Offset: c.Offset, Bandwidth: c.Bandwidth, Flip: c.Flip, Snap: c.Snap})
	case "native":
		return nil, errors.New("camera.driver native is not built yet (phase 3); use sdk")
	}
	return nil, fmt.Errorf("unknown camera driver %q", c.Driver)
}

func (a *app) run(ctx context.Context) error {
	cam, err := a.openCamera()
	if err != nil {
		return fmt.Errorf("opening camera: %w", err)
	}
	defer cam.Close()
	info := cam.Info()
	a.log.Info("camera open", "name", info.Name, "width", info.Width, "height", info.Height,
		"gain", fmt.Sprintf("%d-%d", info.MinGain, info.MaxGain), "exposure", fmt.Sprintf("%v-%v", info.MinExposure, info.MaxExposure))
	label := a.cfg.Camera.Label
	if label == "" {
		label = info.Name
	}

	var obs capture.Observatory
	o := a.cfg.Observatory
	if o.RoofURL != "" || o.RainURL != "" || o.NINAURL != "" {
		p := &observatory.Poller{RoofURL: o.RoofURL, RainURL: o.RainURL, NINAURL: o.NINAURL,
			Interval: o.PollInterval.D(), StaleAfter: o.StaleAfter.D(), Log: a.log}
		go p.Run(ctx)
		obs = p
	}

	srv := &web.Server{Cfg: a.cfg, Loc: a.loc, Hub: a.hub, DB: a.q, Log: a.log, Camera: label}
	h, err := srv.Handler()
	if err != nil {
		return err
	}
	errc := make(chan error, 2)
	go func() { errc <- web.Serve(ctx, a.cfg.Listen, h, a.log) }()
	go a.up.Run(ctx, a.hub)
	a.startAlpaca(ctx)

	// Products for any night the Pi missed at dawn.
	active := ""
	if astro.SunAltitude(time.Now(), a.cfg.Latitude, a.cfg.Longitude) < a.cfg.Exposure.SunAngle {
		active = astro.NightDate(time.Now(), a.loc)
	}
	a.bg.Add(1)
	go func() {
		defer a.bg.Done()
		a.proc.CatchUp(ctx, active)
		a.up.CatchUp(ctx)
	}()

	loop := &capture.Loop{
		Cfg: a.cfg, Loc: a.loc, Cam: cam, Obs: obs, DB: a.q, Hub: a.hub, Overlay: a.overlay, Log: a.log,
		NightEnded: func(n string) {
			a.bg.Add(1)
			go func() {
				defer a.bg.Done()
				if _, err := a.proc.Process(ctx, n); err != nil {
					a.log.Warn("processing night", "night", n, "err", err)
				}
			}()
		},
	}
	go func() { errc <- loop.Run(ctx) }()

	err = <-errc
	if errors.Is(err, context.Canceled) {
		err = nil
	}
	// A replay that finished on its own leaves the web server up so the
	// results can be looked at; anything else shuts down.
	if err == nil && ctx.Err() == nil && a.cfg.Camera.Driver == "fake" {
		a.log.Info("replay finished; still serving until interrupted")
		a.wait()
		<-ctx.Done()
		return nil
	}
	a.wait()
	return err
}

// startAlpaca serves the ObservingConditions device. It is advisory, so a
// port it can't bind is a warning, never a reason to stop capturing.
func (a *app) startAlpaca(ctx context.Context) {
	c := a.cfg.Alpaca
	if c.Listen == "" {
		return
	}
	dev := &alpaca.Server{Src: a.hub, MaxAge: c.MaxAge.D(), BlackLevel: a.cfg.Analysis.BlackLevel}
	go func() {
		if err := web.Serve(ctx, c.Listen, dev.Handler(), a.log); err != nil {
			a.log.Warn("alpaca device stopped", "addr", c.Listen, "err", err)
		}
	}()
	if !c.Discovery {
		return
	}
	_, p, err := net.SplitHostPort(c.Listen)
	port, perr := strconv.Atoi(p)
	if err != nil || perr != nil {
		a.log.Warn("alpaca discovery off: listen has no port", "addr", c.Listen)
		return
	}
	go alpaca.StartDiscovery(ctx, port)
}

// writeReport renders a night's report into its products folder.
func (a *app) writeReport(ctx context.Context, res night.Result) (string, error) {
	frames, err := a.q.FramesForNight(ctx, res.Night)
	if err != nil {
		return "", err
	}
	events, _ := a.q.EventsForNight(ctx, res.Night)
	in := report.Input{
		Night: res.Night, Loc: a.loc, Camera: a.cfg.Camera.Label, Frames: frames, Events: events,
		TimelapseURL: a.up.TimelapseURL(res),
	}
	if in.Camera == "" {
		in.Camera = "all-sky camera"
	}
	if res.Keogram != "" {
		in.KeogramPath = filepath.Join(res.Dir, res.Keogram)
	}
	if res.Startrails != "" {
		in.StartrailsPath = filepath.Join(res.Dir, res.Startrails)
	}
	html, err := report.Render(in)
	if err != nil {
		return "", err
	}
	const name = "report.html"
	return name, fsutil.WriteFileAtomic(filepath.Join(res.Dir, name), html)
}
