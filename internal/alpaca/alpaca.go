// Package alpaca exposes the all-sky camera as an ASCOM Alpaca
// ObservingConditions device, so N.I.N.A. can read cloud cover from the Pi
// whether or not anything else is running. It is advisory weather data
// only, never a SafetyMonitor: cloud must never gate the roof (skyq §8).
//
// Unknown is not clear. A stale frame, a shut roof, twilight or a missing
// clear-sky baseline answers with an Alpaca error (ValueNotSet, 1026),
// which clients show as unavailable. It is never reported as zero cloud.
//
// Ported from skyq's internal/alpaca, which served the same device from
// the NUC before skycam replaced its sky analysis.
package alpaca

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/exploded/skycam/internal/capture"
	"github.com/exploded/skycam/internal/exposure"
	"github.com/exploded/skycam/internal/owm"
)

// Alpaca error numbers (ASCOM spec).
const (
	errNotImplemented = 0x400 // 1024
	errInvalidValue   = 0x401 // 1025
	errValueNotSet    = 0x402 // 1026
)

const (
	deviceName = "skycam all-sky camera"
	deviceDesc = "Cloud cover and sky brightness from the observatory all-sky camera (advisory; never a safety monitor)"
	driverInfo = "skycam, github.com/exploded/skycam"
	driverVer  = "1.0"
	uniqueID   = "c7a2beab-1bbb-4d16-a3c2-98eb7a9174df"
)

// Source is where the device reads the newest frame; capture.Hub is one.
type Source interface {
	Latest() *capture.Record
}

// WeatherSource supplies ambient conditions (the OpenWeatherMap
// pass-through), so N.I.N.A.'s FITS headers keep temperature, humidity,
// pressure, dew point and wind when its weather source is skycam.
type WeatherSource interface {
	Current() (owm.Reading, error)
}

// Server implements the Alpaca HTTP API over the newest frame.
type Server struct {
	Src        Source
	MaxAge     time.Duration // a frame older than this is unavailable
	BlackLevel float64       // ADU subtracted before normalising brightness
	SQMOffset  float64       // added to the sky magnitude for SkyQuality
	Now        func() time.Time
	// Weather is optional; nil leaves the ambient sensors not implemented.
	Weather WeatherSource

	txn       atomic.Uint32
	connected atomic.Bool
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Handler returns the routes: device API and management API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/management/apiversions", func(w http.ResponseWriter, r *http.Request) {
		s.reply(w, r, []int{1}, 0, "")
	})
	mux.HandleFunc("/management/v1/description", func(w http.ResponseWriter, r *http.Request) {
		s.reply(w, r, map[string]string{
			"ServerName": "skycam", "Manufacturer": "exploded",
			"ManufacturerVersion": driverVer, "Location": "observatory all-sky camera",
		}, 0, "")
	})
	mux.HandleFunc("/management/v1/configureddevices", func(w http.ResponseWriter, r *http.Request) {
		s.reply(w, r, []map[string]any{{
			"DeviceName": deviceName, "DeviceType": "ObservingConditions",
			"DeviceNumber": 0, "UniqueID": uniqueID,
		}}, 0, "")
	})
	mux.HandleFunc("/api/v1/observingconditions/0/", s.device)
	return mux
}

// frame returns the newest frame, or why there is none worth reporting.
func (s *Server) frame() (*capture.Record, string) {
	rec := s.Src.Latest()
	if rec == nil {
		return nil, "no frames yet"
	}
	if age := s.now().Sub(rec.Time); s.MaxAge > 0 && age > s.MaxAge {
		return nil, fmt.Sprintf("newest frame is %s old", age.Round(time.Second))
	}
	return rec, ""
}

