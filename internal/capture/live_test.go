package capture

import (
	"context"
	"image"
	"io"
	"log/slog"
	"math"
	"testing"
	"time"

	"github.com/exploded/skycam/internal/camera"
	"github.com/exploded/skycam/internal/config"
	"github.com/exploded/skycam/internal/exposure"
)

// laggingVideo is a video stream whose frames show the settings from lag
// frames ago, as the SDK's in-flight frames do.
type laggingVideo struct {
	lag     int
	scene   float64 // mean brightness per second of exposure at unity gain
	history []camera.Settings
}

func (v *laggingVideo) mean(current camera.Settings) float64 {
	v.history = append(v.history, current)
	i := max(0, len(v.history)-1-v.lag)
	s := v.history[i]
	return min(255, v.scene*s.Exposure.Seconds()*exposure.GainFactor(s.Gain))
}

var liveLim = exposure.Limits{
	MinExposure: 32 * time.Microsecond, MaxExposure: 500 * time.Millisecond,
	MinGain: 0, MaxGain: 100, Target: 100,
}

// run steps auto-exposure over n frames and returns each frame's mean.
func run(step func(mean float64) camera.Settings, start camera.Settings, v *laggingVideo, n int) []float64 {
	s := start
	means := make([]float64, n)
	for i := range means {
		means[i] = v.mean(s)
		s = step(means[i])
	}
	return means
}

// reversals counts how often the error from target changes sign by more
// than the deadband: each one is the picture flipping between too bright
// and too dark.
func reversals(means []float64, target float64) int {
	n, last := 0, 0
	for _, m := range means {
		sign := 0
		switch {
		case m > target*1.1:
			sign = 1
		case m < target*0.9:
			sign = -1
		}
		if sign != 0 && last != 0 && sign != last {
			n++
		}
		if sign != 0 {
			last = sign
		}
	}
	return n
}

func TestLiveExposureSettles(t *testing.T) {
	// The real lag isn't known, so it must settle whatever it is.
	for lag := 0; lag <= 8; lag++ {
		for _, start := range []camera.Settings{
			{Exposure: 10 * time.Millisecond},  // 20x too dark, as after a long still
			{Exposure: 500 * time.Millisecond}, // clipped, as at a bright dawn
		} {
			v := &laggingVideo{lag: lag, scene: 500}
			ae := liveExposure{s: start}
			means := run(func(m float64) camera.Settings { s, _ := ae.frame(m, 0, liveLim); return s }, start, v, 80)

			if r := reversals(means, liveLim.Target); r > 1 {
				t.Errorf("lag %d from %v: flipped between too bright and too dark %d times: %.0f", lag, start.Exposure, r, means)
			}
			for _, m := range means[60:] {
				if m < liveLim.Target*0.85 || m > liveLim.Target*1.15 {
					t.Errorf("lag %d from %v: did not settle near %v: last frames %.0f", lag, start.Exposure, liveLim.Target, means[60:])
					break
				}
			}
		}
	}
}

// evenScene is a camera looking at a uniform scene: every pixel is scene ×
// exposure × linear gain, clipped at 255.
type evenScene struct {
	scene float64
	video camera.Settings
}

func (c *evenScene) Info() camera.Info {
	return camera.Info{Width: 64, Height: 48, MinExposure: 32 * time.Microsecond, MaxExposure: time.Hour, MaxGain: 100}
}

func (c *evenScene) frame(s camera.Settings) camera.Frame {
	g := image.NewGray(image.Rect(0, 0, 64, 48))
	v := uint8(min(255, math.Round(c.scene*totalLight(s))))
	for i := range g.Pix {
		g.Pix[i] = v
	}
	return camera.Frame{Image: g, Start: time.Now(), Exposure: s.Exposure, Gain: s.Gain, TempC: camera.NoTemp}
}

func (c *evenScene) Capture(ctx context.Context, s camera.Settings) (camera.Frame, error) {
	return c.frame(s), nil
}
func (c *evenScene) StartVideo(s camera.Settings) error { c.video = s; return nil }
func (c *evenScene) SetVideo(s camera.Settings) error   { c.video = s; return nil }
func (c *evenScene) VideoFrame(ctx context.Context) (camera.Frame, error) {
	return c.frame(c.video), nil
}
func (c *evenScene) StopVideo() error { return nil }
func (c *evenScene) Close() error     { return nil }

func TestLiveHandsExposureToStills(t *testing.T) {
	// The loop starts at either the night maximum (blown out here) or the
	// day mid-range (far too dark). Live view meters its way to the right
	// setting; the next still must start from that, not the old one.
	cfg := config.Default()
	cfg.Live.IdleStop = config.Duration(300 * time.Millisecond)
	cam := &evenScene{scene: 700}
	l := &Loop{Cfg: cfg, Loc: time.UTC, Cam: cam, Hub: NewHub(),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), clock: wallClock{}}
	l.Hub.Demand()
	if err := l.runLive(context.Background()); err != nil {
		t.Fatal(err)
	}
	target := l.mode().Target
	if got := cam.frame(l.settings).Image.Pix[0]; math.Abs(float64(got)-target) > target*0.15 {
		t.Errorf("%s still after live: %v gives mean %d, want about %v", modeName(l.night), l.settings, got, target)
	}
}

func TestLiveExposureWithoutSettlingSwings(t *testing.T) {
	// The old loop metered every frame. With the same lag it overshoots
	// back and forth, which is what showed in live view.
	start := camera.Settings{Exposure: 10 * time.Millisecond}
	v := &laggingVideo{lag: 3, scene: 500}
	s := start
	means := run(func(m float64) camera.Settings { s = exposure.Next(s, m, 0, liveLim); return s }, start, v, 60)
	if r := reversals(means, liveLim.Target); r < 3 {
		t.Errorf("expected the unsettled loop to swing; %d reversals: %.0f", r, means)
	}
}
