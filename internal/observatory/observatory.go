// Package observatory polls the roof controller, the rain sensor and
// N.I.N.A. so each frame can record and show the observatory's state.
//
// Everything here is read-only and advisory. A source that stops answering
// goes stale and reads "unknown" — never "closed" or "safe" by default.
// The ESP32 devices are read on their port-80 /status.json, never on the
// Alpaca port 11111, which belongs to N.I.N.A.
package observatory

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const Unknown = "unknown"

// Snapshot is the observatory at one moment, with staleness applied.
type Snapshot struct {
	// Roof is OPEN, CLOSED, OPENING, CLOSING, ERROR or unknown.
	Roof string `json:"roof"`
	// Rain is SAFE, RAIN or unknown.
	Rain string    `json:"rain"`
	NINA NINAState `json:"nina"`
}

// NINAState is what N.I.N.A.'s Advanced API reports. Connected is false
// when N.I.N.A. isn't answering or nothing is connected.
type NINAState struct {
	Connected bool    `json:"connected"`
	Mount     string  `json:"mount"`  // tracking, parked, slewing, stopped
	Filter    string  `json:"filter"` //
	CameraC   float64 `json:"camera_c"`
	Cooler    bool    `json:"cooler"`
}

// RoofOpen reports whether the roof is known to be fully open.
func (s Snapshot) RoofOpen() bool { return s.Roof == "OPEN" }

// Lines formats the snapshot for the overlay's bottom-left block.
func (s Snapshot) Lines() []Line {
	lines := []Line{{
		Text:  fmt.Sprintf("Roof: %s   Rain: %s", label(s.Roof), label(s.Rain)),
		Alert: s.Rain == "RAIN" || s.Roof == "ERROR",
	}}
	if n := s.NINA; n.Connected {
		var parts []string
		if n.Mount != "" {
			parts = append(parts, "Mount "+n.Mount)
		}
		if n.Filter != "" {
			parts = append(parts, "Filter "+n.Filter)
		}
		if !math.IsNaN(n.CameraC) {
			parts = append(parts, fmt.Sprintf("Camera %.1f C", n.CameraC))
		}
		if len(parts) > 0 {
			lines = append(lines, Line{Text: "NINA: " + strings.Join(parts, " · ")})
		}
	}
	return lines
}

// Line mirrors overlay.Line without importing it.
type Line struct {
	Text  string
	Alert bool
}

func label(s string) string {
	if s == "" {
		return Unknown
	}
	return s
}

// Poller keeps the latest reading from each source.
type Poller struct {
	RoofURL    string
	RainURL    string
	NINAURL    string
	Interval   time.Duration
	StaleAfter time.Duration
	Timeout    time.Duration // per request; zero means 4 s
	Client     *http.Client
	Log        *slog.Logger

	mu     sync.Mutex
	roof   reading[string]
	rain   reading[string]
	nina   reading[NINAState]
	failed map[string]bool // sources whose last poll failed, to log once
}

type reading[T any] struct {
	v  T
	at time.Time
}

func (r reading[T]) fresh(now time.Time, stale time.Duration) (T, bool) {
	var zero T
	if r.at.IsZero() || now.Sub(r.at) > stale {
		return zero, false
	}
	return r.v, true
}

// Snapshot returns the current state as of now.
func (p *Poller) Snapshot(now time.Time) Snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := Snapshot{Roof: Unknown, Rain: Unknown, NINA: NINAState{CameraC: math.NaN()}}
	if v, ok := p.roof.fresh(now, p.StaleAfter); ok {
		s.Roof = v
	}
	if v, ok := p.rain.fresh(now, p.StaleAfter); ok {
		s.Rain = v
	}
	if v, ok := p.nina.fresh(now, p.StaleAfter); ok {
		s.NINA = v
	}
	return s
}

