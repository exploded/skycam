// Package capture runs the camera: choose day or night settings, expose,
// measure, draw the overlay, save, and hand the frame on. It also runs the
// live view, which borrows the camera from still capture while someone is
// watching.
package capture

import (
	"bytes"
	"context"

	"fmt"
	"image"
	"image/jpeg"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/exploded/skycam/internal/analysis"
	"github.com/exploded/skycam/internal/astro"
	"github.com/exploded/skycam/internal/camera"
	"github.com/exploded/skycam/internal/config"
	"github.com/exploded/skycam/internal/exposure"
	"github.com/exploded/skycam/internal/fsutil"
	"github.com/exploded/skycam/internal/observatory"
	"github.com/exploded/skycam/internal/overlay"
	"github.com/exploded/skycam/internal/store"
	"github.com/exploded/skycam/internal/store/db"
)

// Record is one still after processing.
type Record struct {
	Time      time.Time // exposure start, observatory zone
	Night     string    // YYYYMMDD
	Mode      string    // day | night
	File      string    // name under images/<night>/, "" if not kept
	Exposure  time.Duration
	Gain      int
	TempC     float64
	SunAlt    float64
	MoonAlt   float64
	MoonIllum float64
	Mean      float64
	Band      float64
	SkyLevel  float64
	SkyMag    float64
	Stars     int
	Cloud     float64 // 0–1, NaN unknown
	Obs       observatory.Snapshot
	JPEG      []byte // the overlaid frame as saved
}

// Sky is the cloud classification for display.
func (r *Record) Sky() string { return analysis.Sky(r.Cloud) }

// Observatory is where the loop reads the roof, rain and N.I.N.A. state.
type Observatory interface {
	Snapshot(now time.Time) observatory.Snapshot
}

// Clock is the loop's sense of time. The real one is the wall clock; the
// fake camera's replay supplies its own.
type Clock interface {
	Now() time.Time
	Sleep(ctx context.Context, d time.Duration) error
}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }
func (wallClock) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// replayer is the fake camera in replay mode.
type replayer interface {
	Replaying() bool
	Now() time.Time
	Done() bool
}

type replayClock struct{ r replayer }

func (c replayClock) Now() time.Time                                   { return c.r.Now() }
func (c replayClock) Sleep(ctx context.Context, d time.Duration) error { return ctx.Err() }

// Loop owns the camera.
type Loop struct {
	Cfg     config.Config
	Loc     *time.Location
	Cam     camera.Camera
	Obs     Observatory // may be nil
	DB      *db.Queries // may be nil
	Hub     *Hub
	Overlay *overlay.Renderer
	Log     *slog.Logger
	// NightEnded is called when dawn ends a night, with that night's date.
	// It runs on the capture goroutine, so it must hand the work off and
	// return at once.
	NightEnded func(night string)

	clock     Clock
	settings  camera.Settings
	night     bool
	started   bool
	hot       *analysis.HotMap
	baseline  *Baseline
	lastNight string
	lastSat   float64
	// liveBlocked holds live mode off after a night session hit its cap,
	// so a viewer left watching can't keep the stills from resuming.
	liveBlocked time.Time
}

const maxFailures = 10

// Run captures until ctx ends. It returns an error after repeated camera
// failures so systemd can restart the service (and the USB device with it).
func (l *Loop) Run(ctx context.Context) error {
	l.clock = wallClock{}
	if r, ok := l.Cam.(replayer); ok && r.Replaying() {
		l.clock = replayClock{r}
	}
	info := l.Cam.Info()
	l.hot = analysis.NewHotMap(info.Width, info.Height)
	l.baseline = NewBaseline(l.Cfg.Analysis.ClearStars)
	if l.DB != nil {
		l.seedBaseline(ctx)
	}
	failures := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		if r, ok := l.Cam.(replayer); ok && r.Done() {
			l.endNight()
			return nil
		}
		now := l.clock.Now()
		if l.Hub != nil && now.After(l.liveBlocked) && l.Hub.WantLive(now, l.Cfg.Live.IdleStop.D()) {
			if err := l.runLive(ctx); err != nil && ctx.Err() == nil {
				l.Log.Warn("live view stopped", "err", err)
				// Don't spin straight back into a failing live mode.
				l.clock.Sleep(ctx, 5*time.Second)
			}
			continue
		}
		start := now
		if err := l.still(ctx, now); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			failures++
			l.Log.Warn("capture failed", "err", err, "failures", failures)
			if failures >= maxFailures {
				return fmt.Errorf("camera failed %d times in a row: %w", failures, err)
			}
			l.clock.Sleep(ctx, time.Duration(failures)*2*time.Second)
			continue
		}
		failures = 0
		l.clock.Sleep(ctx, l.mode().Interval.D()-time.Since(start))
	}
}

func (l *Loop) mode() config.Mode {
	if l.night {
		return l.Cfg.Exposure.Night
	}
	return l.Cfg.Exposure.Day
}

