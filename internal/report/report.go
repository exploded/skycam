// Package report renders the night report: one self-contained HTML file
// (inline CSS, SVG charts, embedded thumbnails) that reads the same opened
// from disk, from the Pi, or on deepspaceplace.com/reports/skycam/.
package report

import (
	"bytes"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"image"
	"image/jpeg"
	"math"
	"os"
	"sort"
	"time"

	"golang.org/x/image/draw"

	"github.com/exploded/skycam/internal/analysis"
	"github.com/exploded/skycam/internal/store"
	"github.com/exploded/skycam/internal/store/db"
)

//go:embed report.css
var css string

//go:embed page.tmpl
var pageTmpl string

//go:embed index.tmpl
var indexTmpl string

var (
	page  = template.Must(template.New("page").Parse(pageTmpl))
	index = template.Must(template.New("index").Parse(indexTmpl))
)

// Input is everything one night's report is drawn from.
type Input struct {
	Night  string // YYYYMMDD
	Loc    *time.Location
	Camera string
	Frames []db.Frame // night-mode frames, in time order
	Events []db.Event
	// Product files on disk ("" when not made) and where a browser can
	// fetch the timelapse.
	KeogramPath    string
	StartrailsPath string
	TimelapseURL   string
}

type stat struct{ K, V, Small string }

type legendItem struct {
	Label string
	Color template.CSS
}

type event struct{ Time, Kind, Detail string }

type hourRow struct {
	Hour, Frames, Stars, Cloud, Roof, Exposure string
}

type chartJS struct {
	SVG    string     `json:"svg"`
	Box    string     `json:"box"`
	Tip    string     `json:"tip"`
	Rail   bool       `json:"rail"`
	H      int        `json:"h"`
	Span   float64    `json:"span"`
	YMax   float64    `json:"ymax"`
	Origin float64    `json:"o"` // minutes past midnight of the chart origin
	Fmt    string     `json:"fmt"`
	Series []seriesJS `json:"series"`
}

type seriesJS struct {
	Name  string `json:"name"`
	Color string `json:"color"`
	Pts   []Pt   `json:"pts"`
}

type pageData struct {
	Title, Verdict string
	CSS            template.CSS
	Stats          []stat
	StarsChart     template.HTML
	SkyChart       template.HTML
	ExpChart       template.HTML
	TempChart      template.HTML
	HasTemp        bool
	StarsLegend    []legendItem
	Keogram        template.URL
	Startrails     template.URL
	Timelapse      string
	Events         []event
	Hours          []hourRow
	RoofNote       string
	ChartsJSON     template.JS
	Foot           string
}

