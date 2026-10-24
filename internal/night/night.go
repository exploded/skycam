// Package night turns a night's stills into its products at dawn — keogram,
// startrails, timelapse and report — and enforces retention. Each step is
// independent: one failing never stops the others.
package night

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	xdraw "golang.org/x/image/draw"

	"github.com/exploded/skycam/internal/config"
	"github.com/exploded/skycam/internal/fsutil"
	"github.com/exploded/skycam/internal/overlay"
	"github.com/exploded/skycam/internal/store/db"
)

// Result names the products made for a night (files under Dir; "" means
// not made).
type Result struct {
	Night      string
	Dir        string
	Frames     int
	Timelapse  string
	Keogram    string
	Startrails string
	Report     string
}

// Files lists the products that exist, as full paths.
func (r Result) Files() []string {
	var out []string
	for _, n := range []string{r.Timelapse, r.Keogram, r.Startrails, r.Report} {
		if n != "" {
			out = append(out, filepath.Join(r.Dir, n))
		}
	}
	return out
}

// ReportFunc writes the night report into dir and returns its file name.
type ReportFunc func(ctx context.Context, res Result) (string, error)

type Processor struct {
	Cfg     config.Config
	Loc     *time.Location
	DB      *db.Queries // may be nil
	Overlay *overlay.Renderer
	Log     *slog.Logger
	Report  ReportFunc // may be nil
	// Done is called after a night's products are made, e.g. to upload them.
	Done func(ctx context.Context, res Result)

	mu sync.Mutex
}

var reFrame = regexp.MustCompile(`^image-(\d{14})\.jpg$`)

// Frames lists a night's stills in time order.
func Frames(imagesDir, night string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(imagesDir, night))
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && reFrame.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// Process makes every product for night. Only one night is processed at a
// time; the Pi has four cores and capture needs one.
func (p *Processor) Process(ctx context.Context, night string) (Result, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	res := Result{Night: night, Dir: filepath.Join(p.Cfg.ProductsDir(), night)}
	names, err := Frames(p.Cfg.ImagesDir(), night)
	if err != nil {
		return res, err
	}
	res.Frames = len(names)
	if len(names) == 0 {
		return res, fmt.Errorf("night %s has no frames", night)
	}
	if err := os.MkdirAll(res.Dir, 0o755); err != nil {
		return res, err
	}
	start := time.Now()
	p.Log.Info("processing night", "night", night, "frames", len(names))

	closed := p.roofClosed(ctx, night)
	keo, trails, err := p.stack(night, names, closed)
	if err != nil {
		p.Log.Warn("keogram/startrails failed", "night", night, "err", err)
	} else {
		if err := writeJPEG(filepath.Join(res.Dir, "keogram-"+night+".jpg"), keo); err == nil {
			res.Keogram = "keogram-" + night + ".jpg"
		} else {
			p.Log.Warn("writing keogram", "err", err)
		}
		if trails != nil {
			if err := writeJPEG(filepath.Join(res.Dir, "startrails-"+night+".jpg"), trails); err == nil {
				res.Startrails = "startrails-" + night + ".jpg"
			} else {
				p.Log.Warn("writing startrails", "err", err)
			}
		}
	}

	if p.Cfg.Timelapse.Enabled {
		name := "allsky-" + night + ".mp4"
		if err := p.timelapse(ctx, night, names, filepath.Join(res.Dir, name)); err != nil {
			p.Log.Warn("timelapse failed", "night", night, "err", err)
		} else {
			res.Timelapse = name
		}
	}

	if p.Report != nil {
		if name, err := p.Report(ctx, res); err != nil {
			p.Log.Warn("report failed", "night", night, "err", err)
		} else {
			res.Report = name
		}
	}

	if p.DB != nil {
		err := p.DB.UpsertNight(ctx, db.UpsertNightParams{
			Night: night, ProcessedAt: time.Now().UnixMilli(), Frames: int64(res.Frames),
			Timelapse: res.Timelapse, Keogram: res.Keogram, Startrails: res.Startrails, Report: res.Report,
		})
		if err != nil {
			p.Log.Warn("recording night", "err", err)
		}
	}
	p.Log.Info("night processed", "night", night, "took", time.Since(start).Round(time.Second),
		"timelapse", res.Timelapse != "", "keogram", res.Keogram != "", "startrails", res.Startrails != "", "report", res.Report != "")
	if p.Done != nil {
		p.Done(ctx, res)
	}
	p.Retain(ctx, time.Now())
	return res, nil
}

