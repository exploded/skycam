// Package overlay draws the text on each saved frame.
//
// The top-left block copies allsky's layout line for line (red date and
// time, exposure, gain, sensor temperature, camera). skyq measures cloud in
// a band from 6% to 30% of the frame height, and its validated thresholds
// were set with allsky's text in that band, so the block must stay where
// it was. Anything new goes bottom-left, outside the band.
package overlay

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"math"
	"sync"
	"time"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

// Text is what goes on one frame.
type Text struct {
	Time     time.Time // already in the observatory's zone
	Exposure time.Duration
	Gain     int
	TempC    float64 // NaN hides the line's value
	Camera   string
	// Status lines for the bottom-left corner, top to bottom. Empty hides
	// the block.
	Status []Line
}

// Line is one status line; Alert draws it in amber.
type Line struct {
	Text  string
	Alert bool
}

var (
	red   = color.RGBA{0xff, 0x00, 0x00, 0xff}
	white = color.RGBA{0xff, 0xff, 0xff, 0xff}
	amber = color.RGBA{0xff, 0xb0, 0x20, 0xff}
	shade = color.RGBA{0x00, 0x00, 0x00, 0xff}
)

type Renderer struct {
	font *opentype.Font

	mu    sync.Mutex
	faces map[float64]font.Face
}

func New() (*Renderer, error) {
	f, err := opentype.Parse(goregular.TTF)
	if err != nil {
		return nil, err
	}
	return &Renderer{font: f, faces: map[float64]font.Face{}}, nil
}

func (r *Renderer) face(size float64) font.Face {
	r.mu.Lock()
	defer r.mu.Unlock()
	if f, ok := r.faces[size]; ok {
		return f
	}
	f, err := opentype.NewFace(r.font, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		panic(err) // only fails for a nil font, which New rules out
	}
	r.faces[size] = f
	return f
}

// Exposure formats an exposure the way allsky does.
func Exposure(d time.Duration) string {
	s := d.Seconds()
	if s >= 1 {
		return fmt.Sprintf("Exposure: %.1f sec", s)
	}
	return fmt.Sprintf("Exposure: %.2f ms (%.2f sec)", float64(d.Microseconds())/1000, s)
}

// Draw returns a colour copy of g with the text on it.
func (r *Renderer) Draw(g *image.Gray, t Text) *image.RGBA {
	b := g.Bounds()
	out := image.NewRGBA(b)
	draw.Draw(out, b, g, b.Min, draw.Src)
	r.DrawOn(out, t)
	return out
}

// DrawOn writes the text onto img in place.
func (r *Renderer) DrawOn(img *image.RGBA, t Text) {
	b := img.Bounds()
	k := float64(b.Dy()) / 960 // allsky's sizes were for a 960-line frame
	x := b.Min.X + int(19*k)
	px := func(v float64) int { return b.Min.Y + int(math.Round(v*k)) }

	temp := "Sensor: n/a"
	if !math.IsNaN(t.TempC) {
		temp = fmt.Sprintf("Sensor: %.1f C", t.TempC)
	}
	r.text(img, x, px(35), 26*k, red, false, t.Time.Format("02-01-2006  15:04:05"))
	r.text(img, x, px(58), 19*k, white, false, Exposure(t.Exposure))
	r.text(img, x, px(84), 19*k, white, false, fmt.Sprintf("Gain: %d", t.Gain))
	r.text(img, x, px(109), 19*k, white, false, temp)
	r.text(img, x, px(135), 15*k, white, false, t.Camera)

	if len(t.Status) == 0 {
		return
	}
	lineH := 24 * k
	y := float64(b.Max.Y) - 14*k - lineH*float64(len(t.Status)-1)
	for _, l := range t.Status {
		c := white
		if l.Alert {
			c = amber
		}
		r.text(img, x, int(math.Round(y)), 18*k, c, true, l.Text)
		y += lineH
	}
}

// text draws s with its baseline at y. outline puts a dark edge round the
// glyphs so the line stays readable over a bright roof or a lit sky; the
// top-left block has none, to match allsky.
func (r *Renderer) text(img *image.RGBA, x, y int, size float64, c color.RGBA, outline bool, s string) {
	if s == "" {
		return
	}
	d := font.Drawer{Dst: img, Face: r.face(math.Round(size*2) / 2)}
	if outline {
		d.Src = image.NewUniform(shade)
		for _, o := range [][2]int{{-1, 0}, {1, 0}, {0, -1}, {0, 1}, {-1, -1}, {1, 1}, {-1, 1}, {1, -1}} {
			d.Dot = fixed.P(x+o[0], y+o[1])
			d.DrawString(s)
		}
	}
	d.Src = image.NewUniform(c)
	d.Dot = fixed.P(x, y)
	d.DrawString(s)
}

// Label draws s with its baseline at (x, y), outlined, for keogram hour
// marks and the like.
func (r *Renderer) Label(img *image.RGBA, x, y int, size float64, s string) {
	r.text(img, x, y, size, white, true, s)
}