func limits(m config.Mode, info camera.Info) exposure.Limits {
	lim := exposure.Limits{
		MinExposure: m.MinExposure.D(), MaxExposure: m.MaxExposure.D(),
		MinGain: m.MinGain, MaxGain: m.MaxGain, Target: m.Target,
	}
	if info.MinExposure > 0 {
		lim.MinExposure = max(lim.MinExposure, info.MinExposure)
	}
	if info.MaxExposure > 0 {
		lim.MaxExposure = min(lim.MaxExposure, info.MaxExposure)
	}
	if info.MaxGain > 0 {
		lim.MaxGain = min(lim.MaxGain, info.MaxGain)
		lim.MinGain = min(lim.MinGain, lim.MaxGain)
	}
	return lim
}

// updateMode switches day/night on the sun's altitude and fires the
// end-of-night hook at dawn.
func (l *Loop) updateMode(now time.Time) {
	night := astro.SunAltitude(now, l.Cfg.Latitude, l.Cfg.Longitude) < l.Cfg.Exposure.SunAngle
	lim := limits(l.modeFor(night), l.Cam.Info())
	switch {
	case !l.started:
		l.started = true
		l.night = night
		// Start mid-range; the controller converges in a few frames.
		l.settings = exposure.Allocate(math.Sqrt(lim.MinExposure.Seconds()*lim.MaxExposure.Seconds())*exposure.GainFactor(lim.MinGain), lim)
		if night {
			l.settings = camera.Settings{Exposure: lim.MaxExposure, Gain: lim.MinGain}
		}
		l.Log.Info("capture starting", "mode", modeName(night), "exposure", l.settings.Exposure, "gain", l.settings.Gain)
	case night != l.night:
		l.Log.Info("switching mode", "to", modeName(night))
		wasNight := l.night
		l.night = night
		l.settings = exposure.Clamp(l.settings, lim)
		if wasNight && !night {
			l.endNight()
		}
	}
}

func (l *Loop) modeFor(night bool) config.Mode {
	if night {
		return l.Cfg.Exposure.Night
	}
	return l.Cfg.Exposure.Day
}

func modeName(night bool) string {
	if night {
		return "night"
	}
	return "day"
}

func (l *Loop) endNight() {
	if l.lastNight == "" || l.NightEnded == nil {
		return
	}
	n := l.lastNight
	l.lastNight = ""
	l.Log.Info("night ended", "night", n)
	l.NightEnded(n)
}

// still takes, processes and stores one frame.
func (l *Loop) still(ctx context.Context, now time.Time) error {
	l.updateMode(now)
	m := l.mode()
	lim := limits(m, l.Cam.Info())
	f, err := l.Cam.Capture(ctx, l.settings)
	if err != nil {
		return err
	}
	rec, err := l.process(f, m.Save)
	if err != nil {
		return err
	}
	l.settings = exposure.Next(l.settings, rec.Mean, l.lastSat, lim)
	if l.night && m.Save {
		l.lastNight = rec.Night
	}
	if l.Hub != nil {
		l.Hub.Publish(rec)
	}
	return nil
}

// process measures f, draws the overlay, writes the JPEG and the row.
func (l *Loop) process(f camera.Frame, save bool) (*Record, error) {
	cfg := l.Cfg
	g := f.Image
	b := g.Bounds()
	w, h := b.Dx(), b.Dy()
	t := f.Start.In(l.Loc)

	rec := &Record{
		Time: t, Night: astro.NightDate(t, l.Loc), Mode: modeName(l.night),
		Exposure: f.Exposure, Gain: f.Gain, TempC: f.TempC,
		SunAlt:    astro.SunAltitude(t, cfg.Latitude, cfg.Longitude),
		MoonAlt:   astro.MoonAltitude(t, cfg.Latitude, cfg.Longitude),
		MoonIllum: astro.MoonIllumination(t),
		Cloud:     math.NaN(),
		Obs:       observatory.Snapshot{Roof: observatory.Unknown, Rain: observatory.Unknown, NINA: observatory.NINAState{CameraC: math.NaN()}},
	}
	if l.Obs != nil {
		rec.Obs = l.Obs.Snapshot(time.Now())
	}

	exclude := config.PixelRects(cfg.Exposure.Exclude, w, h)
	rec.Mean, l.lastSat = exposure.Meter(g, cfg.Exposure.Meter.Pixels(w, h), exclude)

	sky := cfg.Analysis.Sky.Pixels(w, h)
	bg := analysis.EstimateBackground(g, sky)
	rec.SkyLevel = bg.Median()
	rec.SkyMag = analysis.SkyMag(rec.SkyLevel, cfg.Analysis.BlackLevel, f.Exposure.Seconds(), exposure.GainFactor(f.Gain), cfg.Analysis.SkyZeroPoint)
	stars := analysis.FindStars(g, sky, bg, analysis.StarOptions{Sigma: cfg.Analysis.StarSigma, MinDelta: 4, Exclude: exclude, Hot: l.hot})
	l.hot.Update(stars)
	rec.Stars = len(stars)
	// Cloud only means something with the roof known open and the sky
	// properly dark. Anything else stays unknown — never "clear".
	if l.night && rec.Obs.RoofOpen() && rec.SunAlt < -12 {
		l.baseline.Add(rec.Stars)
		rec.Cloud = analysis.CloudCover(rec.Stars, l.baseline.Value())
	}

	var out image.Image = g
	if cfg.Overlay.Enabled && l.Overlay != nil {
		txt := overlay.Text{Time: t, Exposure: f.Exposure, Gain: f.Gain, TempC: f.TempC, Camera: l.cameraLabel()}
		if cfg.Overlay.Observatory {
			txt.Status = statusLines(rec)
		}
		out = l.Overlay.Draw(g, txt)
	}
	rec.Band = analysis.BandLuminance(out, cfg.Analysis.BandTop, cfg.Analysis.BandBottom)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, out, &jpeg.Options{Quality: cfg.JPEGQuality}); err != nil {
		return nil, fmt.Errorf("encoding frame: %w", err)
	}
	rec.JPEG = buf.Bytes()

	if save {
		rec.File = "image-" + t.Format("20060102150405") + ".jpg"
		path := filepath.Join(cfg.ImagesDir(), rec.Night, rec.File)
		if err := fsutil.WriteFileAtomic(path, rec.JPEG); err != nil {
			return nil, err
		}
	}
	// The latest frame, for /image.jpg and anything that wants a file.
	if err := fsutil.WriteFileAtomic(filepath.Join(cfg.DataDir, "current", "image.jpg"), rec.JPEG); err != nil {
		l.Log.Warn("writing current image", "err", err)
	}
	if l.DB != nil {
		l.insert(rec)
	}
	return rec, nil
}

