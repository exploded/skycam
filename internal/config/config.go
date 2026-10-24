// Package config loads skycam's JSON config. The file holds the /images
// password, the relay token and the S3 keys, so it lives outside the repo
// (/etc/skycam/config.json on the Pi); config.example.json ships instead.
// Every field has a default, so a missing key never means zero.
package config

import (
	"encoding/json"
	"fmt"
	"image"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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

	// MaxFailures is how many captures may fail in a row before skycam
	// exits, so systemd restarts it and reopens the USB device.
	MaxFailures int `json:"max_failures"`
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
	// Deadband is how far, as a fraction, the brightness may stray from the
	// target before the settings change, so the timelapse doesn't flicker.
	Deadband float64 `json:"deadband"`
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
	Timeout    Duration `json:"timeout"` // per request
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
	// SQMOffset is added to the sky magnitude to approximate an SQM reading
	// in mag/arcsec², which the Alpaca device reports as SkyQuality. 0.6
	// puts a clear, moonless 2026-09-02 at 19.2, the published estimate for
	// Donvale; set it from a real SQM reading taken under a clear, moonless
	// sky with the roof open.
	SQMOffset float64 `json:"sqm_offset"`
	Sky       Rect    `json:"sky"` // region used for stars and sky brightness

	// MinDelta is a floor on the star threshold in ADU, so JPEG ringing on
	// a flat frame doesn't count.
	MinDelta float64 `json:"min_delta"`
	// MaxStarArea is the biggest blob, in pixels of a 7×7 box, that counts
	// as a star; bigger is a light, the moon or a roof edge.
	MaxStarArea int `json:"max_star_area"`
	// DarkFrac skips background blocks below this fraction of the sky
	// level: the telescope, roof edge and trees.
	DarkFrac float64 `json:"dark_frac"`
	// HotDecay is how much of a pixel's detection rate carries to the next
	// frame (0.95 remembers about 20 frames); above HotThreshold it is hot.
	HotDecay     float64 `json:"hot_decay"`
	HotThreshold float64 `json:"hot_threshold"`
	// The clear-sky baseline is the BaselinePercentile of the last
	// BaselineWindow dark, roof-open star counts, once there are
	// BaselineMin of them.
	BaselineWindow     int     `json:"baseline_window"`
	BaselineMin        int     `json:"baseline_min"`
	BaselinePercentile float64 `json:"baseline_percentile"`
}

type Timelapse struct {
	Enabled bool   `json:"enabled"`
	FPS     int    `json:"fps"`
	Codec   string `json:"codec"` // libx264 or h264_v4l2m2m
	CRF     int    `json:"crf"`
	FFmpeg  string `json:"ffmpeg"`
	Preset  string `json:"preset"`  // libx264 -preset
	Bitrate string `json:"bitrate"` // h264_v4l2m2m -b:v, e.g. "8M"
}

// Products tunes the keogram, startrails and thumbnail made at dawn.
type Products struct {
	// Frames brighter than StartrailsMaxMean (0–255) are left out of the
	// startrails; fewer than StartrailsMinFrames dark frames makes none.
	StartrailsMaxMean   float64 `json:"startrails_max_mean"`
	StartrailsMinFrames int     `json:"startrails_min_frames"`
	// KeogramColumn is where the keogram slice is taken, as a fraction of
	// the width; 0.5 is the centre.
	KeogramColumn   float64 `json:"keogram_column"`
	KeogramMinWidth int     `json:"keogram_min_width"`
	JPEGQuality     int     `json:"jpeg_quality"`
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
	URL         string   `json:"url"` // https://skycam.mchugh.au
	Token       string   `json:"token"`
	Every       Duration `json:"every"`        // still upload cadence
	LiveFPS     float64  `json:"live_fps"`     //
	LiveWidth   int      `json:"live_width"`   // live frames are scaled to this width
	LiveQuality int      `json:"live_quality"` // JPEG quality of those frames
}

// Alpaca serves the camera as an ASCOM Alpaca ObservingConditions device,
// so N.I.N.A. can read cloud cover from the Pi. Advisory only.
type Alpaca struct {
	Listen    string   `json:"listen"`    // ":11111"; empty turns the device off
	Discovery bool     `json:"discovery"` // answer Alpaca discovery on UDP 32227
	MaxAge    Duration `json:"max_age"`   // older frames read as unavailable
}

