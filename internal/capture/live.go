package capture

import (
	"bytes"
	"context"
	"database/sql"
	"image"
	"image/jpeg"
	"math"
	"time"

	"github.com/exploded/skycam/internal/camera"
	"github.com/exploded/skycam/internal/config"
	"github.com/exploded/skycam/internal/exposure"
	"github.com/exploded/skycam/internal/overlay"
	"github.com/exploded/skycam/internal/store/db"
)

// liveBlockFor is how long live mode stays off after a night session hit
// its cap.
const liveBlockFor = 30 * time.Minute

// runLive streams short exposures while anyone is watching. Stills stop
// meanwhile, so at night the session is capped and logged as an event for
// the report.
func (l *Loop) runLive(ctx context.Context) error {
	info := l.Cam.Info()
	now := time.Now()
	l.updateMode(l.clock.Now())
	m := l.mode()
	lim := limits(m, info)
	lim.MaxExposure = min(lim.MaxExposure, l.Cfg.Live.MaxExposure.D())
	lim.MinExposure = min(lim.MinExposure, lim.MaxExposure)
	// Exposures are short, so let gain go as high as the camera allows.
	if info.MaxGain > 0 {
		lim.MaxGain = info.MaxGain
	}
	s := exposure.Clamp(l.settings, lim)
	if err := l.Cam.StartVideo(s); err != nil {
		return err
	}
	defer l.Cam.StopVideo()
	l.Hub.setLive(true, now)
	defer l.Hub.setLive(false, time.Now())
	l.Log.Info("live view started", "mode", modeName(l.night), "exposure", s.Exposure, "gain", s.Gain)
	defer l.Log.Info("live view stopped")

	if l.DB != nil && l.night {
		id, err := l.DB.StartEvent(ctx, db.StartEventParams{Night: l.lastNightOr(now), Kind: "live", StartedAt: now.UnixMilli()})
		if err == nil {
			defer l.DB.EndEvent(context.Background(), db.EndEventParams{EndedAt: toNull(time.Now().UnixMilli()), ID: id})
		}
	}

	fps := l.Cfg.Relay.LiveFPS
	if fps <= 0 {
		fps = 2
	}
	gap := time.Duration(float64(time.Second) / fps)
	var lastPub time.Time
	ae := liveExposure{s: s}
	for {
		if ctx.Err() != nil {
			return nil
		}
		now := time.Now()
		if !l.Hub.WantLive(now, l.Cfg.Live.IdleStop.D()) {
			return nil
		}
		if l.night && now.Sub(l.liveStart(now)) > l.Cfg.Live.NightLimit.D() {
			l.Log.Info("live view hit its night limit; stills resume", "limit", l.Cfg.Live.NightLimit.D())
			l.liveBlocked = now.Add(liveBlockFor)
			return nil
		}
		f, err := l.Cam.VideoFrame(ctx)
		if err != nil {
			return err
		}
		w, h := f.Image.Bounds().Dx(), f.Image.Bounds().Dy()
		mean, sat := exposure.Meter(f.Image, l.Cfg.Exposure.Meter.Pixels(w, h), pixelRects(l, w, h))
		if next, changed := ae.frame(mean, sat, lim); changed {
			if err := l.Cam.SetVideo(next); err != nil {
				return err
			}
		}
		if now.Sub(lastPub) < gap {
			continue
		}
		lastPub = now
		l.Hub.PublishLive(l.liveJPEG(f))
	}
}

// liveSettleMax caps how many video frames liveExposure waits for a new
// setting to show, in case the scene changed at the same moment.
const liveSettleMax = 10

// liveExposure is auto-exposure for video mode. The SDK hands back frames
// already in flight, so for a few frames after a change the image still
// shows the old setting. Metering those and stepping again overshoots, and
// live view swings between too bright and too dark. So after each change
// it waits until the brightness has moved at least halfway (on a log
// scale) toward what the new setting should give, however many frames
// that takes.
type liveExposure struct {
	s       camera.Settings
	waiting int     // frames waited since the last change; 0 when settled
	from    float64 // mean when the setting changed
	want    float64 // expected log change in the mean
}

func totalLight(s camera.Settings) float64 {
	return s.Exposure.Seconds() * exposure.GainFactor(s.Gain)
}

// frame takes one video frame's measurement and returns the settings to
// use, and whether they changed.
func (e *liveExposure) frame(mean, saturated float64, lim exposure.Limits) (camera.Settings, bool) {
	if e.waiting > 0 {
		moved := math.Log(max(mean, 0.5) / max(e.from, 0.5))
		if moved*e.want < 0.25*e.want*e.want && e.waiting < liveSettleMax {
			e.waiting++
			return e.s, false
		}
		e.waiting = 0
	}
	next := exposure.Next(e.s, mean, saturated, lim)
	if next == e.s {
		return e.s, false
	}
	e.from, e.want = mean, math.Log(totalLight(next)/totalLight(e.s))
	e.s, e.waiting = next, 1
	return next, true
}

func (l *Loop) liveStart(now time.Time) time.Time {
	if on, since := l.Hub.LiveState(); on && !since.IsZero() {
		return since
	}
	return now
}

func (l *Loop) lastNightOr(now time.Time) string {
	if l.lastNight != "" {
		return l.lastNight
	}
	return now.In(l.Loc).Add(-12 * time.Hour).Format("20060102")
}

func (l *Loop) liveJPEG(f camera.Frame) []byte {
	var buf bytes.Buffer
	if l.Overlay != nil {
		txt := overlay.Text{
			Time: f.Start.In(l.Loc), Exposure: f.Exposure, Gain: f.Gain, TempC: f.TempC,
			Camera: l.cameraLabel() + "  LIVE",
		}
		if l.Obs != nil {
			for _, s := range l.Obs.Snapshot(time.Now()).Lines() {
				txt.Status = append(txt.Status, overlay.Line{Text: s.Text, Alert: s.Alert})
			}
		}
		rgba := l.Overlay.Draw(f.Image, txt)
		jpeg.Encode(&buf, rgba, &jpeg.Options{Quality: 75})
		return buf.Bytes()
	}
	jpeg.Encode(&buf, f.Image, &jpeg.Options{Quality: 75})
	return buf.Bytes()
}

func pixelRects(l *Loop, w, h int) []image.Rectangle {
	return config.PixelRects(l.Cfg.Exposure.Exclude, w, h)
}

func toNull(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: true} }
