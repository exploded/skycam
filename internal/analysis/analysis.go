// Package analysis measures each frame: skyq's band luminance, a star
// count, and a normalised sky brightness. Together with the roof state
// they give the cloud estimate. All of it is advisory — nothing here may
// gate the roof (skyq SPEC §8).
package analysis

import (
	"image"
	"image/color"
	"math"
	"sort"
)

// Default crop band, from skyq (full width, 6–30% of height).
const (
	DefaultBandTop    = 0.06
	DefaultBandBottom = 0.30
)

const (
	gridW = 64
	gridH = 16
)

// BandLuminance is skyq's luminance metric (internal/allsky/luminance.go),
// ported so the numbers carry on across the switch from allsky: the mean
// Rec.601 luma of the band between topFrac and bottomFrac of the height,
// box-averaged over a 64×16 grid. Measure it on the frame as saved (with
// the overlay), as skyq does.
func BandLuminance(img image.Image, topFrac, bottomFrac float64) float64 {
	b := img.Bounds()
	y0 := b.Min.Y + int(float64(b.Dy())*topFrac)
	y1 := b.Min.Y + int(float64(b.Dy())*bottomFrac)
	if y1 <= y0 {
		return math.NaN()
	}
	luma := lumaFunc(img)
	var total float64
	cells := 0
	for gy := 0; gy < gridH; gy++ {
		cy0 := y0 + (y1-y0)*gy/gridH
		cy1 := y0 + (y1-y0)*(gy+1)/gridH
		for gx := 0; gx < gridW; gx++ {
			cx0 := b.Min.X + b.Dx()*gx/gridW
			cx1 := b.Min.X + b.Dx()*(gx+1)/gridW
			var sum float64
			n := 0
			for y := cy0; y < cy1; y++ {
				for x := cx0; x < cx1; x++ {
					sum += luma(x, y)
					n++
				}
			}
			if n > 0 {
				total += sum / float64(n)
				cells++
			}
		}
	}
	if cells == 0 {
		return math.NaN()
	}
	return total / float64(cells)
}

// lumaFunc returns a fast per-pixel luma reader for the common image types
// and falls back to image.At for the rest. Values match skyq's: Rec.601 on
// the 8-bit channels.
func lumaFunc(img image.Image) func(x, y int) float64 {
	switch m := img.(type) {
	case *image.Gray:
		return func(x, y int) float64 { return float64(m.Pix[m.PixOffset(x, y)]) }
	case *image.RGBA:
		return func(x, y int) float64 {
			i := m.PixOffset(x, y)
			return 0.299*float64(m.Pix[i]) + 0.587*float64(m.Pix[i+1]) + 0.114*float64(m.Pix[i+2])
		}
	case *image.YCbCr:
		return func(x, y int) float64 {
			c := m.YCbCrAt(x, y)
			r, g, b := color.YCbCrToRGB(c.Y, c.Cb, c.Cr)
			return 0.299*float64(r) + 0.587*float64(g) + 0.114*float64(b)
		}
	}
	return func(x, y int) float64 {
		r, g, b, _ := img.At(x, y).RGBA()
		return 0.299*float64(r>>8) + 0.587*float64(g>>8) + 0.114*float64(b>>8)
	}
}

// Background is a coarse map of sky level and noise, one cell per block.
type Background struct {
	block  int
	cols   int
	rows   int
	origin image.Point
	level  []float64
	sigma  []float64
}

const bgBlock = 32

