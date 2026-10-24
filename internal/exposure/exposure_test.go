package exposure

import (
	"image"
	"testing"
	"time"

	"github.com/exploded/skycam/internal/camera"
)

var night = Limits{MinExposure: time.Millisecond, MaxExposure: 5 * time.Second, MinGain: 50, MaxGain: 50, Target: 70}
var nightGain = Limits{MinExposure: time.Millisecond, MaxExposure: 10 * time.Second, MinGain: 0, MaxGain: 300, Target: 70}
var day = Limits{MinExposure: 32 * time.Microsecond, MaxExposure: 2 * time.Second, MinGain: 0, MaxGain: 0, Target: 100}

func TestNext(t *testing.T) {
	for _, tc := range []struct {
		name      string
		last      camera.Settings
		mean, sat float64
		lim       Limits
		check     func(camera.Settings) bool
	}{
		{"on target holds", camera.Settings{Exposure: time.Second, Gain: 50}, 72, 0, night,
			func(s camera.Settings) bool { return s.Exposure == time.Second && s.Gain == 50 }},
		{"dark night pins at max exposure", camera.Settings{Exposure: 5 * time.Second, Gain: 50}, 8, 0, night,
			func(s camera.Settings) bool { return s.Exposure == 5*time.Second && s.Gain == 50 }},
		{"too bright halves at least", camera.Settings{Exposure: time.Second, Gain: 50}, 200, 0, night,
			func(s camera.Settings) bool {
				return s.Exposure < 500*time.Millisecond && s.Exposure > 250*time.Millisecond
			}},
		{"step is capped at 4x", camera.Settings{Exposure: 10 * time.Millisecond, Gain: 50}, 1, 0, night,
			func(s camera.Settings) bool { return s.Exposure == 40*time.Millisecond }},
		{"saturation forces a big cut", camera.Settings{Exposure: time.Second, Gain: 0}, 90, 0.3, day,
			func(s camera.Settings) bool { return s.Exposure <= 340*time.Millisecond }},
		{"gain rises only after max exposure", camera.Settings{Exposure: 10 * time.Second, Gain: 0}, 20, 0, nightGain,
			func(s camera.Settings) bool { return s.Exposure == 10*time.Second && s.Gain > 0 && s.Gain <= 300 }},
		{"gain falls before exposure does", camera.Settings{Exposure: 10 * time.Second, Gain: 200}, 120, 0, nightGain,
			func(s camera.Settings) bool { return s.Exposure == 10*time.Second && s.Gain < 200 }},
		{"day floor", camera.Settings{Exposure: 40 * time.Microsecond, Gain: 0}, 255, 1, day,
			func(s camera.Settings) bool { return s.Exposure == 32*time.Microsecond }},
	} {
		got := Next(tc.last, tc.mean, tc.sat, tc.lim)
		if !tc.check(got) {
			t.Errorf("%s: got %v gain %d", tc.name, got.Exposure, got.Gain)
		}
	}
}

// TestConverges runs the controller against a linear scene and checks it
// settles within the deadband in a reasonable number of frames.
func TestConverges(t *testing.T) {
	scene := 40.0 // brightness per second at gain factor 1
	s := camera.Settings{Exposure: time.Millisecond, Gain: 0}
	lim := Limits{MinExposure: time.Millisecond, MaxExposure: 20 * time.Second, MinGain: 0, MaxGain: 0, Target: 100}
	for i := 0; i < 20; i++ {
		mean := min(255, scene*s.Exposure.Seconds()*GainFactor(s.Gain))
		s = Next(s, mean, 0, lim)
	}
	mean := scene * s.Exposure.Seconds()
	if mean < 85 || mean > 115 {
		t.Errorf("after 20 frames mean %.1f (exposure %v), want ~100", mean, s.Exposure)
	}
}

func TestMeter(t *testing.T) {
	img := image.NewGray(image.Rect(0, 0, 100, 100))
	for i := range img.Pix {
		img.Pix[i] = 100
	}
	// A bright block that the exclusion should hide.
	for y := 0; y < 20; y++ {
		for x := 0; x < 20; x++ {
			img.Pix[img.PixOffset(x, y)] = 255
		}
	}
	mean, sat := Meter(img, img.Bounds(), []image.Rectangle{image.Rect(0, 0, 20, 20)})
	if mean != 100 || sat != 0 {
		t.Errorf("with exclusion: mean %.1f sat %.3f", mean, sat)
	}
	mean, sat = Meter(img, img.Bounds(), nil)
	if mean <= 100 || sat < 0.03 {
		t.Errorf("without exclusion: mean %.1f sat %.3f", mean, sat)
	}
}
