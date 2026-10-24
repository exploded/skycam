package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// The shipped example must load: Load rejects unknown keys, so a renamed
// field would otherwise only show up on the Pi.
func TestExampleLoads(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "config.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Exposure.Night.MaxExposure.D() != 5*time.Second || cfg.Camera.Driver != "sdk" {
		t.Errorf("unexpected values: %+v", cfg.Exposure.Night)
	}
}

func TestUnknownKeyRejected(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	os.WriteFile(p, []byte(`{"camera": {"drvier": "fake"}}`), 0o644)
	if _, err := Load(p); err == nil {
		t.Fatal("typo accepted")
	}
}

func TestPartialFileKeepsDefaults(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	os.WriteFile(p, []byte(`{"listen": ":8080"}`), 0o644)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":8080" || cfg.RetentionNights != 14 || cfg.Exposure.Night.MinGain != 50 {
		t.Errorf("defaults lost: %+v", cfg)
	}
}

func TestSettingsOverConfig(t *testing.T) {
	base := Default()
	base.DataDir = t.TempDir()
	base.ImagesPassword = "from-config"
	os.WriteFile(base.SettingsPath(), []byte(`{
  "exposure": {"night": {"max_exposure": "8s"}},
  "observatory": {"roof_url": "http://192.168.1.50/status.json"},
  "images_password": "from-page",
  "data_dir": "/elsewhere"
}`), 0o644)
	cfg, err := WithSettings(base)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Exposure.Night.MaxExposure.D() != 8*time.Second || cfg.Observatory.RoofURL != "http://192.168.1.50/status.json" {
		t.Errorf("settings not applied: %+v %+v", cfg.Exposure.Night, cfg.Observatory)
	}
	if cfg.ImagesPassword != "from-config" || cfg.DataDir != base.DataDir {
		t.Errorf("locked keys taken from settings: %q %q", cfg.ImagesPassword, cfg.DataDir)
	}
	if cfg.Exposure.Night.MinGain != 50 || cfg.Exposure.Day.MaxExposure.D() != 2*time.Second {
		t.Error("untouched settings lost")
	}
	o := Overrides(base, cfg)
	if len(o) != 2 || o["exposure.night.max_exposure"] != "8s" {
		t.Errorf("overrides %v", o)
	}
}

// A bad settings.json must not stop the camera: config.json alone runs.
func TestBadSettingsIgnored(t *testing.T) {
	for name, body := range map[string]string{
		"invalid": `{"exposure": {"night": {"min_exposure": "10s", "max_exposure": "1s"}}}`,
		"unknown": `{"camera": {"drvier": "fake"}}`,
		"broken":  `{"exposure": `,
		"alpaca":  `{"observatory": {"roof_url": "http://192.168.1.142:11111/status.json"}}`,
	} {
		base := Default()
		base.DataDir = t.TempDir()
		os.WriteFile(base.SettingsPath(), []byte(body), 0o644)
		cfg, err := WithSettings(base)
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
		if !reflect.DeepEqual(cfg, base) {
			t.Errorf("%s: config changed", name)
		}
	}
}

func TestNoSettingsFile(t *testing.T) {
	base := Default()
	base.DataDir = t.TempDir()
	if cfg, err := WithSettings(base); err != nil || !reflect.DeepEqual(cfg, base) {
		t.Errorf("%v", err)
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	base := Default()
	cur := base
	cur.Exposure.Exclude = []Rect{{0, 0, 0.5, 0.2}}
	cur.Live.BlockFor = Duration(5 * time.Minute)
	cur.Analysis.Sky.Y1 = 0.8
	b, err := EncodeSettings(Overrides(base, cur))
	if err != nil {
		t.Fatal(err)
	}
	base.DataDir = t.TempDir()
	cur.DataDir = base.DataDir
	os.WriteFile(base.SettingsPath(), b, 0o644)
	got, err := WithSettings(base)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, cur) {
		t.Errorf("round trip lost changes:\n%s", b)
	}
}