func statusLines(rec *Record) []overlay.Line {
	var lines []overlay.Line
	for _, s := range rec.Obs.Lines() {
		lines = append(lines, overlay.Line{Text: s.Text, Alert: s.Alert})
	}
	if !math.IsNaN(rec.Cloud) {
		lines = append(lines, overlay.Line{Text: fmt.Sprintf("Sky: %s (%d stars)", rec.Sky(), rec.Stars)})
	}
	return lines
}

func (l *Loop) cameraLabel() string {
	if l.Cfg.Camera.Label != "" {
		return l.Cfg.Camera.Label
	}
	return l.Cam.Info().Name
}

func (l *Loop) insert(r *Record) {
	err := l.DB.InsertFrame(context.Background(), db.InsertFrameParams{
		TakenAt: r.Time.UnixMilli(), Night: r.Night, Mode: r.Mode, File: r.File,
		ExposureUs: r.Exposure.Microseconds(), Gain: int64(r.Gain), TempC: store.Float(r.TempC),
		SunAlt: r.SunAlt, MoonAlt: r.MoonAlt, MoonIllum: r.MoonIllum,
		Mean: r.Mean, BandLum: nanZero(r.Band), SkyLevel: nanZero(r.SkyLevel), SkyMag: store.Float(r.SkyMag),
		Stars: int64(r.Stars), Cloud: store.Float(r.Cloud),
		Roof: r.Obs.Roof, Rain: r.Obs.Rain, NinaMount: r.Obs.NINA.Mount, NinaFilter: r.Obs.NINA.Filter,
	})
	if err != nil {
		l.Log.Warn("storing frame", "err", err)
	}
}

func nanZero(v float64) float64 {
	if math.IsNaN(v) {
		return 0
	}
	return v
}

func (l *Loop) seedBaseline(ctx context.Context) {
	since := time.Now().AddDate(0, 0, -l.Cfg.RetentionNights).UnixMilli()
	counts, err := l.DB.ClearSkyStars(ctx, db.ClearSkyStarsParams{TakenAt: since, Limit: baselineWindow})
	if err != nil {
		l.Log.Warn("reading star baseline", "err", err)
		return
	}
	for i := len(counts) - 1; i >= 0; i-- {
		l.baseline.Add(int(counts[i]))
	}
}

// Baseline is the clear-sky star count: the 90th percentile of recent
// dark, roof-open frames. Cloudy frames sit below it and don't drag it
// down; a fixed value from config overrides it.
type Baseline struct {
	mu     sync.Mutex
	fixed  int
	recent []int
}

const (
	baselineWindow = 2000 // about one long night of frames
	baselineMin    = 30   // samples before the baseline is trusted
)

func NewBaseline(fixed int) *Baseline { return &Baseline{fixed: fixed} }

func (b *Baseline) Add(stars int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.recent = append(b.recent, stars)
	if len(b.recent) > baselineWindow {
		b.recent = b.recent[len(b.recent)-baselineWindow:]
	}
}

// Value is the baseline, or NaN until there are enough samples.
func (b *Baseline) Value() float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fixed > 0 {
		return float64(b.fixed)
	}
	if len(b.recent) < baselineMin {
		return math.NaN()
	}
	s := append([]int(nil), b.recent...)
	sort.Ints(s)
	return float64(s[len(s)*9/10])
}

// ensure the image dirs exist up front so the first save can't race the
// web server's listing.
func EnsureDirs(cfg config.Config) error {
	for _, d := range []string{cfg.ImagesDir(), cfg.ProductsDir(), filepath.Join(cfg.DataDir, "current")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	return nil
}
