package alpaca

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/exploded/skycam/internal/capture"
	"github.com/exploded/skycam/internal/observatory"
)

type stubSource struct{ rec *capture.Record }

func (s stubSource) Latest() *capture.Record { return s.rec }

var now = time.Date(2026, 10, 24, 23, 0, 0, 0, time.UTC)

// darkOpen is a frame the capture loop would have estimated cloud for.
func darkOpen(cloud float64) *capture.Record {
	return &capture.Record{
		Time: now.Add(-30 * time.Second), Mode: "night", SunAlt: -30,
		Exposure: 5 * time.Second, Gain: 0, SkyLevel: 30, Cloud: cloud,
		Obs: observatory.Snapshot{Roof: "OPEN", Rain: "SAFE"},
	}
}

func newTestServer(t *testing.T, rec *capture.Record) *httptest.Server {
	t.Helper()
	s := &Server{Src: stubSource{rec}, MaxAge: 5 * time.Minute, Now: func() time.Time { return now }}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, url string) envelope {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %s", url, resp.Status)
	}
	var env envelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	return env
}

const dev = "/api/v1/observingconditions/0/"

func TestManagementAPI(t *testing.T) {
	srv := newTestServer(t, nil)
	env := get(t, srv.URL+"/management/v1/configureddevices")
	devices, ok := env.Value.([]any)
	if !ok || len(devices) != 1 {
		t.Fatalf("configureddevices = %v", env.Value)
	}
	d := devices[0].(map[string]any)
	if d["DeviceType"] != "ObservingConditions" {
		t.Errorf("DeviceType = %v; must never be SafetyMonitor", d["DeviceType"])
	}
	if get(t, srv.URL+"/management/apiversions").ErrorNumber != 0 {
		t.Error("apiversions errored")
	}
}

func TestEnvelopeAndCaseInsensitiveParams(t *testing.T) {
	srv := newTestServer(t, nil)
	env := get(t, srv.URL+dev+"name?clienttransactionid=77")
	if env.ClientTransactionID != 77 {
		t.Errorf("ClientTransactionID echo = %d, want 77", env.ClientTransactionID)
	}
	if env.ServerTransactionID == 0 {
		t.Error("ServerTransactionID must be non-zero")
	}
	if env.ErrorNumber != 0 || env.Value == "" {
		t.Errorf("name: %+v", env)
	}
}

func TestCloudCover(t *testing.T) {
	stale := darkOpen(0.2)
	stale.Time = now.Add(-10 * time.Minute)
	shut := darkOpen(math.NaN())
	shut.Obs.Roof = "CLOSED"
	unknown := darkOpen(math.NaN())
	unknown.Obs.Roof = observatory.Unknown
	dusk := darkOpen(math.NaN())
	dusk.SunAlt = -8
	noBase := darkOpen(math.NaN())

	for _, tc := range []struct {
		name string
		rec  *capture.Record
		want float64 // NaN: must be ValueNotSet
		why  string
	}{
		{"clear", darkOpen(0.1), 10, ""},
		{"cloudy", darkOpen(0.85), 85, ""},
		{"no frames", nil, math.NaN(), "no frames yet"},
		{"stale", stale, math.NaN(), "10m0s old"},
		{"roof shut", shut, math.NaN(), "roof closed"},
		{"roof unknown", unknown, math.NaN(), "roof unknown"},
		{"twilight", dusk, math.NaN(), "not dark enough"},
		{"no baseline", noBase, math.NaN(), "baseline"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := get(t, newTestServer(t, tc.rec).URL+dev+"cloudcover")
			if math.IsNaN(tc.want) {
				// Unknown is not clear: an error, never a zero.
				if env.ErrorNumber != errValueNotSet || env.Value != nil {
					t.Fatalf("got %+v, want ValueNotSet with no value", env)
				}
				if !strings.Contains(env.ErrorMessage, tc.why) {
					t.Errorf("message %q, want it to mention %q", env.ErrorMessage, tc.why)
				}
				return
			}
			if env.ErrorNumber != 0 || env.Value != tc.want {
				t.Errorf("got %+v, want %v", env, tc.want)
			}
		})
	}
}

