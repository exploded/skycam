package owm

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The values N.I.N.A. showed on 2026-09-03: temp 10.83 °C, humidity 77%,
// pressure 1011.56 hPa, wind 1.11 m/s @ 341°, gust 1.72 m/s. Its dew point
// (7.04 °C) is derived, so ours must land close.
const sampleBody = `{
	"main": {"temp": 10.83, "pressure": 1011.56, "humidity": 77},
	"wind": {"speed": 1.11, "deg": 341, "gust": 1.72},
	"rain": {"1h": 0.5}
}`

func fakeOWM(t *testing.T, body string, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/data/2.5/weather" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("appid") != "testkey" || q.Get("units") != "metric" {
			t.Errorf("query = %v", q)
		}
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchParsesAndDerivesDewPoint(t *testing.T) {
	srv := fakeOWM(t, sampleBody, http.StatusOK)
	p := New(Options{APIKey: "testkey", Lat: -37.8, Lon: 145.0, BaseURL: srv.URL})
	r, err := p.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r.Temperature != 10.83 || r.Humidity != 77 || r.Pressure != 1011.56 {
		t.Errorf("main fields: %+v", r)
	}
	if r.WindSpeed != 1.11 || r.WindDir != 341 || r.WindGust != 1.72 {
		t.Errorf("wind fields: %+v", r)
	}
	if r.RainRate != 0.5 {
		t.Errorf("RainRate = %v, want 0.5", r.RainRate)
	}
	// N.I.N.A. derived 7.04 °C from the same inputs; Magnus constants
	// differ slightly between implementations, so allow a small band.
	if math.Abs(r.DewPoint-7.0) > 0.3 {
		t.Errorf("DewPoint = %.2f, want ~7.0", r.DewPoint)
	}
}

func TestFetchOmittedGustAndRain(t *testing.T) {
	srv := fakeOWM(t, `{"main":{"temp":5,"pressure":1000,"humidity":50},"wind":{"speed":3.2,"deg":90}}`, http.StatusOK)
	p := New(Options{APIKey: "testkey", BaseURL: srv.URL})
	r, err := p.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r.WindGust != r.WindSpeed {
		t.Errorf("absent gust: WindGust = %v, want wind speed %v", r.WindGust, r.WindSpeed)
	}
	if r.RainRate != 0 {
		t.Errorf("absent rain: RainRate = %v, want 0", r.RainRate)
	}
}

func TestCurrentGoesStale(t *testing.T) {
	srv := fakeOWM(t, sampleBody, http.StatusOK)
	now := time.Date(2026, 9, 3, 22, 0, 0, 0, time.UTC)
	p := New(Options{
		APIKey: "testkey", BaseURL: srv.URL,
		MaxAge: 30 * time.Minute,
		Now:    func() time.Time { return now },
	})
	p.poll(context.Background())
	if _, err := p.Current(); err != nil {
		t.Fatalf("fresh reading unavailable: %v", err)
	}
	now = now.Add(31 * time.Minute)
	if _, err := p.Current(); err == nil {
		t.Fatal("31-minute-old reading served as current — unknown must not read as known")
	}
}

func TestPollKeepsLastReadingThroughErrors(t *testing.T) {
	var fail atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Write([]byte(sampleBody))
	}))
	t.Cleanup(srv.Close)

	p := New(Options{APIKey: "testkey", BaseURL: srv.URL})
	p.poll(context.Background())
	fail.Store(true)
	p.poll(context.Background())
	// One failed fetch inside MaxAge must not blank the sensors.
	r, err := p.Current()
	if err != nil {
		t.Fatalf("reading dropped after one failed poll: %v", err)
	}
	if r.Temperature != 10.83 {
		t.Errorf("Temperature = %v, want the pre-failure 10.83", r.Temperature)
	}
}

func TestNeverFetchedIsError(t *testing.T) {
	p := New(Options{APIKey: "testkey"})
	if _, err := p.Current(); err == nil {
		t.Fatal("Current with no fetch yet must error")
	}
}

func TestFetchErrorHidesKey(t *testing.T) {
	// The error is logged and shipped to monitor; it must not carry the key.
	p := New(Options{APIKey: "s3cr3t-key", BaseURL: "http://127.0.0.1:1"})
	_, err := p.Fetch(context.Background())
	if err == nil {
		t.Fatal("fetch from a closed port succeeded")
	}
	if strings.Contains(err.Error(), "s3cr3t-key") {
		t.Errorf("error leaks the API key: %v", err)
	}
}
