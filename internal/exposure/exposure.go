// Package exposure is skycam's auto-exposure: measure the last frame, then
// choose exposure and gain for the next one. It is pure — no camera, no
// clock — so its behaviour is pinned down by table tests.
//
// The controller works in "total light" = exposure × linear gain. It steps
// that toward the target brightness, then spends it on exposure first and
// only raises gain once exposure is at its limit, which keeps noise down.
package exposure

import (
	"image"
	"math"
	"time"

	"github.com/exploded/skycam/internal/camera"
)

// Limits is the envelope for one mode (day or night).
type Limits struct {
	MinExposure time.Duration
	MaxExposure time.Duration
	MinGain     int
	MaxGain     int
	Target      float64 // mean brightness, 0–255
}

const (
	// Damping < 1 moves part of the way each frame, so a passing car's
	// headlights or a satellite flare doesn't swing the next frame.
	damping = 0.8
	maxStep = 4.0
	// deadband: within ±10% of the target, leave the settings alone so the
	// timelapse doesn't flicker.
	deadband = 0.10
	// saturatedFrac above which the frame is treated as blown out.
	saturatedFrac = 0.05
)

// GainFactor converts ZWO gain (0.1 dB steps) to a linear factor.
func GainFactor(gain int) float64 { return math.Pow(10, float64(gain)/200) }

// Next returns the settings for the next frame, given the settings the
// last frame used and what it measured.
func Next(last camera.Settings, mean, saturated float64, lim Limits) camera.Settings {
	m := max(mean, 0.5)
	ratio := lim.Target / m
	if saturated > saturatedFrac {
		// The mean under-reports a clipped frame; come down hard.
		ratio = min(ratio, 0.25)
	}
	step := 1.0
	if math.Abs(ratio-1) > deadband {
		step = math.Pow(ratio, damping)
		step = min(max(step, 1/maxStep), maxStep)
	}
	total := last.Exposure.Seconds() * GainFactor(last.Gain) * step
	return Allocate(total, lim)
}

// Allocate splits total light (seconds × linear gain) into exposure and
// gain within lim: exposure first, then gain.
func Allocate(total float64, lim Limits) camera.Settings {
	exp := total / GainFactor(lim.MinGain)
	if exp <= lim.MaxExposure.Seconds() {
		d := time.Duration(exp * float64(time.Second))
		return camera.Settings{Exposure: clampDur(d, lim.MinExposure, lim.MaxExposure), Gain: lim.MinGain}
	}
	g := int(math.Round(200 * math.Log10(total/lim.MaxExposure.Seconds())))
	return camera.Settings{Exposure: lim.MaxExposure, Gain: min(max(g, lim.MinGain), lim.MaxGain)}
}

// Clamp forces s into lim, keeping its total light where it can. Used when
// switching between day and night.
func Clamp(s camera.Settings, lim Limits) camera.Settings {
	return Allocate(s.Exposure.Seconds()*GainFactor(s.Gain), lim)
}

func clampDur(d, lo, hi time.Duration) time.Duration {
	return min(max(d, lo), hi)
}

// Meter returns the mean brightness of img inside region, skipping the
// excluded rectangles, and the fraction of those pixels at 250 or above.
// It samples every other pixel in each direction, which is plenty for a
// mean and quarters the work on the Pi.
func Meter(img *image.Gray, region image.Rectangle, exclude []image.Rectangle) (mean, saturated float64) {
	region = region.Intersect(img.Bounds())
	var sum, n, sat int
	for y := region.Min.Y; y < region.Max.Y; y += 2 {
	row:
		for x := region.Min.X; x < region.Max.X; x += 2 {
			p := image.Pt(x, y)
			for _, r := range exclude {
				if p.In(r) {
					continue row
				}
			}
			v := img.Pix[img.PixOffset(x, y)]
			sum += int(v)
			n++
			if v >= 250 {
				sat++
			}
		}
	}
	if n == 0 {
		return 0, 0
	}
	return float64(sum) / float64(n), float64(sat) / float64(n)
}
