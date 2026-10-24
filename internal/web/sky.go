package web

import (
	"encoding/json"
	"net/http"
	"regexp"
	"time"

	"github.com/exploded/skycam/internal/analysis"
	"github.com/exploded/skycam/internal/store"
)

// The sky API is a contract: skyq on the NUC reads a night's cloud series,
// onset and roof state from it instead of measuring stills itself.
// web_test.go pins its shape.

var nightRe = regexp.MustCompile(`^\d{8}$`)

type skyNightJSON struct {
	Night     string         `json:"night"` // YYYYMMDD, allsky's night date
	Timezone  string         `json:"timezone"`
	Onset     *time.Time     `json:"onset"` // null if cloud never set in
	OnsetRule string         `json:"onset_rule"`
	Frames    []skyFrameJSON `json:"frames"`
}

type skyFrameJSON struct {
	Time      time.Time `json:"time"`  // exposure start, observatory zone
	Image     string    `json:"image"` // path under /images/, "" if not kept; 404 once retention deletes it (14 nights)
	ExposureS float64   `json:"exposure_s"`
	Gain      int64     `json:"gain"`
	Stars     int64     `json:"stars"`
	Cloud     *float64  `json:"cloud"` // 0-1; null unless roof open and sun below -12°
	SkyLevel  float64   `json:"sky_level"`
	SkyMag    *float64  `json:"sky_mag"`
	SunAlt    float64   `json:"sun_alt"`
	MoonAlt   float64   `json:"moon_alt"`
	MoonIllum float64   `json:"moon_illum"`
	Roof      string    `json:"roof"` // OPEN, CLOSED, OPENING, CLOSING, ERROR or unknown
	Rain      string    `json:"rain"` // SAFE, RAIN or unknown
}

// skyNight serves GET /api/nights/{night}/sky: every night-mode frame of
// one night, oldest first.
func (s *Server) skyNight(w http.ResponseWriter, r *http.Request) {
	n := r.PathValue("night")
	if !nightRe.MatchString(n) {
		http.Error(w, "night must be YYYYMMDD", http.StatusBadRequest)
		return
	}
	if s.DB == nil {
		http.Error(w, "no database", http.StatusServiceUnavailable)
		return
	}
	rows, err := s.DB.FramesForNight(r.Context(), n)
	if err != nil {
		s.Log.Warn("sky api", "night", n, "err", err)
		http.Error(w, "reading frames failed", http.StatusInternalServerError)
		return
	}
	if len(rows) == 0 {
		http.Error(w, "no frames for that night", http.StatusNotFound)
		return
	}

	out := skyNightJSON{
		Night: n, Timezone: s.Loc.String(), Frames: make([]skyFrameJSON, len(rows)),
		OnsetRule: "first of 3 consecutive frames with cloud >= 0.5; unknown breaks a run",
	}
	cover := make([]float64, len(rows))
	for i, f := range rows {
		img := ""
		if f.File != "" {
			img = "/images/" + f.Night + "/" + f.File
		}
		cover[i] = store.Value(f.Cloud)
		out.Frames[i] = skyFrameJSON{
			Time: time.UnixMilli(f.TakenAt).In(s.Loc), Image: img,
			ExposureS: float64(f.ExposureUs) / 1e6, Gain: f.Gain, Stars: f.Stars,
			Cloud: ptr(cover[i]), SkyLevel: f.SkyLevel, SkyMag: ptr(store.Value(f.SkyMag)),
			SunAlt: f.SunAlt, MoonAlt: f.MoonAlt, MoonIllum: f.MoonIllum, Roof: f.Roof, Rain: f.Rain,
		}
	}
	if i := analysis.CloudOnset(cover); i >= 0 {
		out.Onset = &out.Frames[i].Time
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	json.NewEncoder(w).Encode(out)
}
