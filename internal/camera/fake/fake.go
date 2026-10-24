// Package fake is a Camera that replays a folder of JPEGs — typically a
// night copied from allsky.local/images/<YYYYMMDD>/. It lets the whole
// pipeline (exposure, overlay, timelapse, report) run on a PC.
//
// Each source frame stands for what the real camera saw at RefExposure and
// RefGain. A capture at other settings scales the pixels by the ratio of
// exposure × linear gain, so the auto-exposure loop has something real to
// converge on.
package fake

import (
	"context"
	"fmt"
	"image"
	"image/draw"
	_ "image/jpeg"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/exploded/skycam/internal/camera"
)

// Allsky's night settings, which produced the frames this usually replays.
const (
	RefExposure = 5 * time.Second
	RefGain     = 50
)

var reStamp = regexp.MustCompile(`(\d{14})\.jpe?g$`)

type Camera struct {
	files    []string
	loc      *time.Location
	realtime bool

	mu    sync.Mutex
	next  int
	video *camera.Settings
	info  camera.Info
}

// Open lists dir's JPEGs in name order. With realtime false, captures
// return immediately and the frames carry the timestamps in their file
// names (allsky's image-YYYYMMDDHHMMSS.jpg), so a whole night replays in
// minutes; Now reports that replayed time.
func Open(dir string, loc *time.Location, realtime bool) (*Camera, error) {
	var files []string
	for _, pat := range []string{"*.jpg", "*.jpeg"} {
		m, err := filepath.Glob(filepath.Join(dir, pat))
		if err != nil {
			return nil, err
		}
		files = append(files, m...)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("%w: no JPEGs in %s", camera.ErrNoCamera, dir)
	}
	sort.Strings(files)
	c := &Camera{files: files, loc: loc, realtime: realtime}
	img, err := load(files[0])
	if err != nil {
		return nil, err
	}
	b := img.Bounds()
	c.info = camera.Info{
		Name:        "Fake (" + filepath.Base(dir) + ")",
		Width:       b.Dx(),
		Height:      b.Dy(),
		PixelSizeUM: 3.75,
		BitDepth:    8,
		MinExposure: 32 * time.Microsecond,
		MaxExposure: 1000 * time.Second,
		MinGain:     0,
		MaxGain:     100,
	}
	return c, nil
}

func (c *Camera) Info() camera.Info { return c.info }

// Replaying reports whether frames carry replayed timestamps rather than
// the wall clock.
func (c *Camera) Replaying() bool { return !c.realtime }

// Now is the timestamp of the frame the next capture will return, or the
// wall clock in realtime mode. After the last frame it is one second past
// the last timestamp, so the loop sees time move on.
func (c *Camera) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.realtime {
		return time.Now()
	}
	if c.next < len(c.files) {
		return c.stamp(c.files[c.next])
	}
	return c.stamp(c.files[len(c.files)-1]).Add(time.Second)
}

// Done reports whether every file has been replayed (never in realtime
// mode, which loops).
func (c *Camera) Done() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.realtime && c.next >= len(c.files)
}

func (c *Camera) stamp(path string) time.Time {
	m := reStamp.FindStringSubmatch(filepath.Base(path))
	if m == nil {
		return time.Now()
	}
	t, err := time.ParseInLocation("20060102150405", m[1], c.loc)
	if err != nil {
		return time.Now()
	}
	return t
}

func (c *Camera) Capture(ctx context.Context, s camera.Settings) (camera.Frame, error) {
	c.mu.Lock()
	if c.video != nil {
		c.mu.Unlock()
		return camera.Frame{}, fmt.Errorf("fake: capture while video is running")
	}
	if !c.realtime && c.next >= len(c.files) {
		c.mu.Unlock()
		return camera.Frame{}, fmt.Errorf("fake: replay finished")
	}
	path := c.files[c.next%len(c.files)]
	c.next++
	if c.realtime && c.next >= len(c.files) {
		c.next = 0
	}
	c.mu.Unlock()

	start := time.Now()
	if !c.realtime {
		start = c.stamp(path)
	} else {
		select {
		case <-time.After(s.Exposure):
		case <-ctx.Done():
			return camera.Frame{}, ctx.Err()
		}
	}
	img, err := load(path)
	if err != nil {
		return camera.Frame{}, err
	}
	scale(img, s)
	return camera.Frame{Image: img, Start: start, Exposure: s.Exposure, Gain: s.Gain, TempC: 20}, nil
}

func (c *Camera) StartVideo(s camera.Settings) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.video = &s
	return nil
}

func (c *Camera) SetVideo(s camera.Settings) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.video == nil {
		return fmt.Errorf("fake: video not running")
	}
	c.video = &s
	return nil
}

// VideoFrame returns the most recently captured still, rescaled, at the
// requested exposure (but no faster than 10 fps).
func (c *Camera) VideoFrame(ctx context.Context) (camera.Frame, error) {
	c.mu.Lock()
	if c.video == nil {
		c.mu.Unlock()
		return camera.Frame{}, fmt.Errorf("fake: video not running")
	}
	s := *c.video
	i := c.next - 1
	if i < 0 {
		i = 0
	}
	path := c.files[i%len(c.files)]
	c.mu.Unlock()

	wait := max(s.Exposure, 100*time.Millisecond)
	select {
	case <-time.After(wait):
	case <-ctx.Done():
		return camera.Frame{}, ctx.Err()
	}
	img, err := load(path)
	if err != nil {
		return camera.Frame{}, err
	}
	scale(img, s)
	return camera.Frame{Image: img, Start: time.Now(), Exposure: s.Exposure, Gain: s.Gain, TempC: 20}, nil
}

func (c *Camera) StopVideo() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.video = nil
	return nil
}

func (c *Camera) Close() error { return nil }

func load(path string) (*image.Gray, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	src, _, err := image.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("fake: %s: %w", filepath.Base(path), err)
	}
	if g, ok := src.(*image.Gray); ok {
		return g, nil
	}
	g := image.NewGray(src.Bounds())
	draw.Draw(g, g.Bounds(), src, src.Bounds().Min, draw.Src)
	return g, nil
}

// scale brightens or darkens img as if taken at s instead of the
// reference settings, clipping like a sensor would.
func scale(img *image.Gray, s camera.Settings) {
	ref := RefExposure.Seconds() * math.Pow(10, float64(RefGain)/200)
	got := s.Exposure.Seconds() * math.Pow(10, float64(s.Gain)/200)
	k := got / ref
	if math.Abs(k-1) < 1e-6 {
		return
	}
	var lut [256]uint8
	for i := range lut {
		v := float64(i) * k
		lut[i] = uint8(min(255, math.Round(v)))
	}
	for i, p := range img.Pix {
		img.Pix[i] = lut[p]
	}
}
