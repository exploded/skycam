package web

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/exploded/skycam/internal/capture"
	"github.com/exploded/skycam/internal/config"
)

// Every config key must be on the page or locked to config.json, so a new
// setting can't be forgotten; and every field must name a real key.
func TestSettingsCoverConfig(t *testing.T) {
	flat := config.Flatten(config.Default())
	covered := map[string]bool{}
	for _, g := range settingsGroups {
		for _, f := range g.Fields {
			found := false
			for k := range flat {
				if k == f.Key || (f.Kind == "rect" && strings.HasPrefix(k, f.Key+".")) {
					covered[k], found = true, true
				}
			}
			if !found {
				t.Errorf("field %s is not a config key", f.Key)
			}
			if config.IsLocked(f.Key) {
				t.Errorf("field %s is locked", f.Key)
			}
		}
	}
	for k := range flat {
		if !covered[k] && !config.IsLocked(k) {
			t.Errorf("config key %s is neither on the settings page nor locked", k)
		}
	}
}

func settingsServer(t *testing.T) (*httptest.Server, *Server, *atomic.Int32) {
	t.Helper()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.ImagesPassword = "pw-7f3a9c"
	var restarts atomic.Int32
	s := &Server{Cfg: cfg, Base: cfg, Loc: time.UTC, Hub: capture.NewHub(), Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Restart: func() { restarts.Add(1) }}
	h, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, s, &restarts
}

// formFor is the form the page would submit for cfg.
func formFor(cfg config.Config) url.Values {
	flat := config.Flatten(cfg)
	v := url.Values{}
	for _, g := range settingsGroups {
		for _, f := range g.Fields {
			if f.Kind == "bool" {
				if on, _ := flat[f.Key].(bool); on {
					v.Set(f.Key, "on")
				}
				continue
			}
			v.Set(f.Key, formatValue(flat, f))
		}
	}
	return v
}

func post(t *testing.T, srv *httptest.Server, path string, form url.Values, origin string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestSettingsSave(t *testing.T) {
	srv, s, restarts := settingsServer(t)
	code, body := get(t, srv.URL+"/settings", false)
	if code != 200 || !strings.Contains(body, "observatory.roof_url") || strings.Contains(body, "pw-7f3a9c") {
		t.Fatalf("page %d (and secrets must not show)", code)
	}

	form := formFor(s.Cfg)
	form.Set("exposure.night.max_exposure", "8s")
	form.Set("observatory.rain_url", "http://192.168.1.60/status.json")
	code, body = post(t, srv, "/settings", form, srv.URL)
	if code != 200 || !strings.Contains(body, "Saved") {
		t.Fatalf("save %d: %s", code, body)
	}
	time.Sleep(700 * time.Millisecond)
	if restarts.Load() != 1 {
		t.Errorf("restarts %d", restarts.Load())
	}
	o, err := config.ReadSettings(s.Cfg.SettingsPath())
	if err != nil || len(o) != 2 || o["exposure.night.max_exposure"] != "8s" || o["observatory.rain_url"] != "http://192.168.1.60/status.json" {
		t.Errorf("settings.json %v %v", o, err)
	}

	// Saving the unchanged form changes nothing and doesn't restart.
	if _, body := post(t, srv, "/settings", formFor(s.Cfg), srv.URL); !strings.Contains(body, "Nothing changed") {
		t.Errorf("unchanged save: %s", body)
	}
}

func TestSettingsRejectsBadValues(t *testing.T) {
	srv, s, restarts := settingsServer(t)
	for name, change := range map[string][2]string{
		"not a duration": {"exposure.night.max_exposure", "five seconds"},
		"min over max":   {"exposure.night.min_exposure", "10s"},
		"alpaca port":    {"observatory.roof_url", "http://192.168.1.50:11111/status.json"},
		"bad rect":       {"exposure.meter", "0, 0.2, 1"},
		"bad option":     {"timelapse.preset", "warp"},
	} {
		form := formFor(s.Cfg)
		form.Set(change[0], change[1])
		code, body := post(t, srv, "/settings", form, srv.URL)
		if code != 200 || !strings.Contains(body, "Not saved") {
			t.Errorf("%s: %d, no error shown", name, code)
		}
	}
	if _, err := os.Stat(s.Cfg.SettingsPath()); err == nil || restarts.Load() != 0 {
		t.Error("a bad form was saved")
	}
}

func TestSettingsReset(t *testing.T) {
	srv, s, restarts := settingsServer(t)
	s.Cfg.Live.BlockFor = config.Duration(time.Minute) // as if settings.json had it
	code, body := post(t, srv, "/settings/reset", nil, srv.URL)
	if code != 200 || !strings.Contains(body, "live.block_for") {
		t.Fatalf("reset %d: %s", code, body)
	}
	time.Sleep(700 * time.Millisecond)
	if o, _ := config.ReadSettings(s.Cfg.SettingsPath()); len(o) != 0 || restarts.Load() != 1 {
		t.Errorf("after reset: %v, %d restarts", o, restarts.Load())
	}
}

func TestSettingsLocalOnly(t *testing.T) {
	_, s, _ := settingsServer(t)
	h, _ := s.Handler()
	// The Pi's home subnet, with global IPv6 as home networks have.
	orig := localPrefixes
	localPrefixes = func() []netip.Prefix {
		return []netip.Prefix{netip.MustParsePrefix("2001:db8:1::/64")}
	}
	t.Cleanup(func() { localPrefixes = orig })
	for _, c := range []struct {
		remote, host, origin string
		method               string
		want                 int
	}{
		{"192.168.1.5:5000", "192.168.1.53", "", http.MethodGet, 200},
		{"127.0.0.1:5000", "localhost:8999", "", http.MethodGet, 200},
		{"[fd00::5]:5000", "allsky.local", "", http.MethodGet, 200},
		{"[fe80::1]:5000", "allsky", "", http.MethodGet, 200},
		{"8.8.8.8:5000", "192.168.1.53", "", http.MethodGet, 403},
		{"[2001:db8:1:0:17da:7304:a7a9:f82a]:5000", "allsky.local", "", http.MethodGet, 200},
		{"[2001:db8::1]:5000", "allsky.local", "", http.MethodGet, 403},
		// DNS rebinding: an outside name pointed at the Pi.
		{"192.168.1.5:5000", "evil.example.com", "", http.MethodGet, 403},
		// A page elsewhere posting to the Pi.
		{"192.168.1.5:5000", "allsky.local", "https://evil.example.com", http.MethodPost, 403},
	} {
		req := httptest.NewRequest(c.method, "http://"+c.host+"/settings", strings.NewReader(""))
		req.RemoteAddr, req.Host = c.remote, c.host
		if c.origin != "" {
			req.Header.Set("Origin", c.origin)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s %s from %s (host %s): %d, want %d", c.method, "/settings", c.remote, c.host, rec.Code, c.want)
		}
	}
}

func TestCheckSource(t *testing.T) {
	roof := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"shutter": "CLOSED", "openSw": false, "closedSw": true}`))
	}))
	defer roof.Close()
	srv, _, _ := settingsServer(t)
	_, body := post(t, srv, "/settings/check", url.Values{"source": {"roof"}, "observatory.roof_url": {roof.URL + "/status.json"}}, srv.URL)
	if !strings.Contains(body, "CLOSED") {
		t.Errorf("check: %s", body)
	}
	_, body = post(t, srv, "/settings/check", url.Values{"source": {"rain"}, "observatory.rain_url": {roof.URL + "/status.json"}}, srv.URL)
	if !strings.Contains(body, `class="bad"`) {
		t.Errorf("rain check of a roof: %s", body)
	}
}
