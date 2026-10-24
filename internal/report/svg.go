package report

// Copied from skyq (github.com/exploded/skyq, internal/report/svg.go) so the
// two night reports share one look. skyq's design/CHARTS.md is the spec for
// geometry, draw order and classes; keep the two copies in step.
import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// Pt is one chart point: minutes from the chart origin, the y value, and
// (for the index chart) the raw star count and target for the tooltip.
type Pt struct {
	M      float64 `json:"m"`
	V      float64 `json:"v"`
	Stars  int     `json:"stars,omitempty"`
	Target string  `json:"target,omitempty"`
}

// Series is one line on a chart. Color is a CSS custom property reference,
// e.g. "var(--series-1)" — assigned by entity, never by rank (CHARTS.md
// rule 3).
type Series struct {
	Name  string
	Label string
	Color string
	Pts   []Pt
}

// EventMark is a failure tick on the event rail.
type EventMark struct {
	M    float64
	Kind string // "solve" → triangle/critical, anything else → diamond/warning
}

// Band is a shaded time window: an autofocus run, or a stretch the roof was
// shut over the all-sky camera.
type Band struct{ M0, M1 float64 }

// Divider is a dashed vertical rule at a target change, labelled with the
// new target's name.
type Divider struct {
	M     float64
	Label string
}

// ChartOpts mirrors the reference builder's options object.
type ChartOpts struct {
	H          int     // viewBox height: 320 index, 210 luminance
	YMax       float64 // values clamp here rather than escaping the plot
	YTicks     int
	GapMin     float64 // start a new subpath when a gap exceeds this
	YLabel     string
	Rail       bool // event rail below the plot (B becomes 44)
	Ref        *float64
	RefLabel   string
	Dividers   []Divider // one per target change (CHARTS.md draw order 7)
	StartLabel string    // the night's first target, named at the plot's top left
	SpanMin    float64   // chart span in minutes
	TickStart  float64   // minutes offset of the first hour tick
	TickLabel  func(m float64) string
	Bands      []Band
	RoofBands  []Band // hatched, labelled "roof shut"; luminance chart only
	Events     []EventMark
	YFmt       func(v float64) string
}

const (
	// roofHatchID is unique per page only because the luminance chart is the
	// one chart that carries roof bands (CHARTS.md draw order 1).
	roofHatchID   = "roof-hatch"
	roofLabelMinW = 64

	chartW = 940
	padL   = 54
	padR   = 62
	padT   = 30
)