// OpenWeatherMap passes ambient conditions through the Alpaca device. An
// empty APIKey turns it off; its cloud cover is never used.
type OpenWeatherMap struct {
	APIKey   string   `json:"api_key"`
	Interval Duration `json:"interval"` // between fetches; OWM updates every 10 min
	MaxAge   Duration `json:"max_age"`  // older readings read as unavailable
}

type Live struct {
	// NightLimit caps a live session at night, because live mode pauses
	// still capture and leaves a gap in the timelapse.
	NightLimit Duration `json:"night_limit"`
	// IdleStop ends live mode this long after the last viewer left.
	IdleStop    Duration `json:"idle_stop"`
	MaxExposure Duration `json:"max_exposure"`
	// BlockFor keeps live mode off this long after a night session hit its
	// limit, so a forgotten tab can't hold the stills off.
	BlockFor    Duration `json:"block_for"`
	JPEGQuality int      `json:"jpeg_quality"`
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

	Camera      Camera         `json:"camera"`
	Exposure    Exposure       `json:"exposure"`
	Overlay     Overlay        `json:"overlay"`
	Observatory Observatory    `json:"observatory"`
	Analysis    Analysis       `json:"analysis"`
	Timelapse   Timelapse      `json:"timelapse"`
	Products    Products       `json:"products"`
	Live        Live           `json:"live"`
	S3          S3             `json:"s3"`
	Relay       Relay          `json:"relay"`
	Alpaca      Alpaca         `json:"alpaca"`
	Weather     OpenWeatherMap `json:"openweathermap"`

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
			Driver:      "sdk",
			SDKPath:     "/usr/local/lib/libASICamera2.so",
			Bandwidth:   40,
			MaxFailures: 10,
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
			Deadband: 0.10,
		},
		Overlay: Overlay{Enabled: true, Observatory: true},
		Observatory: Observatory{
			RoofURL:      "http://192.168.1.142/status.json",
			RainURL:      "http://192.168.1.141/status.json",
			NINAURL:      "http://192.168.1.144:1888",
			PollInterval: Duration(10 * time.Second),
			StaleAfter:   Duration(60 * time.Second),
			Timeout:      Duration(4 * time.Second),
		},
		Analysis: Analysis{
			BandTop:      0.06,
			BandBottom:   0.30,
			StarSigma:    5,
			SkyZeroPoint: 20,
			SQMOffset:    0.6,
			Sky:          Rect{0, 0.15, 1, 0.85},
			MinDelta:     4,
			MaxStarArea:  20,
			DarkFrac:     0.6,
			HotDecay:     0.95,
			HotThreshold: 0.5,
			// About one long night of frames.
			BaselineWindow:     2000,
			BaselineMin:        30,
			BaselinePercentile: 90,
		},
		Timelapse: Timelapse{Enabled: true, FPS: 25, Codec: "libx264", CRF: 23, FFmpeg: "ffmpeg", Preset: "veryfast", Bitrate: "8M"},
		Products:  Products{StartrailsMaxMean: 90, StartrailsMinFrames: 10, KeogramColumn: 0.5, KeogramMinWidth: 1280, JPEGQuality: 90},
		Live: Live{
			NightLimit:  Duration(10 * time.Minute),
			IdleStop:    Duration(60 * time.Second),
			MaxExposure: Duration(500 * time.Millisecond),
			BlockFor:    Duration(30 * time.Minute),
			JPEGQuality: 75,
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
			Every:       Duration(30 * time.Second),
			LiveFPS:     2,
			LiveWidth:   960,
			LiveQuality: 70,
		},
		Alpaca:  Alpaca{Listen: ":11111", Discovery: true, MaxAge: Duration(5 * time.Minute)},
		Weather: OpenWeatherMap{Interval: Duration(10 * time.Minute), MaxAge: Duration(30 * time.Minute)},
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

// X264Presets are libx264's -preset values, fastest first.
var X264Presets = []string{"ultrafast", "superfast", "veryfast", "faster", "fast", "medium", "slow", "slower", "veryslow"}

var reBitrate = regexp.MustCompile(`^[1-9][0-9]*[kKM]?$`)

