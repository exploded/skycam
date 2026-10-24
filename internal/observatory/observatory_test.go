package observatory

import (
	"context"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPollAndStaleness(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/roof", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"shutter":"OPEN","connected":true,"closedSw":false,"openSw":true,"moving":false}`)
	})
	mux.HandleFunc("/rain", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"safe":false,"gpio":1}`)
	})
	mux.HandleFunc("/v2/api/equipment/mount/info", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"Response":{"Connected":true,"TrackingEnabled":true,"AtPark":false,"Slewing":false},"Success":true}`)
	})
	mux.HandleFunc("/v2/api/equipment/filterwheel/info", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"Response":{"Connected":true,"SelectedFilter":{"Name":"Ha","Id":3}},"Success":true}`)
	})
	mux.HandleFunc("/v2/api/equipment/camera/info", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"Response":{"Connected":true,"CoolerOn":true,"Temperature":-10},"Success":true}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := &Poller{
		RoofURL: srv.URL + "/roof", RainURL: srv.URL + "/rain", NINAURL: srv.URL + "/v2/api",
		Interval: time.Hour, StaleAfter: time.Minute, Client: srv.Client(),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), failed: map[string]bool{},
	}
	p.pollAll(context.Background())

	s := p.Snapshot(time.Now())
	if s.Roof != "OPEN" || s.Rain != "RAIN" {
		t.Fatalf("roof %q rain %q", s.Roof, s.Rain)
	}
	if !s.NINA.Connected || s.NINA.Mount != "tracking" || s.NINA.Filter != "Ha" || s.NINA.CameraC != -10 {
		t.Fatalf("nina %+v", s.NINA)
	}
	lines := s.Lines()
	if len(lines) != 2 || !lines[0].Alert || lines[1].Text != "NINA: Mount tracking · Filter Ha · Camera -10.0 C" {
		t.Fatalf("lines %+v", lines)
	}

	// Two minutes later with no new poll, every source is unknown.
	s = p.Snapshot(time.Now().Add(2 * time.Minute))
	if s.Roof != Unknown || s.Rain != Unknown || s.NINA.Connected || !math.IsNaN(s.NINA.CameraC) {
		t.Fatalf("stale snapshot %+v", s)
	}
	if s.RoofOpen() {
		t.Fatal("unknown roof must not read open")
	}
}

func TestDownSourceStaysUnknown(t *testing.T) {
	p := &Poller{
		RoofURL: "http://127.0.0.1:1/status.json", Interval: time.Hour, StaleAfter: time.Minute,
		Client: &http.Client{Timeout: time.Second}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		failed: map[string]bool{},
	}
	p.pollAll(context.Background())
	if s := p.Snapshot(time.Now()); s.Roof != Unknown {
		t.Fatalf("roof %q, want unknown", s.Roof)
	}
}
