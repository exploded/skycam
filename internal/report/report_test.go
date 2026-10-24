package report

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/exploded/skycam/internal/store/db"
)

func TestRenderNight(t *testing.T) {
	loc, _ := time.LoadLocation("Australia/Melbourne")
	start := time.Date(2026, 10, 23, 20, 10, 0, 0, loc)
	var frames []db.Frame
	for i := 0; i < 600; i++ {
		f := db.Frame{
			TakenAt: start.Add(time.Duration(i) * 50 * time.Second).UnixMilli(), Night: "20261023", Mode: "night",
			ExposureUs: 5e6, Gain: 50, TempC: sql.NullFloat64{Float64: 25, Valid: true},
			SunAlt: -30, MoonIllum: 0.4, SkyLevel: 30, Stars: 500, Roof: "OPEN", Rain: "SAFE",
		}
		switch {
		case i < 60:
			f.Roof, f.Stars = "CLOSED", 5
		case i > 450:
			f.Stars = 40
			f.Cloud = sql.NullFloat64{Float64: 0.92, Valid: true}
		default:
			f.Cloud = sql.NullFloat64{Float64: 0.02, Valid: true}
		}
		frames = append(frames, f)
	}
	html, err := Render(Input{Night: "20261023", Loc: loc, Camera: "Test cam", Frames: frames})
	if err != nil {
		t.Fatal(err)
	}
	s := string(html)
	for _, want := range []string{"Night sky Fri 23 Oct 2026", "roof shut", "clear-sky baseline", "Hour by hour", "Test cam"} {
		if !strings.Contains(s, want) {
			t.Errorf("report missing %q", want)
		}
	}
	if strings.Contains(s, "ZgotmplZ") {
		t.Error("template filtered a value to ZgotmplZ")
	}
}

func TestRenderIndex(t *testing.T) {
	b, err := RenderIndex([]IndexEntry{{Night: "20261023", Href: "2026-10-23.html", Summary: "Clear"}}, time.UTC)
	if err != nil || !strings.Contains(string(b), "Fri 23 Oct 2026") {
		t.Fatalf("index: %v\n%s", err, b)
	}
}