func (s *Server) device(w http.ResponseWriter, r *http.Request) {
	prop := strings.ToLower(strings.TrimPrefix(r.URL.Path, "/api/v1/observingconditions/0/"))
	if prop == "" || strings.Contains(prop, "/") {
		http.Error(w, "unknown device endpoint", http.StatusBadRequest)
		return
	}
	if r.Method == http.MethodPut {
		s.put(w, r, prop)
		return
	}

	switch prop {
	case "connected":
		s.reply(w, r, s.connected.Load(), 0, "")
	case "name":
		s.reply(w, r, deviceName, 0, "")
	case "description":
		s.reply(w, r, deviceDesc, 0, "")
	case "driverinfo":
		s.reply(w, r, driverInfo, 0, "")
	case "driverversion":
		s.reply(w, r, driverVer, 0, "")
	case "interfaceversion":
		s.reply(w, r, 1, 0, "")
	case "supportedactions":
		s.reply(w, r, []string{}, 0, "")
	case "averageperiod":
		s.reply(w, r, 0.0, 0, "")

	case "cloudcover":
		rec, why := s.frame()
		switch {
		case rec == nil:
			s.reply(w, r, nil, errValueNotSet, "cloud cover unavailable: "+why)
		case math.IsNaN(rec.Cloud):
			s.reply(w, r, nil, errValueNotSet, "cloud cover unavailable: "+cloudReason(rec))
		default:
			s.reply(w, r, math.Max(0, math.Min(100, 100*rec.Cloud)), 0, "")
		}
	case "skybrightness":
		rec, why := s.frame()
		if rec == nil {
			s.reply(w, r, nil, errValueNotSet, "sky brightness unavailable: "+why)
			return
		}
		// With the roof over it the camera measures the roof, not the sky.
		if !rec.Obs.RoofOpen() {
			s.reply(w, r, nil, errValueNotSet, "sky brightness unavailable: roof "+strings.ToLower(rec.Obs.Roof))
			return
		}
		v := s.brightness(rec)
		if math.IsNaN(v) {
			s.reply(w, r, nil, errValueNotSet, "sky brightness unavailable: sky level not measured")
			return
		}
		s.reply(w, r, v, 0, "")
	case "timesincelastupdate":
		switch sensor := strings.ToLower(param(r, "SensorName")); {
		case ambientSensors[sensor]:
			if s.Weather == nil {
				s.reply(w, r, nil, errNotImplemented, "sensor not implemented")
				return
			}
			cur, err := s.Weather.Current()
			if err != nil {
				s.reply(w, r, nil, errValueNotSet, err.Error())
				return
			}
			s.reply(w, r, s.now().Sub(cur.At).Seconds(), 0, "")
		case sensor == "", sensor == "cloudcover", sensor == "skybrightness", sensor == "skyquality":
			rec := s.Src.Latest()
			if rec == nil {
				s.reply(w, r, nil, errValueNotSet, "no frames yet")
				return
			}
			s.reply(w, r, s.now().Sub(rec.Time).Seconds(), 0, "")
		default:
			s.reply(w, r, nil, errNotImplemented, "sensor not implemented")
		}
	case "sensordescription":
		s.reply(w, r, s.sensorDescription(param(r, "SensorName")), 0, "")

	case "skyquality":
		rec, why := s.frame()
		switch {
		case rec == nil:
			s.reply(w, r, nil, errValueNotSet, "sky quality unavailable: "+why)
		case !rec.Obs.RoofOpen():
			s.reply(w, r, nil, errValueNotSet, "sky quality unavailable: roof "+strings.ToLower(rec.Obs.Roof))
		case rec.Mode != "night":
			s.reply(w, r, nil, errValueNotSet, "sky quality unavailable: daylight")
		case math.IsNaN(rec.SkyMag) || math.IsInf(rec.SkyMag, 0):
			s.reply(w, r, nil, errValueNotSet, "sky quality unavailable: sky level not measured")
		default:
			s.reply(w, r, rec.SkyMag+s.SQMOffset, 0, "")
		}

	case "temperature", "humidity", "pressure", "dewpoint",
		"windspeed", "winddirection", "windgust", "rainrate":
		s.ambient(w, r, prop)

	case "starfwhm", "skytemperature":
		s.reply(w, r, nil, errNotImplemented, "sensor not implemented: "+prop)

	default:
		http.Error(w, "unknown property "+prop, http.StatusBadRequest)
	}
}

// cloudReason explains a frame with no cloud estimate, matching the gate
// in capture: night mode, roof known open, sun below −12°, and a baseline.
func cloudReason(rec *capture.Record) string {
	switch {
	case !rec.Obs.RoofOpen():
		return "roof " + strings.ToLower(rec.Obs.Roof)
	case rec.Mode != "night" || rec.SunAlt >= -12:
		return "not dark enough"
	default:
		return "no clear-sky baseline yet"
	}
}

// brightness is the sky level normalised to one second at unity gain:
// instrumental ADU per second, larger is brighter. It is comparable across
// the auto-exposure's changes, but it is not lux.
func (s *Server) brightness(rec *capture.Record) float64 {
	secs := rec.Exposure.Seconds()
	if secs <= 0 || math.IsNaN(rec.SkyLevel) {
		return math.NaN()
	}
	return math.Max(0, rec.SkyLevel-s.BlackLevel) / (secs * exposure.GainFactor(rec.Gain))
}

