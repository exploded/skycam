package alpaca

import (
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/exploded/skycam/internal/owm"
)

type stubWeather struct {
	r   owm.Reading
	err error
}

func (s stubWeather) Current() (owm.Reading, error) { return s.r, s.err }

func newWeatherServer(t *testing.T, w WeatherSource) *httptest.Server {
	t.Helper()
	s := &Server{Src: stubSource{}, MaxAge: 5 * time.Minute, Now: func() time.Time { return now }, Weather: w}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func TestAmbientSensorsPassThrough(t *testing.T) {
	srv := newWeatherServer(t, stubWeather{r: owm.Reading{
		Temperature: 10.83, Humidity: 77, Pressure: 1011.56, DewPoint: 6.96,
		WindSpeed: 1.11, WindDir: 341, WindGust: 1.72, RainRate: 0, At: now,
	}})
	want := map[string]float64{
		"temperature": 10.83, "humidity": 77, "pressure": 1011.56,
		"dewpoint": 6.96, "windspeed": 1.11, "winddirection": 341,
		"windgust": 1.72, "rainrate": 0,
	}
	for prop, v := range want {
		env := get(t, srv.URL+dev+prop)
		if env.ErrorNumber != 0 {
			t.Errorf("%s: ErrorNumber=%d %s", prop, env.ErrorNumber, env.ErrorMessage)
			continue
		}
		// RainRate 0 is a real value (no rain) and must serialise, not vanish.
		if got, ok := env.Value.(float64); !ok || got != v {
			t.Errorf("%s = %v, want %v", prop, env.Value, v)
		}
	}
}

func TestAmbientUnavailableIsError(t *testing.T) {
	// A stale or failed weather source answers 1026, never the last number
	// presented as current.
	srv := newWeatherServer(t, stubWeather{err: errors.New("weather data 45m0s old")})
	for _, prop := range []string{"temperature", "dewpoint", "rainrate"} {
		if env := get(t, srv.URL+dev+prop); env.ErrorNumber != errValueNotSet {
			t.Errorf("%s: ErrorNumber=%d, want %d", prop, env.ErrorNumber, errValueNotSet)
		}
	}
}

func TestTimeSinceLastUpdatePerSensor(t *testing.T) {
	srv := newWeatherServer(t, stubWeather{r: owm.Reading{Temperature: 10, At: now.Add(-5 * time.Minute)}})
	// A weather sensor gives the age of the OpenWeatherMap reading.
	env := get(t, srv.URL+dev+"timesincelastupdate?sensorname=Temperature")
	if env.ErrorNumber != 0 || env.Value != 300.0 {
		t.Errorf("weather age = %+v, want 300", env)
	}
	// No sensor name is the camera's answer: no frames yet.
	if env := get(t, srv.URL+dev+"timesincelastupdate"); env.ErrorNumber != errValueNotSet {
		t.Errorf("device-wide: ErrorNumber=%d, want %d", env.ErrorNumber, errValueNotSet)
	}
}

func TestAmbientSensorDescription(t *testing.T) {
	srv := newWeatherServer(t, stubWeather{})
	env := get(t, srv.URL+dev+"sensordescription?SensorName=Temperature")
	if s, _ := env.Value.(string); env.ErrorNumber != 0 || s == "not implemented" || s == "" {
		t.Errorf("Temperature description = %+v, want the OpenWeatherMap note", env)
	}
}