// roofClosed returns the files whose frame was taken with the roof known
// closed. Unknown is not closed: a roof opened by hand must still count.
func (p *Processor) roofClosed(ctx context.Context, night string) map[string]bool {
	out := map[string]bool{}
	if p.DB == nil {
		return out
	}
	rows, err := p.DB.FramesForNight(ctx, night)
	if err != nil {
		return out
	}
	for _, r := range rows {
		if r.File != "" && r.Roof == "CLOSED" {
			out[r.File] = true
		}
	}
	return out
}

// startrailsMaxMean leaves out frames brighter than this (twilight, cloud
// lit by town, the roof under a light) — they would wash the trails out.
const startrailsMaxMean = 90

// stack makes the keogram (the centre column of every frame, side by side)
// and startrails (the per-pixel maximum of the dark, roof-open frames) in
// one decoding pass.
func (p *Processor) stack(night string, names []string, closed map[string]bool) (*image.RGBA, *image.Gray, error) {
	var keo *image.RGBA
	var trails *image.Gray
	var exclude []image.Rectangle
	used := 0
	for i, name := range names {
		g, err := decodeLuma(filepath.Join(p.Cfg.ImagesDir(), night, name))
		if err != nil {
			p.Log.Warn("skipping unreadable frame", "file", name, "err", err)
			continue
		}
		b := g.Bounds()
		if keo == nil {
			keo = image.NewRGBA(image.Rect(0, 0, len(names), b.Dy()))
			exclude = config.PixelRects(p.Cfg.Exposure.Exclude, b.Dx(), b.Dy())
		}
		cx := b.Min.X + b.Dx()/2
		for y := 0; y < min(b.Dy(), keo.Bounds().Dy()); y++ {
			v := g.Pix[g.PixOffset(cx, b.Min.Y+y)]
			keo.SetRGBA(i, y, color.RGBA{v, v, v, 0xff})
		}
		if closed[name] || meanOf(g) > startrailsMaxMean {
			continue
		}
		if trails == nil {
			trails = image.NewGray(b)
		} else if trails.Bounds() != b {
			continue
		}
		for j, v := range g.Pix {
			if v > trails.Pix[j] {
				trails.Pix[j] = v
			}
		}
		used++
	}
	if keo == nil {
		return nil, nil, fmt.Errorf("no readable frames")
	}
	if trails != nil {
		// The overlay text would stack into a smear; blank it.
		for _, r := range exclude {
			draw.Draw(trails, r, image.Black, image.Point{}, draw.Src)
		}
		if used < 10 {
			trails = nil
		}
	}
	// A short or sparse night makes a narrow keogram; widen it by repeating
	// columns before labelling, so the labels stay legible.
	if w := keo.Bounds().Dx(); w < keogramMinWidth {
		wide := image.NewRGBA(image.Rect(0, 0, keogramMinWidth, keo.Bounds().Dy()))
		xdraw.NearestNeighbor.Scale(wide, wide.Bounds(), keo, keo.Bounds(), xdraw.Src, nil)
		keo = wide
	}
	p.labelKeogram(keo, night, names, float64(keo.Bounds().Dx())/float64(len(names)))
	return keo, trails, nil
}

const keogramMinWidth = 1280

// labelKeogram marks each hour with a line and its time, as allsky does.
// xs is the keogram's width per frame.
func (p *Processor) labelKeogram(keo *image.RGBA, night string, names []string, xs float64) {
	if p.Overlay == nil || len(names) < 2 {
		return
	}
	h := keo.Bounds().Dy()
	last, lastX := -1, -100
	for i, name := range names {
		t, err := time.ParseInLocation("20060102150405", reFrame.FindStringSubmatch(name)[1], p.Loc)
		if err != nil {
			continue
		}
		if t.Hour() == last {
			continue
		}
		if last >= 0 {
			x := int(float64(i) * xs)
			for y := 0; y < h; y += 4 {
				keo.SetRGBA(x, y, color.RGBA{0xff, 0xff, 0xff, 0xff})
			}
			// Label only where the last one has room; a short night of
			// sparse frames packs the hours close together.
			if x-lastX >= 40 {
				p.Overlay.Label(keo, x+4, h-12, 28, t.Format("15"))
				lastX = x
			}
		}
		last = t.Hour()
	}
	if t, err := time.ParseInLocation("20060102", night, p.Loc); err == nil {
		p.Overlay.Label(keo, 8, 34, 28, t.Format("2 Jan 2006"))
	}
}

