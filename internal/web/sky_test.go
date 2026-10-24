package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/exploded/skycam/internal/capture"
	"github.com/exploded/skycam/internal/config"
	"github.com/exploded/skycam/internal/store"
	"github.com/exploded/skycam/internal/store/db"
)

// TestSkyAPI pins the contract skyq reads: one night's frames with cloud,
// roof and image paths, and the onset by the documented rule.
func TestSkyAPI(t *testing.T) {
	sqlDB, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	q := db.New(sqlDB)

	loc, _ := time.LoadLocation("Australia/Melbourne")
	start := time.Date(2026, 10, 23, 22, 0, 0, 0, loc)
	nan := math.NaN()
	// Roof shut for two frames, then clear, then cloud from frame 4.
	cover := []float64{nan, nan, 0.1, 0.2, 0.6, 0.7, 0.9}
	for i, c := range cover {
		roof := "OPEN"
		if math.IsNaN(c) {
			roof = "CLOSED"
		}
		at := start.Add(time.Duration(i) * 20 * time.Second)
		err := q.InsertFrame(context.Background(), db.InsertFrameParams{
			TakenAt: at.UnixMilli(), Night: "20261023", Mode: "night",
			File: "image-" + at.Format("20060102150405") + ".jpg", ExposureUs: 5_000_000, Gain: 50,
			SunAlt: -30, SkyLevel: 30, SkyMag: sql.NullFloat64{Float64: 18.2, Valid: true},
			Stars: 400, Cloud: store.Float(c), Roof: roof, Rain: "SAFE",
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	// A day frame of the same night must not appear.
	q.InsertFrame(context.Background(), db.InsertFrameParams{TakenAt: start.Add(-6 * time.Hour).UnixMilli(), Night: "20261023", Mode: "day"})

	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.ImagesUser, cfg.ImagesPassword = "skyq", "secret"
	s := &Server{Cfg: cfg, Loc: loc, Hub: capture.NewHub(), DB: q, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Camera: "Test"}
	h, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	if code, _ := get(t, srv.URL+"/api/nights/20261023/sky", false); code != http.StatusUnauthorized {
		t.Errorf("without auth: %d, want 401", code)
	}
	if code, _ := get(t, srv.URL+"/api/nights/20261024/sky", true); code != http.StatusNotFound {
		t.Errorf("empty night: %d, want 404", code)
	}
	if code, _ := get(t, srv.URL+"/api/nights/2026-10-23/sky", true); code != http.StatusBadRequest {
		t.Errorf("bad night: %d, want 400", code)
	}

	code, body := get(t, srv.URL+"/api/nights/20261023/sky", true)
	if code != http.StatusOK {
		t.Fatalf("sky: %d %s", code, body)
	}
	var got struct {
		Night    string
		Timezone string
		Onset    *time.Time
		Frames   []struct {
			Time      time.Time
			Image     string
			ExposureS float64 `json:"exposure_s"`
			Cloud     *float64
			Roof      string
		}
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if got.Night != "20261023" || got.Timezone != "Australia/Melbourne" || len(got.Frames) != len(cover) {
		t.Fatalf("night %q zone %q frames %d", got.Night, got.Timezone, len(got.Frames))
	}
	f0 := got.Frames[0]
	if f0.Cloud != nil || f0.Roof != "CLOSED" {
		t.Errorf("roof-shut frame: cloud %v roof %q; unknown must be null, never 0", f0.Cloud, f0.Roof)
	}
	if f0.Image != "/images/20261023/image-20261023220000.jpg" || f0.ExposureS != 5 {
		t.Errorf("frame 0: image %q exposure %v", f0.Image, f0.ExposureS)
	}
	if got.Onset == nil || !got.Onset.Equal(start.Add(4*20*time.Second)) {
		t.Errorf("onset = %v, want %v", got.Onset, start.Add(80*time.Second))
	}
}