var ambientSensors = map[string]bool{
	"temperature": true, "humidity": true, "pressure": true, "dewpoint": true,
	"windspeed": true, "winddirection": true, "windgust": true, "rainrate": true,
}

// ambient answers the OpenWeatherMap pass-through sensors: 1024 when no
// weather source is configured (N.I.N.A. greys the field out), 1026 when
// the source has no fresh reading (never a stale number shown as current),
// otherwise the value.
func (s *Server) ambient(w http.ResponseWriter, r *http.Request, prop string) {
	if s.Weather == nil {
		s.reply(w, r, nil, errNotImplemented, "sensor not implemented: "+prop+" (no openweathermap api_key configured)")
		return
	}
	cur, err := s.Weather.Current()
	if err != nil {
		s.reply(w, r, nil, errValueNotSet, prop+" unavailable: "+err.Error())
		return
	}
	var v float64
	switch prop {
	case "temperature":
		v = cur.Temperature
	case "humidity":
		v = cur.Humidity
	case "pressure":
		v = cur.Pressure
	case "dewpoint":
		v = cur.DewPoint
	case "windspeed":
		v = cur.WindSpeed
	case "winddirection":
		v = cur.WindDir
	case "windgust":
		v = cur.WindGust
	case "rainrate":
		v = cur.RainRate
	}
	s.reply(w, r, v, 0, "")
}

func (s *Server) sensorDescription(sensor string) string {
	sensor = strings.ToLower(sensor)
	switch {
	case sensor == "cloudcover":
		return "Cloud cover from the all-sky camera's star count against its clear-sky baseline; only with the roof open and the sun below -12 degrees. Advisory."
	case sensor == "skybrightness":
		return "All-sky camera sky level in ADU per second at unity gain. Instrumental units, not lux."
	case sensor == "skyquality":
		return "Approximate sky brightness in mag/arcsec^2 from the all-sky camera, calibrated by one offset (analysis.sqm_offset); not a metered SQM. Higher is darker."
	case ambientSensors[sensor] && s.Weather != nil:
		return "OpenWeatherMap current conditions for the site coordinates (dew point derived with the Magnus formula)."
	default:
		return "not implemented"
	}
}

func (s *Server) put(w http.ResponseWriter, r *http.Request, prop string) {
	switch prop {
	case "connected":
		v := strings.ToLower(param(r, "Connected"))
		if v != "true" && v != "false" {
			s.reply(w, r, nil, errInvalidValue, "Connected must be true or false")
			return
		}
		s.connected.Store(v == "true")
		s.reply(w, r, nil, 0, "")
	case "averageperiod":
		if v, err := strconv.ParseFloat(param(r, "AveragePeriod"), 64); err != nil || v != 0 {
			s.reply(w, r, nil, errInvalidValue, "only AveragePeriod 0 is supported")
			return
		}
		s.reply(w, r, nil, 0, "")
	case "refresh":
		s.reply(w, r, nil, 0, "") // every frame is already current
	default:
		s.reply(w, r, nil, errNotImplemented, "cannot set "+prop)
	}
}

// param fetches a query or form parameter case-insensitively: the Alpaca
// spec requires accepting parameter names as the client cases them.
func param(r *http.Request, name string) string {
	r.ParseForm()
	for k, v := range r.Form {
		if strings.EqualFold(k, name) && len(v) > 0 {
			return v[0]
		}
	}
	return ""
}

// envelope is the standard Alpaca response. Value must not be omitempty:
// a legitimate false or 0 (Connected=false, AveragePeriod=0) would vanish.
type envelope struct {
	Value               any    `json:"Value"`
	ClientTransactionID uint32 `json:"ClientTransactionID"`
	ServerTransactionID uint32 `json:"ServerTransactionID"`
	ErrorNumber         int    `json:"ErrorNumber"`
	ErrorMessage        string `json:"ErrorMessage"`
}

func (s *Server) reply(w http.ResponseWriter, r *http.Request, value any, errNum int, errMsg string) {
	ctid, _ := strconv.ParseUint(param(r, "ClientTransactionID"), 10, 32)
	env := envelope{
		Value:               value,
		ClientTransactionID: uint32(ctid),
		ServerTransactionID: s.txn.Add(1),
		ErrorNumber:         errNum,
		ErrorMessage:        errMsg,
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(env); err != nil {
		slog.Warn("alpaca: encoding response", "err", err)
	}
}