// EstimateBackground takes the median and a MAD-based sigma of each block
// of region. Medians ignore stars, so this is the sky between them.
func EstimateBackground(img *image.Gray, region image.Rectangle) *Background {
	region = region.Intersect(img.Bounds())
	bg := &Background{
		block:  bgBlock,
		cols:   (region.Dx() + bgBlock - 1) / bgBlock,
		rows:   (region.Dy() + bgBlock - 1) / bgBlock,
		origin: region.Min,
	}
	bg.level = make([]float64, bg.cols*bg.rows)
	bg.sigma = make([]float64, bg.cols*bg.rows)
	var hist [256]int
	for r := 0; r < bg.rows; r++ {
		for c := 0; c < bg.cols; c++ {
			cell := image.Rect(region.Min.X+c*bgBlock, region.Min.Y+r*bgBlock,
				region.Min.X+(c+1)*bgBlock, region.Min.Y+(r+1)*bgBlock).Intersect(region)
			hist = [256]int{}
			n := 0
			for y := cell.Min.Y; y < cell.Max.Y; y++ {
				row := img.Pix[img.PixOffset(cell.Min.X, y):img.PixOffset(cell.Max.X, y)]
				for _, v := range row {
					hist[v]++
				}
				n += len(row)
			}
			med := histQuantile(&hist, n, 0.5)
			// MAD from the same histogram: count by |v − med|.
			var dev [256]int
			for v, k := range hist {
				d := v - int(med)
				if d < 0 {
					d = -d
				}
				dev[d] += k
			}
			mad := histQuantile(&dev, n, 0.5)
			i := r*bg.cols + c
			bg.level[i] = med
			// 1.4826 × MAD estimates σ for Gaussian noise. Floor it: an
			// 8-bit dark sky can have a MAD of zero.
			bg.sigma[i] = max(1.4826*mad, 0.8)
		}
	}
	return bg
}

func histQuantile(h *[256]int, n int, q float64) float64 {
	if n == 0 {
		return 0
	}
	target := int(q * float64(n))
	acc := 0
	for v, k := range h {
		acc += k
		if acc > target {
			return float64(v)
		}
	}
	return 255
}

func (bg *Background) at(x, y int) (level, sigma float64) {
	c := min(max((x-bg.origin.X)/bg.block, 0), bg.cols-1)
	r := min(max((y-bg.origin.Y)/bg.block, 0), bg.rows-1)
	i := r*bg.cols + c
	return bg.level[i], bg.sigma[i]
}

// Median is the median of the block medians: the typical sky level.
func (bg *Background) Median() float64 {
	if len(bg.level) == 0 {
		return math.NaN()
	}
	s := append([]float64(nil), bg.level...)
	sort.Float64s(s)
	return s[len(s)/2]
}

// Noise is the median block sigma.
func (bg *Background) Noise() float64 {
	if len(bg.sigma) == 0 {
		return math.NaN()
	}
	s := append([]float64(nil), bg.sigma...)
	sort.Float64s(s)
	return s[len(s)/2]
}

// Star is one detection.
type Star struct {
	X, Y int
	Peak float64 // above background
}

// StarOptions tune detection.
type StarOptions struct {
	Sigma float64 // threshold above background, in σ
	// MinDelta is a floor on the threshold in ADU, so a flat, noiseless
	// frame doesn't count JPEG ringing.
	MinDelta float64
	// MaxArea rejects blobs bigger than a star: lights, the moon, roof
	// edges. Counted in a 7×7 box round the peak.
	MaxArea int
	Exclude []image.Rectangle
	// Hot, if set, masks pixels known to be hot.
	Hot *HotMap
	// DarkFrac skips background blocks below this fraction of the median
	// sky level (default 0.6).
	DarkFrac float64
}

func (o StarOptions) darkFrac() float64 {
	if o.DarkFrac <= 0 {
		return 0.6
	}
	return o.DarkFrac
}

// FindStars returns local maxima in region that stand Sigma above the
// local background, have at least one neighbour above half that (a hot
// pixel is a lone pixel; a star is spread by the lens), and are small.
func FindStars(img *image.Gray, region image.Rectangle, bg *Background, o StarOptions) []Star {
	region = region.Intersect(img.Bounds()).Inset(3)
	if o.MaxArea == 0 {
		o.MaxArea = 20
	}
	// With the roof open the frame holds the telescope, the roof edge and
	// trees, all much darker than the sky and full of edges and LEDs that
	// look like stars. Blocks far below the typical sky level are skipped.
	// A uniformly dark frame (moonless sky, or the shut roof) has a sky
	// level too low for the ratio to mean anything, so it skips nothing.
	skyLevel := bg.Median()
	darkCut := -1.0
	if skyLevel >= 8 {
		darkCut = o.darkFrac() * skyLevel
	}
	var stars []Star
	stride := img.Stride
	for y := region.Min.Y; y < region.Max.Y; y++ {
	pixel:
		for x := region.Min.X; x < region.Max.X; x++ {
			i := img.PixOffset(x, y)
			v := float64(img.Pix[i])
			level, sigma := bg.at(x, y)
			if level < darkCut {
				continue
			}
			thr := max(o.Sigma*sigma, o.MinDelta)
			if v-level < thr {
				continue
			}
			// Local maximum: strictly above the earlier neighbours, at least
			// equal to the later ones, so a flat-topped star counts once.
			p := img.Pix
			if !(v > float64(p[i-stride-1]) && v > float64(p[i-stride]) && v > float64(p[i-stride+1]) && v > float64(p[i-1]) &&
				v >= float64(p[i+1]) && v >= float64(p[i+stride-1]) && v >= float64(p[i+stride]) && v >= float64(p[i+stride+1])) {
				continue
			}
			pt := image.Pt(x, y)
			for _, r := range o.Exclude {
				if pt.In(r) {
					continue pixel
				}
			}
			if o.Hot != nil && o.Hot.IsHot(x, y) {
				continue
			}
			half := level + thr/2
			spread := 0
			for _, j := range []int{i - stride, i + stride, i - 1, i + 1} {
				if float64(p[j]) > half {
					spread++
				}
			}
			if spread == 0 {
				continue
			}
			area := 0
			for yy := y - 3; yy <= y+3; yy++ {
				row := img.PixOffset(x-3, yy)
				for k := 0; k < 7; k++ {
					if float64(p[row+k]) > half {
						area++
					}
				}
			}
			if area > o.MaxArea {
				continue
			}
			stars = append(stars, Star{X: x, Y: y, Peak: v - level})
		}
	}
	return stars
}