// RenderChart returns the inner SVG markup for one chart. The <svg> element
// itself lives in the page template so ids and classes stay in one place.
func RenderChart(series []Series, o ChartOpts) string {
	padB := 30
	if o.Rail {
		padB = 44
	}
	pw := float64(chartW - padL - padR)
	ph := float64(o.H - padT - padB)
	X := func(m float64) float64 { return padL + (m/o.SpanMin)*pw }
	Y := func(v float64) float64 { return padT + ph - (math.Min(v, o.YMax)/o.YMax)*ph }

	var b strings.Builder

	// 1. autofocus bands, then roof bands. The roof hatch is a pattern so it
	// reads differently from an autofocus band without leaning on colour.
	for _, band := range o.Bands {
		fmt.Fprintf(&b, `<rect x="%.1f" y="%d" width="%.1f" height="%.1f" fill="var(--band)"/>`,
			X(band.M0), padT, math.Max(2, X(band.M1)-X(band.M0)), ph)
	}
	if len(o.RoofBands) > 0 {
		fmt.Fprintf(&b, `<defs><pattern id="%s" width="6" height="6" patternUnits="userSpaceOnUse" patternTransform="rotate(45)">`+
			`<rect width="6" height="6" fill="var(--band)"/><path d="M0 0 V6" stroke="var(--roof)" stroke-width="1.5"/></pattern></defs>`, roofHatchID)
	}
	for _, band := range o.RoofBands {
		fmt.Fprintf(&b, `<rect x="%.1f" y="%d" width="%.1f" height="%.1f" fill="url(#%s)"/>`,
			X(band.M0), padT, math.Max(2, X(band.M1)-X(band.M0)), ph, roofHatchID)
	}
	// 2–3. gridlines + y tick labels
	for i := 0; i <= o.YTicks; i++ {
		v := o.YMax * float64(i) / float64(o.YTicks)
		fmt.Fprintf(&b, `<line x1="%d" x2="%d" y1="%.1f" y2="%.1f" class="gl"/>`, padL, chartW-padR, Y(v), Y(v))
		fmt.Fprintf(&b, `<text x="%d" y="%.1f" class="tick" text-anchor="end">%s</text>`, padL-9, Y(v)+4, o.YFmt(v))
	}
	// 4. x ticks every 60 minutes
	for m := o.TickStart; m <= o.SpanMin; m += 60 {
		fmt.Fprintf(&b, `<line x1="%.1f" x2="%.1f" y1="%.1f" y2="%.1f" class="ax"/>`, X(m), X(m), padT+ph, padT+ph+4)
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" class="tick" text-anchor="middle">%s</text>`, X(m), padT+ph+16, o.TickLabel(m))
	}
	// 5. baseline
	fmt.Fprintf(&b, `<line x1="%d" x2="%d" y1="%.1f" y2="%.1f" class="ax"/>`, padL, chartW-padR, padT+ph, padT+ph)
	// 6. axis label
	fmt.Fprintf(&b, `<text x="%d" y="%d" class="alab" text-anchor="start">%s</text>`, padL-9, padT-11, esc(o.YLabel))
	// 7. reference rules
	if o.Ref != nil {
		fmt.Fprintf(&b, `<line x1="%d" x2="%d" y1="%.1f" y2="%.1f" class="refline"/>`, padL, chartW-padR, Y(*o.Ref), Y(*o.Ref))
		fmt.Fprintf(&b, `<text x="%d" y="%.1f" class="reftxt" text-anchor="end">%s</text>`, chartW-padR-2, Y(*o.Ref)-6, esc(o.RefLabel))
	}
	if o.StartLabel != "" {
		fmt.Fprintf(&b, `<text x="%d" y="%d" class="reftxt">%s</text>`, padL+6, padT+11, esc(o.StartLabel))
	}
	for _, band := range o.RoofBands {
		// Named only where the band is wide enough to hold the words.
		if X(band.M1)-X(band.M0) >= roofLabelMinW {
			fmt.Fprintf(&b, `<text x="%.1f" y="%d" class="reftxt">roof shut</text>`, X(band.M0)+5, padT+11)
		}
	}
	for _, dv := range o.Dividers {
		fmt.Fprintf(&b, `<line x1="%.1f" x2="%.1f" y1="%d" y2="%.1f" class="refline"/>`, X(dv.M), X(dv.M), padT, padT+ph)
		fmt.Fprintf(&b, `<text x="%.1f" y="%d" class="reftxt">%s</text>`, X(dv.M)+5, padT+11, esc(dv.Label))
	}
	// 8–9. series: line, markers, direct label. Labels are collected and
	// drawn after every line, both to keep the draw order (CHARTS.md 9) and
	// so converging series can be nudged apart before anything is written.
	var labels []dirLabel
	for _, s := range series {
		if len(s.Pts) == 0 {
			continue
		}
		var d strings.Builder
		prev := math.Inf(-1)
		for _, p := range s.Pts {
			cmd := " L"
			if math.IsInf(prev, -1) || p.M-prev > o.GapMin {
				cmd = " M"
			}
			fmt.Fprintf(&d, "%s%.1f %.1f", cmd, X(p.M), Y(p.V))
			prev = p.M
		}
		fmt.Fprintf(&b, `<path d="%s" fill="none" stroke="%s" stroke-width="2" stroke-linejoin="round" stroke-linecap="round"/>`,
			strings.TrimSpace(d.String()), s.Color)
		if len(s.Pts) <= 150 {
			for _, p := range s.Pts {
				fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="2.8" fill="%s" stroke="var(--surface-1)" stroke-width="1.5"/>`,
					X(p.M), Y(p.V), s.Color)
			}
		}
		last := s.Pts[len(s.Pts)-1]
		labels = append(labels, dirLabel{
			X: math.Min(chartW-3, X(last.M)+9), Y: Y(last.V) + 4,
			Color: s.Color, Text: s.Label,
		})
	}
	for _, l := range spreadLabels(labels, float64(padT), float64(o.H)) {
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" class="dl" fill="%s">%s</text>`, l.X, l.Y, l.Color, esc(l.Text))
	}
	// 10. event rail
	if o.Rail {
		for _, e := range o.Events {
			y := padT + ph + 24
			color := "var(--warning)"
			if e.Kind == "solve" {
				color = "var(--critical)"
			}
			x := X(e.M)
			if e.Kind == "solve" {
				fmt.Fprintf(&b, `<path d="M%.1f %.1f L%.1f %.1f L%.1f %.1f Z" fill="%s"/>`,
					x, y-5, x+5, y+4, x-5, y+4, color)
			} else {
				fmt.Fprintf(&b, `<path d="M%.1f %.1f L%.1f %.1f L%.1f %.1f L%.1f %.1f Z" fill="%s"/>`,
					x, y-5, x+5, y, x, y+5, x-5, y, color)
			}
			fmt.Fprintf(&b, `<line x1="%.1f" x2="%.1f" y1="%d" y2="%.1f" stroke="%s" stroke-width="1" stroke-dasharray="2 3" opacity=".45"/>`,
				x, x, padT, padT+ph, color)
		}
	}
	// 11. crosshair + halo, hidden until hover; the hover script finds them
	// as the last line and circle in the SVG.
	fmt.Fprintf(&b, `<line x1="0" x2="0" y1="%d" y2="%.1f" class="ax cross" opacity="0"/>`, padT, padT+ph)
	b.WriteString(`<circle r="5.5" fill="none" stroke-width="2" class="halo" opacity="0"/>`)
	return b.String()
}

// dirLabel is one direct series label, at the series' last point.
type dirLabel struct {
	X, Y        float64
	Color, Text string
}

// dirLabelGap is the least vertical distance between two direct labels
// sharing an x position — a little over the 11.5px font size, so the two
// lines of text clear each other.
const dirLabelGap = 13

// spreadLabels pushes overlapping direct labels apart. Two series that end
// the night at almost the same value — RA and Dec guide error routinely do
// — would otherwise print one on top of the other and read as neither.
func spreadLabels(labels []dirLabel, top, height float64) []dirLabel {
	if len(labels) < 2 {
		return labels
	}
	out := append([]dirLabel(nil), labels...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Y < out[j].Y })
	for i := 1; i < len(out); i++ {
		// Only labels in the same column can collide.
		if math.Abs(out[i].X-out[i-1].X) > 24 {
			continue
		}
		if gap := out[i].Y - out[i-1].Y; gap < dirLabelGap {
			out[i].Y = out[i-1].Y + dirLabelGap
		}
	}
	// A stack pushed past the bottom slides back up as a block.
	if over := out[len(out)-1].Y - (height - 4); over > 0 {
		for i := range out {
			out[i].Y = math.Max(top+4, out[i].Y-over)
		}
	}
	return out
}

func esc(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}