func (r Rect) valid() bool {
	return r.X0 >= 0 && r.Y0 >= 0 && r.X1 <= 1 && r.Y1 <= 1 && r.X0 < r.X1 && r.Y0 < r.Y1
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
	if c.Latitude < -90 || c.Latitude > 90 || c.Longitude < -180 || c.Longitude > 180 {
		errs = append(errs, "latitude must be within ±90 and longitude within ±180")
	}
	if _, err := c.Location(); err != nil {
		errs = append(errs, fmt.Sprintf("timezone %q: %v", c.Timezone, err))
	}
	for name, q := range map[string]int{"jpeg_quality": c.JPEGQuality, "live.jpeg_quality": c.Live.JPEGQuality,
		"relay.live_quality": c.Relay.LiveQuality, "products.jpeg_quality": c.Products.JPEGQuality} {
		if q < 1 || q > 100 {
			errs = append(errs, name+" must be 1-100")
		}
	}
	for name, f := range map[string]float64{"exposure.deadband": c.Exposure.Deadband, "analysis.dark_frac": c.Analysis.DarkFrac,
		"analysis.hot_decay": c.Analysis.HotDecay, "analysis.hot_threshold": c.Analysis.HotThreshold,
		"products.keogram_column": c.Products.KeogramColumn} {
		if f < 0 || f > 1 {
			errs = append(errs, name+" must be between 0 and 1")
		}
	}
	for name, r := range map[string]Rect{"exposure.meter": c.Exposure.Meter, "analysis.sky": c.Analysis.Sky} {
		if !r.valid() {
			errs = append(errs, name+": need 0 <= x0 < x1 <= 1 and 0 <= y0 < y1 <= 1")
		}
	}
	for i, r := range c.Exposure.Exclude {
		if !r.valid() {
			errs = append(errs, fmt.Sprintf("exposure.exclude[%d]: need 0 <= x0 < x1 <= 1 and 0 <= y0 < y1 <= 1", i))
		}
	}
	for name, n := range map[string]int{"camera.max_failures": c.Camera.MaxFailures, "analysis.max_star_area": c.Analysis.MaxStarArea,
		"analysis.baseline_window": c.Analysis.BaselineWindow, "analysis.baseline_min": c.Analysis.BaselineMin,
		"products.startrails_min_frames": c.Products.StartrailsMinFrames, "products.keogram_min_width": c.Products.KeogramMinWidth} {
		if n < 1 {
			errs = append(errs, name+" must be at least 1")
		}
	}
	if p := c.Analysis.BaselinePercentile; p <= 0 || p > 100 {
		errs = append(errs, "analysis.baseline_percentile must be above 0 and at most 100")
	}
	for name, d := range map[string]Duration{"observatory.poll_interval": c.Observatory.PollInterval,
		"observatory.timeout": c.Observatory.Timeout, "relay.every": c.Relay.Every} {
		if d <= 0 {
			errs = append(errs, name+" must be positive")
		}
	}
	// The ESP32s answer status.json on port 80; 11111 is N.I.N.A.'s Alpaca.
	for name, u := range map[string]string{"observatory.roof_url": c.Observatory.RoofURL, "observatory.rain_url": c.Observatory.RainURL,
		"observatory.nina_url": c.Observatory.NINAURL} {
		if u == "" {
			continue
		}
		pu, err := url.Parse(u)
		switch {
		case err != nil || (pu.Scheme != "http" && pu.Scheme != "https") || pu.Host == "":
			errs = append(errs, name+": want an http:// URL")
		case name != "observatory.nina_url" && pu.Port() == "11111":
			errs = append(errs, name+": the device is read on port 80, not Alpaca's 11111")
		}
	}
	if c.Timelapse.Codec != "libx264" && c.Timelapse.Codec != "h264_v4l2m2m" {
		errs = append(errs, fmt.Sprintf("timelapse.codec %q: want libx264 or h264_v4l2m2m", c.Timelapse.Codec))
	}
	if !slices.Contains(X264Presets, c.Timelapse.Preset) {
		errs = append(errs, fmt.Sprintf("timelapse.preset %q: want one of %s", c.Timelapse.Preset, strings.Join(X264Presets, ", ")))
	}
	if !reBitrate.MatchString(c.Timelapse.Bitrate) {
		errs = append(errs, fmt.Sprintf("timelapse.bitrate %q: want a number with an optional k or M, e.g. 8M", c.Timelapse.Bitrate))
	}
	if len(errs) > 0 {
		slices.Sort(errs) // several checks walk maps
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