// HotMap learns pixels that fire in most frames. Stars drift across the
// sensor; a hot pixel stays put. Each frame's detections update a decaying
// rate per pixel; a pixel above half is masked.
type HotMap struct {
	w, h int
	rate []float32
}

func NewHotMap(w, h int) *HotMap { return &HotMap{w: w, h: h, rate: make([]float32, w*h)} }

const hotDecay = 0.95

// Update decays every pixel's rate and bumps the detected ones. Stars are
// tested before the update, so a frame's own detections never mask
// themselves.
func (m *HotMap) Update(stars []Star) {
	for i := range m.rate {
		m.rate[i] *= hotDecay
	}
	for _, s := range stars {
		if s.X >= 0 && s.Y >= 0 && s.X < m.w && s.Y < m.h {
			m.rate[s.Y*m.w+s.X] += 1 - hotDecay
		}
	}
}

func (m *HotMap) IsHot(x, y int) bool {
	if x < 0 || y < 0 || x >= m.w || y >= m.h {
		return false
	}
	return m.rate[y*m.w+x] > 0.5
}

// SkyMag converts the sky level to a relative magnitude per pixel: the
// brightness the sensor would see in one second at unity gain, on a log
// scale. Lower is brighter, as for an SQM, but it is not calibrated to
// one — use it for trends within this camera only.
func SkyMag(level, blackLevel float64, exposureSec float64, gainFactor float64, zeroPoint float64) float64 {
	flux := (level - blackLevel) / (exposureSec * gainFactor)
	if flux <= 0 || math.IsNaN(flux) || math.IsInf(flux, 0) {
		return math.NaN()
	}
	return zeroPoint - 2.5*math.Log10(flux)
}

// CloudCover estimates the cloudy fraction (0–1) from the star count and a
// clear-sky baseline. NaN when there is no baseline.
func CloudCover(stars int, clearStars float64) float64 {
	if clearStars <= 0 || math.IsNaN(clearStars) {
		return math.NaN()
	}
	return min(max(1-float64(stars)/clearStars, 0), 1)
}

// Sky classifies cloud cover for display.
func Sky(cover float64) string {
	switch {
	case math.IsNaN(cover):
		return "unknown"
	case cover < 0.3:
		return "clear"
	case cover < 0.7:
		return "partly cloudy"
	default:
		return "cloudy"
	}
}

// The cloud-onset rule other tools rely on (skyq reads it from the sky
// API): the first of OnsetRun consecutive estimates at OnsetCloud or more.
const (
	OnsetCloud = 0.5
	OnsetRun   = 3
)

// CloudOnset returns the index of the frame where cloud set in, or -1 if
// it never did. An unknown estimate (NaN: roof shut, twilight) breaks a
// run rather than extending it, because unknown is not cloudy either.
func CloudOnset(cover []float64) int {
	run := 0
	for i, c := range cover {
		if math.IsNaN(c) || c < OnsetCloud {
			run = 0
			continue
		}
		if run++; run == OnsetRun {
			return i - OnsetRun + 1
		}
	}
	return -1
}