func TestSkyBrightness(t *testing.T) {
	// 30 ADU over 5 s at gain 0 is 6 ADU/s.
	if env := get(t, newTestServer(t, darkOpen(0.1)).URL+dev+"skybrightness"); env.ErrorNumber != 0 || env.Value != 6.0 {
		t.Errorf("open roof: %+v, want 6", env)
	}
	// With the roof over it the camera measures the roof.
	shut := darkOpen(math.NaN())
	shut.Obs.Roof = "CLOSED"
	if env := get(t, newTestServer(t, shut).URL+dev+"skybrightness"); env.ErrorNumber != errValueNotSet {
		t.Errorf("roof shut: %+v, want ValueNotSet", env)
	}
}

func TestTimeSinceLastUpdate(t *testing.T) {
	srv := newTestServer(t, darkOpen(0.1))
	if env := get(t, srv.URL+dev+"timesincelastupdate?SensorName=CloudCover"); env.Value != 30.0 {
		t.Errorf("cloudcover age = %+v, want 30", env)
	}
	if env := get(t, srv.URL+dev+"timesincelastupdate?SensorName=Humidity"); env.ErrorNumber != errNotImplemented {
		t.Errorf("humidity age = %+v, want NotImplemented", env)
	}
	if env := get(t, newTestServer(t, nil).URL+dev+"timesincelastupdate"); env.ErrorNumber != errValueNotSet {
		t.Errorf("no frames: %+v, want ValueNotSet", env)
	}
}

func TestNotImplementedSensors(t *testing.T) {
	srv := newTestServer(t, darkOpen(0.1))
	for _, p := range []string{"skyquality", "temperature", "humidity", "windspeed", "rainrate"} {
		if e := get(t, srv.URL+dev+p); e.ErrorNumber != errNotImplemented {
			t.Errorf("%s: ErrorNumber=%d, want %d", p, e.ErrorNumber, errNotImplemented)
		}
	}
}

// putForm sends an Alpaca PUT (form-encoded body, as the spec requires).
func putForm(t *testing.T, u string, vals url.Values) envelope {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, u, strings.NewReader(vals.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var env envelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	return env
}

func TestConnectedRoundTrip(t *testing.T) {
	srv := newTestServer(t, nil)
	// Connected=false must serialise as Value:false, not vanish.
	env := get(t, srv.URL+dev+"connected")
	if v, ok := env.Value.(bool); !ok || v {
		t.Fatalf("initial connected = %v, want false", env.Value)
	}
	if e := putForm(t, srv.URL+dev+"connected", url.Values{"Connected": {"True"}, "ClientTransactionID": {"5"}}); e.ErrorNumber != 0 {
		t.Fatalf("PUT connected: %+v", e)
	}
	env = get(t, srv.URL+dev+"connected")
	if v, ok := env.Value.(bool); !ok || !v {
		t.Errorf("after PUT connected = %v, want true", env.Value)
	}
}

func TestAveragePeriodOnlyZero(t *testing.T) {
	srv := newTestServer(t, nil)
	if env := putForm(t, srv.URL+dev+"averageperiod", url.Values{"AveragePeriod": {"5"}}); env.ErrorNumber != errInvalidValue {
		t.Errorf("AveragePeriod=5: ErrorNumber=%d, want %d", env.ErrorNumber, errInvalidValue)
	}
}

func TestUnknownPropertyIs400(t *testing.T) {
	srv := newTestServer(t, nil)
	resp, err := http.Get(srv.URL + dev + "roofcontrol")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown property status = %d, want 400", resp.StatusCode)
	}
}

func TestNoSafetyMonitorAnywhere(t *testing.T) {
	// Cloud must never gate the roof: no safetymonitor path may answer.
	srv := newTestServer(t, darkOpen(0.1))
	resp, err := http.Get(srv.URL + "/api/v1/safetymonitor/0/issafe")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("a safetymonitor endpoint answered; cloud must never gate the roof")
	}
	if !strings.Contains(deviceDesc, "never a safety monitor") {
		t.Error("device description should state the advisory-only stance")
	}
}