// Run polls until ctx ends.
func (p *Poller) Run(ctx context.Context) {
	if p.Client == nil {
		p.Client = &http.Client{Timeout: p.timeout()}
	}
	if p.Log == nil {
		p.Log = slog.Default()
	}
	p.failed = map[string]bool{}
	t := time.NewTicker(p.Interval)
	defer t.Stop()
	for {
		p.pollAll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (p *Poller) timeout() time.Duration {
	if p.Timeout > 0 {
		return p.Timeout
	}
	return 4 * time.Second
}

// Check reads one source once — "roof", "rain" or "nina" at url — and
// describes what it answered, so the settings page can try an address
// before saving it.
func Check(ctx context.Context, source, url string, timeout time.Duration) (string, error) {
	p := &Poller{RoofURL: url, RainURL: url, NINAURL: url, Timeout: timeout}
	p.Client = &http.Client{Timeout: p.timeout()}
	switch source {
	case "roof":
		return p.pollRoof(ctx)
	case "rain":
		return p.pollRain(ctx)
	case "nina":
		st, err := p.pollNINA(ctx)
		if err != nil {
			return "", err
		}
		if l := (Snapshot{NINA: st}).Lines(); len(l) > 1 {
			return l[1].Text, nil
		}
		return "connected, no mount", nil
	}
	return "", fmt.Errorf("unknown source %q", source)
}

func (p *Poller) pollAll(ctx context.Context) {
	var wg sync.WaitGroup
	if p.RoofURL != "" {
		wg.Go(func() {
			v, err := p.pollRoof(ctx)
			p.record("roof", err, func(now time.Time) { p.roof = reading[string]{v, now} })
		})
	}
	if p.RainURL != "" {
		wg.Go(func() {
			v, err := p.pollRain(ctx)
			p.record("rain", err, func(now time.Time) { p.rain = reading[string]{v, now} })
		})
	}
	if p.NINAURL != "" {
		wg.Go(func() {
			v, err := p.pollNINA(ctx)
			p.record("nina", err, func(now time.Time) { p.nina = reading[NINAState]{v, now} })
		})
	}
	wg.Wait()
}

// record stores a successful reading, and logs a source going down or
// coming back once rather than on every poll.
func (p *Poller) record(src string, err error, store func(time.Time)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		if !p.failed[src] {
			p.Log.Info("observatory source not answering", "source", src, "err", err)
			p.failed[src] = true
		}
		return
	}
	if p.failed[src] {
		p.Log.Info("observatory source back", "source", src)
		p.failed[src] = false
	}
	store(time.Now())
}

func (p *Poller) getJSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := p.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

// pollRoof reads the roll-off controller's status.json. Its "shutter" is
// already OPEN/CLOSED/OPENING/CLOSING/ERROR, derived by the controller from
// its limit switches, so it is taken as is. Both switches made at once is
// a wiring fault whatever the label says.
func (p *Poller) pollRoof(ctx context.Context) (string, error) {
	var st struct {
		Shutter  string `json:"shutter"`
		OpenSw   bool   `json:"openSw"`
		ClosedSw bool   `json:"closedSw"`
	}
	if err := p.getJSON(ctx, p.RoofURL, &st); err != nil {
		return "", err
	}
	s := strings.ToUpper(strings.TrimSpace(st.Shutter))
	switch {
	case st.OpenSw && st.ClosedSw:
		return "ERROR", nil
	case s == "":
		return "", fmt.Errorf("roof status has no shutter field")
	}
	return s, nil
}

func (p *Poller) pollRain(ctx context.Context) (string, error) {
	var st struct {
		Safe *bool `json:"safe"`
	}
	if err := p.getJSON(ctx, p.RainURL, &st); err != nil {
		return "", err
	}
	if st.Safe == nil {
		return "", fmt.Errorf("rain status has no safe field")
	}
	if *st.Safe {
		return "SAFE", nil
	}
	return "RAIN", nil
}

// ninaEnvelope is the Advanced API's wrapper round every response.
type ninaEnvelope struct {
	Response json.RawMessage `json:"Response"`
	Success  bool            `json:"Success"`
	Error    string          `json:"Error"`
}

func (p *Poller) ninaGet(ctx context.Context, path string, out any) error {
	var env ninaEnvelope
	base := strings.TrimSuffix(strings.TrimRight(p.NINAURL, "/"), "/v2/api")
	if err := p.getJSON(ctx, base+"/v2/api"+path, &env); err != nil {
		return err
	}
	if !env.Success {
		return fmt.Errorf("NINA %s: %s", path, env.Error)
	}
	return json.Unmarshal(env.Response, out)
}

// nanFloat decodes a number or the string "NaN" the plugin sends for a
// value it doesn't have.
type nanFloat float64

func (f *nanFloat) UnmarshalJSON(b []byte) error {
	var v float64
	if json.Unmarshal(b, &v) == nil {
		*f = nanFloat(v)
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	if v, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
		*f = nanFloat(v)
	} else {
		*f = nanFloat(math.NaN())
	}
	return nil
}

func (p *Poller) pollNINA(ctx context.Context) (NINAState, error) {
	st := NINAState{CameraC: math.NaN()}
	var mount struct {
		Connected       bool
		AtPark          bool
		Slewing         bool
		TrackingEnabled bool
	}
	if err := p.ninaGet(ctx, "/equipment/mount/info", &mount); err != nil {
		return st, err
	}
	st.Connected = true
	switch {
	case !mount.Connected:
	case mount.Slewing:
		st.Mount = "slewing"
	case mount.AtPark:
		st.Mount = "parked"
	case mount.TrackingEnabled:
		st.Mount = "tracking"
	default:
		st.Mount = "stopped"
	}
	// The filter wheel and camera are extras: a failure there keeps the
	// mount reading.
	var fw struct {
		Connected      bool
		SelectedFilter struct{ Name string }
	}
	if p.ninaGet(ctx, "/equipment/filterwheel/info", &fw) == nil && fw.Connected {
		st.Filter = strings.TrimSpace(fw.SelectedFilter.Name)
	}
	var cam struct {
		Connected   bool
		CoolerOn    bool
		Temperature nanFloat
	}
	if p.ninaGet(ctx, "/equipment/camera/info", &cam) == nil && cam.Connected {
		st.CameraC = float64(cam.Temperature)
		st.Cooler = cam.CoolerOn
	}
	return st, nil
}
