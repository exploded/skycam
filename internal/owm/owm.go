// Package owm polls OpenWeatherMap's Current Weather API for the ambient
// conditions the all-sky camera cannot see: temperature, humidity,
// pressure, wind, rain. skycam passes these through its Alpaca device so
// switching N.I.N.A.'s weather source from OpenWeatherMap to skycam loses
// nothing from the FITS headers. Ported from skyq.
//
// OpenWeatherMap's cloud cover is deliberately NOT used: a satellite-model
// number refreshed every 10 minutes is exactly the lagging signal the
// all-sky camera exists to beat.
//
// Unknown is not known applies here too: a reading older than MaxAge
// answers as unavailable, never as a stale number presented as current.
package owm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// Reading is one set of current conditions, metric units throughout.
type Reading struct {
	Temperature float64 // °C
	Humidity    float64 // %
	Pressure    float64 // hPa
	DewPoint    float64 // °C, computed (Magnus) — not in the 2.5 API
	WindSpeed   float64 // m/s
	WindDir     float64 // degrees
	WindGust    float64 // m/s; wind speed when the API omits gust
	RainRate    float64 // mm/h (API's rain over the last hour; 0 when absent)
	At          time.Time
}

type Options struct {
	APIKey   string
	Lat, Lon float64
	// BaseURL overrides the API host for tests. Default
	// https://api.openweathermap.org.
	BaseURL string
	// Interval between fetches. Default 10 min — OpenWeatherMap itself only
	// refreshes that often, so polling faster buys nothing.
	Interval time.Duration
	// MaxAge after which Current returns an error. Default 30 min (three
	// missed fetches).
	MaxAge time.Duration
	Client *http.Client
	Now    func() time.Time // test hook
}

// Poller fetches on an interval and serves the latest reading.
type Poller struct {
	opts Options

	mu   sync.Mutex
	cur  Reading
	ok   bool
	last error
}

func New(opts Options) *Poller {
	if opts.BaseURL == "" {
		opts.BaseURL = "https://api.openweathermap.org"
	}
	if opts.Interval <= 0 {
		opts.Interval = 10 * time.Minute
	}
	if opts.MaxAge <= 0 {
		opts.MaxAge = 30 * time.Minute
	}
	if opts.Client == nil {
		opts.Client = &http.Client{Timeout: 15 * time.Second}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Poller{opts: opts}
}

// Run fetches immediately, then on every interval, until ctx is done.
func (p *Poller) Run(ctx context.Context) {
	p.poll(ctx)
	t := time.NewTicker(p.opts.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.poll(ctx)
		}
	}
}

func (p *Poller) poll(ctx context.Context) {
	r, err := p.Fetch(ctx)
	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		// Keep the previous reading; Current's MaxAge check decides when it
		// stops being served.
		p.last = err
		slog.Warn("openweathermap", "err", err)
		return
	}
	p.cur, p.ok, p.last = r, true, nil
}

// Current returns the latest reading, or an error when there is none or it
// is older than MaxAge.
func (p *Poller) Current() (Reading, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.ok {
		if p.last != nil {
			return Reading{}, fmt.Errorf("no weather data: %w", p.last)
		}
		return Reading{}, errors.New("no weather data yet")
	}
	if age := p.opts.Now().Sub(p.cur.At); age > p.opts.MaxAge {
		return Reading{}, fmt.Errorf("weather data %s old", age.Round(time.Minute))
	}
	return p.cur, nil
}

// owmResponse is the subset of api.openweathermap.org/data/2.5/weather we
// read. Pointers mark the fields the API omits when there is nothing to
// report (gust, rain).
type owmResponse struct {
	Main struct {
		Temp     float64 `json:"temp"`
		Pressure float64 `json:"pressure"`
		Humidity float64 `json:"humidity"`
	} `json:"main"`
	Wind struct {
		Speed float64  `json:"speed"`
		Deg   float64  `json:"deg"`
		Gust  *float64 `json:"gust"`
	} `json:"wind"`
	Rain struct {
		OneH *float64 `json:"1h"`
	} `json:"rain"`
}

// Fetch performs one API call. Exposed for the Alpaca Refresh action.
func (p *Poller) Fetch(ctx context.Context) (Reading, error) {
	q := url.Values{}
	q.Set("lat", fmt.Sprintf("%.4f", p.opts.Lat))
	q.Set("lon", fmt.Sprintf("%.4f", p.opts.Lon))
	q.Set("units", "metric")
	q.Set("appid", p.opts.APIKey)
	u := p.opts.BaseURL + "/data/2.5/weather?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Reading{}, err
	}
	resp, err := p.opts.Client.Do(req)
	if err != nil {
		// A url.Error quotes the URL, and the URL carries the API key; this
		// error is logged and shipped to monitor, so drop the URL.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return Reading{}, fmt.Errorf("fetching current weather: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return Reading{}, fmt.Errorf("current weather: HTTP %s", resp.Status)
	}
	var body owmResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return Reading{}, fmt.Errorf("decoding current weather: %w", err)
	}

	r := Reading{
		Temperature: body.Main.Temp,
		Humidity:    body.Main.Humidity,
		Pressure:    body.Main.Pressure,
		DewPoint:    DewPoint(body.Main.Temp, body.Main.Humidity),
		WindSpeed:   body.Wind.Speed,
		WindDir:     body.Wind.Deg,
		WindGust:    body.Wind.Speed, // gust can never be below the mean
		At:          p.opts.Now(),
	}
	if body.Wind.Gust != nil {
		r.WindGust = math.Max(*body.Wind.Gust, body.Wind.Speed)
	}
	if body.Rain.OneH != nil {
		r.RainRate = *body.Rain.OneH
	}
	return r, nil
}

// DewPoint computes the dew point from temperature (°C) and relative
// humidity (%) with the Magnus formula — the 2.5 API does not provide it,
// so like N.I.N.A.'s own OpenWeatherMap client we derive it.
func DewPoint(tempC, rh float64) float64 {
	const a, b = 17.62, 243.12
	if rh <= 0 {
		rh = 0.001 // avoid log(0); a bone-dry reading rounds to a very low dew point
	}
	gamma := math.Log(rh/100) + a*tempC/(b+tempC)
	return b * gamma / (a - gamma)
}
