// Package config loads skycam's JSON config. The file holds the /images
// password, the relay token and the S3 keys, so it lives outside the repo
// (/etc/skycam/config.json on the Pi); config.example.json ships instead.
// Every field has a default, so a missing key never means zero.
package config

import (
	"encoding/json"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Duration is a time.Duration that reads "5s" / "250ms" from JSON.
type Duration time.Duration

func (d Duration) D() time.Duration { return time.Duration(d) }

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"5s\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// Rect is a region of the frame in fractions of width and height, so it
// survives a change of resolution.
type Rect struct {
	X0 float64 `json:"x0"`
	Y0 float64 `json:"y0"`
	X1 float64 `json:"x1"`
	Y1 float64 `json:"y1"`
}

type Camera struct {
	// Driver is "sdk" (ZWO's libASICamera2 via purego), "native" (pure Go
	// USB, phase 3) or "fake" (replays JPEGs, for development).
	Driver    string `json:"driver"`
	SDKPath   string `json:"sdk_path"`  // libASICamera2.so
	Label     string `json:"label"`     // overlay name; empty = what the camera reports
	Offset    int    `json:"offset"`    // ASI_OFFSET (black level)
	Bandwidth int    `json:"bandwidth"` // ASI_BANDWIDTHOVERLOAD, percent
	Flip      int    `json:"flip"`      // 0 none, 1 horizontal, 2 vertical, 3 both
	// Snap uses the SDK's snap mode for stills instead of allsky's
	// one-frame video capture, which is the reliable path on the ASI120.
	Snap bool `json:"snap"`

	FakeDir      string `json:"fake_dir"`      // folder of JPEGs to replay
	FakeRealtime bool   `json:"fake_realtime"` // sleep for each exposure
}

// Mode is the exposure envelope for day or night.
type Mode struct {
	Interval    Duration `json:"interval"` // start-to-start cadence
	MinExposure Duration `json:"min_exposure"`
	MaxExposure Duration `json:"max_exposure"`
	MinGain     int      `json:"min_gain"`
	MaxGain     int      `json:"max_gain"`
	Target      float64  `json:"target"` // mean brightness to aim for, 0–255
	Save        bool     `json:"save"`   // keep the frames in images/<night>
}

type Exposure struct {
	// SunAngle is the sun altitude, in degrees, below which it is night.
	// −6° (civil twilight) matches allsky's default.
	SunAngle float64 `json:"sun_angle"`
	Day      Mode    `json:"day"`
	Night    Mode    `json:"night"`
	// Meter is where brightness is measured. Exclude covers the overlay
	// text, the telescope and anything else that is not sky or roof.
	Meter   Rect   `json:"meter"`
	Exclude []Rect `json:"exclude"`
}

type Overlay struct {
	Enabled bool `json:"enabled"`
	// Observatory adds the roof, rain and N.I.N.A. lines at the bottom
	// left. The top-left block always keeps allsky's layout.
	Observatory bool `json:"observatory"`
}

type Observatory struct {
	RoofURL      string   `json:"roof_url"` // rolloff /status.json on port 80, never 11111
	RainURL      string   `json:"rain_url"` // rg11 /status.json on port 80
	NINAURL      string   `json:"nina_url"` // Advanced API base, e.g. http://192.168.1.144:1888
	PollInterval Duration `json:"poll_interval"`
	// StaleAfter is how old a reading may be before it reads unknown.
	StaleAfter Duration `json:"stale_after"`
}

type Analysis struct {
	BandTop    float64 `json:"band_top"`    // skyq's luminance band
	BandBottom float64 `json:"band_bottom"` //
	StarSigma  float64 `json:"star_sigma"`  // detection threshold above background, in σ
	// ClearStars is the star count of a clear, moonless frame. Zero means
	// learn it from recent nights.
	ClearStars int `json:"clear_stars"`
	// SkyZeroPoint turns normalised flux into a relative magnitude. It is
	// not calibrated against an SQM; the number is for trends only.
	SkyZeroPoint float64 `json:"sky_zero_point"`
	BlackLevel   float64 `json:"black_level"` // ADU subtracted before normalising
	Sky          Rect    `json:"sky"`         // region used for stars and sky brightness
}

type Timelapse struct {
	Enabled bool   `json:"enabled"`
	FPS     int    `json:"fps"`
	Codec   string `json:"codec"` // libx264 or h264_v4l2m2m
	CRF     int    `json:"crf"`
	FFmpeg  string `json:"ffmpeg"`
}

type S3 struct {
	Enabled         bool     `json:"enabled"`
	Bucket          string   `json:"bucket"`
	Region          string   `json:"region"`
	Prefix          string   `json:"prefix"`
	AccessKeyID     string   `json:"access_key_id"`
	SecretAccessKey string   `json:"secret_access_key"`
	PublicBaseURL   string   `json:"public_base_url"` // how browsers reach the objects
	LatestEvery     Duration `json:"latest_every"`    // upload image.jpg at most this often
	AllFrames       bool     `json:"all_frames"`
}

type Relay struct {
	URL       string   `json:"url"` // https://skycam.mchugh.au
	Token     string   `json:"token"`
	Every     Duration `json:"every"`      // still upload cadence
	LiveFPS   float64  `json:"live_fps"`   //
	LiveWidth int      `json:"live_width"` // live frames are scaled to this width
}

// Alpaca serves the camera as an ASCOM Alpaca ObservingConditions device,
// so N.I.N.A. can read cloud cover from the Pi. Advisory only.
type Alpaca struct {
	Listen    string   `json:"listen"`    // ":11111"; empty turns the device off
	Discovery bool     `json:"discovery"` // answer Alpaca discovery on UDP 32227
	MaxAge    Duration `json:"max_age"`   // older frames read as unavailable
}

type Live struct {
	// NightLimit caps a live session at night, because live mode pauses
	// still capture and leaves a gap in the timelapse.
	NightLimit Duration `json:"night_limit"`
	// IdleStop ends live mode this long after the last viewer left.
	IdleStop    Duration `json:"idle_stop"`
	MaxExposure Duration `json:"max_exposure"`
}

type Config struct {
	Timezone  string  `json:"timezone"`
	Latitude  float64 `json:"latitude"`  // degrees, south negative
	Longitude float64 `json:"longitude"` // degrees, west negative

	Listen  string `json:"listen"`   // ":80" on the Pi
	DataDir string `json:"data_dir"` // /var/lib/skycam

	// ImagesUser/ImagesPassword guard /images/, which skyq reads with
	// basic auth exactly as it read allsky's.
	ImagesUser     string `json:"images_user"`
	ImagesPassword string `json:"images_password"`

	JPEGQuality     int `json:"jpeg_quality"`
	RetentionNights int `json:"retention_nights"` // frames
	ProductDays     int `json:"product_days"`     // mp4, keogram, startrails, report

	Camera      Camera      `json:"camera"`
	Exposure    Exposure    `json:"exposure"`
	Overlay     Overlay     `json:"overlay"`
	Observatory Observatory `json:"observatory"`
	Analysis    Analysis    `json:"analysis"`
	Timelapse   Timelapse   `json:"timelapse"`
	Live        Live        `json:"live"`
	S3          S3          `json:"s3"`
	Relay       Relay       `json:"relay"`
	Alpaca      Alpaca      `json:"alpaca"`

	MonitorURL    string `json:"monitor_url"`
	MonitorAPIKey string `json:"monitor_api_key"`
}

// Default is the observatory as allsky ran it: an ASI120MM Mini inside the
// roll-off, 5 s at gain 50 at night, a frame about every 20 s.
func Default() Config {
	return Config{
		Timezone:  "Australia/Melbourne",
		Latitude:  -37.0,
		Longitude: 145.0,
		Listen:    ":80",
		DataDir:   "/var/lib/skycam",

		JPEGQuality:     90,
		RetentionNights: 14,
		ProductDays:     365,

		Camera: Camera{
			Driver:    "sdk",
			SDKPath:   "/usr/local/lib/libASICamera2.so",
			Bandwidth: 40,
		},
		Exposure: Exposure{
			SunAngle: -6,
			Day: Mode{
				Interval:    Duration(30 * time.Second),
				MinExposure: Duration(32 * time.Microsecond),
				MaxExposure: Duration(2 * time.Second),
				MinGain:     0,
				MaxGain:     0,
				Target:      100,
				Save:        false,
			},
			Night: Mode{
				Interval:    Duration(20 * time.Second),
				MinExposure: Duration(1 * time.Millisecond),
				MaxExposure: Duration(5 * time.Second),
				MinGain:     50,
				MaxGain:     50,
				Target:      70,
				Save:        true,
			},
			Meter: Rect{0, 0.15, 1, 0.85},
			Exclude: []Rect{
				{0, 0, 0.3, 0.15}, // allsky-style text block
				{0, 0.88, 0.5, 1}, // observatory lines
			},
		},
		Overlay: Overlay{Enabled: true, Observatory: true},
		Observatory: Observatory{
			RoofURL:      "http://192.168.1.142/status.json",
			RainURL:      "http://192.168.1.141/status.json",
			NINAURL:      "http://192.168.1.144:1888",
			PollInterval: Duration(10 * time.Second),
			StaleAfter:   Duration(60 * time.Second),
		},
		Analysis: Analysis{
			BandTop:      0.06,
			BandBottom:   0.30,
			StarSigma:    5,
			SkyZeroPoint: 20,
			Sky:          Rect{0, 0.15, 1, 0.85},
		},
		Timelapse: Timelapse{Enabled: true, FPS: 25, Codec: "libx264", CRF: 23, FFmpeg: "ffmpeg"},
		Live: Live{
			NightLimit:  Duration(10 * time.Minute),
			IdleStop:    Duration(60 * time.Second),
			MaxExposure: Duration(500 * time.Millisecond),
		},
		// allsky's bucket and layout; the allsky-uploader IAM user can write
		// under allsky/ and the bucket policy makes it public.
		S3: S3{
			Bucket:      "deepspaceplace-allsky",
			Region:      "ap-southeast-2",
			Prefix:      "allsky/",
			LatestEvery: Duration(5 * time.Minute),
		},
		Relay: Relay{
			Every:     Duration(30 * time.Second),
			LiveFPS:   2,
			LiveWidth: 960,
		},
		Alpaca: Alpaca{Listen: ":11111", Discovery: true, MaxAge: Duration(5 * time.Minute)},
	}
}

// Load reads path over the defaults, so the file only needs the keys that
// differ. Unknown keys are an error: a typo should not silently fall back
// to a default.
func Load(path string) (Config, error) {
	cfg := Default()
	f, err := os.Open(path)
	if err != nil {
		return cfg, err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, cfg.validate()
}

func (c Config) validate() error {
	var errs []string
	switch c.Camera.Driver {
	case "sdk", "native", "fake":
	default:
		errs = append(errs, fmt.Sprintf("camera.driver %q: want sdk, native or fake", c.Camera.Driver))
	}
	if c.Camera.Driver == "fake" && c.Camera.FakeDir == "" {
		errs = append(errs, "camera.fake_dir is required for the fake driver")
	}
	for name, m := range map[string]Mode{"day": c.Exposure.Day, "night": c.Exposure.Night} {
		if m.MinExposure <= 0 || m.MaxExposure < m.MinExposure {
			errs = append(errs, fmt.Sprintf("exposure.%s: need 0 < min_exposure <= max_exposure", name))
		}
		if m.MaxGain < m.MinGain {
			errs = append(errs, fmt.Sprintf("exposure.%s: max_gain < min_gain", name))
		}
		if m.Interval <= 0 {
			errs = append(errs, fmt.Sprintf("exposure.%s: interval must be positive", name))
		}
	}
	if c.Analysis.BandBottom <= c.Analysis.BandTop {
		errs = append(errs, "analysis: band_bottom must be below band_top")
	}
	if c.S3.Enabled && (c.S3.Bucket == "" || c.S3.AccessKeyID == "" || c.S3.SecretAccessKey == "") {
		errs = append(errs, "s3: bucket, access_key_id and secret_access_key are required when enabled")
	}
	if c.Relay.URL != "" && c.Relay.Token == "" {
		errs = append(errs, "relay: token is required when url is set")
	}
	if len(errs) > 0 {
		return fmt.Errorf("config: %s", strings.Join(errs, "; "))
	}
	return nil
}

// Location returns the observatory's time zone.
func (c Config) Location() (*time.Location, error) {
	if c.Timezone == "" {
		return time.Local, nil
	}
	return time.LoadLocation(c.Timezone)
}

// Paths under DataDir. images/ mirrors allsky's layout so skyq's fetcher
// reads it unchanged.
func (c Config) ImagesDir() string   { return filepath.Join(c.DataDir, "images") }
func (c Config) ProductsDir() string { return filepath.Join(c.DataDir, "products") }
func (c Config) DBPath() string      { return filepath.Join(c.DataDir, "skycam.db") }

// Pixels converts r to a pixel rectangle in a w×h frame.
func (r Rect) Pixels(w, h int) image.Rectangle {
	return image.Rect(int(r.X0*float64(w)), int(r.Y0*float64(h)), int(r.X1*float64(w)), int(r.Y1*float64(h)))
}

// PixelRects converts each rect for a w×h frame.
func PixelRects(rs []Rect, w, h int) []image.Rectangle {
	out := make([]image.Rectangle, len(rs))
	for i, r := range rs {
		out[i] = r.Pixels(w, h)
	}
	return out
}