// decodeLuma reads a JPEG's brightness plane. skycam's frames are colour
// JPEGs of a mono image (so the overlay can be red); the Y plane is the
// image.
func decodeLuma(path string) (*image.Gray, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, err := jpeg.Decode(f)
	if err != nil {
		return nil, err
	}
	switch m := img.(type) {
	case *image.Gray:
		return m, nil
	case *image.YCbCr:
		b := m.Bounds()
		g := image.NewGray(b)
		for y := b.Min.Y; y < b.Max.Y; y++ {
			copy(g.Pix[g.PixOffset(b.Min.X, y):g.PixOffset(b.Max.X, y)], m.Y[m.YOffset(b.Min.X, y):m.YOffset(b.Max.X, y)])
		}
		return g, nil
	}
	g := image.NewGray(img.Bounds())
	draw.Draw(g, g.Bounds(), img, img.Bounds().Min, draw.Src)
	return g, nil
}

func meanOf(g *image.Gray) float64 {
	var sum int
	for i := 0; i < len(g.Pix); i += 7 {
		sum += int(g.Pix[i])
	}
	return float64(sum) / float64((len(g.Pix)+6)/7)
}

func writeJPEG(path string, img image.Image) error {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(path, buf.Bytes())
}

// timelapse runs ffmpeg over a concat list of the night's frames.
func (p *Processor) timelapse(ctx context.Context, night string, names []string, out string) error {
	tl := p.Cfg.Timelapse
	fps := max(tl.FPS, 1)
	list := filepath.Join(filepath.Dir(out), ".frames.txt")
	var b strings.Builder
	dur := fmt.Sprintf("%.6f", 1/float64(fps))
	for _, n := range names {
		path := filepath.ToSlash(filepath.Join(p.Cfg.ImagesDir(), night, n))
		fmt.Fprintf(&b, "file '%s'\nduration %s\n", strings.ReplaceAll(path, "'", `'\''`), dur)
	}
	// The concat demuxer ignores the last entry's duration unless the
	// file is repeated.
	fmt.Fprintf(&b, "file '%s'\n", filepath.ToSlash(filepath.Join(p.Cfg.ImagesDir(), night, names[len(names)-1])))
	if err := os.WriteFile(list, []byte(b.String()), 0o644); err != nil {
		return err
	}
	defer os.Remove(list)

	tmp := out + ".part.mp4"
	args := []string{"-y", "-loglevel", "error", "-f", "concat", "-safe", "0", "-i", list,
		"-vf", fmt.Sprintf("fps=%d,scale=trunc(iw/2)*2:trunc(ih/2)*2,format=yuv420p", fps)}
	switch tl.Codec {
	case "h264_v4l2m2m":
		args = append(args, "-c:v", "h264_v4l2m2m", "-b:v", "8M")
	default:
		args = append(args, "-c:v", "libx264", "-preset", "veryfast", "-crf", fmt.Sprint(max(tl.CRF, 1)))
	}
	args = append(args, "-movflags", "+faststart", tmp)
	cmd := exec.CommandContext(ctx, tl.FFmpeg, args...)
	if outb, err := cmd.CombinedOutput(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("ffmpeg: %w: %s", err, strings.TrimSpace(string(outb)))
	}
	return os.Rename(tmp, out)
}

// Retain deletes frames older than RetentionNights and products (and frame
// rows) older than ProductDays.
func (p *Processor) Retain(ctx context.Context, now time.Time) {
	today := now.In(p.Loc)
	cutFrames := today.AddDate(0, 0, -p.Cfg.RetentionNights).Format("20060102")
	cutProducts := today.AddDate(0, 0, -p.Cfg.ProductDays).Format("20060102")
	prune := func(dir, cut string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if e.IsDir() && len(e.Name()) == 8 && e.Name() < cut {
				if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
					p.Log.Warn("retention", "dir", e.Name(), "err", err)
				} else {
					p.Log.Info("retention removed", "dir", filepath.Join(filepath.Base(dir), e.Name()))
				}
			}
		}
	}
	if p.Cfg.RetentionNights > 0 {
		prune(p.Cfg.ImagesDir(), cutFrames)
	}
	if p.Cfg.ProductDays > 0 {
		prune(p.Cfg.ProductsDir(), cutProducts)
		if p.DB != nil {
			p.DB.DeleteFramesBefore(ctx, today.AddDate(0, 0, -p.Cfg.ProductDays).UnixMilli())
		}
	}
}

// CatchUp processes any night that has frames but no products — the Pi
// was off at dawn, or the service restarted mid-processing. active is the
// night still being captured ("" by day); it is left alone.
func (p *Processor) CatchUp(ctx context.Context, active string) {
	entries, err := os.ReadDir(p.Cfg.ImagesDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		n := e.Name()
		if !e.IsDir() || len(n) != 8 || n == active {
			continue
		}
		if _, err := os.Stat(filepath.Join(p.Cfg.ProductsDir(), n)); err == nil {
			continue
		}
		if ctx.Err() != nil {
			return
		}
		if _, err := p.Process(ctx, n); err != nil {
			p.Log.Warn("catch-up", "night", n, "err", err)
		}
	}
}
