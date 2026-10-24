package capture

import (
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/exploded/skycam/internal/camera/fake"
	"github.com/exploded/skycam/internal/config"
	"github.com/exploded/skycam/internal/observatory"
	"github.com/exploded/skycam/internal/overlay"
	"github.com/exploded/skycam/internal/store"
	"github.com/exploded/skycam/internal/store/db"
)

// starfield writes n synthetic night frames: a dark sky with stars, the
// last third under "cloud" (brighter, few stars).
func starfield(t *testing.T, dir string, start time.Time, n int) {
	t.Helper()
	rng := rand.New(rand.NewPCG(1, 2))
	type star struct{ x, y int }
	stars := make([]star, 400)
	for i := range stars {
		stars[i] = star{rng.IntN(1260) + 10, rng.IntN(940) + 10}
	}
	for f := 0; f < n; f++ {
		g := image.NewGray(image.Rect(0, 0, 1280, 960))
		cloudy := f >= 2*n/3
		bg := 30.0
		if cloudy {
			bg = 60
		}
		for i := range g.Pix {
			g.Pix[i] = uint8(bg + rng.NormFloat64()*2)
		}
		visible := stars
		if cloudy {
			visible = stars[:20]
		}
		for _, s := range visible {
			x := s.x + f/4 // drift, so stars aren't learned as hot pixels
			for dy := -1; dy <= 1; dy++ {
				for dx := -1; dx <= 1; dx++ {
					v := 120.0
					if dx != 0 || dy != 0 {
						v = 70
					}
					g.SetGray(min(x+dx, 1279), s.y+dy, color.Gray{Y: uint8(min(bg+v, 255))})
				}
			}
		}
		name := "image-" + start.Add(time.Duration(f)*20*time.Second).Format("20060102150405") + ".jpg"
		out, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		jpeg.Encode(out, g, &jpeg.Options{Quality: 95})
		out.Close()
	}
}

type openRoof struct{}

func (openRoof) Snapshot(time.Time) observatory.Snapshot {
	return observatory.Snapshot{Roof: "OPEN", Rain: "SAFE", NINA: observatory.NINAState{CameraC: math.NaN()}}
}

func TestLoopReplaysANight(t *testing.T) {
	loc, _ := time.LoadLocation("Australia/Melbourne")
	src := t.TempDir()
	// 01:00 local: the sun is far below −12°, so frames are dark night.
	start := time.Date(2026, 10, 24, 1, 0, 0, 0, loc)
	const n = 90
	starfield(t, src, start, n)

	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.Camera = config.Camera{Driver: "fake", FakeDir: src, Label: "Test cam"}
	if err := EnsureDirs(cfg); err != nil {
		t.Fatal(err)
	}
	sqlDB, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	q := db.New(sqlDB)
	cam, err := fake.Open(src, loc, false)
	if err != nil {
		t.Fatal(err)
	}
	ov, _ := overlay.New()
	var ended string
	l := &Loop{Cfg: cfg, Loc: loc, Cam: cam, Obs: openRoof{}, DB: q, Hub: NewHub(), Overlay: ov,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), NightEnded: func(night string) { ended = night }}
	if err := l.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	if ended != "20261023" {
		t.Errorf("night ended %q, want 20261023 (allsky's date rule)", ended)
	}
	saved, _ := filepath.Glob(filepath.Join(cfg.ImagesDir(), "20261023", "image-*.jpg"))
	if len(saved) != n {
		t.Errorf("saved %d frames, want %d", len(saved), n)
	}
	rows, err := q.FramesForNight(context.Background(), "20261023")
	if err != nil || len(rows) != n {
		t.Fatalf("rows %d err %v", len(rows), err)
	}
	// Clear frames should read clear and cloudy ones cloudy once the
	// baseline has enough samples.
	var clearHits, cloudHits, clearN, cloudN int
	for i, r := range rows {
		if !r.Cloud.Valid {
			continue
		}
		if i < 2*n/3 {
			clearN++
			if r.Cloud.Float64 < 0.3 {
				clearHits++
			}
		} else {
			cloudN++
			if r.Cloud.Float64 > 0.7 {
				cloudHits++
			}
		}
	}
	if clearN == 0 || cloudN == 0 || clearHits < clearN*8/10 || cloudHits < cloudN*8/10 {
		t.Errorf("clear %d/%d, cloudy %d/%d", clearHits, clearN, cloudHits, cloudN)
	}
	if _, err := os.Stat(filepath.Join(cfg.DataDir, "current", "image.jpg")); err != nil {
		t.Errorf("no current image: %v", err)
	}
}

func TestBaseline(t *testing.T) {
	b := NewBaseline(0)
	if !math.IsNaN(b.Value()) {
		t.Fatal("baseline before any samples should be NaN")
	}
	for i := 0; i < 100; i++ {
		b.Add(500)
	}
	for i := 0; i < 50; i++ {
		b.Add(20) // a cloudy spell must not drag the clear baseline down
	}
	if v := b.Value(); v != 500 {
		t.Errorf("baseline %v, want 500", v)
	}
	if v := NewBaseline(321).Value(); v != 321 {
		t.Errorf("fixed baseline %v", v)
	}
}

func TestHubLiveDemand(t *testing.T) {
	h := NewHub()
	now := time.Now()
	if h.WantLive(now, time.Minute) {
		t.Fatal("no viewers yet")
	}
	_, stopTap := h.LiveSubscribe()
	defer stopTap()
	if h.WantLive(now, time.Minute) {
		t.Fatal("a tap (the relay uploader) must not keep live mode on")
	}
	_, stop := h.Watch()
	if !h.WantLive(time.Now(), time.Minute) {
		t.Fatal("a viewer should want live")
	}
	stop()
	if !h.WantLive(time.Now(), time.Minute) {
		t.Fatal("live should linger for the idle period after the last viewer")
	}
	if h.WantLive(time.Now().Add(2*time.Minute), time.Minute) {
		t.Fatal("live should stop after the idle period")
	}
}

func TestLiveWakeOnViewer(t *testing.T) {
	h := NewHub()
	wake := h.LiveWake()
	select {
	case <-wake:
		t.Fatal("woke with no viewer")
	default:
	}
	_, stop := h.Watch()
	defer stop()
	select {
	case <-wake:
	case <-time.After(time.Second):
		t.Fatal("a new viewer did not wake the loop")
	}
	select {
	case <-h.LiveWake():
		t.Fatal("the next wake channel should start open")
	default:
	}
}