// Render returns the report HTML.
func Render(in Input) ([]byte, error) {
	if len(in.Frames) == 0 {
		return nil, fmt.Errorf("night %s has no frames to report", in.Night)
	}
	loc := in.Loc
	t0 := time.UnixMilli(in.Frames[0].TakenAt).In(loc)
	t1 := time.UnixMilli(in.Frames[len(in.Frames)-1].TakenAt).In(loc)
	origin := t0.Truncate(time.Hour)
	if origin.After(t0) {
		origin = origin.Add(-time.Hour)
	}
	span := math.Max(60, math.Ceil(t1.Sub(origin).Minutes()/60)*60)
	minutes := func(ms int64) float64 { return time.UnixMilli(ms).Sub(origin).Minutes() }
	originMin := float64(origin.Hour()*60 + origin.Minute())
	tickLabel := func(m float64) string { return origin.Add(time.Duration(m) * time.Minute).Format("15:04") }

	var stars, sky, exp, temp []Pt
	var roofBands []Band
	var open, dark, clear, cloudKnown float64
	var bandStart = -1.0
	tempLo, tempHi := math.Inf(1), math.Inf(-1)
	bestMag := math.NaN()
	for i, f := range in.Frames {
		m := minutes(f.TakenAt)
		stars = append(stars, Pt{M: m, V: float64(f.Stars)})
		sky = append(sky, Pt{M: m, V: f.SkyLevel})
		exp = append(exp, Pt{M: m, V: float64(f.ExposureUs) / 1e6})
		if f.TempC.Valid {
			temp = append(temp, Pt{M: m, V: f.TempC.Float64})
			tempLo, tempHi = math.Min(tempLo, f.TempC.Float64), math.Max(tempHi, f.TempC.Float64)
		}
		// Time each frame stands for: up to the next frame, at most 2 min.
		dt := 0.0
		if i+1 < len(in.Frames) {
			dt = math.Min(2, minutes(in.Frames[i+1].TakenAt)-m)
		}
		if f.Roof == "OPEN" {
			open += dt
		}
		if f.SunAlt < -12 && f.Roof == "OPEN" {
			dark += dt
			if mag := store.Value(f.SkyMag); !math.IsNaN(mag) && (math.IsNaN(bestMag) || mag > bestMag) {
				bestMag = mag
			}
		}
		if f.Cloud.Valid {
			cloudKnown += dt
			if f.Cloud.Float64 < 0.3 {
				clear += dt
			}
		}
		// Hatch only where the roof is known shut or moving; unknown gets
		// no band (skyq's rule — a roof opened by hand must not read shut).
		shut := f.Roof == "CLOSED" || f.Roof == "OPENING" || f.Roof == "CLOSING"
		switch {
		case shut && bandStart < 0:
			bandStart = m
		case !shut && bandStart >= 0:
			roofBands = append(roofBands, Band{M0: bandStart, M1: m})
			bandStart = -1
		}
	}
	if bandStart >= 0 {
		roofBands = append(roofBands, Band{M0: bandStart, M1: minutes(in.Frames[len(in.Frames)-1].TakenAt)})
	}

	nightDate, _ := time.ParseInLocation("20060102", in.Night, loc)
	d := pageData{
		Title: "Night sky " + nightDate.Format("Mon 2 Jan 2006"),
		CSS:   template.CSS(css),
		Foot: fmt.Sprintf("skycam · %s · %d frames %s–%s · generated %s", in.Camera, len(in.Frames),
			t0.Format("15:04"), t1.Format("15:04"), time.Now().In(loc).Format("2 Jan 15:04")),
		Timelapse: in.TimelapseURL,
	}
	d.Verdict = verdict(open, dark, clear, cloudKnown)
	d.Stats = []stat{
		{K: "Frames", V: fmt.Sprint(len(in.Frames)), Small: t0.Format("15:04") + "–" + t1.Format("15:04")},
		{K: "Roof open", V: hm(open)},
		{K: "Clear", V: pct(clear, cloudKnown), Small: "of " + hm(cloudKnown) + " measured"},
		{K: "Moon", V: fmt.Sprintf("%.0f%%", in.Frames[len(in.Frames)/2].MoonIllum*100), Small: "lit at midnight"},
	}
	if !math.IsNaN(bestMag) {
		d.Stats = append(d.Stats, stat{K: "Darkest sky", V: fmt.Sprintf("%.2f", bestMag), Small: "relative mag"})
	}
	if !math.IsInf(tempLo, 0) {
		d.Stats = append(d.Stats, stat{K: "Sensor", V: fmt.Sprintf("%.0f–%.0f °C", tempLo, tempHi)})
	}

	charts := []chartJS{}
	add := func(id string, h int, ymax float64, f string, s []Series) {
		cj := chartJS{SVG: "c" + id, Box: "b" + id, Tip: "t" + id, H: h, Span: span, YMax: ymax, Origin: originMin, Fmt: f}
		for _, x := range s {
			cj.Series = append(cj.Series, seriesJS{Name: x.Name, Color: x.Color, Pts: thin(x.Pts, 400)})
		}
		charts = append(charts, cj)
	}

	// Stars: the cloud signal. The baseline rule is the clear-sky count the
	// cloud estimate divides by, when there is one.
	starMax := niceMax(maxV(stars), 4)
	so := ChartOpts{H: 260, YMax: starMax, YTicks: 4, GapMin: 6, YLabel: "Stars detected", SpanMin: span,
		TickLabel: tickLabel, RoofBands: roofBands, YFmt: func(v float64) string { return fmt.Sprintf("%.0f", v) }}
	if b := baseline(in.Frames); b > 0 {
		so.Ref, so.RefLabel = &b, "clear-sky baseline"
	}
	starSeries := []Series{{Name: "Stars", Label: "stars", Color: "var(--series-1)", Pts: stars}}
	d.StarsChart = template.HTML(RenderChart(starSeries, so))
	add("1", so.H, so.YMax, "num", starSeries)

	skySeries := []Series{{Name: "Sky level", Label: "sky", Color: "var(--series-2)", Pts: sky}}
	ko := ChartOpts{H: 210, YMax: niceMax(maxV(sky), 4), YTicks: 4, GapMin: 6, YLabel: "Sky background (ADU)", SpanMin: span,
		TickLabel: tickLabel, YFmt: func(v float64) string { return fmt.Sprintf("%.0f", v) }}
	d.SkyChart = template.HTML(RenderChart(skySeries, ko))
	add("2", ko.H, ko.YMax, "num", skySeries)

	expSeries := []Series{{Name: "Exposure", Label: "exp", Color: "var(--series-3)", Pts: exp}}
	eo := ChartOpts{H: 210, YMax: niceMax(maxV(exp), 4), YTicks: 4, GapMin: 6, YLabel: "Exposure (s)", SpanMin: span,
		TickLabel: tickLabel, YFmt: func(v float64) string { return trimFloat(v) }}
	d.ExpChart = template.HTML(RenderChart(expSeries, eo))
	add("3", eo.H, eo.YMax, "num", expSeries)

	if len(temp) > 0 && tempLo >= 0 {
		d.HasTemp = true
		tempSeries := []Series{{Name: "Sensor", Label: "°C", Color: "var(--series-4)", Pts: temp}}
		to := ChartOpts{H: 210, YMax: niceMax(tempHi, 4), YTicks: 4, GapMin: 6, YLabel: "Sensor temperature (°C)", SpanMin: span,
			TickLabel: tickLabel, YFmt: func(v float64) string { return fmt.Sprintf("%.0f", v) }}
		d.TempChart = template.HTML(RenderChart(tempSeries, to))
		add("4", to.H, to.YMax, "num", tempSeries)
	}
	if len(roofBands) > 0 {
		d.RoofNote = "Hatched columns: the roof was shut or moving, so the camera saw the roof, not the sky."
	}
	js, err := json.Marshal(charts)
	if err != nil {
		return nil, err
	}
	d.ChartsJSON = template.JS(js)

	if in.KeogramPath != "" {
		d.Keogram = thumb(in.KeogramPath, 940, 0)
	}
	if in.StartrailsPath != "" {
		d.Startrails = thumb(in.StartrailsPath, 640, 0)
	}
	for _, e := range in.Events {
		ev := event{Time: time.UnixMilli(e.StartedAt).In(loc).Format("15:04"), Kind: e.Kind, Detail: e.Note}
		if e.Kind == "live" {
			ev.Detail = "Live view; no stills"
			if e.EndedAt.Valid {
				ev.Detail += " for " + hm(float64(e.EndedAt.Int64-e.StartedAt)/60000)
			}
		}
		d.Events = append(d.Events, ev)
	}
	d.Hours = hours(in.Frames, loc)

	var buf bytes.Buffer
	if err := page.Execute(&buf, d); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func verdict(open, dark, clear, known float64) string {
	switch {
	case open < 1:
		return "The roof stayed shut all night."
	case known < 1:
		return fmt.Sprintf("Roof open for %s; not dark enough, or no clear-sky baseline yet, to judge cloud.", hm(open))
	}
	return fmt.Sprintf("Roof open for %s. Clear for %s of the %s measured (%s).", hm(open), hm(clear), hm(known), pct(clear, known))
}

// baseline estimates the clear-sky star count the cloud figures used,
// working back from frames where both are known: cloud = 1 − stars/base.
func baseline(frames []db.Frame) float64 {
	var vals []float64
	for _, f := range frames {
		if f.Cloud.Valid && f.Cloud.Float64 > 0.01 && f.Cloud.Float64 < 0.99 {
			vals = append(vals, float64(f.Stars)/(1-f.Cloud.Float64))
		}
	}
	if len(vals) == 0 {
		return 0
	}
	sort.Float64s(vals)
	return math.Round(vals[len(vals)/2])
}

func hours(frames []db.Frame, loc *time.Location) []hourRow {
	type acc struct {
		n, stars, cloudN, open int
		cloud                  float64
		exp                    int64
		at                     time.Time
	}
	var order []string
	byHour := map[string]*acc{}
	for _, f := range frames {
		t := time.UnixMilli(f.TakenAt).In(loc)
		k := t.Format("15:00")
		a := byHour[k]
		if a == nil {
			a = &acc{at: t}
			byHour[k] = a
			order = append(order, k)
		}
		a.n++
		a.stars += int(f.Stars)
		a.exp += f.ExposureUs
		if f.Cloud.Valid {
			a.cloud += f.Cloud.Float64
			a.cloudN++
		}
		if f.Roof == "OPEN" {
			a.open++
		}
	}
	var rows []hourRow
	for _, k := range order {
		a := byHour[k]
		r := hourRow{Hour: k, Frames: fmt.Sprint(a.n), Stars: fmt.Sprint(a.stars / a.n), Cloud: "—",
			Exposure: trimFloat(float64(a.exp)/float64(a.n)/1e6) + " s"}
		if a.cloudN > 0 {
			c := a.cloud / float64(a.cloudN)
			r.Cloud = fmt.Sprintf("%.0f%% · %s", c*100, analysis.Sky(c))
		}
		switch {
		case a.open == a.n:
			r.Roof = "open"
		case a.open == 0:
			r.Roof = "shut/unknown"
		default:
			r.Roof = fmt.Sprintf("open %d/%d", a.open, a.n)
		}
		rows = append(rows, r)
	}
	return rows
}

// thumb embeds a JPEG as a data URL, so the report stays a single file. It
// is scaled to at most width pixels wide, keeping its aspect ratio, or to
// exactly width×height when height is set.
func thumb(path string, width, height int) template.URL {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	src, err := jpeg.Decode(f)
	if err != nil {
		return ""
	}
	b := src.Bounds()
	var img image.Image = src
	if height > 0 || b.Dx() > width {
		h := height
		if h == 0 {
			h = b.Dy() * width / b.Dx()
		}
		dst := image.NewRGBA(image.Rect(0, 0, width, h))
		draw.ApproxBiLinear.Scale(dst, dst.Bounds(), src, b, draw.Src, nil)
		img = dst
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
		return ""
	}
	return template.URL("data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(buf.Bytes()))
}

// thin keeps at most n points for the hover data; the SVG keeps them all.
func thin(p []Pt, n int) []Pt {
	if len(p) <= n {
		return p
	}
	out := make([]Pt, 0, n)
	step := float64(len(p)) / float64(n)
	for i := 0.0; int(i) < len(p); i += step {
		out = append(out, p[int(i)])
	}
	return out
}

func maxV(p []Pt) float64 {
	m := 0.0
	for _, x := range p {
		m = math.Max(m, x.V)
	}
	return m
}

// niceMax rounds v up to a value that splits into ticks evenly.
func niceMax(v float64, ticks int) float64 {
	if v <= 0 || math.IsNaN(v) {
		return float64(ticks)
	}
	step := v / float64(ticks)
	mag := math.Pow(10, math.Floor(math.Log10(step)))
	for _, k := range []float64{1, 2, 2.5, 5, 10} {
		if k*mag >= step {
			return k * mag * float64(ticks)
		}
	}
	return 10 * mag * float64(ticks)
}

func trimFloat(v float64) string {
	switch {
	case v >= 10:
		return fmt.Sprintf("%.0f", v)
	case v >= 1:
		return fmt.Sprintf("%.1f", v)
	default:
		return fmt.Sprintf("%.2f", v)
	}
}

func hm(minutes float64) string {
	m := int(math.Round(minutes))
	return fmt.Sprintf("%dh %02dm", m/60, m%60)
}

func pct(a, b float64) string {
	if b <= 0 {
		return "—"
	}
	return fmt.Sprintf("%.0f%%", 100*a/b)
}

// IndexEntry is one night on the index page.
type IndexEntry struct {
	Night   string // YYYYMMDD
	Href    string
	Summary string
}

// RenderIndex lists the nights, newest first.
func RenderIndex(entries []IndexEntry, loc *time.Location) ([]byte, error) {
	type row struct {
		Date, Href, Summary string
	}
	var rows []row
	for _, e := range entries {
		t, err := time.ParseInLocation("20060102", e.Night, loc)
		if err != nil {
			continue
		}
		rows = append(rows, row{Date: t.Format("Mon 2 Jan 2006"), Href: e.Href, Summary: e.Summary})
	}
	var buf bytes.Buffer
	err := index.Execute(&buf, struct {
		CSS  template.CSS
		Rows []row
	}{template.CSS(css), rows})
	return buf.Bytes(), err
}
