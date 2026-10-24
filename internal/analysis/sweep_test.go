package analysis

import (
	"image"
	"image/color"
	"image/draw"
	_ "image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// TestSweep runs the detector over a folder of real frames and logs the
// numbers, for tuning. Set SKYCAM_FRAMES to a night folder copied from
// allsky.local/images/<YYYYMMDD>/ and run with -v.
func TestSweep(t *testing.T) {
	dir := os.Getenv("SKYCAM_FRAMES")
	if dir == "" {
		t.Skip("SKYCAM_FRAMES not set")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.jpg"))
	sort.Strings(files)
	var hot *HotMap
	for _, f := range files {
		g := loadGray(t, f)
		b := g.Bounds()
		if hot == nil {
			hot = NewHotMap(b.Dx(), b.Dy())
		}
		region := image.Rect(0, int(0.15*float64(b.Dy())), b.Dx(), int(0.85*float64(b.Dy())))
		bg := EstimateBackground(g, region)
		stars := FindStars(g, region, bg, StarOptions{Sigma: 5, MinDelta: 4, Hot: hot})
		if out := os.Getenv("SKYCAM_MARK"); out != "" && filepath.Base(f) == os.Getenv("SKYCAM_MARK_FRAME") {
			markStars(t, g, stars, hot, out)
		}
		hot.Update(stars)
		t.Logf("%s band=%5.1f sky=%5.1f noise=%4.2f stars=%4d",
			filepath.Base(f), BandLuminance(g, DefaultBandTop, DefaultBandBottom), bg.Median(), bg.Noise(), len(stars))
	}
}

func loadGray(t *testing.T, path string) *image.Gray {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	src, _, err := image.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	g := image.NewGray(src.Bounds())
	draw.Draw(g, g.Bounds(), src, src.Bounds().Min, draw.Src)
	return g
}

// markStars writes a stretched copy of g with a box round each star, for
// eyeballing what the detector counts.
func markStars(t *testing.T, g *image.Gray, stars []Star, hot *HotMap, path string) {
	t.Helper()
	out := image.NewRGBA(g.Bounds())
	for i, v := range g.Pix {
		s := uint8(min(int(v)*6, 255))
		out.Pix[i*4], out.Pix[i*4+1], out.Pix[i*4+2], out.Pix[i*4+3] = s, s, s, 255
	}
	box := func(x, y int, c color.RGBA) {
		for d := -4; d <= 4; d++ {
			out.Set(x+d, y-4, c)
			out.Set(x+d, y+4, c)
			out.Set(x-4, y+d, c)
			out.Set(x+4, y+d, c)
		}
	}
	for _, s := range stars {
		box(s.X, s.Y, color.RGBA{0, 255, 0, 255})
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	png.Encode(f, out)
}
